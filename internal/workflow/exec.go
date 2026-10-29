package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	vxexec "github.com/capyflow/vortexagent/tools/exec"

	"github.com/capyflow/friday/internal/approval"
)

// 输出与扫描上限：与框架 exec 工具同量级，防止一个脚本把内存吃掉。
const (
	defaultMaxOutput   = 100 << 10 // 单条命令 stdout/stderr 各 100KB
	defaultReadCap     = 1 << 20   // files.read 单文件 1MB
	defaultGlobCap     = 2000      // 命中过多直接拒绝而不是截断（对齐 ZCode：静默的部分视图更危险）
	defaultGrepHits    = 2000
	defaultGrepBytes   = 256 << 10
	defaultGitDiffCap  = 512 << 10
	defaultGitLogCount = 20
	defaultGitLogMax   = 100
	defaultRunTimeout  = 5 * time.Minute
	absoluteMaxTimeout = 30 * time.Minute
)

// AskRequest 是一次子代理委派。
type AskRequest struct {
	// Agent 是脚本里的具名子代理（空 = 匿名）。名字同时是会话身份：
	// 同名多次 ask 落在同一个独立会话里，因此宿主侧的 FIFO 语义由 Go 的 per-key 锁给出。
	Agent string
	// Persona 是 agent() 创建时冻结的人格，形如 {"system":"..."} 或直接一个字符串。
	Persona json.RawMessage
	// Seq 是该子代理上的第几次 ask（从 1 起），用于轨迹对齐。
	Seq int
	// Instructions 是本轮任务全文。
	Instructions string
	// Schema 非空时，子代理必须交出符合该 JSON Schema 的结构化结果。
	// raw ask（无类型参数）不带这个字段；字面量 "null" 同样按无处理。
	Schema json.RawMessage
}

// WantsSchema 报告这次委派是否要求结构化结果。
func (r AskRequest) WantsSchema() bool {
	s := strings.TrimSpace(string(r.Schema))
	return s != "" && s != "null"
}

// PersonaText 取出人格里的系统提示词。
func (r AskRequest) PersonaText() string {
	if len(r.Persona) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r.Persona, &s); err == nil {
		return s
	}
	var obj struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal(r.Persona, &obj); err == nil {
		return obj.System
	}
	return ""
}

// AskReply 是子代理的回答。Typed 时 Value 为已校验的 JSON；否则 Text 为最终文本。
type AskReply struct {
	Text  string
	Value json.RawMessage
}

// Typed 返回给宿主的 result 载荷形态是否为结构化值。
func (r AskReply) Typed() bool { return len(r.Value) > 0 }

// WorldResult 是一条命令的结果。非零退出码是值，不是错误——脚本按 exitCode 分支。
type WorldResult struct {
	ExitCode  int    `json:"exitCode"`
	Stdout    string `json:"stdout"`
	Truncated bool   `json:"-"`
	Stderr    string `json:"stderr"`
}

// GrepMatch 是一条命中行。
type GrepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GitStatus 是工作区状态。
type GitStatus struct {
	Branch    string   `json:"branch,omitempty"`
	Clean     bool     `json:"clean"`
	Staged    []string `json:"staged"`
	Unstaged  []string `json:"unstaged"`
	Untracked []string `json:"untracked"`
}

// GitCommit 是一条提交。
type GitCommit struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

// Executor 履约脚本发起的全部跨界调用。
//
// 实现方的硬性要求：每一次调用都必须过权限层与记账，脚本没有任何直接触达机器的路径；
// 也不许用 Registry.Call 之类的裸调旁路 Hooks。
type Executor interface {
	Ask(ctx context.Context, req AskRequest) (AskReply, error)
	WorldRun(ctx context.Context, cmd string, argv []string, timeout time.Duration) (WorldResult, error)
	FilesGlob(ctx context.Context, pattern string) ([]string, error)
	FilesRead(ctx context.Context, path string) (string, error)
	FilesGrep(ctx context.Context, pattern, glob string) ([]GrepMatch, error)
	GitChangedFiles(ctx context.Context, base string) ([]string, error)
	GitDiff(ctx context.Context, base, path string) (string, error)
	GitStatus(ctx context.Context) (GitStatus, error)
	GitLog(ctx context.Context, count int) ([]GitCommit, error)
}

