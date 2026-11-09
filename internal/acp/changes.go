// changes.go 负责 ACP 工作区的「轮前快照 → 轮末变更 → 查看 diff → 还原」。
//
// 设计边界：
//   - git 仓库优先用 git 自身做基线（`git stash create` 产生一个不碰工作区的
//     dangling commit，未跟踪文件另存副本）；非 git 目录退化为目录副本。
//   - 快照有明确上限（文件数/体积/单文件），超限只降级为“能看不能还原”，
//     绝不假装成功；前端会看到 Snapshot=false 与原因。
//   - 还原只处理本轮变更过的路径，并且要求文件在本轮结束后没再被改过，
//     避免覆盖用户自己的后续编辑。暂存区状态不承诺恢复。
package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	snapshotMaxFiles     = 20000
	snapshotMaxCopyBytes = 100 << 20 // 非 git 全量副本 / 未跟踪文件备份的总上限
	snapshotMaxCopyFile  = 5 << 20   // 单文件超过这个大小只记元信息，不备份内容
	snapshotHashMax      = 5 << 20   // 超过这个大小用 size+mtime 判变化
	diffMaxBytes         = 200 << 10 // 单文件 diff 上限
	diffTotalMax         = 2 << 20   // 单轮 diff 总上限（防止一次改几百个文件把 changes.json 撑爆）
	diffMaxLines         = 2000      // 单次逐行 diff 的行数上限
	diffReadMax          = 2 << 20   // 生成 diff 时最多读取的文件大小
)

// 快照/变更里不进入遍历的重目录（git 仓库的忽略规则另算）。
// .friday 是 Friday 自己的落盘目录（交接摘要、会话记录）：它的 .gitignore 是
// “* + !/.gitignore”，所以 .friday/.gitignore 永远以未跟踪文件的形式留在
// git status 里；不跳过它，非 git 项目里它会被备份、甚至当成 agent 改过的文件。
var snapshotSkipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, ".friday": true,
	"node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, ".next": true, "dist": true, "build": true, "target": true,
}

// FileChange 是一个文件的变更摘要；Diff 只在变更详情接口/磁盘元数据里出现，
// 会话投影里保存的是去掉 Diff 的 Summary，避免 acp-threads.json 膨胀。
type FileChange struct {
	Path      string `json:"path"`
	Status    string `json:"status"` // added | modified | deleted
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
	NewHash   string `json:"newHash,omitempty"`
	Diff      string `json:"diff,omitempty"`
}

// TurnChanges 是一轮的改动汇总。Summary() 去掉逐文件 diff 后放进 Thread.Entry。
type TurnChanges struct {
	TurnID     string       `json:"turnId"`
	At         time.Time    `json:"at"`
	VCS        string       `json:"vcs,omitempty"` // git | copy
	Snapshot   bool         `json:"snapshot"`
	Note       string       `json:"note,omitempty"`
	Files      []FileChange `json:"files"`
	Additions  int          `json:"additions,omitempty"`
	Deletions  int          `json:"deletions,omitempty"`
	RevertedAt *time.Time   `json:"revertedAt,omitempty"`
}

// Summary 返回可安全放进会话投影的副本（去掉逐文件 diff）。
func (c *TurnChanges) Summary() *TurnChanges {
	if c == nil {
		return nil
	}
	out := *c
	out.Files = make([]FileChange, len(c.Files))
	for i, f := range c.Files {
		f.Diff = ""
		out.Files[i] = f
	}
	return &out
}

// FileEntry 是工作区文件树的一行。
type FileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"` // 相对工作区根，slash 分隔
	Dir     bool      `json:"dir"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"modTime,omitempty"`
}

type fileMeta struct {
	Size   int64  `json:"size"`
	Mod    int64  `json:"mod"`
	Hash   string `json:"hash,omitempty"`
	Backed bool   `json:"backed,omitempty"` // 内容已备份（可还原/可做逐行 diff）
}

type snapshotMeta struct {
	Kind        string              `json:"kind"` // git | copy
	Workspace   string              `json:"workspace"`
	Baseline    string              `json:"baseline,omitempty"`
	StartHead   string              `json:"startHead,omitempty"`   // 轮前 HEAD：用来分辨轮内是提交还是切了分支
	StartBranch string              `json:"startBranch,omitempty"` // 轮前分支名（detached 时为空）
	Prefix      string              `json:"prefix,omitempty"`      // 工作区在 git 仓库里的路径前缀
	Start       map[string]fileMeta `json:"start,omitempty"`
	Note        string              `json:"note,omitempty"`
}

type turnSnapshot struct {
	dir  string
	meta snapshotMeta
}

