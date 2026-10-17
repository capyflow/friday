package acp

// manager_test.go 用进程内 fake agent（os.Pipe + JSON-RPC goroutine）驱动
// Manager 全链路回归：基本 turn、空闲回收后的 session/load 重接、恢复失败降级、
// 权限审批两向、取消、进程崩溃恢复、并发槽位、重启标注。不烧真实 agent 配额，
// 结果可复现（P1-A 脚手架的测试侧）。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// ---------- fake agent 侧 ----------

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

type jsonWriter struct {
	mu  sync.Mutex
	w   *bufio.Writer
	seq int64
}

func (j *jsonWriter) send(v map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	b, _ := json.Marshal(v)
	j.w.Write(b)
	j.w.WriteByte('\n')
	j.w.Flush()
}

func (j *jsonWriter) respond(id json.RawMessage, result any) {
	j.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (j *jsonWriter) respondErr(id json.RawMessage, msg string) {
	j.send(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32000, "message": msg}})
}

func (j *jsonWriter) notify(method string, params any) {
	j.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// update 构造一条 session/update 通知的 params。
func update(sessionID, kind string, payload map[string]any) map[string]any {
	u := map[string]any{"sessionUpdate": kind}
	for k, v := range payload {
		u[k] = v
	}
	return map[string]any{"sessionId": sessionID, "update": u}
}

func textBlock(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

// fakeConn 是一次连接的 fake 侧载体（每次 spawn 新建，避免跨连接共享可变状态）。
type fakeConn struct {
	f     *fakeAgent
	w     *jsonWriter
	crash func() // 掐断 agent→client 管道模拟进程崩溃
}

// fakeAgent 是脚本化的 agent 侧实现（同一实例可跨多次 spawn 复用；
// 可变字段以 mu 保护）。
type fakeAgent struct {
	loadSession bool // initialize 里声明 loadSession 能力
	loadOK      bool // session/load 成功？

	mu       sync.Mutex
	methods  []string
	sessions []string
	cur      string

	promptScript func(fc *fakeConn, sessionID string) string // 返回 stopReason
	permSent     chan struct{}                               // 权限请求已发出（非 nil 时）
	cancelSeen   chan struct{}                               // 收到 session/cancel（非 nil 时）
}

func (f *fakeAgent) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.methods...)
}

func (f *fakeAgent) currentSession() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur
}

func (f *fakeAgent) newSessionID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "sess-" + itoa(len(f.sessions)+1)
	f.sessions = append(f.sessions, id)
	f.cur = id
	return id
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (fc *fakeConn) run(r interface{ Read([]byte) (int, error) }) {
	f := fc.f
	dec := json.NewDecoder(r)
	for {
		var msg rpcMsg
		if err := dec.Decode(&msg); err != nil {
			return
		}
		if len(msg.ID) > 0 && msg.Method == "" {
			continue // client 对我们请求的响应（权限答复等），不需要处理
		}
		f.mu.Lock()
		f.methods = append(f.methods, msg.Method)
		f.mu.Unlock()
		switch msg.Method {
		case "initialize":
			fc.w.respond(msg.ID, map[string]any{
				"protocolVersion":   1,
				"agentCapabilities": map[string]any{"loadSession": f.loadSession},
				"authMethods":       []any{},
			})
		case "session/new":
			fc.w.respond(msg.ID, map[string]any{"sessionId": f.newSessionID()})
		case "session/load":
			if f.loadOK {
				// N2：重放两条历史再回空结果（Manager 端应丢弃，不进时间线）。
				sid := f.currentSession()
				fc.w.notify("session/update", update(sid, "agent_message_chunk",
					map[string]any{"content": textBlock("(历史重放) 上一轮内容")}))
				fc.w.notify("session/update", update(sid, "agent_message_chunk",
					map[string]any{"content": textBlock("(历史重放) 第二条")}))
				fc.w.respond(msg.ID, map[string]any{})
			} else {
				fc.w.respondErr(msg.ID, "session not found")
			}
		case "session/prompt":
			sid := f.currentSession()
			promptID := msg.ID
			// 脚本在独立 goroutine 执行：脚本可能阻塞等取消信号，
			// 而 cancel 通知要靠这个 decode 循环才能收到。
			go func() {
				stop := "end_turn"
				if f.promptScript != nil {
					stop = f.promptScript(fc, sid)
				}
				fc.w.respond(promptID, map[string]any{
					"stopReason": stop,
					"usage":      map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15},
				})
			}()
		case "session/cancel":
			if f.cancelSeen != nil {
				select {
				case f.cancelSeen <- struct{}{}:
				default:
				}
			}
		}
	}
}

