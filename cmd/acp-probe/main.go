// Command acp-probe 是一个一次性探测程序：按 stdio 拉起一个真实的 ACP agent，
// 走完 initialize → session/new → session/prompt，把全部报文落盘并统计几个决定
// Friday 接入方案形状的事实——SessionUpdate 的量级与字段、权限请求的次数与并发、
// 不声明 fs/terminal 能力时 agent 是否仍能干活、以及 stopReason/usage 的真实取值。
//
// 它不引用 Friday 任何业务代码，跑完即可删除。
//
//	go run ./cmd/acp-probe -cmd opencode -args acp
//	go run ./cmd/acp-probe -cmd npx -args "-y,@zed-industries/codex-acp" -permissions allow
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// transcript 记录双向原始报文，附带方向与相对时刻。
type transcript struct {
	mu  sync.Mutex
	w   *bufio.Writer
	t0  time.Time
	seq int
}

func (t *transcript) log(dir string, raw []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	fmt.Fprintf(t.w, `{"i":%d,"t_ms":%d,"dir":"%s","bytes":%d,"raw":%q}`+"\n",
		t.seq, time.Since(t.t0).Milliseconds(), dir, len(raw), string(raw))
}

type teeReader struct {
	r io.Reader
	t *transcript
}

func (x *teeReader) Read(p []byte) (int, error) {
	n, err := x.r.Read(p)
	if n > 0 {
		x.t.log("in", p[:n])
	}
	return n, err
}

type teeWriter struct {
	w io.Writer
	t *transcript
}

func (x *teeWriter) Write(p []byte) (int, error) {
	n, err := x.w.Write(p)
	if n > 0 {
		x.t.log("out", p[:n])
	}
	return n, err
}

// probe 实现 acp.Client 的 9 个稳定方法，只做计数与落盘。
type probe struct {
	t0          time.Time
	t           *transcript
	sandbox     string
	policy      string // deny | allow | first
	fsEnabled   bool
	maxMsgBytes int

	mu            sync.Mutex
	updateCount   int
	updateBytes   int
	typeCount     map[string]int
	typeSample    map[string]int // 每类最多详录 3 条
	permCount     int
	permInflight  int
	permMaxPar    int
	permWaits     []time.Duration
	optKinds      map[string]int
	permLocations int
	permRawInput  int
	fsReads       int
	fsWrites      int
	termCalls     map[string]int
	lastUsage     string
	answer        strings.Builder
	mcpHits       int
	firstUpdate   time.Time
	lastUpdate    time.Time
}

func newProbe(t *transcript, sandbox, policy string, fsEnabled bool) *probe {
	return &probe{
		t0:         time.Now(),
		t:          t,
		sandbox:    sandbox,
		policy:     policy,
		fsEnabled:  fsEnabled,
		typeCount:  map[string]int{},
		typeSample: map[string]int{},
		optKinds:   map[string]int{},
		termCalls:  map[string]int{},
	}
}

func (p *probe) stamp() int64 { return time.Since(p.t0).Milliseconds() }

// takeAnswer 取回并清空已累积的 agent 文本，用于分轮统计。
func (p *probe) takeAnswer() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.answer.String()
	p.answer.Reset()
	return s
}

func (p *probe) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	b, _ := json.Marshal(n)
	name := updateKind(n.Update)
	maxChunk := 400
	if len(b) > p.maxMsgBytes {
		p.maxMsgBytes = len(b)
	}
	p.mu.Lock()
	p.updateCount++
	p.updateBytes += len(b)
	p.typeCount[name]++
	now := time.Now()
	if p.firstUpdate.IsZero() {
		p.firstUpdate = now
	}
	p.lastUpdate = now
	detail := p.typeSample[name] < 3
	p.typeSample[name]++
	p.mu.Unlock()

	if n.Update.UsageUpdate != nil {
		uj, _ := json.Marshal(n.Update.UsageUpdate)
		p.mu.Lock()
		p.lastUsage = string(uj)
		p.mu.Unlock()
	}

	if n.Update.AgentMessageChunk != nil && n.Update.AgentMessageChunk.Content.Text != nil {
		p.mu.Lock()
		p.answer.WriteString(n.Update.AgentMessageChunk.Content.Text.Text)
		p.mu.Unlock()
	}

	if detail {
		s := string(b)
		if len(s) > maxChunk {
			s = s[:maxChunk] + "...(truncated)"
		}
		fmt.Fprintf(os.Stderr, "  [%5dms] %s %s\n", p.stamp(), name, s)
	}
	return nil
}