// Local 是基于工作区文件系统的 Executor 实现。
//
// 子代理那一侧只留一个 Asker 注入点：一期竖切用它接假实现，T3 换成走 Brain.AskWith
// 的真实实现（那条实现才带权限与记账）。
type Local struct {
	// Root 是工作区根目录：files.*、git.* 与 world.run 的 cwd。
	Root string
	// Guard 决定一条命令能不能跑；nil 时用毁灭性黑名单。
	Guard func(cmdline string) error
	// Asker 执行一轮子代理对话。
	Asker func(ctx context.Context, req AskRequest) (AskReply, error)
	// MaxOutput / DefaultTimeout / MaxTimeout 为 0 时取包内默认。
	MaxOutput      int
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

// GuardDenyPatterns 是黑名单兜底：与权限层用同一个匹配器，避免两套语义。
func GuardDenyPatterns(cmdline string) error {
	if strings.TrimSpace(cmdline) == "" {
		return errors.New("命令为空")
	}
	args := map[string]any{"command": cmdline}
	for _, p := range approval.DenyPatterns() {
		if vxexec.CommandDenyMatcher(p, args) {
			return fmt.Errorf("命令命中毁灭性黑名单：%s", p)
		}
	}
	return nil
}

func (l *Local) guard(cmdline string) error {
	if l.Guard != nil {
		return l.Guard(cmdline)
	}
	return GuardDenyPatterns(cmdline)
}

func (l *Local) maxOutput() int {
	if l.MaxOutput > 0 {
		return l.MaxOutput
	}
	return defaultMaxOutput
}

func (l *Local) timeout(d time.Duration) time.Duration {
	maxT := l.MaxTimeout
	if maxT <= 0 {
		maxT = absoluteMaxTimeout
	}
	def := l.DefaultTimeout
	if def <= 0 {
		def = defaultRunTimeout
	}
	if d <= 0 {
		d = def
	}
	if d > maxT {
		d = maxT
	}
	return d
}

// Ask 转发给注入的 Asker。
func (l *Local) Ask(ctx context.Context, req AskRequest) (AskReply, error) {
	if l.Asker == nil {
		return AskReply{}, errors.New("本子切未接入子代理执行（见 docs/WORKFLOW.md 第十一节 T3）")
	}
	return l.Asker(ctx, req)
}

// WorldRun 以固定 argv 执行命令，绝不经过 shell：脚本里的命令名是字面量，参数是数组元素，
// 因此不存在管道/重定向/变量展开。非零退出码作为结果返回，不报错。
func (l *Local) WorldRun(ctx context.Context, cmd string, argv []string, timeout time.Duration) (WorldResult, error) {
	line := strings.Join(append([]string{cmd}, argv...), " ")
	if err := l.guard(line); err != nil {
		return WorldResult{}, err
	}
	path, err := exec.LookPath(cmd)
	if err != nil {
		return WorldResult{}, fmt.Errorf("命令不存在：%s", cmd)
	}

	cctx, cancel := context.WithTimeout(ctx, l.timeout(timeout))
	defer cancel()
	c := exec.CommandContext(cctx, path, argv...)
	c.Dir = l.Root
	var out, errBuf cappedBuffer
	out.limit = l.maxOutput()
	errBuf.limit = l.maxOutput()
	c.Stdout, c.Stderr = &out, &errBuf
	runErr := c.Run()

	res := WorldResult{Stdout: out.String(), Stderr: errBuf.String()}
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		return res, fmt.Errorf("命令超时（%s）：%s", timeout, cmd)
	case runErr == nil:
		res.ExitCode = 0
	case c.ProcessState != nil:
		res.ExitCode = c.ProcessState.ExitCode()
	default:
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return res, fmt.Errorf("命令未能执行：%w", runErr)
		}
		res.ExitCode = ee.ExitCode()
	}
	return res, nil
}