// defaultScript 演出一轮完整 update 流：思考、两段碎片文本（验证聚合）、
// 工具调用与状态更新、一条应被丢弃的大通知。
func defaultScript(fc *fakeConn, sid string) string {
	fc.w.notify("session/update", update(sid, "agent_thought_chunk",
		map[string]any{"content": textBlock("思考：先看看目录")}))
	fc.w.notify("session/update", update(sid, "agent_message_chunk",
		map[string]any{"content": textBlock("你好，")}))
	fc.w.notify("session/update", update(sid, "agent_message_chunk",
		map[string]any{"content": textBlock("这是回答。")}))
	fc.w.notify("session/update", update(sid, "tool_call", map[string]any{
		"toolCallId": "t1", "title": "运行 ls", "kind": "execute", "status": "in_progress"}))
	fc.w.notify("session/update", update(sid, "tool_call_update", map[string]any{
		"toolCallId": "t1", "status": "completed"}))
	fc.w.notify("session/update", update(sid, "available_commands_update",
		map[string]any{"commands": []any{}}))
	return "end_turn"
}

// sendPermissionRequest 由脚本调用：向 client 发一次权限请求并等它被处理。
func (fc *fakeConn) sendPermissionRequest(sid, title string) {
	fc.w.mu.Lock()
	fc.w.seq++
	reqID, _ := json.Marshal(fc.w.seq)
	fc.w.mu.Unlock()
	fc.w.send(map[string]any{"jsonrpc": "2.0", "id": reqID,
		"method": "session/request_permission", "params": map[string]any{
			"sessionId": sid,
			"options": []any{
				map[string]any{"optionId": "allow", "kind": "allow_once", "name": "允许"},
				map[string]any{"optionId": "reject", "kind": "reject_once", "name": "拒绝"},
			},
			"toolCall": map[string]any{"toolCallId": "t9", "title": title, "kind": "execute"},
		}})
}

// ---------- 测试装配 ----------

// spawnFake 用 os.Pipe 对接 fake agent，替换 Manager.spawn（每次调用新建一对管道，
// 支持崩溃后重新拉起）。
func spawnFake(f *fakeAgent) func(config.AcpAgentConfig, string, time.Duration, permissionHook, func()) (*agentProc, error) {
	return func(cfg config.AcpAgentConfig, workspace string, handshake time.Duration,
		onPermission permissionHook, onExit func()) (*agentProc, error) {

		toAgentR, toAgentW, err := os.Pipe() // client → agent
		if err != nil {
			return nil, err
		}
		fromAgentR, fromAgentW, err := os.Pipe() // agent → client
		if err != nil {
			return nil, err
		}
		var closeOnce sync.Once
		kill := func() {
			closeOnce.Do(func() {
				toAgentW.Close()
				fromAgentR.Close()
			})
		}
		fc := &fakeConn{
			f:     f,
			w:     &jsonWriter{w: bufio.NewWriter(fromAgentW)},
			crash: func() { _ = fromAgentW.Close() },
		}
		b := newBridge(onPermission)
		conn := acp.NewClientSideConnection(b, toAgentW, fromAgentR)
		go func() {
			fc.run(toAgentR)
			fromAgentW.Close()
		}()

		p := &agentProc{
			agent: cfg.Name, workspace: workspace,
			conn: conn, events: b.events, bridge: b,
			done: conn.Done(), kill: kill,
		}
		ictx, icancel := context.WithTimeout(context.Background(), handshake)
		defer icancel()
		initRes, err := conn.Initialize(ictx, acp.InitializeRequest{
			ProtocolVersion: acp.ProtocolVersionNumber,
			ClientInfo:      &acp.Implementation{Name: "friday-test", Version: "0"},
		})
		if err != nil {
			kill()
			return nil, err
		}
		p.loadSession = initRes.AgentCapabilities.LoadSession
		go func() {
			<-p.done
			kill()
			if onExit != nil {
				onExit()
			}
		}()
		return p, nil
	}
}