// beginTurnSnapshot 在轮次开始前记录工作区基线。失败时返回 nil，调用方按
// “本轮没有快照”处理（不影响 agent 干活）。
func beginTurnSnapshot(workspace, baseDir, threadID, turnID string) (*turnSnapshot, error) {
	if baseDir == "" || threadID == "" || turnID == "" {
		return nil, errors.New("快照目录不可用")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return nil, errors.New("工作区目录不存在或不可读")
	}
	dir := filepath.Join(baseDir, threadID, turnID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &turnSnapshot{dir: dir, meta: snapshotMeta{Workspace: abs}}
	if isGitWorkspace(abs) {
		if err := s.prepareGit(); err == nil {
			return s, nil
		} else {
			// git 基线没建起来（unborn HEAD、stash 失败等）：回退目录副本，
			// 不把半份 git 元数据留给后续还原。
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				return nil, fmt.Errorf("git 快照失败（%v），清理临时目录失败: %w", err, rmErr)
			}
			if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
				return nil, mkErr
			}
			s = &turnSnapshot{dir: dir, meta: snapshotMeta{Workspace: abs}}
			s.meta.Note = "git 快照不可用，已改用目录副本：" + err.Error()
		}
	}
	if err := s.prepareCopy(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := s.writeMeta(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return s, nil
}

func isGitWorkspace(dir string) bool {
	out, err := gitOutput(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true" && gitHasCommit(dir)
}

func gitHasCommit(dir string) bool {
	_, err := gitOutput(dir, "rev-parse", "--verify", "HEAD")
	return err == nil
}

func (s *turnSnapshot) prepareGit() error {
	head, err := gitOutput(s.meta.Workspace, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	baseline := ""
	if out, err := gitOutput(s.meta.Workspace, "-c", "user.name=Friday", "-c", "user.email=friday@local", "stash", "create"); err == nil {
		baseline = strings.TrimSpace(out)
	} else {
		return fmt.Errorf("git stash create: %w", err)
	}
	// stash create 只覆盖跟踪文件：工作区只有未跟踪文件时它本来就该返回空，
	// 这不是失败——未跟踪文件由下面的 Start 备份单独记录，HEAD 就是合法基线。
	// 判据必须带 -uno：不带的话 Friday 自己留下的 .friday/.gitignore 永远让
	// status 非空，于是每个“干净开局”的轮次都会误判成“git 没能生成快照”，
	// 退化成目录副本扫描——既不看 .gitignore，也把切分支当成 agent 改了几百个文件。
	if baseline == "" {
		if dirty, _ := gitOutput(s.meta.Workspace, "status", "--porcelain", "-uno", "--", "."); strings.TrimSpace(dirty) != "" {
			return errors.New("跟踪文件有改动但 git 没能生成快照")
		}
		baseline = strings.TrimSpace(head)
	}
	prefix, _ := gitOutput(s.meta.Workspace, "rev-parse", "--show-prefix")
	branch, _ := gitOutput(s.meta.Workspace, "symbolic-ref", "--short", "-q", "HEAD")
	s.meta.Kind = "git"
	s.meta.Baseline = baseline
	s.meta.StartHead = strings.TrimSpace(head)
	s.meta.StartBranch = strings.TrimSpace(branch)
	s.meta.Prefix = strings.TrimSpace(prefix)
	s.meta.Start = map[string]fileMeta{}

	untracked, err := gitList(s.meta.Workspace, "ls-files", "--others", "--exclude-standard", "-z", "--", ".")
	if err != nil {
		return err
	}
	if len(untracked) > snapshotMaxFiles {
		s.meta.Note = fmt.Sprintf("未跟踪文件超过 %d 个，仅记录前 %d 个", snapshotMaxFiles, snapshotMaxFiles)
		untracked = untracked[:snapshotMaxFiles]
	}
	copied := int64(0)
	unbacked := 0
	for _, rel := range untracked {
		rel, err := cleanRel(rel)
		if err != nil || rel == "" || relHasSkipDir(rel) {
			continue
		}
		src := filepath.Join(s.meta.Workspace, filepath.FromSlash(rel))
		meta, err := fileMetaOf(src)
		if err != nil {
			continue
		}
		if meta.Size <= snapshotMaxCopyFile && copied+meta.Size <= snapshotMaxCopyBytes {
			dst := filepath.Join(s.dir, "untracked", filepath.FromSlash(rel))
			if err := copyRegularFile(src, dst); err == nil {
				meta.Backed = true
				copied += meta.Size
			}
		}
		if !meta.Backed {
			unbacked++
		}
		s.meta.Start[rel] = meta
	}
	if unbacked > 0 {
		s.meta.Note = joinNote(s.meta.Note, fmt.Sprintf("%d 个未跟踪文件超过单文件/总量上限，未备份内容，不能还原", unbacked))
	}
	return s.writeMeta()
}

func (s *turnSnapshot) prepareCopy() error {
	s.meta.Kind = "copy"
	s.meta.Start = map[string]fileMeta{}
	files, note, err := workspaceFileMetas(s.meta.Workspace)
	if err != nil {
		return err
	}
	if note != "" {
		s.meta.Note = joinNote(s.meta.Note, note)
	}
	copied := int64(0)
	unbacked := 0
	for rel, meta := range files {
		if meta.Size <= snapshotMaxCopyFile && copied+meta.Size <= snapshotMaxCopyBytes {
			src := filepath.Join(s.meta.Workspace, filepath.FromSlash(rel))
			dst := filepath.Join(s.dir, "base", filepath.FromSlash(rel))
			if err := copyRegularFile(src, dst); err == nil {
				meta.Backed = true
				copied += meta.Size
			}
		}
		if !meta.Backed {
			unbacked++
		}
		s.meta.Start[rel] = meta
	}
	if unbacked > 0 {
		s.meta.Note = joinNote(s.meta.Note, fmt.Sprintf("%d 个文件超过单文件/总量上限，未备份内容，不能还原", unbacked))
	}
	return nil
}

func (s *turnSnapshot) writeMeta() error {
	b, err := json.MarshalIndent(s.meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "meta.json"), b, 0o600)
}

// Finish 计算本轮改动并落盘 changes.json。即使没有改动也返回一个空汇总，
// 让前端能区分“没改文件”和“没做快照”。
func (s *turnSnapshot) Finish() *TurnChanges {
	ch := &TurnChanges{
		TurnID:   filepath.Base(s.dir),
		At:       time.Now(),
		VCS:      s.meta.Kind,
		Snapshot: true,
	}
	var files map[string]*FileChange
	if s.meta.Kind == "git" {
		files = s.finishGit()
	} else {
		files = s.finishCopy()
	}
	// 轮内切分支会把还原基线换成当前 HEAD，note 也在这一步才补齐：
	// 元数据要落回磁盘，Changes()/Revert() 之后读到的才是算完的口径。
	ch.Note = s.meta.Note
	if err := s.writeMeta(); err != nil {
		ch.Note = joinNote(ch.Note, "快照元数据更新失败："+err.Error())
	}
	for _, f := range files {
		s.fillDiff(f)
	}
	diffBytes := 0
	for _, f := range files {
		if f.Diff != "" {
			if diffBytes+len(f.Diff) > diffTotalMax {
				f.Diff = ""
				f.Error = "本轮 diff 总量超过上限，这个文件只保留变更状态（还原不受影响）"
				continue
			}
			diffBytes += len(f.Diff)
		}
	}
	ch.Files = make([]FileChange, 0, len(files))
	for _, f := range files {
		ch.Files = append(ch.Files, *f)
		ch.Additions += f.Additions
		ch.Deletions += f.Deletions
	}
	sort.Slice(ch.Files, func(i, j int) bool { return ch.Files[i].Path < ch.Files[j].Path })
	if err := writeChanges(s.dir, ch); err != nil {
		ch.Note = joinNote(ch.Note, "变更记录落盘失败："+err.Error())
	}
	return ch
}

func (s *turnSnapshot) finishGit() map[string]*FileChange {
	out := map[string]*FileChange{}
	// 轮内 HEAD 动了要分两种情况：提交（沿同一条线往前）算 agent 干的活，
	// 切分支/回退不算——否则一次 checkout 就把整条分支差异报成“本轮改动几百个文件”。
	switched := false
	baseline := s.meta.Baseline
	if cur, move := s.headMove(); move == "switch" {
		baseline = cur
		switched = true
		s.meta.Baseline = cur // 还原目标也跟着换：只还原本分支上的未提交改动
		s.meta.Note = joinNote(s.meta.Note, fmt.Sprintf("本轮工作区从 %s 切到了 %s（切分支/回退），分支之间的差异不计入本轮改动；切换后如果有提交，那些提交也不在统计里，还原只针对工作区里的未提交改动",
			s.refLabel(s.meta.StartBranch, s.meta.StartHead), s.refLabel(s.currentBranch(), cur)))
	} else if move == "commit" {
		n, _ := gitOutput(s.meta.Workspace, "rev-list", "--count", s.meta.StartHead+".."+cur)
		s.meta.Note = joinNote(s.meta.Note, fmt.Sprintf("本轮提交了 %s 个 commit（%s → %s）；「还原本轮」只恢复文件内容，不会撤销提交",
			strings.TrimSpace(n), shortRef(s.meta.StartHead), shortRef(cur)))
	}
	// --relative 让输出路径相对工作区，而不是仓库根（工作区可能是仓库子目录）。
	raw, err := gitOutput(s.meta.Workspace, "diff", "--name-status", "-z", "--no-renames", "--relative", baseline, "--", ".")
	if err == nil {
		parts := strings.Split(raw, "\x00")
		for i := 0; i+1 < len(parts); i += 2 {
			status := strings.TrimSpace(parts[i])
			if status == "" {
				continue
			}
			rel, err := cleanRel(parts[i+1])
			if err != nil || rel == "" {
				continue
			}
			st := "modified"
			switch {
			case strings.HasPrefix(status, "A"):
				st = "added"
			case strings.HasPrefix(status, "D"):
				st = "deleted"
			}
			out[rel] = &FileChange{Path: rel, Status: st}
		}
	} else {
		s.meta.Note = joinNote(s.meta.Note, "读取 git 差异失败："+err.Error())
	}

	untracked, _ := gitList(s.meta.Workspace, "ls-files", "--others", "--exclude-standard", "-z", "--", ".")
	end := map[string]fileMeta{}
	for _, raw := range untracked {
		rel, err := cleanRel(raw)
		// 跳过目录的口径要和轮前备份一致，否则 .friday/.gitignore 这类
		// 轮前就没进 Start 的文件会被当成“本轮新增”。
		if err != nil || rel == "" || relHasSkipDir(rel) {
			continue
		}
		abs := filepath.Join(s.meta.Workspace, filepath.FromSlash(rel))
		meta, err := fileMetaOf(abs)
		if err != nil {
			continue
		}
		end[rel] = meta
		if start, ok := s.meta.Start[rel]; ok {
			if !sameFileMeta(start, meta) && out[rel] == nil {
				out[rel] = &FileChange{Path: rel, Status: "modified", NewHash: meta.Hash}
			}
			continue
		}
		out[rel] = &FileChange{Path: rel, Status: "added", NewHash: meta.Hash}
	}
	// 轮前是未跟踪、现在不见了 → 视为本轮删除。切了分支时这条不成立：
	// 那些文件可能在新分支里变成跟踪文件，或者本来就不属于这条分支。
	if !switched {
		for rel := range s.meta.Start {
			if _, ok := end[rel]; !ok && out[rel] == nil {
				out[rel] = &FileChange{Path: rel, Status: "deleted"}
			}
		}
	}
	for rel, f := range out {
		abs := filepath.Join(s.meta.Workspace, filepath.FromSlash(rel))
		if meta, err := fileMetaOf(abs); err == nil && f.Status != "deleted" {
			f.NewHash = meta.Hash
		}
	}
	return out
}

// headMove 判断本轮期间 HEAD 有没有移动、怎么移动的：
//   - "commit"：还在同一条分支上，且轮前 HEAD 是当前 HEAD 的祖先，这轮往前提交了；
//   - "switch"：分支名变了，或者轮前 HEAD 已经不是当前 HEAD 的祖先（rebase/reset）；
//   - ""：没变，或读不出来（按没变处理，不阻塞统计主流程）。
func (s *turnSnapshot) headMove() (string, string) {
	if s.meta.StartHead == "" {
		return "", ""
	}
	out, err := gitOutput(s.meta.Workspace, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", ""
	}
	cur := strings.TrimSpace(out)
	if cur == "" || cur == s.meta.StartHead {
		return cur, ""
	}
	// 必须先看分支名：切到一个“包含轮前提交”的下游分支时，祖先判断也会成立，
	// 但那条分支的历史差异不是本轮改的。
	if s.currentBranch() != s.meta.StartBranch {
		return cur, "switch"
	}
	if _, err := gitOutput(s.meta.Workspace, "merge-base", "--is-ancestor", s.meta.StartHead, cur); err == nil {
		return cur, "commit"
	}
	return cur, "switch"
}

func (s *turnSnapshot) currentBranch() string {
	out, _ := gitOutput(s.meta.Workspace, "symbolic-ref", "--short", "-q", "HEAD")
	return strings.TrimSpace(out)
}

func (s *turnSnapshot) refLabel(branch, head string) string {
	if branch != "" {
		return branch
	}
	return shortRef(head)
}

func shortRef(ref string) string {
	if len(ref) > 8 {
		return ref[:8]
	}
	return ref
}

// workspaceFileMetas 给出副本快照要跟踪的文件。git 仓库里必须问 git 而不是裸扫目录：
// 裸扫会把 bin/、生成物、嵌套 worktree 这些被 .gitignore 掉的东西算进来，既撑爆备份
// 额度（结果真正改的文件反而没备份、不能还原），也会把一次 checkout 报成几百个改动。
func workspaceFileMetas(workspace string) (map[string]fileMeta, string, error) {
	if files, note, ok := gitWorkspaceFileMetas(workspace); ok {
		return files, note, nil
	}
	return scanWorkspace(workspace)
}

// gitWorkspaceFileMetas 在“能问 git”时按 git 的口径列文件（跟踪 + 未跟踪且没被忽略）。
// ok=false 表示这里不是可用的 git 仓库，调用方退回目录扫描。
func gitWorkspaceFileMetas(workspace string) (map[string]fileMeta, string, bool) {
	inside, err := gitOutput(workspace, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		return nil, "", false
	}
	paths, err := gitList(workspace, "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", ".")
	if err != nil {
		return nil, "", false
	}
	files := map[string]fileMeta{}
	note := ""
	for _, raw := range paths {
		rel, cerr := cleanRel(raw)
		if cerr != nil || rel == "" || relHasSkipDir(rel) {
			continue
		}
		if len(files) >= snapshotMaxFiles {
			return files, fmt.Sprintf("文件超过 %d 个，仅记录前 %d 个", snapshotMaxFiles, snapshotMaxFiles), true
		}
		meta, merr := fileMetaOf(filepath.Join(workspace, filepath.FromSlash(rel)))
		if merr != nil {
			continue // 读不到（已删除/无权限）：交给“消失即删除”那条判定
		}
		files[rel] = meta
	}
	return files, note, true
}

// relHasSkipDir 只看目录段，末段是文件名：叫 build 的文件不该被当成 build/ 目录跳过。
func relHasSkipDir(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, seg := range parts[:len(parts)-1] {
		if snapshotSkipDirs[seg] {
			return true
		}
	}
	return false
}

func (s *turnSnapshot) finishCopy() map[string]*FileChange {
	out := map[string]*FileChange{}
	end, note, err := workspaceFileMetas(s.meta.Workspace)
	if err != nil {
		s.meta.Note = joinNote(s.meta.Note, "扫描工作区失败："+err.Error())
		return out
	}
	if note != "" {
		s.meta.Note = joinNote(s.meta.Note, note)
	}
	for rel, meta := range end {
		start, ok := s.meta.Start[rel]
		if !ok {
			out[rel] = &FileChange{Path: rel, Status: "added", NewHash: meta.Hash}
		} else if !sameFileMeta(start, meta) {
			out[rel] = &FileChange{Path: rel, Status: "modified", NewHash: meta.Hash}
		}
	}
	for rel := range s.meta.Start {
		if _, ok := end[rel]; !ok {
			out[rel] = &FileChange{Path: rel, Status: "deleted"}
		}
	}
	return out
}

func (s *turnSnapshot) fillDiff(f *FileChange) {
	if f.Error != "" || f.Binary {
		return
	}
	abs := filepath.Join(s.meta.Workspace, filepath.FromSlash(f.Path))
	// git 跟踪的文件优先用 git diff：它处理换行、文件模式与删除更准确。
	if s.meta.Kind == "git" {
		if _, ok := s.meta.Start[f.Path]; !ok {
			if out, err := gitDiffOutput(s.meta.Workspace, "diff", "--no-color", "--no-ext-diff", "--unified=3", s.meta.Baseline, "--", "./"+f.Path); err == nil && strings.TrimSpace(out) != "" {
				if strings.Contains(out, "Binary files") {
					f.Binary = true
					f.Error = "二进制文件，未生成文本 diff"
					return
				}
				s.setDiff(f, out)
				return
			}
		}
	}
	newData, newOK := readCapped(abs, diffReadMax)
	if f.Status == "deleted" {
		newData, newOK = nil, false
	}
	var oldData []byte
	oldOK := false
	if f.Status != "added" {
		oldData, oldOK = s.readBackup(f.Path)
	}
	if !oldOK && f.Status != "added" {
		f.Error = "轮前内容没有备份（文件过大或超出快照上限），只能查看变更状态"
		return
	}
	if !newOK && f.Status != "deleted" {
		f.Error = "读取当前文件失败或文件过大，未生成 diff"
		return
	}
	d, add, del, binary, truncated := lineDiff(oldData, newData, f.Path)
	f.Additions, f.Deletions, f.Binary, f.Truncated = add, del, binary, truncated
	f.Diff = d
	if binary {
		f.Error = "二进制文件，未生成文本 diff"
	}
}

func (s *turnSnapshot) setDiff(f *FileChange, diff string) {
	f.Diff = diff
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			f.Additions++
		case strings.HasPrefix(line, "-"):
			f.Deletions++
		}
	}
	if len(diff) > diffMaxBytes {
		f.Diff = diff[:diffMaxBytes] + "\n…（diff 过长，已截断）"
		f.Truncated = true
	}
}