func updateKind(u acp.SessionUpdate) string {
	switch {
	case u.UserMessageChunk != nil:
		return "user_message_chunk"
	case u.AgentMessageChunk != nil:
		return "agent_message_chunk"
	case u.AgentThoughtChunk != nil:
		return "agent_thought_chunk"
	case u.ToolCall != nil:
		return "tool_call"
	case u.ToolCallUpdate != nil:
		return "tool_call_update"
	case u.Plan != nil:
		return "plan"
	case u.PlanUpdate != nil:
		return "plan_update"
	case u.PlanRemoved != nil:
		return "plan_removed"
	case u.AvailableCommandsUpdate != nil:
		return "available_commands_update"
	case u.CurrentModeUpdate != nil:
		return "current_mode_update"
	case u.ConfigOptionUpdate != nil:
		return "config_option_update"
	case u.SessionInfoUpdate != nil:
		return "session_info_update"
	case u.UsageUpdate != nil:
		return "usage_update"
	}
	return "unknown"
}

func (p *probe) RequestPermission(_ context.Context, r acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	start := time.Now()
	p.mu.Lock()
	p.permCount++
	p.permInflight++
	if p.permInflight > p.permMaxPar {
		p.permMaxPar = p.permInflight
	}
	if len(r.ToolCall.Locations) > 0 {
		p.permLocations++
	}
	if r.ToolCall.RawInput != nil {
		p.permRawInput++
	}
	for _, o := range r.Options {
		p.optKinds[string(o.Kind)]++
	}
	p.mu.Unlock()

	b, _ := json.Marshal(r)
	fmt.Fprintf(os.Stderr, "\n== [%dms] request_permission %s\n", p.stamp(), firstN(string(b), 900))

	cancelled := func() acp.RequestPermissionResponse {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Cancelled: &acp.RequestPermissionOutcomeCancelled{},
		}}
	}
	if len(r.Options) == 0 {
		p.finishPerm(start)
		return cancelled(), nil
	}

	pick := 0
	switch p.policy {
	case "deny":
		p.finishPerm(start)
		return cancelled(), nil
	case "first":
		pick = 0
	default: // allow: 优先 allow_once，其次任何 allow_*
		pick = -1
		for i, o := range r.Options {
			if o.Kind == acp.PermissionOptionKindAllowOnce {
				pick = i
				break
			}
		}
		if pick < 0 {
			for i, o := range r.Options {
				if strings.HasPrefix(string(o.Kind), "allow") {
					pick = i
					break
				}
			}
		}
		if pick < 0 {
			pick = 0
		}
	}
	p.finishPerm(start)
	fmt.Fprintf(os.Stderr, "   -> 选中 option[%d]=%s kind=%s\n", pick, r.Options[pick].OptionId, r.Options[pick].Kind)
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: r.Options[pick].OptionId},
	}}, nil
}

func (p *probe) finishPerm(start time.Time) {
	p.mu.Lock()
	p.permInflight--
	p.permWaits = append(p.permWaits, time.Since(start))
	p.mu.Unlock()
}

