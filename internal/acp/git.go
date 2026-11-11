// git.go 是工作区「Git 管理」面板的后端：分支、状态、历史三类只读查询，
// 外加几类必须过审批闸门的写操作（切分支 / 建分支 / 暂存 / 取消暂存 / 提交）。
//
// 定位与验证面板一致：Friday 自己 spawn 的进程不经过 agent，所以写操作一律
// 进审批队列（approval.Queue），用户在审批卡上点「批准」才真正落地；只读
// 查询不设闸——它不改仓库。
//
// 三条硬边界：
//   - 命令只在会话自己的工作目录里跑，且用 exec.Command 直接传 argv，不走
//     shell，所以不存在命令注入；参数另有一层白名单校验（见 gitOpArgs）。
//   - 不做 force push、不做 reset --hard、不删分支、不改远端配置：这些既
//     不可逆，又容易把别人的工作弄丢，不在面板能力范围内。
//   - 非 git 目录要明说「这里不是 git 仓库」，不把命令报错伪装成空列表。
package acp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/approval"
)

const (
	gitLogDefaultLimit = 30  // 历史默认条数
	gitLogMaxLimit     = 200 // 再多前端也读不完，且回包会肥
	gitOpMaxOutput     = 32 * 1024
	gitOpApprovalWait  = 5 * time.Minute
	gitOpHistoryKeep   = 20
)

// Git 操作的生命周期状态。
const (
	GitOpAwaiting = "awaiting" // 已提交，等用户在审批卡上批准
	GitOpRunning  = "running"  // 已批准并在跑
	GitOpOK       = "ok"       // 成功
	GitOpFailed   = "failed"   // git 自己报错（命令跑了，但没成功）
	GitOpDenied   = "denied"   // 审批被拒绝或超时
	GitOpError    = "error"    // 根本没跑起来（不是仓库 / git 不存在）
)

// GitFileStatus 是一个工作区文件的改动状态。
type GitFileStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"` // 两字符：X=暂存区，Y=工作区
	Stage  string `json:"stage"`  // "staged" | "unstaged" | "both" | ""
}

// GitStatus 是仓库当前状态。
type GitStatus struct {
	IsRepo     bool            `json:"isRepo"`
	Branch     string          `json:"branch"`
	Detached   bool            `json:"detached"`
	Upstream   string          `json:"upstream"`
	Ahead      int             `json:"ahead"`
	Behind     int             `json:"behind"`
	Staged     int             `json:"staged"`
	Modified   int             `json:"modified"`
	Untracked  int             `json:"untracked"`
	Deleted    int             `json:"deleted"`
	Conflicted int             `json:"conflicted"`
	Files      []GitFileStatus `json:"files"`
	Note       string          `json:"note,omitempty"`
}

// GitCommit 是一条历史。
type GitCommit struct {
	Hash    string    `json:"hash"`
	Short   string    `json:"short"`
	Author  string    `json:"author"`
	Email   string    `json:"email"`
	Date    time.Time `json:"date"`
	Subject string    `json:"subject"`
	Body    string    `json:"body,omitempty"`
}

// GitBranch 是一个分支（本地或远端）。
type GitBranch struct {
	Name     string    `json:"name"`
	Current  bool      `json:"current"`
	Remote   bool      `json:"remote"`
	Upstream string    `json:"upstream,omitempty"`
	Last     string    `json:"last,omitempty"`
	Date     time.Time `json:"date,omitempty"`
}

// GitOp 是一次 Git 写操作的记录。
type GitOp struct {
	ID         string    `json:"id"`
	Op         string    `json:"op"`
	Arg        string    `json:"arg,omitempty"`
	Cwd        string    `json:"cwd"`
	State      string    `json:"state"`
	Message    string    `json:"message,omitempty"`
	Output     string    `json:"output,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	ApprovedAt time.Time `json:"approvedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`

	// cancelled 标记「等审批时用户点了停止」：此时执行 goroutine 不能再把
	// 状态改回 denied，否则记录会在同一秒里从「已停止」翻成「审批未通过」。
	cancelled bool
}

// GitOpRequest 是前端提交的一次写操作。
type GitOpRequest struct {
	Op  string `json:"op"`
	Arg string `json:"arg,omitempty"`
}

// gitOpRegistry 保护写操作记录与取消句柄：独立于 m.mu，别让长命令跑完时
// 去抢会话锁。
type gitOpRegistry struct {
	mu      sync.Mutex
	runs    map[string][]*GitOp // threadID → 记录（新的在前）
	handles map[string]*gitOpHandle
}

