// verify.go 负责「验证面板」：在工作区目录里跑一条检查命令（测试/lint/build），
// 把退出码和输出尾部还给用户，外加按项目类型给出常用命令建议。
//
// 设计边界：
//   - 这是 Friday 自己 spawn 的进程，不经过 agent，所以必须走同一套审批闸门
//     （approval.Queue，强制入队）+ 明确超时；agent 说「做完了」不算验证过。
//   - 命令是用户手填的一行 shell，Friday 只做黑名单兜底（approval.DenyPatterns），
//     不承诺沙箱；审批卡上写清目录与超时，让用户判断。
//   - 输出落临时文件再取尾部，不在内存里堆全量（npm test 可以刷到很大）；
//     超过上限时明确标 truncated，不静默截断。
//   - 同一条会话同时只跑一个验证命令，跑完保留最近 10 条记录；记录不落盘——
//     进程重启后「在跑」就没有意义了，投影里也不该留假状态。
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/capyflow/friday/internal/approval"
)

const (
	checkCommandMax     = 512              // 一行命令的长度上限
	checkHistoryKeep    = 10               // 每条会话保留的验证记录数
	checkOutputTail     = 64 * 1024        // 展示用的输出尾部字节数
	checkDefaultTimeout = 5 * time.Minute  // 未指定时的执行超时
	checkMinTimeout     = 2 * time.Second  // 下限：再短就只剩误杀
	checkMaxTimeout     = 30 * time.Minute // 上限：长构建请交给 agent 的会话轮次
	checkApprovalWait   = 5 * time.Minute  // 验证命令是自己点的，不等 30 分钟
)

// 验证命令的生命周期状态。
const (
	CheckAwaiting = "awaiting" // 已提交，等用户在审批卡上批准
	CheckRunning  = "running"  // 已批准并在跑
	CheckPassed   = "passed"   // 退出码 0
	CheckFailed   = "failed"   // 退出码非 0
	CheckTimeout  = "timeout"  // 撞上执行超时被杀
	CheckKilled   = "killed"   // 用户点了停止
	CheckDenied   = "denied"   // 审批被拒绝或超时
	CheckError    = "error"    // 根本没跑起来（目录不存在/命令不可解析）
)

// CheckRun 是一次验证命令的运行记录。
type CheckRun struct {
	ID         string    `json:"id"`
	Command    string    `json:"command"`
	Cwd        string    `json:"cwd"`
	State      string    `json:"state"`
	ExitCode   int       `json:"exitCode"` // -1 表示没拿到退出码（没跑成/被杀）
	Output     string    `json:"output,omitempty"`
	HasOutput  bool      `json:"hasOutput,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	Note       string    `json:"note,omitempty"`
	TimeoutSec int       `json:"timeoutSec"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	DurationMs int64     `json:"durationMs,omitempty"`

	// cancelled 标记「等审批时用户点了停止」：此时执行 goroutine 不能再把
	// 状态改回 denied，否则记录会在同一秒里从「已停止」翻成「审批未通过」。
	cancelled bool
}

// CheckSuggestion 是一条按项目类型推断出的常用检查命令。
type CheckSuggestion struct {
	Label   string `json:"label"`   // 展示名（npm test）
	Command string `json:"command"` // 实际执行的整行命令
	Source  string `json:"source"`  // 依据：哪个文件说明这个项目这么跑
}

// checkHandle 是进行中运行的取消凭据（不进 JSON）。
type checkHandle struct {
	cancel context.CancelFunc
	pend   *approval.Pending
}

// checkRegistry 保护运行记录与取消句柄；独立于 m.mu，避免长命令跑完时
// 去抢会话锁。
type checkRegistry struct {
	mu      sync.Mutex
	runs    map[string][]*CheckRun // threadID → 记录（新的在前）
	handles map[string]*checkHandle
}

func (m *Manager) checkReg() *checkRegistry { return &m.checks }

// Checks 返回某会话的验证命令记录（新的在前）；没有记录时回空切片。
func (m *Manager) Checks(threadID string) []CheckRun {
	reg := m.checkReg()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	out := make([]CheckRun, 0, len(reg.runs[threadID]))
	for _, r := range reg.runs[threadID] {
		out = append(out, *r)
	}
	return out
}

// Check 读取单次运行记录。
func (m *Manager) Check(threadID, runID string) (CheckRun, error) {
	reg := m.checkReg()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, r := range reg.runs[threadID] {
		if r.ID == runID {
			return *r, nil
		}
	}
	return CheckRun{}, errors.New("未找到这次验证运行")
}