func (s *turnSnapshot) readBackup(rel string) ([]byte, bool) {
	sub := "base"
	if s.meta.Kind == "git" {
		sub = "untracked"
	}
	b, ok := readCapped(filepath.Join(s.dir, sub, filepath.FromSlash(rel)), diffReadMax)
	return b, ok
}

// Revert 还原本轮变更。只处理本轮实际改过的路径；文件在本轮结束后又被改过
// 就拒绝，避免覆盖用户后续编辑。先整轮预检、再动手，避免做到一半才发现
// 某个文件没有备份，留下半还原状态。
func (s *turnSnapshot) Revert() (*TurnChanges, error) {
	ch, err := readTurnChanges(s.dir)
	if err != nil {
		return nil, err
	}
	if ch.RevertedAt != nil {
		return nil, errors.New("本轮改动已经还原过")
	}
	type action struct {
		rel  string
		kind string // remove | copy | git
		src  string
	}
	actions := make([]action, 0, len(ch.Files))
	for i := range ch.Files {
		f := &ch.Files[i]
		rel, err := cleanRel(f.Path)
		if err != nil || rel == "" {
			return nil, fmt.Errorf("非法路径 %q", f.Path)
		}
		abs := filepath.Join(s.meta.Workspace, filepath.FromSlash(rel))
		switch f.Status {
		case "deleted":
			if _, err := os.Stat(abs); err == nil {
				return nil, fmt.Errorf("%s 在本轮之后又被创建，已停止还原（先处理这个文件再试）", rel)
			}
		case "modified":
			if _, err := os.Stat(abs); err != nil {
				return nil, fmt.Errorf("%s 在本轮之后被删除，已停止还原（先处理这个文件再试）", rel)
			}
		}
		if f.NewHash != "" {
			if meta, err := fileMetaOf(abs); err == nil && meta.Hash != "" && meta.Hash != f.NewHash {
				return nil, fmt.Errorf("%s 在本轮之后又被改过，已停止还原（先处理这个文件再试）", rel)
			}
		}
		if f.Status == "added" {
			actions = append(actions, action{rel: rel, kind: "remove"})
			continue
		}
		if start, ok := s.meta.Start[rel]; ok && start.Backed {
			sub := "base"
			if s.meta.Kind == "git" {
				sub = "untracked"
			}
			src := filepath.Join(s.dir, sub, filepath.FromSlash(rel))
			if info, err := os.Stat(src); err != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s 的轮前副本不可用，无法还原", rel)
			}
			actions = append(actions, action{rel: rel, kind: "copy", src: src})
			continue
		}
		if s.meta.Kind == "git" {
			tracked, err := s.gitHasPath(rel)
			if err != nil {
				return nil, err
			}
			if !tracked {
				actions = append(actions, action{rel: rel, kind: "remove"})
				continue
			}
			actions = append(actions, action{rel: rel, kind: "git"})
			continue
		}
		return nil, fmt.Errorf("%s 没有可用的轮前副本，无法还原", rel)
	}

	for _, a := range actions {
		abs := filepath.Join(s.meta.Workspace, filepath.FromSlash(a.rel))
		switch a.kind {
		case "remove":
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("删除 %s 失败: %w", a.rel, err)
			}
		case "copy":
			if err := copyRegularFile(a.src, abs); err != nil {
				return nil, fmt.Errorf("恢复 %s 失败: %w", a.rel, err)
			}
		case "git":
			if _, err := gitOutput(s.meta.Workspace, "checkout", s.meta.Baseline, "--", "./"+a.rel); err != nil {
				return nil, fmt.Errorf("恢复 %s 失败: %w", a.rel, err)
			}
		}
	}
	now := time.Now()
	ch.RevertedAt = &now
	if err := writeChanges(s.dir, ch); err != nil {
		return nil, err
	}
	return ch, nil
}