func testConfig(t *testing.T) config.AcpConfig {
	t.Helper()
	return config.AcpConfig{
		Enabled: true, MaxConcurrent: 2, IdleKillSeconds: 600,
		TurnTimeoutSeconds: 60, HandshakeTimeoutSeconds: 5,
		Agents: []config.AcpAgentConfig{{Name: "fake", Command: "fake-bin"}},
	}
}

func newTestManager(t *testing.T, cfg *config.AcpConfig, approver *approval.Queue, f *fakeAgent) *Manager {
	t.Helper()
	dir := t.TempDir()
	if cfg == nil {
		c := testConfig(t)
		cfg = &c
	}
	m := NewManager(func() config.AcpConfig { return *cfg }, approver, filepath.Join(dir, "threads.json"))
	if f != nil {
		m.spawn = spawnFake(f)
	}
	t.Cleanup(m.Close)
	return m
}

func mustThread(t *testing.T, m *Manager) Thread {
	t.Helper()
	th, err := m.Create("fake", t.TempDir(), "")
	if err != nil {
		t.Fatalf("创建 thread 失败: %v", err)
	}
	return th
}

// collector 收集 hooks 回调（并发安全）。
type collector struct {
	mu    sync.Mutex
	text  strings.Builder
	steps []string
	stats []string
}

func (c *collector) hooks() TurnHooks {
	return TurnHooks{
		OnText:    func(t string) { c.mu.Lock(); c.text.WriteString(t); c.mu.Unlock() },
		OnStep:    func(st agent.Step) { c.mu.Lock(); c.steps = append(c.steps, st.Name+"/"+st.Status); c.mu.Unlock() },
		OnStatus:  func(msg string) { c.mu.Lock(); c.stats = append(c.stats, msg); c.mu.Unlock() },
		OnThought: func(string) {},
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

func containsMethod(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ---------- 用例 ----------

func TestSendBasicTurn(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	c := &collector{}
	res, err := m.Send(context.Background(), th.ID, "打个招呼", nil, c.hooks())
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.Answer != "你好，这是回答。" {
		t.Fatalf("回答聚合不符: %q", res.Answer)
	}
	if res.StopReason != "end_turn" || res.In != 10 || res.Out != 5 {
		t.Fatalf("turn 结果不符: %+v", res)
	}
	if !strings.Contains(c.text.String(), "你好，这是回答。") {
		t.Fatalf("SSE 文本缺失: %q", c.text.String())
	}
	if len(c.steps) != 2 || c.steps[0] != "运行 ls/run" || c.steps[1] != "运行 ls/ok" {
		t.Fatalf("步骤事件不符: %v", c.steps)
	}
	got, ok := m.Get(th.ID)
	if !ok {
		t.Fatal("thread 丢失")
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "user" || got.Messages[1].Role != "assistant" {
		t.Fatalf("投影不符: %+v", got.Messages)
	}
	if got.Title == "" {
		t.Fatal("标题未从首条消息生成")
	}
}

func TestIdleReconnectUsesLoadSession(t *testing.T) {
	cfg := testConfig(t)
	cfg.IdleKillSeconds = 1
	f := &fakeAgent{loadSession: true, loadOK: true, promptScript: defaultScript}
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)

	if _, err := m.Send(context.Background(), th.ID, "第一轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	waitFor(t, 3*time.Second, "空闲回收", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.live) == 0
	})

	if _, err := m.Send(context.Background(), th.ID, "第二轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	joined := strings.Join(f.recorded(), ",")
	if strings.Count(joined, "session/load") != 1 {
		t.Fatalf("应恰好重接一次，实际方法序列: %v", f.recorded())
	}
	got, _ := m.Get(th.ID)
	if got.Degraded {
		t.Fatal("重接成功不应标记降级")
	}
	if len(got.Messages) != 4 { // 两轮问答；重放不入投影
		t.Fatalf("重放不应进入投影，实际: %+v", got.Messages)
	}
}

func TestLoadFailDegraded(t *testing.T) {
	cfg := testConfig(t)
	cfg.IdleKillSeconds = 1
	f := &fakeAgent{loadSession: true, loadOK: false, promptScript: defaultScript}
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)

	if _, err := m.Send(context.Background(), th.ID, "第一轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	waitFor(t, 3*time.Second, "空闲回收", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.live) == 0
	})

	res, err := m.Send(context.Background(), th.ID, "第二轮", nil, TurnHooks{})
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Fatalf("降级后仍应正常完成: %+v", res)
	}
	got, _ := m.Get(th.ID)
	if !got.Degraded {
		t.Fatal("恢复失败应标记降级")
	}
	found := false
	for _, e := range got.Messages {
		if e.Role == "note" && strings.Contains(e.Text, "上下文已重置") {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少降级标注: %+v", got.Messages)
	}
}

func TestDegradeResumeSkipsLoad(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents[0].DegradeResume = true
	cfg.IdleKillSeconds = 1
	f := &fakeAgent{loadSession: true, loadOK: true, promptScript: defaultScript}
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)
	if _, err := m.Send(context.Background(), th.ID, "第一轮", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "空闲回收", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.live) == 0
	})
	if _, err := m.Send(context.Background(), th.ID, "第二轮", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if containsMethod(f.recorded(), "session/load") {
		t.Fatal("degradeResume 应跳过 session/load")
	}
}