type gitOpHandle struct {
	cancel context.CancelFunc
	pend   *approval.Pending
}

// GitOp 支持的写操作白名单：键是前端传的 op，值是拼 argv 的函数。
// 参数全部走 argv（不经 shell），这里只做「像不像合法名字」的第二道校验。
var gitOpBuilders = map[string]func(arg string) ([]string, error){
	"checkout":     gitArgCheckout,
	"createBranch": gitArgCreateBranch,
	"stage":        gitArgStage,
	"unstage":      gitArgUnstage,
	"commit":       gitArgCommit,
}

func (m *Manager) gitReg() *gitOpRegistry { return &m.gitOps }

// GitStatus 读仓库状态（分支、领先落后、改动文件）。
func (m *Manager) GitStatus(threadID string) (*GitStatus, error) {
	cwd, err := m.checkCwd(threadID)
	if err != nil {
		return nil, err
	}
	return gitStatusAt(cwd)
}

// GitLog 读最近若干条提交历史。
func (m *Manager) GitLog(threadID string, limit int) ([]GitCommit, error) {
	cwd, err := m.checkCwd(threadID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = gitLogDefaultLimit
	}
	if limit > gitLogMaxLimit {
		limit = gitLogMaxLimit
	}
	return gitLogAt(cwd, limit)
}

// GitBranches 列出本地与远端分支。
func (m *Manager) GitBranches(threadID string) ([]GitBranch, error) {
	cwd, err := m.checkCwd(threadID)
	if err != nil {
		return nil, err
	}
	return gitBranchesAt(cwd)
}

// GitOps 返回这条会话的写操作记录（新的在前）。
func (m *Manager) GitOps(threadID string) []GitOp {
	reg := m.gitReg()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	out := make([]GitOp, 0, len(reg.runs[threadID]))
	for _, r := range reg.runs[threadID] {
		out = append(out, *r)
	}
	return out
}

// GitOpByID 读一次写操作的详情。
func (m *Manager) GitOpByID(threadID, opID string) (GitOp, error) {
	reg := m.gitReg()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, r := range reg.runs[threadID] {
		if r.ID == opID {
			return *r, nil
		}
	}
	return GitOp{}, errors.New("未找到这次 Git 操作")
}

// StartGitOp 提交一次 Git 写操作：先过审批闸门，批准后在会话主目录执行。
// 返回的记录初始为 awaiting，前端轮询 GitOps 看后续状态。
func (m *Manager) StartGitOp(threadID string, req GitOpRequest) (*GitOp, error) {
	cwd, err := m.checkCwd(threadID)
	if err != nil {
		return nil, err
	}
	op := strings.TrimSpace(req.Op)
	if op == "" {
		return nil, errors.New("缺少 op")
	}
	build, ok := gitOpBuilders[op]
	if !ok {
		return nil, fmt.Errorf("不支持的 Git 操作 %q", op)
	}
	args, err := build(strings.TrimSpace(req.Arg))
	if err != nil {
		return nil, err
	}
	if _, err := gitOutput(cwd, "rev-parse", "--is-inside-work-tree"); err != nil {
		return nil, errors.New("这个目录不是 git 仓库")
	}
	// 切分支只认真分支/ref：参数若是文件路径，git switch 会把它当成 ref 去查，
	// 查不到就报错——这里先查一次，给出更清楚的话，也挡住路径形态的误用。
	if op == "checkout" {
		if _, err := gitOutput(cwd, "rev-parse", "--verify", "--quiet", req.Arg+"^{commit}"); err != nil {
			return nil, fmt.Errorf("没有这个分支或提交：%s", req.Arg)
		}
	}

	reg := m.gitReg()
	run := &GitOp{
		ID:        fmt.Sprintf("g%d", time.Now().UnixNano()),
		Op:        op,
		Arg:       strings.TrimSpace(req.Arg),
		Cwd:       cwd,
		State:     GitOpAwaiting,
		StartedAt: time.Now(),
	}
	reg.mu.Lock()
	if reg.runs == nil {
		reg.runs = map[string][]*GitOp{}
	}
	if reg.handles == nil {
		reg.handles = map[string]*gitOpHandle{}
	}
	reg.runs[threadID] = append([]*GitOp{run}, reg.runs[threadID]...)
	if n := len(reg.runs[threadID]); n > gitOpHistoryKeep {
		reg.runs[threadID] = reg.runs[threadID][:gitOpHistoryKeep]
	}
	reg.mu.Unlock()

	// 执行超时从「批准之后」才起算：审批最多等 5 分钟，若从提交那刻就计时，
	// 用户慢一点批准就会莫名超时——「等 5 分钟」得是真的。
	ctx, cancel := context.WithCancel(context.Background())
	reg.mu.Lock()
	reg.handles[run.ID] = &gitOpHandle{cancel: cancel}
	reg.mu.Unlock()

	var pend *approval.Pending
	if m.approver != nil {
		// 与 agent 权限卡同源：无论全局审批模式如何都要人点一次。
		pend = m.approver.RequestCard(approval.Card{
			Tool:    "Git 操作",
			Summary: fmt.Sprintf("在 %s 执行：%s（git %s）", filepath.Base(cwd), gitOpLabel(op), strings.Join(args, " ")),
			Detail:  fmt.Sprintf("操作：%s\n目录：%s\n命令：git %s", gitOpLabel(op), cwd, strings.Join(args, " ")),
			Origin:  m.ThreadOrigin(threadID, ""),
		}, gitOpApprovalWait)
		reg.mu.Lock()
		reg.handles[run.ID].pend = pend
		reg.mu.Unlock()
	}

	go m.runGitOp(threadID, run, pend, ctx, cancel, args)
	return run, nil
}