func (s *turnSnapshot) gitHasPath(rel string) (bool, error) {
	repoPath := s.meta.Prefix + rel
	_, err := gitOutput(s.meta.Workspace, "cat-file", "-e", s.meta.Baseline+":"+repoPath)
	if err == nil {
		return true, nil
	}
	// cat-file 对“不存在”返回非零；区分真正的 git 故障与不存在。
	if _, statErr := gitOutput(s.meta.Workspace, "rev-parse", "--is-inside-work-tree"); statErr != nil {
		return false, err
	}
	return false, nil
}

// ---------- 对外查询（Manager 用） ----------

func (m *Manager) snapshotBase() string {
	// 优先跟着线程投影文件走：生产里它是 <数据目录>/acp-threads.json，
	// 快照落在 <数据目录>/acp/snapshots；测试用临时 threads.json 时也自然隔离。
	if m.path != "" {
		return filepath.Join(filepath.Dir(m.path), "acp", "snapshots")
	}
	if m.registryBase != "" {
		return filepath.Join(m.registryBase, "snapshots")
	}
	return ""
}

// Files 列出某会话工作区目录下的直接子项（懒加载，不递归）。
func (m *Manager) Files(threadID, rel string) (entries []FileEntry, fullPath string, truncated bool, err error) {
	m.mu.Lock()
	th, ok := m.threads[threadID]
	if !ok {
		m.mu.Unlock()
		return nil, "", false, errors.New("未找到该会话")
	}
	root := th.Cwd
	m.mu.Unlock()
	if strings.TrimSpace(root) == "" {
		return nil, "", false, errors.New("该会话没有工作目录")
	}
	return listWorkspaceFiles(root, rel)
}