func TestPermissionApproved(t *testing.T) {
	approver := approval.New("off", nil, time.Second) // off 模式验证 RequestAlways 无视模式
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.sendPermissionRequest(sid, "执行 rm")
		if f.permSent != nil {
			f.permSent <- struct{}{}
		}
		time.Sleep(400 * time.Millisecond) // 等审批流走完（测试 goroutine 在此期间点卡）
		fc.w.notify("session/update", update(sid, "agent_message_chunk",
			map[string]any{"content": textBlock("已获授权，完成。")}))
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	decided := make(chan struct{})
	go func() {
		defer close(decided)
		<-f.permSent
		waitFor(t, 2*time.Second, "审批入队", func() bool { return len(approver.Pending()) == 1 })
		approver.Decide(approver.Pending()[0].ID, true)
	}()

	res, err := m.Send(context.Background(), th.ID, "需要授权的活", nil, TurnHooks{})
	<-decided
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.Answer != "已获授权，完成。" {
		t.Fatalf("回答不符: %q", res.Answer)
	}
}

func TestPermissionDenied(t *testing.T) {
	approver := approval.New("confirm", nil, time.Second)
	f := &fakeAgent{}
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.sendPermissionRequest(sid, "执行危险操作")
		time.Sleep(400 * time.Millisecond)
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	go func() {
		waitFor(t, 2*time.Second, "审批入队", func() bool { return len(approver.Pending()) == 1 })
		approver.Decide(approver.Pending()[0].ID, false)
	}()
	if _, err := m.Send(context.Background(), th.ID, "干个危险的", nil, TurnHooks{}); err != nil {
		t.Fatalf("拒绝后 turn 应正常收尾: %v", err)
	}
}

func TestStopCancelled(t *testing.T) {
	f := &fakeAgent{}
	f.cancelSeen = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.w.notify("session/update", update(sid, "agent_message_chunk",
			map[string]any{"content": textBlock("开头…")}))
		<-f.cancelSeen // 等 client 的取消通知
		return "cancelled"
	}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	done := make(chan struct{})
	var res TurnResult
	var sendErr error
	go func() {
		defer close(done)
		res, sendErr = m.Send(context.Background(), th.ID, "长任务", nil, TurnHooks{})
	}()
	waitFor(t, 2*time.Second, "turn 启动", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.live[th.ID] != nil
	})
	time.Sleep(100 * time.Millisecond) // 让第一段 chunk 先到
	if !m.Stop(th.ID) {
		t.Fatal("Stop 应成功发出取消")
	}
	<-done
	if sendErr != nil {
		t.Fatalf("取消不应算失败（C8）: %v", sendErr)
	}
	if res.StopReason != "cancelled" {
		t.Fatalf("stopReason 应为 cancelled: %+v", res)
	}
}