// StopGitOp 取消一次写操作：待批的撤卡，在跑的杀掉进程组。
func (m *Manager) StopGitOp(threadID, opID string) error {
	reg := m.gitReg()
	reg.mu.Lock()
	var target *GitOp
	for _, r := range reg.runs[threadID] {
		if r.ID == opID {
			target = r
			break
		}
	}
	if target == nil {
		reg.mu.Unlock()
		return errors.New("未找到这次 Git 操作")
	}
	if target.State != GitOpAwaiting && target.State != GitOpRunning {
		reg.mu.Unlock()
		return errors.New("这次操作已经结束了")
	}
	handle := reg.handles[opID]
	if handle == nil {
		reg.mu.Unlock()
		return errors.New("这次操作的取消凭据已丢失（进程可能已重启）")
	}
	if target.State == GitOpAwaiting {
		// 还没起进程（等审批）时状态当场定死为「已停止」：goroutine 之后只会看到
		// cancelled，不会再把它翻成「审批未通过」。
		target.cancelled = true
		target.State = GitOpFailed
		target.Message = "已手动停止（命令未执行）"
		target.FinishedAt = time.Now()
	}
	reg.mu.Unlock()

	if handle.pend != nil {
		handle.pend.Cancel()
	}
	handle.cancel()
	return nil
}

// runGitOp 是写操作的执行 goroutine：等审批 → 起 git → 收状态与输出尾部。
func (m *Manager) runGitOp(threadID string, run *GitOp, pend *approval.Pending,
	ctx context.Context, cancel context.CancelFunc, args []string) {
	defer cancel()
	reg := m.gitReg()
	defer func() {
		reg.mu.Lock()
		delete(reg.handles, run.ID)
		reg.mu.Unlock()
	}()

	set := func(mut func(*GitOp)) {
		reg.mu.Lock()
		if !run.cancelled { // 用户已点停止：状态由 StopGitOp 定，这里不再翻
			mut(run)
		}
		reg.mu.Unlock()
	}

	if pend != nil {
		allow, _ := pend.Wait()
		if !allow {
			set(func(r *GitOp) {
				r.State = GitOpDenied
				r.Message = "审批未通过（拒绝或超时），未执行"
				r.FinishedAt = time.Now()
			})
			return
		}
	}
	if ctx.Err() != nil {
		set(func(r *GitOp) {
			r.State = GitOpFailed
			r.Message = "等待审批时已取消，未执行"
			r.FinishedAt = time.Now()
		})
		return
	}

	// 批准之后才起算执行超时：审批最多等 5 分钟，那段时间不该算进命令的执行预算里。
	set(func(r *GitOp) {
		r.ApprovedAt = time.Now()
		r.State = GitOpRunning
	})
	runCtx, runCancel := context.WithTimeout(ctx, gitOpTimeout)
	defer runCancel()

	out, err := gitRunContext(runCtx, run.Cwd, args...)
	msg := ""
	state := GitOpOK
	if err != nil {
		state = GitOpFailed
		msg = err.Error()
	}
	if len(out) > gitOpMaxOutput {
		out = out[len(out)-gitOpMaxOutput:]
	}
	set(func(r *GitOp) {
		r.State = state
		r.Message = msg
		r.Output = out
		r.FinishedAt = time.Now()
	})
}

// ---------- 只读查询的实现 ----------