// Changes 读取某一轮的完整改动（含逐文件 diff）。
func (m *Manager) Changes(threadID, turnID string) (*TurnChanges, error) {
	turnID, err := safeTurnID(turnID)
	if err != nil {
		return nil, err
	}
	base := m.snapshotBase()
	if base == "" {
		return nil, errors.New("快照目录不可用")
	}
	m.mu.Lock()
	_, ok := m.threads[threadID]
	m.mu.Unlock()
	if !ok {
		return nil, errors.New("未找到该会话")
	}
	dir := filepath.Join(base, threadID, turnID)
	ch, err := readTurnChanges(dir)
	if err != nil {
		return nil, err
	}
	return ch, nil
}

// Revert 还原某一轮改动。
func (m *Manager) Revert(threadID, turnID string) (*TurnChanges, error) {
	turnID, err := safeTurnID(turnID)
	if err != nil {
		return nil, err
	}
	base := m.snapshotBase()
	if base == "" {
		return nil, errors.New("快照目录不可用")
	}
	m.mu.Lock()
	th, ok := m.threads[threadID]
	if !ok {
		m.mu.Unlock()
		return nil, errors.New("未找到该会话")
	}
	cwd := th.Cwd
	m.mu.Unlock()
	if m.workspaceBusy(cwd) {
		return nil, errors.New("同一目录还有别的会话在跑，等它结束再还原")
	}
	dir := filepath.Join(base, threadID, turnID)
	meta, err := readSnapshotMeta(dir)
	if err != nil {
		return nil, err
	}
	s := &turnSnapshot{dir: dir, meta: meta}
	ch, err := s.Revert()
	if err != nil {
		return nil, err
	}
	// 投影里的摘要同步标记已还原，刷新页面后按钮状态不会“复活”。
	m.mu.Lock()
	if th, ok := m.threads[threadID]; ok {
		for i := range th.Messages {
			if th.Messages[i].Changes != nil && th.Messages[i].Changes.TurnID == turnID {
				th.Messages[i].Changes.RevertedAt = ch.RevertedAt
			}
		}
		if serr := m.saveLocked(); serr != nil {
			log.Printf("[智能体] 还原后投影落盘失败: %v", serr)
		}
	}
	m.mu.Unlock()
	return ch, nil
}