// CheckSuggestions 返回这个会话工作目录的检查命令建议。
func (m *Manager) CheckSuggestions(threadID string) ([]CheckSuggestion, error) {
	cwd, err := m.checkCwd(threadID)
	if err != nil {
		return nil, err
	}
	return suggestChecks(cwd), nil
}

// StartCheck 提交一条验证命令：先过审批闸门，批准后在会话主目录执行。
// 返回的记录初始为 awaiting，前端轮询 Checks 看后续状态。
func (m *Manager) StartCheck(threadID, command string, timeout time.Duration) (*CheckRun, error) {
	cwd, err := m.checkCwd(threadID)
	if err != nil {
		return nil, err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("命令为空")
	}
	if len(command) > checkCommandMax {
		return nil, fmt.Errorf("命令过长（%d 字符，上限 %d）", len(command), checkCommandMax)
	}
	if strings.ContainsAny(command, "\x00") {
		return nil, errors.New("命令含空字节")
	}
	if _, err := os.Stat(cwd); err != nil {
		return nil, fmt.Errorf("工作目录不可用: %s", cwd)
	}
	if deny := checkDenyReason(command); deny != "" {
		return nil, fmt.Errorf("命令含毁灭性片段 %q，Friday 不执行", deny)
	}
	switch {
	case timeout <= 0:
		timeout = checkDefaultTimeout
	case timeout < checkMinTimeout:
		timeout = checkMinTimeout
	case timeout > checkMaxTimeout:
		timeout = checkMaxTimeout
	}

	reg := m.checkReg()
	run := &CheckRun{
		ID:         fmt.Sprintf("c%d", time.Now().UnixNano()),
		Command:    command,
		Cwd:        cwd,
		State:      CheckRunning,
		ExitCode:   -1,
		TimeoutSec: int(timeout / time.Second),
		StartedAt:  time.Now(),
	}
	reg.mu.Lock()
	for _, r := range reg.runs[threadID] {
		if r.State == CheckAwaiting || r.State == CheckRunning {
			reg.mu.Unlock()
			return nil, errors.New("这条会话已有验证命令在跑或待批，先停掉它")
		}
	}
	if reg.runs == nil {
		reg.runs = map[string][]*CheckRun{}
	}
	if reg.handles == nil {
		reg.handles = map[string]*checkHandle{}
	}
	run.State = CheckAwaiting
	reg.runs[threadID] = append([]*CheckRun{run}, reg.runs[threadID]...)
	if n := len(reg.runs[threadID]); n > checkHistoryKeep {
		reg.runs[threadID] = reg.runs[threadID][:checkHistoryKeep]
	}
	reg.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	reg.mu.Lock()
	reg.handles[run.ID] = &checkHandle{cancel: cancel}
	reg.mu.Unlock()

	var pend *approval.Pending
	if m.approver != nil {
		// 与 agent 权限卡同源：无论全局审批模式如何都要人点一次。
		pend = m.approver.RequestCard(approval.Card{
			Tool:    "验证命令",
			Summary: fmt.Sprintf("在 %s 执行：%s", filepath.Base(cwd), clipLine(command, 80)),
			Detail:  fmt.Sprintf("目录：%s\n超时：%d 秒\n命令：%s", cwd, int(timeout/time.Second), command),
			Origin:  m.ThreadOrigin(threadID, ""),
		}, checkApprovalWait)
		reg.mu.Lock()
		reg.handles[run.ID].pend = pend
		reg.mu.Unlock()
	} else {
		log.Printf("[智能体] 未装配审批队列，验证命令直接执行: %s", clipLine(command, 80))
	}

	go m.runCheck(threadID, run, pend, ctx, cancel)
	return run, nil
}

// StopCheck 取消一次验证运行：待批的撤卡，在跑的杀进程组。
func (m *Manager) StopCheck(threadID, runID string) error {
	reg := m.checkReg()
	reg.mu.Lock()
	var target *CheckRun
	for _, r := range reg.runs[threadID] {
		if r.ID == runID {
			target = r
			break
		}
	}
	if target == nil {
		reg.mu.Unlock()
		return errors.New("未找到这次验证运行")
	}
	if target.State != CheckAwaiting && target.State != CheckRunning {
		reg.mu.Unlock()
		return errors.New("这次运行已经结束了")
	}
	handle := reg.handles[runID]
	if handle == nil {
		reg.mu.Unlock()
		return errors.New("这次运行的取消凭据已丢失（进程可能已重启）")
	}
	// 还没起进程（等审批）时状态当场定死为「已停止」：goroutine 之后只会看到
	// cancelled，不会再把它翻成「审批未通过」。
	if target.State == CheckAwaiting {
		target.cancelled = true
		target.State = CheckKilled
		target.Note = "已手动停止（进程未启动）"
		target.FinishedAt = time.Now()
	}
	reg.mu.Unlock()

	if handle.pend != nil {
		handle.pend.Cancel()
	}
	handle.cancel() // 超时/取消都走 ctx：CommandContext 会杀掉进程组
	return nil
}