func TestCrashThenRecover(t *testing.T) {
	cfg := testConfig(t)
	f := &fakeAgent{loadSession: true, loadOK: true}
	var once sync.Once
	f.promptScript = func(fc *fakeConn, sid string) string {
		var crashed bool
		f.mu.Lock()
		crashed = len(f.sessions) == 0 || f.cur == f.sessions[0]
		f.mu.Unlock()
		fc.w.notify("session/update", update(sid, "agent_message_chunk",
			map[string]any{"content": textBlock("崩溃前…")}))
		if crashed {
			once.Do(func() {
				time.Sleep(50 * time.Millisecond)
				fc.crash() // 第一轮：中途掐断 agent→client 管道
			})
			time.Sleep(2 * time.Second) // 等 client 侧感知断开
		}
		return "end_turn"
	}
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)

	_, err := m.Send(context.Background(), th.ID, "第一轮", nil, TurnHooks{})
	if err == nil {
		t.Fatal("崩溃轮应返回错误")
	}
	got, _ := m.Get(th.ID)
	found := false
	for _, e := range got.Messages {
		if e.Role == "note" && strings.Contains(e.Text, "连接中断") {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少崩溃标注: %+v", got.Messages)
	}

	// 恢复：重新拉起 + session/load 成功 → 不降级。
	if _, err := m.Send(context.Background(), th.ID, "第二轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("恢复轮失败: %v", err)
	}
	got, _ = m.Get(th.ID)
	if got.Degraded {
		t.Fatal("load 成功不应降级")
	}
	if !containsMethod(f.recorded(), "session/load") {
		t.Fatalf("恢复应尝试 session/load: %v", f.recorded())
	}
}

func TestConcurrencySlotFull(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxConcurrent = 1
	f := &fakeAgent{promptScript: func(fc *fakeConn, sid string) string {
		time.Sleep(300 * time.Millisecond)
		return "end_turn"
	}}
	m := newTestManager(t, &cfg, nil, f)
	th1 := mustThread(t, m)
	th2 := mustThread(t, m)

	done1 := make(chan struct{})
	go func() { defer close(done1); _, _ = m.Send(context.Background(), th1.ID, "先", nil, TurnHooks{}) }()
	waitFor(t, 2*time.Second, "第一个 turn 启动", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.live[th1.ID] != nil
	})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if _, err := m.Send(ctx2, th2.ID, "后", nil, TurnHooks{}); err == nil {
		t.Fatal("槽位满时应随 ctx 超时失败")
	} else if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "并发已满") {
		t.Fatalf("错误应为取消/并发满: %v", err)
	}
	<-done1
}

func TestRestartMarksInterrupted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	// 预置一个 running 态的 thread 文件（模拟重启前崩溃）。
	body := `{"threads":[{"id":"t1","agent":"fake","workspace":"/tmp","title":"x","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z","state":"running","messages":[]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	m := NewManager(func() config.AcpConfig { return cfg }, nil, path)
	defer m.Close()
	got, ok := m.Get("t1")
	if !ok {
		t.Fatal("thread 未加载")
	}
	if got.State != StateInterrupted {
		t.Fatalf("running 应转 interrupted，实际 %q", got.State)
	}
}

func TestDeleteThread(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	if _, err := m.Send(context.Background(), th.ID, "hello", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if !m.Delete(th.ID) {
		t.Fatal("删除失败")
	}
	if _, ok := m.Get(th.ID); ok {
		t.Fatal("删除后仍能取到")
	}
	if _, err := m.Send(context.Background(), th.ID, "again", nil, TurnHooks{}); err == nil {
		t.Fatal("删除后 Send 应报错")
	}
}

func TestOrphanedStateComputed(t *testing.T) {
	cfg := testConfig(t)
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)

	// 删除 agent 配置（cfgFn 返回同一变量，改它即改配置源）。
	orphaned := testConfig(t)
	orphaned.Agents = nil
	cfg = orphaned
	m.Reload(orphaned)

	list := m.List()
	if len(list) != 1 || list[0].State != StateOrphaned {
		t.Fatalf("agent 删除后应展示 orphaned: %+v", list)
	}
	if _, err := m.Send(context.Background(), th.ID, "hello", nil, TurnHooks{}); err == nil {
		t.Fatal("orphaned thread 的 Send 应报错")
	}
}