func (m *Manager) workspaceBusy(cwd string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lv := range m.live {
		if lv.proc != nil && lv.proc.cwd == cwd && lv.takeSink() != nil {
			return true
		}
	}
	return false
}

// pruneSnapshots 只保留最近 keep 轮快照：再老的 diff 已无还原需求，
// 但会话投影里的摘要仍保留（UI 会显示“快照已清理”）。
func (m *Manager) pruneSnapshots(threadID string, keep int) {
	if keep <= 0 {
		return
	}
	base := filepath.Join(m.snapshotBase(), threadID)
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) <= keep {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		ii, ierr := entries[i].Info()
		jj, jerr := entries[j].Info()
		if ierr != nil || jerr != nil {
			return entries[i].Name() > entries[j].Name()
		}
		return ii.ModTime().After(jj.ModTime())
	})
	for _, e := range entries[keep:] {
		_ = os.RemoveAll(filepath.Join(base, e.Name()))
	}
}

// ---------- 文件树 ----------

func listWorkspaceFiles(root, rel string) ([]FileEntry, string, bool, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", false, err
	}
	rel, err = cleanRel(rel)
	if err != nil {
		return nil, "", false, err
	}
	target := filepath.Join(rootAbs, filepath.FromSlash(rel))
	if !withinRoot(rootAbs, target) {
		return nil, "", false, errors.New("路径越界")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, "", false, fmt.Errorf("读取目录失败: %w", err)
	}
	out := make([]FileEntry, 0, len(entries))
	truncated := false
	for _, e := range entries {
		if e.Name() == ".git" || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if len(out) >= 1000 {
			truncated = true
			break
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		child := path.Join(rel, e.Name())
		if rel == "" {
			child = e.Name()
		}
		item := FileEntry{Name: e.Name(), Path: child, Dir: e.IsDir()}
		if !e.IsDir() {
			item.Size = info.Size()
			item.ModTime = info.ModTime()
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, target, truncated, nil
}

// ---------- 扫描与工具 ----------

func scanWorkspace(root string) (map[string]fileMeta, string, error) {
	files := map[string]fileMeta{}
	note := ""
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 权限问题的目录跳过，不中断整轮
		}
		if d.IsDir() {
			if p != root && snapshotSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		meta, merr := fileMetaOf(p)
		if merr != nil {
			return nil
		}
		if len(files) >= snapshotMaxFiles {
			note = fmt.Sprintf("文件超过 %d 个，仅记录前 %d 个", snapshotMaxFiles, snapshotMaxFiles)
			return filepath.SkipAll
		}
		files[rel] = meta
		return nil
	})
	return files, note, err
}