// relPath 把工作区内的相对路径解析成绝对路径，并确保没跑出 Root。
// 脚本给的路径是不可信输入：符号链接与 .. 一律拒绝。
func (l *Local) relPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("路径为空")
	}
	if strings.HasPrefix(p, string(filepath.Separator)) || filepath.IsAbs(p) {
		return "", fmt.Errorf("只接受工作区相对路径，得到：%s", p)
	}
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("路径不允许 ..：%s", p)
	}
	clean := filepath.Clean(p)
	abs := filepath.Join(l.Root, clean)
	rootAbs, err := filepath.Abs(l.Root)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) && abs != rootAbs {
		return "", fmt.Errorf("路径越出工作区：%s", p)
	}
	info, err := os.Lstat(abs)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("拒绝符号链接：%s", p)
	}
	return abs, nil
}

// FilesGlob 返回匹配的工作区相对路径（排序）。命中过多直接拒绝，不做静默截断。
func (l *Local) FilesGlob(ctx context.Context, pattern string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.Contains(pattern, "..") {
		return nil, errors.New("glob 不允许 ..")
	}
	fsys := os.DirFS(l.Root)
	limit := defaultGlobCap
	var matches []string
	walkErr := doublestar.GlobWalk(fsys, pattern, func(path string, d fs.DirEntry) error {
		if d == nil || d.IsDir() {
			return nil
		}
		matches = append(matches, path)
		if len(matches) > limit {
			return fmt.Errorf("glob 命中超过 %d 个文件，请收窄模式：%s", limit, pattern)
		}
		return nil
	}, doublestar.WithFilesOnly(), doublestar.WithNoFollow())
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(matches)
	return matches, nil
}

// FilesRead 读一个工作区文件为 UTF-8 文本。
func (l *Local) FilesRead(ctx context.Context, path string) (string, error) {
	abs, err := l.relPath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("文件读不到：%s", path)
	}
	if info.IsDir() {
		return "", fmt.Errorf("那是一个目录：%s", path)
	}
	if info.Size() > defaultReadCap {
		return "", fmt.Errorf("文件超过 %d 字节，请交给子代理分段处理：%s（%d 字节）", defaultReadCap, path, info.Size())
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// FilesGrep 用正则扫工作区内容。
//
// 注意：Go 的 regexp 是 RE2，不支持后向引用与 lookbehind——写脚本时按 RE2 语法即可，
// 这一点与 ZCode（ripgrep）不同，编写契约里要说明。
func (l *Local) FilesGrep(ctx context.Context, pattern, glob string) ([]GrepMatch, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("grep 正则无效：%w", err)
	}
	if glob == "" {
		glob = "**"
	}
	var (
		out   []GrepMatch
		total int
	)
	walkErr := filepath.WalkDir(l.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 读不到的目录跳过，不让整个 grep 失败
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || (strings.HasPrefix(name, ".") && p != l.Root) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(l.Root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		ok, err := doublestar.Match(glob, rel)
		if err != nil || !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > defaultReadCap {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || !isProbablyText(b) {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !re.MatchString(line) {
				continue
			}
			out = append(out, GrepMatch{Path: rel, Line: i + 1, Text: truncate(line, 400)})
			total += len(line)
			if len(out) > defaultGrepHits || total > defaultGrepBytes {
				return fmt.Errorf("grep 命中超过 %d 条或 %d 字节，请收窄模式或 glob：%s / %s",
					defaultGrepHits, defaultGrepBytes, pattern, glob)
			}
		}
		return nil
	})
	if walkErr != nil {
		// 超限与 ctx 取消都从 WalkDir 的错误里冒出来，不做静默降级
		return nil, walkErr
	}
	return out, nil
}

func isProbablyText(b []byte) bool {
	n := len(b)
	if n > 4096 {
		n = 4096
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return false
		}
	}
	return true
}