// gitRepoAt 判断目录是否在 git 仓库里。
func gitRepoAt(cwd string) bool {
	out, err := gitOutput(cwd, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

func gitStatusAt(cwd string) (*GitStatus, error) {
	st := &GitStatus{Files: []GitFileStatus{}}
	if !gitRepoAt(cwd) {
		st.Note = "这个目录不是 git 仓库"
		return st, nil
	}
	st.IsRepo = true

	branch, err := gitOutput(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		st.Note = "读分支失败：" + err.Error()
		return st, nil
	}
	st.Branch = strings.TrimSpace(branch)
	if st.Branch == "HEAD" {
		st.Detached = true
		st.Branch = "HEAD"
	}

	if up, err := gitOutput(cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		st.Upstream = strings.TrimSpace(up)
	}
	// 领先/落后：远端没有时 git 会报错，这里当作 0/0，不打扰用户。
	if cnt, err := gitOutput(cwd, "rev-list", "--left-right", "--count", "HEAD...@{upstream}"); err == nil {
		fields := strings.Fields(cnt)
		if len(fields) == 2 {
			st.Ahead, _ = parseIntSafe(fields[0])
			st.Behind, _ = parseIntSafe(fields[1])
		}
	}

	out, _, err := gitRun(cwd, "status", "--porcelain=v1", "-z")
	if err != nil {
		st.Note = "读工作区状态失败：" + err.Error()
		return st, nil
	}
	// porcelain -z：改名/复制会给两条记录——先是「新路径 + 状态位」，再是
	// 「旧路径」单独一条（没有状态位）。后者要跳掉，否则同一个改动会算两条。
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		xy := entry[:2]
		path := strings.TrimSpace(entry[3:])
		if path == "" {
			continue
		}
		// 状态位不是空格的两条：改名（R/C）时后面还跟着一条旧路径记录。
		if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
			i++ // 跳过紧跟其后的旧路径记录
		}
		fs := GitFileStatus{Path: path, Status: xy}
		// 暂存区（X）与工作区（Y）分开看：一个文件可以两边都脏（MM），
		// 这时它既算“已暂存”也算“工作区已改”，前端要能分别操作。
		inStage := xy[0] != ' ' && xy[0] != '?'
		inWork := xy[1] != ' ' && xy[1] != '?'
		switch {
		case inStage && inWork:
			fs.Stage = "both"
			st.Staged++
			st.Modified++
		case inStage:
			fs.Stage = "staged"
			st.Staged++
		case inWork:
			fs.Stage = "unstaged"
			st.Modified++
		}
		if strings.Contains(xy, "?") {
			st.Untracked++
			fs.Stage = "unstaged"
		}
		if strings.Contains(xy, "D") {
			st.Deleted++
		}
		if strings.Contains(xy, "U") {
			st.Conflicted++
		}
		st.Files = append(st.Files, fs)
	}
	return st, nil
}

func gitLogAt(cwd string, limit int) ([]GitCommit, error) {
	out := []GitCommit{}
	if !gitRepoAt(cwd) {
		return out, nil
	}
	// 两个分隔符：\x1f 分字段、\x1e 分记录。用 NUL 是不行的——argv 里
	// 不能带 NUL 字节，带上会被内核直接拒掉（fork/exec: invalid argument）。
	format := "%H\x1f%h\x1f%an\x1f%ae\x1f%ad\x1f%s\x1f%b\x1e"
	raw, err := gitOutput(cwd, "log", "--max-count="+gitItoa(limit), "--date=iso-strict", "--format="+format)
	if err != nil {
		return nil, err
	}
	for _, rec := range strings.Split(raw, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
		if len(f) < 6 {
			continue
		}
		c := GitCommit{
			Hash:    f[0],
			Short:   f[1],
			Author:  f[2],
			Email:   f[3],
			Subject: f[5],
		}
		if len(f) > 6 {
			c.Body = strings.TrimSpace(f[6])
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(f[4])); err == nil {
			c.Date = t
		}
		out = append(out, c)
	}
	return out, nil
}