func fileMetaOf(p string) (fileMeta, error) {
	info, err := os.Stat(p)
	if err != nil {
		return fileMeta{}, err
	}
	meta := fileMeta{Size: info.Size(), Mod: info.ModTime().UnixNano()}
	if info.Size() <= snapshotHashMax {
		if b, err := os.ReadFile(p); err == nil {
			sum := sha256.Sum256(b)
			meta.Hash = hex.EncodeToString(sum[:])
		}
	}
	return meta, nil
}

func sameFileMeta(a, b fileMeta) bool {
	if a.Hash != "" && b.Hash != "" {
		return a.Hash == b.Hash
	}
	return a.Size == b.Size && a.Mod == b.Mod
}

func readCapped(p string, max int64) ([]byte, bool) {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Size() > max {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	return b, true
}

func copyRegularFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("不是普通文件")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func cleanRel(rel string) (string, error) {
	rel = strings.TrimSpace(filepath.ToSlash(rel))
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || rel == "." {
		return "", nil
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("路径越界")
	}
	return clean, nil
}

// safeTurnID 只接受单段目录名：turnID 来自请求，不能带分隔符/上跳。
func safeTurnID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return "", errors.New("非法的轮次 ID")
	}
	return id, nil
}

func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "；" + b
	}
}

func writeChanges(dir string, ch *TurnChanges) error {
	b, err := json.MarshalIndent(ch, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "changes.json"), b, 0o600)
}

func readTurnChanges(dir string) (*TurnChanges, error) {
	b, err := os.ReadFile(filepath.Join(dir, "changes.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("这一轮的改动快照已清理或不存在")
		}
		return nil, err
	}
	var ch TurnChanges
	if err := json.Unmarshal(b, &ch); err != nil {
		return nil, errors.New("改动记录损坏")
	}
	return &ch, nil
}