// runCheck 是验证命令的执行 goroutine：等审批 → 起进程 → 收状态与输出尾部。
func (m *Manager) runCheck(threadID string, run *CheckRun, pend *approval.Pending,
	ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	reg := m.checkReg()
	defer func() { // 结束后释放取消凭据，避免句柄随会话累积
		reg.mu.Lock()
		delete(reg.handles, run.ID)
		reg.mu.Unlock()
	}()

	set := func(mut func(*CheckRun)) {
		reg.mu.Lock()
		if !run.cancelled { // 用户已点停止：状态由 StopCheck 定，这里不再翻
			mut(run)
		}
		reg.mu.Unlock()
	}

	if pend != nil {
		allow, _ := pend.Wait()
		if !allow {
			set(func(r *CheckRun) {
				r.State = CheckDenied
				r.Note = fmt.Sprintf("审批未通过（拒绝或 %d 分钟内未处理），未执行",
					int(checkApprovalWait/time.Minute))
				r.FinishedAt = time.Now()
			})
			return
		}
	}
	if ctx.Err() != nil {
		set(func(r *CheckRun) {
			r.State = CheckKilled
			r.Note = "等待审批时已取消，未执行"
			r.FinishedAt = time.Now()
		})
		return
	}

	outFile, err := os.CreateTemp("", "friday-acp-check-*.log")
	if err != nil {
		set(func(r *CheckRun) {
			r.State = CheckError
			r.Note = fmt.Sprintf("创建输出文件失败: %v", err)
			r.FinishedAt = time.Now()
		})
		return
	}
	outPath := outFile.Name()
	defer os.Remove(outPath)

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", run.Command)
	cmd.Dir = run.Cwd
	cmd.Env = append(os.Environ(), "TERM=dumb", "NO_COLOR=1")
	// 独立进程组：sh→npm→node→测试子进程要一起带走，否则停止只杀掉 shell。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdout = outFile
	cmd.Stderr = outFile

	started := time.Now()
	if err := cmd.Start(); err != nil {
		outFile.Close()
		set(func(r *CheckRun) {
			r.State = CheckError
			r.Note = fmt.Sprintf("命令启动失败: %v", err)
			r.FinishedAt = time.Now()
		})
		return
	}
	set(func(r *CheckRun) { r.State = CheckRunning })

	waitErr := cmd.Wait()
	outSize := int64(0)
	if st, serr := outFile.Stat(); serr == nil {
		outSize = st.Size()
	}
	outFile.Close()

	state := CheckPassed
	note := ""
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		state = CheckTimeout
		note = fmt.Sprintf("超过 %d 秒被终止", run.TimeoutSec)
	case ctx.Err() != nil:
		state = CheckKilled
		note = "已手动停止"
	case waitErr != nil:
		state = CheckFailed
		var ee *exec.ExitError
		if !errors.As(waitErr, &ee) {
			exit = -1
			note = fmt.Sprintf("执行出错: %v", waitErr)
		}
	}
	if state == CheckFailed && exit >= 0 && note == "" {
		note = fmt.Sprintf("退出码 %d", exit)
	}

	set(func(r *CheckRun) {
		r.State = state
		r.ExitCode = exit
		r.Note = note
		r.DurationMs = time.Since(started).Milliseconds()
		r.FinishedAt = time.Now()
		r.Truncated = outSize > checkOutputTail
		r.Output = cleanCheckOutput(readTail(outPath, checkOutputTail))
		r.HasOutput = r.Output != ""
		if r.Output == "" && outSize == 0 {
			r.Output = "（无输出）"
			r.HasOutput = true
		}
	})
	log.Printf("[智能体] 验证命令 %s → %s: %s", run.ID, state, clipLine(run.Command, 80))
}