func gitBranchesAt(cwd string) ([]GitBranch, error) {
	out := []GitBranch{}
	if !gitRepoAt(cwd) {
		return out, nil
	}
	// 用完整 refname 判远端：%(refname:short) 对远端跟踪分支给的是
	// "origin/main"（没有 remotes/ 前缀），光看短名认不出来。
	format := "%(refname)\x1f%(refname:short)\x1f%(objectname:short)\x1f%(upstream:short)\x1f%(HEAD)\x1f%(committerdate:iso-strict)"
	raw, err := gitOutput(cwd, "branch", "--all", "--format="+format)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		if len(f) < 3 {
			continue
		}
		b := GitBranch{Name: f[1], Last: f[2], Current: strings.TrimSpace(f[4]) == "*"}
		if len(f) > 3 {
			b.Upstream = strings.TrimSpace(f[3])
		}
		if len(f) > 5 {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(f[5])); err == nil {
				b.Date = t
			}
		}
		if strings.HasPrefix(f[0], "refs/remotes/") {
			b.Remote = true
		}
		out = append(out, b)
	}
	return out, nil
}

// gitOpLabel 把内部 op 名翻成审批卡上看得懂的中文。
func gitOpLabel(op string) string {
	switch op {
	case "checkout":
		return "切换分支"
	case "createBranch":
		return "新建并切换分支"
	case "stage":
		return "暂存文件"
	case "unstage":
		return "取消暂存"
	case "commit":
		return "提交"
	default:
		return op
	}
}

// gitOpTimeout 是命令本身的执行上限。它是 var 而不是 const：测试要能把它调小，
// 才能在几秒内验出「执行超时从批准之后才起算」这条语义（见 runGitOp）。
var gitOpTimeout = 2 * time.Minute

// ---------- 参数校验 ----------

// gitRefRe 是分支名允许的形态：字母数字与 . _ - /，且不许以 . 或 - 开头。
var gitRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// gitPathRe 是文件路径允许的形态：相对路径，不许绝对路径、不许 .. 逃逸。
var gitPathRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

func gitArgCheckout(arg string) ([]string, error) {
	if arg == "" {
		return nil, errors.New("缺少分支名")
	}
	if !gitRefRe.MatchString(arg) || strings.Contains(arg, "..") {
		return nil, fmt.Errorf("分支名 %q 不合法", arg)
	}
	// 用 switch 而不是 checkout：checkout 认路径（git checkout a.txt 会把文件
	// 恢复成暂存区的内容，用户没提交的改动就被无声扔掉），switch 只认分支。
	return []string{"switch", arg}, nil
}

func gitArgCreateBranch(arg string) ([]string, error) {
	if arg == "" {
		return nil, errors.New("缺少新分支名")
	}
	if !gitRefRe.MatchString(arg) || strings.Contains(arg, "..") {
		return nil, fmt.Errorf("分支名 %q 不合法", arg)
	}
	return []string{"checkout", "-b", arg}, nil
}

func gitArgStage(arg string) ([]string, error) {
	if arg == "" {
		return nil, errors.New("缺少文件路径")
	}
	if !gitPathRe.MatchString(arg) || strings.Contains(arg, "..") {
		return nil, fmt.Errorf("路径 %q 不合法", arg)
	}
	return []string{"add", "--", arg}, nil
}

func gitArgUnstage(arg string) ([]string, error) {
	if arg == "" {
		return nil, errors.New("缺少文件路径")
	}
	if !gitPathRe.MatchString(arg) || strings.Contains(arg, "..") {
		return nil, fmt.Errorf("路径 %q 不合法", arg)
	}
	return []string{"restore", "--staged", "--", arg}, nil
}

func gitArgCommit(arg string) ([]string, error) {
	if arg == "" {
		return nil, errors.New("提交信息不能为空")
	}
	if len(arg) > 500 {
		return nil, errors.New("提交信息过长")
	}
	if strings.ContainsAny(arg, "\x00") {
		return nil, errors.New("提交信息含空字节")
	}
	return []string{"commit", "-m", arg}, nil
}

// ---------- 小工具 ----------

func parseIntSafe(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("不是数字: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func gitItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// gitRunContext 是 gitRun 的可取消版本：写操作要能被杀（超时/用户点停止）。
// gitRunContext 跑一条 git 命令：stdout 原样返回，stderr 拼到 stdout 之后
// （git 的报错都在 stderr；分开丢一边的话前端只能看到一句干巴巴的“失败”）。
// 出错时第二个返回值带上原因，调用方据此标失败态。
func gitRunContext(ctx context.Context, dir string, args ...string) (string, error) {
	out, stderr, err := gitRunCtx(ctx, dir, args...)
	trimmed := strings.TrimSpace(stderr)
	if trimmed != "" {
		if out != "" {
			out += "\n"
		}
		out += trimmed
	}
	if err != nil {
		msg := trimmed
		if msg == "" {
			msg = err.Error()
		}
		return out, errors.New(msg)
	}
	return out, nil
}