// git 执行一次只读 git 命令。参数一律走 argv，不经 shell；值里以 - 开头的一律拒绝，
// 免得 "base" 变成 git 选项。
func (l *Local) git(ctx context.Context, args ...string) (string, error) {
	for i, a := range args {
		if i > 0 && strings.HasPrefix(a, "-") {
			return "", fmt.Errorf("git 参数不允许以 - 开头：%q", a)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, l.timeout(0))
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = l.Root
	var out cappedBuffer
	out.limit = defaultGitDiffCap
	cmd.Stdout = &out
	var errBuf cappedBuffer
	errBuf.limit = 8 << 10
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := errBuf.String()
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s 失败：%s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

// GitChangedFiles：无 base 时是"与 HEAD 相比改过 + 未跟踪"，有 base 时与该 ref 比。
func (l *Local) GitChangedFiles(ctx context.Context, base string) ([]string, error) {
	if base != "" {
		out, err := l.git(ctx, "diff", "--name-only", base, "--")
		return splitLines(out), err
	}
	st, err := l.GitStatus(ctx)
	if err != nil {
		return nil, err
	}
	all := append(append(append([]string{}, st.Staged...), st.Unstaged...), st.Untracked...)
	return dedupeSorted(all), nil
}

// GitDiff 返回 unified diff，可收窄到一个路径。
func (l *Local) GitDiff(ctx context.Context, base, path string) (string, error) {
	if base == "" {
		base = "HEAD"
	}
	args := []string{"diff", base, "--"}
	if path != "" {
		rel, err := l.relPath(path)
		if err != nil {
			return "", err
		}
		rp, err := filepath.Rel(l.Root, rel)
		if err != nil {
			return "", err
		}
		args = append(args, filepath.ToSlash(rp))
	}
	out, err := l.git(ctx, args...)
	if len(out) >= defaultGitDiffCap {
		return "", fmt.Errorf("diff 超过 %d 字节，请收窄 base 或 path", defaultGitDiffCap)
	}
	return out, err
}

// GitStatus 解析 `git status --porcelain -b`。
func (l *Local) GitStatus(ctx context.Context) (GitStatus, error) {
	out, err := l.git(ctx, "status", "--porcelain=v1", "-b", "--untracked-files=all")
	if err != nil {
		return GitStatus{}, err
	}
	st := GitStatus{Staged: []string{}, Unstaged: []string{}, Untracked: []string{}}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			// ## main...origin/main 或 ## HEAD (no branch)
			body := strings.TrimPrefix(line, "## ")
			if strings.HasPrefix(body, "HEAD") {
				continue
			}
			st.Branch = strings.Fields(body)[0]
			continue
		}
		if len(line) < 4 {
			continue
		}
		x, y := line[1], line[2]
		p := strings.TrimSpace(line[3:])
		if idx := strings.Index(p, " -> "); idx >= 0 { // 重命名条目取新名字
			p = p[idx+len(" -> "):]
		}
		switch {
		case x == '?' && y == '?':
			st.Untracked = append(st.Untracked, p)
		case x == 'U' || y == 'U':
			st.Unstaged = append(st.Unstaged, p)
		case y != ' ' && x != ' ':
			st.Staged = append(st.Staged, p)
			st.Unstaged = append(st.Unstaged, p)
		case y != ' ':
			st.Unstaged = append(st.Unstaged, p)
		case x != ' ':
			st.Staged = append(st.Staged, p)
		}
	}
	st.Clean = len(st.Staged) == 0 && len(st.Unstaged) == 0 && len(st.Untracked) == 0
	return st, nil
}

// GitLog 返回最近若干条提交。
func (l *Local) GitLog(ctx context.Context, count int) ([]GitCommit, error) {
	if count <= 0 {
		count = defaultGitLogCount
	}
	if count > defaultGitLogMax {
		return nil, fmt.Errorf("git.log 最多 %d 条，得到 %d", defaultGitLogMax, count)
	}
	out, err := l.git(ctx, "log", fmt.Sprintf("-n%d", count), "--pretty=format:%H\x1f%s\x1f%an\x1f%aI")
	if err != nil {
		return nil, err
	}
	var commits []GitCommit
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\x1f")
		if len(fields) < 4 {
			continue
		}
		commits = append(commits, GitCommit{Hash: fields[0], Subject: fields[1], Author: fields[2], Date: fields[3]})
	}
	return commits, nil
}

// cappedBuffer 收集输出到上限后停止，避免一次命令撑爆内存。
type cappedBuffer struct {
	limit    int
	buf      strings.Builder
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 && b.buf.Len()+len(p) > b.limit {
		room := b.limit - b.buf.Len()
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.overflow = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) String() string {
	if b.overflow {
		return b.buf.String() + "\n…（输出超过上限，后续已丢弃）"
	}
	return b.buf.String()
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return dedupeSorted(out)
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