// checkCwd 取会话锚定的主目录（验证命令就在那儿跑）。
func (m *Manager) checkCwd(threadID string) (string, error) {
	m.mu.Lock()
	th, ok := m.threads[threadID]
	cwd := ""
	if ok {
		cwd = th.Cwd
	}
	m.mu.Unlock()
	if !ok {
		return "", errors.New("未找到该会话")
	}
	if strings.TrimSpace(cwd) == "" {
		return "", errors.New("该会话没有工作目录")
	}
	return cwd, nil
}

// suggestChecks 按仓库根的标志文件给出常用检查命令，最多 6 条。
// 只读标志文件，不解析依赖内容：目标是「一键有个能跑的入口」，不是替代 CI 配置。
func suggestChecks(root string) []CheckSuggestion {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	var out []CheckSuggestion
	add := func(s CheckSuggestion) {
		for _, e := range out {
			if e.Command == s.Command {
				return
			}
		}
		out = append(out, s)
	}

	// Node：包管理器按锁文件/packageManager 字段判断，脚本名按常见优先级取。
	if pathExists(root, "package.json") {
		pm := nodePackageManager(root)
		scripts := nodeScripts(root)
		for _, name := range []string{"test", "lint", "typecheck", "check", "check-types"} {
			_, ok := scripts[name]
			if !ok {
				continue
			}
			cmd := pm + " run " + name
			if name == "test" {
				cmd = pm + " test"
			}
			add(CheckSuggestion{Label: cmd, Command: cmd, Source: "package.json 的 scripts." + name})
		}
	}
	if pathExists(root, "go.mod") {
		add(CheckSuggestion{Label: "go test ./...", Command: "go test ./...", Source: "go.mod"})
	}
	if pathExists(root, "Cargo.toml") {
		add(CheckSuggestion{Label: "cargo test", Command: "cargo test", Source: "Cargo.toml"})
	}
	if pathExists(root, "pyproject.toml") || pathExists(root, "pytest.ini") || pathExists(root, "tox.ini") ||
		(pathExists(root, "setup.cfg") && pathExists(root, "tests")) || (pathExists(root, "requirements.txt") && pathExists(root, "tests")) {
		add(CheckSuggestion{Label: "pytest", Command: "pytest", Source: "Python 项目标志文件"})
	}
	// Makefile 只在没有 Node/Go/Rust 标志时兜底，避免 make 与 npm 两套入口打架。
	if len(out) == 0 && pathExists(root, "Makefile") {
		for _, target := range []string{"test", "check", "lint"} {
			if makefileHasTarget(filepath.Join(root, "Makefile"), target) {
				add(CheckSuggestion{Label: "make " + target, Command: "make " + target, Source: "Makefile 目标 " + target})
				break
			}
		}
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func pathExists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

// nodePackageManager 猜一个能跑脚本的命令名：优先 packageManager 字段，
// 其次锁文件；都没有就用 npm。
func nodePackageManager(root string) string {
	if b, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		var p struct {
			PackageManager string `json:"packageManager"`
		}
		if json.Unmarshal(b, &p) == nil {
			f := strings.ToLower(strings.TrimSpace(p.PackageManager))
			for _, pm := range []string{"pnpm", "yarn", "bun", "npm"} {
				if strings.HasPrefix(f, pm) {
					return pm
				}
			}
		}
	}
	switch {
	case pathExists(root, "pnpm-lock.yaml"):
		return "pnpm"
	case pathExists(root, "yarn.lock"):
		return "yarn"
	case pathExists(root, "bun.lockb") || pathExists(root, "bun.lock"):
		return "bun"
	default:
		return "npm"
	}
}

func nodeScripts(root string) map[string]string {
	b, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return map[string]string{}
	}
	var p struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return map[string]string{}
	}
	return p.Scripts
}

var makeTargetRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+):`)

func makefileHasTarget(path, target string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if m := makeTargetRe.FindStringSubmatch(line); m != nil && m[1] == target {
			return true
		}
	}
	return false
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// cleanCheckOutput 把输出整理成可贴进 <pre> 的文本：去 ANSI、\r 归行、限行数。
func cleanCheckOutput(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	const maxLines = 400
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// checkDenyReason 是本地兜底黑名单：权限层的 deny 规则管的是 exec 工具，
// 这里是 Friday 自己 spawn 的进程，不拦就只剩审批卡一道闸。
func checkDenyReason(command string) string {
	low := strings.ToLower(command)
	for _, p := range approval.DenyPatterns() {
		pat := strings.ToLower(strings.TrimSpace(p))
		if pat == "" {
			continue
		}
		if strings.Contains(low, pat) {
			return p
		}
	}
	return ""
}

func clipLine(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
