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

	configOptions []map[string]any // session/new、load 响应携带的会话配置项（nil = agent 不回报该能力）
	models        map[string]any   // 协议原生 models 状态（nil = agent 不回报）
	modes         map[string]any   // 协议原生 modes 状态（nil = agent 不回报）

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
			resp := map[string]any{"sessionId": f.newSessionID()}
			if f.configOptions != nil {
				resp["configOptions"] = f.configOptions
			}
			if f.models != nil {
				resp["models"] = f.models
			}
			if f.modes != nil {
				resp["modes"] = f.modes
			}
			fc.w.respond(msg.ID, resp)
		case "session/load":
			if f.loadOK {
				// N2：重放两条历史再回空结果（Manager 端应丢弃，不进时间线）。
				sid := f.currentSession()
				fc.w.notify("session/update", update(sid, "agent_message_chunk",
					map[string]any{"content": textBlock("(历史重放) 上一轮内容")}))
				fc.w.notify("session/update", update(sid, "agent_message_chunk",
					map[string]any{"content": textBlock("(历史重放) 第二条")}))
				resp := map[string]any{}
				if f.configOptions != nil {
					resp["configOptions"] = f.configOptions
				}
				if f.models != nil {
					resp["models"] = f.models
				}
				if f.modes != nil {
					resp["modes"] = f.modes
				}
				fc.w.respond(msg.ID, resp)
			} else {
				fc.w.respondErr(msg.ID, "session not found")
			}
		case "session/set_config_option":
			var p struct {
				ConfigId string `json:"configId"`
				Value    string `json:"value"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			f.mu.Lock()
			for _, o := range f.configOptions {
				if o["id"] == p.ConfigId {
					o["currentValue"] = p.Value
				}
			}
			out := append([]map[string]any{}, f.configOptions...)
			f.mu.Unlock()
			fc.w.respond(msg.ID, map[string]any{"configOptions": out})
		case "session/set_model":
			var p struct {
				ModelId string `json:"modelId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			f.mu.Lock()
			if f.models != nil {
				f.models["currentModelId"] = p.ModelId
			}
			f.mu.Unlock()
			fc.w.respond(msg.ID, map[string]any{})
		case "session/set_mode":
			var p struct {
				ModeId string `json:"modeId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			f.mu.Lock()
			if f.modes != nil {
				f.modes["currentModeId"] = p.ModeId
			}
			f.mu.Unlock()
			fc.w.respond(msg.ID, map[string]any{})
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
	// 进度更新（codebuddy 实测不带 status）：不应新起一行工具调用
	fc.w.notify("session/update", update(sid, "tool_call_update", map[string]any{
		"toolCallId": "t1"}))
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

// 降级新开会话意味着旧上下文已丢：上一会话的水位必须清掉，界面不应显示过期数字。
func TestDegradedSessionClearsContextWatermark(t *testing.T) {
	cfg := testConfig(t)
	cfg.IdleKillSeconds = 1
	f := &fakeAgent{loadSession: true, loadOK: false, promptScript: defaultScript}
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)

	if _, err := m.Send(context.Background(), th.ID, "第一轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	// 伪造上一会话留下的水位（fake agent 默认不上报 usage_update）
	m.mu.Lock()
	m.threads[th.ID].ContextUsed = 12000
	m.threads[th.ID].ContextSize = 30000
	m.mu.Unlock()
	waitFor(t, 3*time.Second, "空闲回收", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.live) == 0
	})

	if _, err := m.Send(context.Background(), th.ID, "第二轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	got, _ := m.Get(th.ID)
	if !got.Degraded {
		t.Fatal("恢复失败应标记降级")
	}
	if got.ContextUsed != 0 || got.ContextSize != 0 {
		t.Fatalf("降级新开会话应清空旧水位，实际 used=%d size=%d", got.ContextUsed, got.ContextSize)
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

// turn 结束（完成/超时/被停止）时，无人处理的权限审批卡必须被自动撤下——
// 否则用户会对着一个已死的进程点批准，SDK 的请求 goroutine 也白等 30 分钟。
func TestPermissionReleasedOnTurnEnd(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.sendPermissionRequest(sid, "无人处理的授权")
		time.Sleep(150 * time.Millisecond) // 等权限请求进审批队列
		return "end_turn"                  // agent 不等答复直接收尾（超时场景的代理）
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	go func() {
		waitFor(t, 2*time.Second, "审批卡入队", func() bool { return len(approver.Pending()) == 1 })
	}()
	res, err := m.Send(context.Background(), th.ID, "快点决定", nil, TurnHooks{})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q", res.StopReason)
	}
	// turn 已结束：卡必须被自动撤下（用户从未点过）。
	waitFor(t, 2*time.Second, "审批卡自动撤下", func() bool { return len(approver.Pending()) == 0 })
}

// usage_update 是上下文水位（size=窗口总量、used=占用），turn 结束后应随投影
// 落盘（M-5）；单轮 in/out 记账仍以 PromptResponse.Usage 为准，互不混淆。
func TestUsageContextWatermark(t *testing.T) {
	f := &fakeAgent{}
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.w.notify("session/update", update(sid, "usage_update", map[string]any{
			"size": 200000, "used": 12000,
			"cost": map[string]any{"amount": 0.15, "currency": "USD"},
		}))
		fc.w.notify("session/update", update(sid, "agent_message_chunk",
			map[string]any{"content": textBlock("水位已更新。")}))
		return "end_turn"
	}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	res, err := m.Send(context.Background(), th.ID, "看看水位", nil, TurnHooks{})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.In != 10 || res.Out != 5 {
		t.Fatalf("单轮记账应来自 PromptResponse.Usage，实际 in=%d out=%d", res.In, res.Out)
	}
	got, ok := m.Get(th.ID)
	if !ok {
		t.Fatal("thread 不存在")
	}
	if got.ContextUsed != 12000 || got.ContextSize != 200000 || got.ContextCost != 0.15 {
		t.Fatalf("上下文水位未落盘: used=%d size=%d cost=%v", got.ContextUsed, got.ContextSize, got.ContextCost)
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

// 未选目录时回退到临时会话目录（<数据目录>/acp-sessions/<threadID>，测试里替换根），
// 显式指定的目录仍然优先。
func TestCreateFallsBackToTempSessionsDir(t *testing.T) {
	base := t.TempDir()
	old := sessionsBaseDirFn
	sessionsBaseDirFn = func() (string, error) { return base, nil }
	t.Cleanup(func() { sessionsBaseDirFn = old })

	cfg := testConfig(t)
	m := newTestManager(t, &cfg, nil, nil)

	th, err := m.Create("fake", "", "")
	if err != nil {
		t.Fatalf("不选目录应能创建（回退临时目录）: %v", err)
	}
	want := filepath.Join(base, th.ID)
	if th.Workspace != want {
		t.Fatalf("应回退到 %s，实际 %s", want, th.Workspace)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("回退目录应已创建: %v", err)
	}

	explicit := filepath.Join(t.TempDir(), "chosen")
	th2, err := m.Create("fake", explicit, "")
	if err != nil {
		t.Fatalf("显式目录创建失败: %v", err)
	}
	if th2.Workspace != explicit {
		t.Fatalf("显式目录应优先，实际 %s", th2.Workspace)
	}
}

// ---------- 会话能力（统一清单：configOptions / models / modes 归一化） ----------

// sessionOptions 是 fake 回报的会话配置项：模型下拉 + 模式下拉。
func sessionOptions(currentModel string) []map[string]any {
	return []map[string]any{
		{"id": "model", "name": "模型", "category": "model", "type": "select", "currentValue": currentModel,
			"options": []any{
				map[string]any{"value": "builtin/a", "name": "GLM A"},
				map[string]any{"value": "builtin/b", "name": "GLM B"},
			}},
		{"id": "mode", "name": "模式", "category": "mode", "type": "select", "currentValue": "build",
			"options": []any{
				map[string]any{"value": "build", "name": "Build"},
				map[string]any{"value": "plan", "name": "Plan"},
			}},
	}
}

// sessionModels / sessionModes 是协议原生形态（cc 类 agent 只报这两样）。
func sessionModels(current string) map[string]any {
	return map[string]any{
		"currentModelId": current,
		"availableModels": []any{
			map[string]any{"modelId": "m/opus", "name": "Opus"},
			map[string]any{"modelId": "m/haiku", "name": "Haiku"},
		},
	}
}

func sessionModes(current string) map[string]any {
	return map[string]any{
		"currentModeId": current,
		"availableModes": []any{
			map[string]any{"id": "default", "name": "Default"},
			map[string]any{"id": "plan", "name": "Plan Mode"},
		},
	}
}

func findCap(caps []Capability, id string) *Capability {
	for i := range caps {
		if caps[i].ID == id {
			return &caps[i]
		}
	}
	return nil
}

// 会话响应里的 configOptions 应被归一化透出；set_config_option 切换后
// currentValue 跟随。Capabilities 在会话未建时直接拉起 agent 建会话。
func TestThreadCapabilitiesAndSetOption(t *testing.T) {
	f := &fakeAgent{configOptions: sessionOptions("builtin/a"), promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	caps, err := m.Capabilities(th.ID)
	if err != nil {
		t.Fatalf("Capabilities 失败: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("应透出 2 个能力项: %+v", caps)
	}
	model := findCap(caps, "model")
	if model == nil {
		t.Fatalf("缺少 model 能力项: %+v", caps)
	}
	if model.Kind != "model" || model.Source != "config-option" || model.CurrentValue != "builtin/a" ||
		len(model.Values) != 2 || model.Values[1].Value != "builtin/b" || model.Values[1].Name != "GLM B" {
		t.Fatalf("model 项归一化不符: %+v", model)
	}
	if mode := findCap(caps, "approval_mode"); mode == nil || mode.Source != "config-option" {
		t.Fatalf("mode 应归一化为 approval_mode（config-option）: %+v", caps)
	}

	got, err := m.SetCapability(th.ID, "model", "builtin/b")
	if err != nil {
		t.Fatalf("SetCapability 失败: %v", err)
	}
	if !containsMethod(f.recorded(), "session/set_config_option") {
		t.Fatalf("应路由到 set_config_option: %v", f.recorded())
	}
	if c := findCap(got, "model"); c == nil || c.CurrentValue != "builtin/b" {
		t.Fatalf("切换后 currentValue 未跟随: %+v", got)
	}
}

// turn 中的 config_options_update（agent 切换后回推全量）应刷新缓存。
func TestConfigOptionUpdateDuringTurn(t *testing.T) {
	f := &fakeAgent{configOptions: sessionOptions("builtin/a")}
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.w.notify("session/update", update(sid, "config_options_update",
			map[string]any{"configOptions": sessionOptions("builtin/b")}))
		fc.w.notify("session/update", update(sid, "agent_message_chunk",
			map[string]any{"content": textBlock("好的。")}))
		return "end_turn"
	}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	if _, err := m.Send(context.Background(), th.ID, "切个模型", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	caps, err := m.Capabilities(th.ID)
	if err != nil {
		t.Fatalf("Capabilities 失败: %v", err)
	}
	if c := findCap(caps, "model"); c == nil || c.CurrentValue != "builtin/b" {
		t.Fatalf("config_options_update 应刷新缓存: %+v", caps)
	}
}

// cc 形态：不报 configOptions，只报协议原生 models/modes——模型与审批模式
// 从原生状态兜底归一化，切换分别路由到 set_model / set_mode；thought_level
// 缺席（agent 不支持，不强求）。
func TestSessionModelAndModeFallback(t *testing.T) {
	f := &fakeAgent{models: sessionModels("m/opus"), modes: sessionModes("default"), promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	caps, err := m.Capabilities(th.ID)
	if err != nil {
		t.Fatalf("Capabilities 失败: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("应透出 2 个能力项（model + approval_mode）: %+v", caps)
	}
	model := findCap(caps, "model")
	if model == nil || model.Source != "session-model" || model.CurrentValue != "m/opus" ||
		len(model.Values) != 2 || model.Values[0].Name != "Opus" {
		t.Fatalf("models 应兜底归一化为 model 能力: %+v", model)
	}
	mode := findCap(caps, "approval_mode")
	if mode == nil || mode.Source != "session-mode" || mode.CurrentValue != "default" {
		t.Fatalf("modes 应兜底归一化为 approval_mode 能力: %+v", mode)
	}
	if findCap(caps, "thought_level") != nil {
		t.Fatalf("agent 未报 thought_level 时不应出现该项: %+v", caps)
	}

	if _, err := m.SetCapability(th.ID, "model", "m/haiku"); err != nil {
		t.Fatalf("SetCapability(model) 失败: %v", err)
	}
	if !containsMethod(f.recorded(), "session/set_model") {
		t.Fatalf("应路由到 set_model: %v", f.recorded())
	}
	if _, err := m.SetCapability(th.ID, "approval_mode", "plan"); err != nil {
		t.Fatalf("SetCapability(approval_mode) 失败: %v", err)
	}
	if !containsMethod(f.recorded(), "session/set_mode") {
		t.Fatalf("应路由到 set_mode: %v", f.recorded())
	}
}

// zcode 形态：configOptions 与协议原生 models/modes 双轨并存——config-option
// 优先（热切链路已验证），原生状态被去重；set 走 config-option 路由。
func TestConfigOptionWinsOverSessionState(t *testing.T) {
	f := &fakeAgent{configOptions: sessionOptions("builtin/a"),
		models: sessionModels("m/opus"), modes: sessionModes("default"), promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	caps, err := m.Capabilities(th.ID)
	if err != nil {
		t.Fatalf("Capabilities 失败: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("双轨并存应去重为 2 项: %+v", caps)
	}
	if c := findCap(caps, "model"); c == nil || c.Source != "config-option" {
		t.Fatalf("model 应取 config-option 来源: %+v", caps)
	}

	if _, err := m.SetCapability(th.ID, "model", "builtin/b"); err != nil {
		t.Fatalf("SetCapability 失败: %v", err)
	}
	for _, mth := range f.recorded() {
		if mth == "session/set_model" {
			t.Fatalf("config-option 优先时不应走 set_model: %v", f.recorded())
		}
	}
}

// turn 中的 current_mode_update（agent 切换审批模式后的推送）应刷新缓存，
// 且不覆盖已缓存的可选模式列表。
func TestModeStateUpdateDuringTurn(t *testing.T) {
	f := &fakeAgent{modes: sessionModes("default")}
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.w.notify("session/update", update(sid, "current_mode_update",
			map[string]any{"currentModeId": "plan"}))
		fc.w.notify("session/update", update(sid, "agent_message_chunk",
			map[string]any{"content": textBlock("好的。")}))
		return "end_turn"
	}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	if _, err := m.Send(context.Background(), th.ID, "切个模式", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	caps, err := m.Capabilities(th.ID)
	if err != nil {
		t.Fatalf("Capabilities 失败: %v", err)
	}
	mode := findCap(caps, "approval_mode")
	if mode == nil || mode.CurrentValue != "plan" {
		t.Fatalf("current_mode_update 应刷新当前模式: %+v", mode)
	}
	if len(mode.Values) != 2 {
		t.Fatalf("推送不应覆盖可选模式列表: %+v", mode)
	}
}

// agent 什么都不回报（协议可选能力）时透出空清单，前端据此隐藏全部 chip。
func TestCapabilitiesEmptyWhenAgentOmits(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	caps, err := m.Capabilities(th.ID)
	if err != nil {
		t.Fatalf("Capabilities 失败: %v", err)
	}
	if len(caps) != 0 {
		t.Fatalf("agent 未回报时应为空清单: %+v", caps)
	}
	if _, err := m.SetCapability(th.ID, "model", "x"); err == nil {
		t.Fatalf("空清单上切换应报错（能力不存在）")
	}
}

func TestTimeoutKillsAgentProcess(t *testing.T) {
	f := &fakeAgent{}
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	f.promptScript = func(*fakeConn, string) string {
		<-blocked // 模拟不理会客户端取消的长任务
		return "end_turn"
	}
	cfg := testConfig(t)
	cfg.TurnTimeoutSeconds = 1
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)

	res, err := m.Send(context.Background(), th.ID, "长任务", nil, TurnHooks{})
	if err != nil {
		t.Fatalf("超时不应算失败: %v", err)
	}
	if res.StopReason != "timeout" {
		t.Fatalf("stopReason 应为 timeout: %+v", res)
	}
	m.mu.Lock()
	alive := m.live[th.ID]
	m.mu.Unlock()
	if alive != nil {
		t.Fatal("超时后 agent 进程必须被回收，不能继续在后台跑")
	}
}

func TestStopKillsUnresponsiveAgent(t *testing.T) {
	oldGrace := stopKillGrace
	stopKillGrace = 100 * time.Millisecond
	t.Cleanup(func() { stopKillGrace = oldGrace })

	f := &fakeAgent{}
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	f.promptScript = func(*fakeConn, string) string {
		<-blocked // 不理会 session/cancel
		return "end_turn"
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
	time.Sleep(50 * time.Millisecond)
	if !m.Stop(th.ID) {
		t.Fatal("Stop 应成功发出取消")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("停止未生效时应强杀进程并让 Send 返回")
	}
	if sendErr != nil {
		t.Fatalf("强杀收尾不应算失败: %v", sendErr)
	}
	if res.StopReason != "cancelled" {
		t.Fatalf("强杀应按用户停止收尾: %+v", res)
	}
	m.mu.Lock()
	alive := m.live[th.ID]
	m.mu.Unlock()
	if alive != nil {
		t.Fatal("停止后 agent 进程必须被回收")
	}
}