func readSnapshotMeta(dir string) (snapshotMeta, error) {
	var meta snapshotMeta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return meta, errors.New("这一轮的快照已清理或不存在")
		}
		return meta, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, errors.New("快照元数据损坏")
	}
	return meta, nil
}

// ---------- git 调用 ----------

func gitOutput(dir string, args ...string) (string, error) {
	out, _, err := gitRun(dir, args...)
	return out, err
}

func gitDiffOutput(dir string, args ...string) (string, error) {
	out, _, err := gitRun(dir, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.TrimSpace(out) != "" {
			return out, nil // git diff 有差异时退出码为 1，正常
		}
		return "", err
	}
	return out, nil
}

func gitRun(dir string, args ...string) (string, string, error) {
	return gitRunCtx(context.Background(), dir, args...)
}

// gitRunCtx 是 gitRun 的可取消版本：Git 面板的写操作要能被超时/用户点停止杀掉。
// 环境处理与 gitRun 完全一致（清掉 GIT_DIR 等指向别的仓库的变量）。
func gitRunCtx(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// 不让调用方环境里的 GIT_DIR/GIT_WORK_TREE 把命令指到别的仓库；
	// 其他 git 配置（身份、ignore、credential）照常继承。
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
			strings.HasPrefix(kv, "GIT_INDEX_FILE=") || strings.HasPrefix(kv, "GIT_OBJECT_DIRECTORY=") ||
			strings.HasPrefix(kv, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func gitList(dir string, args ...string) ([]string, error) {
	out, err := gitOutput(dir, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	list := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			list = append(list, p)
		}
	}
	return list, nil
}

// ---------- 逐行 diff（无外部依赖） ----------

type diffOp struct {
	kind byte // ' ' | '-' | '+'
	text string
}

func lineDiff(oldData, newData []byte, label string) (diff string, add, del int, binary, truncated bool) {
	if looksBinary(oldData) || looksBinary(newData) {
		return "", 0, 0, true, false
	}
	oldLines := splitDiffLines(oldData)
	newLines := splitDiffLines(newData)
	if len(oldLines) > diffMaxLines || len(newLines) > diffMaxLines {
		truncated = true
		if len(oldLines) > diffMaxLines {
			oldLines = oldLines[:diffMaxLines]
		}
		if len(newLines) > diffMaxLines {
			newLines = newLines[:diffMaxLines]
		}
	}
	ops := diffOps(oldLines, newLines)
	for _, op := range ops {
		switch op.kind {
		case '+':
			add++
		case '-':
			del++
		}
	}
	diff = renderUnified(ops, label, oldLines, newLines)
	if len(diff) > diffMaxBytes {
		diff = diff[:diffMaxBytes] + "\n…（diff 过长，已截断）"
		truncated = true
	}
	if truncated {
		diff += "\n…（文件超过 " + fmt.Sprint(diffMaxLines) + " 行，仅比较前 " + fmt.Sprint(diffMaxLines) + " 行）"
	}
	return diff, add, del, false, truncated
}

func splitDiffLines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func looksBinary(b []byte) bool {
	n := len(b)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	dp := make([][]uint16, n+1)
	for i := range dp {
		dp[i] = make([]uint16, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	rev := make([]diffOp, 0, n+m)
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1]:
			rev = append(rev, diffOp{' ', a[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			rev = append(rev, diffOp{'+', b[j-1]})
			j--
		default:
			rev = append(rev, diffOp{'-', a[i-1]})
			i--
		}
	}
	for l, r := 0, len(rev)-1; l < r; l, r = l+1, r-1 {
		rev[l], rev[r] = rev[r], rev[l]
	}
	return rev
}

func renderUnified(ops []diffOp, label string, oldLines, newLines []string) string {
	if len(ops) == 0 {
		return ""
	}
	const context = 3
	changes := make([]int, 0)
	for i, op := range ops {
		if op.kind != ' ' {
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", label, label)
	oldNo, newNo := 1, 1
	// 先给每个 op 标注它在旧/新文件中的起始行号。
	type pos struct{ old, new int }
	posOf := make([]pos, len(ops))
	for i, op := range ops {
		posOf[i] = pos{old: oldNo, new: newNo}
		switch op.kind {
		case ' ':
			oldNo++
			newNo++
		case '-':
			oldNo++
		case '+':
			newNo++
		}
	}
	for ci := 0; ci < len(changes); {
		start := changes[ci] - context
		if start < 0 {
			start = 0
		}
		end := changes[ci] + context + 1
		if end > len(ops) {
			end = len(ops)
		}
		ci++
		for ci < len(changes) && changes[ci]-context <= end {
			end = changes[ci] + context + 1
			if end > len(ops) {
				end = len(ops)
			}
			ci++
		}
		oldCount, newCount := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				oldCount++
			}
			if op.kind != '-' {
				newCount++
			}
		}
		oldStart, newStart := posOf[start].old, posOf[start].new
		if oldCount == 0 {
			oldStart--
		}
		if newCount == 0 {
			newStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, op := range ops[start:end] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
			if out.Len() >= diffMaxBytes {
				return out.String()
			}
		}
	}
	return out.String()
}