func (p *probe) ReadTextFile(_ context.Context, r acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	p.mu.Lock()
	p.fsReads++
	p.mu.Unlock()
	fmt.Fprintf(os.Stderr, "  [%dms] fs/read_text_file %s\n", p.stamp(), r.Path)
	if !p.fsEnabled {
		return acp.ReadTextFileResponse{}, errors.New("acp-probe: fs capability not enabled")
	}
	if !p.withinSandbox(r.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("acp-probe: path outside sandbox: %s", r.Path)
	}
	b, err := os.ReadFile(r.Path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	return acp.ReadTextFileResponse{Content: string(b)}, nil
}

func (p *probe) WriteTextFile(_ context.Context, r acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	p.mu.Lock()
	p.fsWrites++
	p.mu.Unlock()
	fmt.Fprintf(os.Stderr, "  [%dms] fs/write_text_file %s (%d bytes)\n", p.stamp(), r.Path, len(r.Content))
	if !p.fsEnabled {
		return acp.WriteTextFileResponse{}, errors.New("acp-probe: fs capability not enabled")
	}
	if !p.withinSandbox(r.Path) {
		return acp.WriteTextFileResponse{}, fmt.Errorf("acp-probe: path outside sandbox: %s", r.Path)
	}
	_ = os.MkdirAll(filepath.Dir(r.Path), 0o755)
	return acp.WriteTextFileResponse{}, os.WriteFile(r.Path, []byte(r.Content), 0o644)
}

func (p *probe) withinSandbox(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(p.sandbox, abs)
	return err == nil && !strings.HasPrefix(rel, "..")
}

// terminal 系列：Friday 不打算实现，这里计数并回错误，用来验证"不声明能力是否真能免掉"。
func (p *probe) terminalCalled(method string) error {
	p.mu.Lock()
	p.termCalls[method]++
	p.mu.Unlock()
	fmt.Fprintf(os.Stderr, "  [%dms] %s —— 被调用了！\n", p.stamp(), method)
	return errors.New("acp-probe: terminal not supported")
}

func (p *probe) CreateTerminal(_ context.Context, r acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, p.terminalCalled("terminal/create: " + r.Command + " " + strings.Join(r.Args, " "))
}

func (p *probe) KillTerminal(_ context.Context, r acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, p.terminalCalled("terminal/kill")
}

func (p *probe) TerminalOutput(_ context.Context, r acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, p.terminalCalled("terminal/output")
}

func (p *probe) ReleaseTerminal(_ context.Context, r acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, p.terminalCalled("terminal/release")
}

func (p *probe) WaitForTerminalExit(_ context.Context, r acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, p.terminalCalled("terminal/wait_for_exit")
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	var (
		cmdFlag      = flag.String("cmd", "opencode", "agent 可执行文件")
		argsFlag     = flag.String("args", "acp", "agent 参数，逗号分隔")
		cwdFlag      = flag.String("cwd", "", "作业工作目录（默认建一个一次性临时目录）")
		promptFlag   = flag.String("prompt", "在当前目录创建 note.txt，内容写今天的日期；然后执行 shell 命令 uname -a 并告诉我输出。", "提交的 prompt")
		permFlag     = flag.String("permissions", "allow", "权限策略：allow | deny | first")
		fsFlag       = flag.Bool("fs", false, "声明并实现 fs/read|fs/write 能力")
		termFlag     = flag.Bool("terminal", false, "声明 terminal 能力（仍回错误）")
		outFlag      = flag.String("out", "acp-probe.jsonl", "原始报文落盘路径")
		approveBlk   = flag.Duration("approve-block", 0, ">0 时 friday_approval_gate 挂住这么久，模拟人工审批等待")
		approveMode  = flag.String("approve-mode", "sync", "审批形状：sync（挂住工具调用）| async（立即回 pending + 轮询）")
		followUp     = flag.String("followup", "", "第二轮 prompt（模拟审批通过后 Friday 主动唤醒作业）")
		followAfter  = flag.Duration("followup-after", 0, ">0 时：第一轮结束后再等这么久发第二轮")
		followDuring = flag.Duration("followup-during", 0, ">0 时：第一轮仍在跑就并发发第二轮，观察对方如何拒绝")
		timeoutFlg   = flag.Duration("timeout", 5*time.Minute, "整个 turn 的上限")
		cancelFlag   = flag.Duration("cancel-after", 0, ">0 时在该时刻发 session/cancel，观察取消语义")
		mockMCPFlag  = flag.Bool("mock-mcp", false, "在本进程起一个 mock MCP HTTP 门面并挂给 agent")
		mockAuthFlag = flag.Bool("mock-auth", false, "mock 门面要求 Authorization: Bearer（验证 agent 转发 header）")
		mcpURLFlag   = flag.String("mcp-url", "", "额外挂给 agent 的 MCP streamable-HTTP 地址")
	)
	flag.Parse()

	sandbox := *cwdFlag
	if sandbox == "" {
		var err error
		sandbox, err = os.MkdirTemp("", "acp-probe-")
		if err != nil {
			die("建临时目录失败: %v", err)
		}
	}
	absSandbox, _ := filepath.Abs(sandbox)

	f, err := os.Create(*outFlag)
	if err != nil {
		die("建报文文件失败: %v", err)
	}
	defer f.Close()
	tr := &transcript{w: bufio.NewWriter(f), t0: time.Now()}
	defer tr.w.Flush()

	args := []string{}
	if strings.TrimSpace(*argsFlag) != "" {
		args = strings.Split(*argsFlag, ",")
	}
	exe, err := exec.LookPath(*cmdFlag)
	if err != nil {
		die("找不到 agent 可执行文件 %q: %v（PATH=%s）", *cmdFlag, err, os.Getenv("PATH"))
	}

	cmd := exec.Command(exe, args...)
	cmd.Dir = absSandbox
	stdin, err := cmd.StdinPipe()
	if err != nil {
		die("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		die("stdout pipe: %v", err)
	}
	stderrFile := filepath.Join(os.TempDir(), fmt.Sprintf("acp-probe-stderr-%s.log", randHex(4)))
	sf, _ := os.Create(stderrFile)
	cmd.Stderr = sf

	if err := cmd.Start(); err != nil {
		die("启动 agent 失败: %v", err)
	}

	p := newProbe(tr, absSandbox, *permFlag, *fsFlag)
	conn := acp.NewClientSideConnection(p, &teeWriter{w: stdin, t: tr}, &teeReader{r: stdout, t: tr})

	ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlg)
	defer cancel()

	go func() {
		<-conn.Done()
		fmt.Fprintf(os.Stderr, "!! 对端断开（agent 进程退出？） at %dms\n", p.stamp())
	}()

	fmt.Fprintf(os.Stderr, "agent: %s %v\ncwd  : %s\nstderr: %s\n报文 : %s\n\n", exe, args, absSandbox, stderrFile, *outFlag)

	initReq := acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "acp-probe", Version: "0.0.1"},
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: *fsFlag, WriteTextFile: *fsFlag},
			Terminal: *termFlag,
		},
	}
	initRes, err := conn.Initialize(ctx, initReq)
	if err != nil {
		die("initialize 失败: %v", err)
	}
	ib, _ := json.Marshal(initRes)
	fmt.Fprintf(os.Stderr, "== initialize 响应\n   %s\n\n", firstN(string(ib), 1600))

	var mm *mockMCP
	// 空清单也必须是 []：nil 会被编成 mcpServers:null，opencode 的 zod 校验
	// 直接报 Invalid params（expected array, received null），探到的就不是 Friday 的报文形状了。
	servers := []acp.McpServer{}
	if *mockMCPFlag {
		var err error
		mm, err = startMockMCP()
		if err != nil {
			die("起 mock MCP 失败: %v", err)
		}
		mm.block = *approveBlk
		mm.mode = *approveMode
		headers := []acp.HttpHeader{}
		if *mockAuthFlag {
			mm.requireAuth = "probe-" + randHex(6)
			headers = append(headers, acp.HttpHeader{
				Name: "Authorization", Value: "Bearer " + mm.requireAuth,
			})
		}
		defer mm.stop()
		servers = append(servers, acp.McpServer{Http: &acp.McpServerHttpInline{
			Name: "friday-probe", Url: mm.url, Headers: headers,
		}})
		fmt.Fprintf(os.Stderr, "挂给 agent 的 MCP 门面: %s（鉴权=%v）\n\n", mm.url, *mockAuthFlag)
	}
	if *mcpURLFlag != "" {
		servers = append(servers, acp.McpServer{Http: &acp.McpServerHttpInline{
			Name: "extra", Url: *mcpURLFlag, Headers: []acp.HttpHeader{},
		}})
	}

	sess, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: absSandbox, McpServers: servers})
	if err != nil {
		die("session/new 失败: %v", err)
	}
	sb, _ := json.Marshal(sess)
	fmt.Fprintf(os.Stderr, "== session/new 响应\n   %s\n\n", firstN(string(sb), 1200))

	var cancelAt time.Time
	if *cancelFlag > 0 {
		go func() {
			time.Sleep(*cancelFlag)
			cancelAt = time.Now()
			fmt.Fprintf(os.Stderr, ">> 发 session/cancel at %dms\n", p.stamp())
			if err := conn.Cancel(context.Background(), acp.CancelNotification{SessionId: sess.SessionId}); err != nil {
				fmt.Fprintf(os.Stderr, ">> session/cancel 出错: %v\n", err)
			}
		}()
	}

	type turnRes struct {
		res     acp.PromptResponse
		err     error
		elapsed time.Duration
		firedAt int64
	}
	var fuDone chan turnRes
	if *followUp != "" && *followDuring > 0 {
		fuDone = make(chan turnRes, 1)
		go func() {
			time.Sleep(*followDuring)
			s := time.Now()
			r, e := conn.Prompt(ctx, acp.PromptRequest{
				SessionId: sess.SessionId,
				Prompt:    []acp.ContentBlock{acp.TextBlock(*followUp)},
			})
			fuDone <- turnRes{res: r, err: e, elapsed: time.Since(s), firedAt: p.stamp()}
		}()
		fmt.Fprintf(os.Stderr, ">> 第二轮将在第一轮仍在进行时（+%s）并发发出\n", *followDuring)
	}

	start := time.Now()
	res, promptErr := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock(*promptFlag)},
	})
	elapsed := time.Since(start)

	turn1Answer := p.takeAnswer()

	var fu *turnRes
	if *followUp != "" && *followAfter > 0 {
		fmt.Fprintf(os.Stderr, ">> 第一轮已收尾，%s 后在同一条会话上发第二轮（唤醒）\n", *followAfter)
		time.Sleep(*followAfter)
		s := time.Now()
		r, e := conn.Prompt(ctx, acp.PromptRequest{
			SessionId: sess.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock(*followUp)},
		})
		fu = &turnRes{res: r, err: e, elapsed: time.Since(s), firedAt: p.stamp()}
	}
	if fuDone != nil {
		select {
		case t := <-fuDone:
			fu = &t
		case <-time.After(2 * time.Second):
			fmt.Fprintf(os.Stderr, ">> 并发第二轮没有返回（被吞或被阻塞）\n")
		}
	}
	turn2Answer := p.takeAnswer()

	tr.mu.Lock()
	tr.w.Flush()
	tr.mu.Unlock()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	sf.Close()

	fmt.Fprintf(os.Stderr, "\n\n========== 探测结论 ==========\n")
	fmt.Fprintf(os.Stderr, "turn 耗时   : %s\n", elapsed.Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "Prompt 错误 : %v\n", promptErr)
	fmt.Fprintf(os.Stderr, "stopReason  : %q\n", res.StopReason)
	ub, _ := json.Marshal(res.Usage)
	fmt.Fprintf(os.Stderr, "usage       : %s\n", ub)
	fmt.Fprintf(os.Stderr, "usage_update: %s\n", p.lastUsage)
	fmt.Fprintf(os.Stderr, "SessionUpdate: %d 条 / %d 字节 / 最大单条 %d B\n", p.updateCount, p.updateBytes, p.maxMsgBytes)
	if !p.firstUpdate.IsZero() {
		fmt.Fprintf(os.Stderr, "   首条 +%dms，末条 +%dms（turn 共 %dms）\n",
			p.firstUpdate.Sub(p.t0).Milliseconds(), p.lastUpdate.Sub(p.t0).Milliseconds(), elapsed.Milliseconds())
	}
	for k, v := range p.typeCount {
		fmt.Fprintf(os.Stderr, "   %-26s %d\n", k, v)
	}
	fmt.Fprintf(os.Stderr, "权限请求    : %d 次，最大并发 %d，agent 最长等待 %v\n",
		p.permCount, p.permMaxPar, maxDur(p.permWaits))
	fmt.Fprintf(os.Stderr, "   带 locations %d/%d，带 rawInput %d/%d，option kind 分布 %v\n",
		p.permLocations, p.permCount, p.permRawInput, p.permCount, p.optKinds)
	fmt.Fprintf(os.Stderr, "反向 fs     : read %d / write %d（能力声明=%v）\n", p.fsReads, p.fsWrites, *fsFlag)
	fmt.Fprintf(os.Stderr, "反向 terminal: %v（声明=%v）\n", p.termCalls, *termFlag)
	if mm != nil {
		fmt.Fprintf(os.Stderr, "反向 MCP 门面被访问情况:\n   %s\n", mm.summary())
	}
	fmt.Fprintf(os.Stderr, "第一轮 agent 文本（%d 字）:\n   %s\n", len(turn1Answer), firstN(turn1Answer, 500))
	if fu != nil {
		fmt.Fprintf(os.Stderr, "\n---- 第二轮（唤醒）----\n")
		fmt.Fprintf(os.Stderr, "发出时刻  : +%dms\n   耗时 %s，err=%v，stopReason=%q\n",
			fu.firedAt, fu.elapsed.Round(time.Millisecond), fu.err, fu.res.StopReason)
		ub2, _ := json.Marshal(fu.res.Usage)
		fmt.Fprintf(os.Stderr, "   usage: %s\n", ub2)
		fmt.Fprintf(os.Stderr, "   文本（%d 字）: %s\n", len(turn2Answer), firstN(turn2Answer, 500))
	}
	if !cancelAt.IsZero() {
		fmt.Fprintf(os.Stderr, "取消发出时刻: %s（turn 实际 %s）\n", cancelAt.Sub(start).Round(time.Millisecond), elapsed.Round(time.Millisecond))
	}
	tail, _ := os.ReadFile(stderrFile)
	last := strings.Split(strings.TrimRight(string(tail), "\n"), "\n")
	if len(last) > 6 {
		last = last[len(last)-6:]
	}
	fmt.Fprintf(os.Stderr, "agent stderr 末尾:\n   %s\n", strings.Join(last, "\n   "))
	fmt.Fprintf(os.Stderr, "沙箱内容:\n")
	_ = filepath.Walk(absSandbox, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(absSandbox, path)
		fmt.Fprintf(os.Stderr, "   %s (%d bytes)\n", rel, info.Size())
		return nil
	})
}

func maxDur(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
