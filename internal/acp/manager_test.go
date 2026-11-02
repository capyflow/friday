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
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/memory"
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
	// resetOnSession=true 模拟"agent 不把选项存进会话"：每次 session/new、load
	// 都把 configOptions / models 的当前值打回初始默认，等客户端重新设。
	resetOnSession bool
	extraRoots     bool // initialize 里声明 sessionCapabilities.additionalDirectories

	configOptions []map[string]any // session/new、load 响应携带的会话配置项（nil = agent 不回报该能力）
	models        map[string]any   // 协议原生 models 状态（nil = agent 不回报）
	modes         map[string]any   // 协议原生 modes 状态（nil = agent 不回报）

	mu           sync.Mutex
	methods      []string
	sessions     []string
	cur          string
	setCalls     []string          // "config:model=builtin/b" / "model:m2" / "mode:plan"
	spawns       int               // 进程拉起次数（验证 M-6 的复用）
	prompts      [][]string        // 每次 session/prompt 的文本块（按顺序）
	optDefault   map[string]string // 首次建会话时记下的初始值（供 resetOnSession 回滚）
	modelDefault string
	newReq       map[string]any // 最近一次 session/new 的 params（查 cwd/additionalDirectories）
	loadReq      map[string]any // 最近一次 session/load 的 params

	promptScript func(fc *fakeConn, sessionID string) string // 返回 stopReason
	permSent     chan struct{}                               // 权限请求已发出（非 nil 时）
	cancelSeen   chan struct{}                               // 收到 session/cancel（非 nil 时）
}

func (f *fakeAgent) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.methods...)
}

// resetOptions 在 resetOnSession 时把选项打回初始默认；另外首次调用时记下默认值。
func (f *fakeAgent) resetOptions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.optDefault == nil {
		f.optDefault = map[string]string{}
		for _, o := range f.configOptions {
			if id, ok := o["id"].(string); ok {
				if cur, ok := o["currentValue"].(string); ok {
					f.optDefault[id] = cur
				}
			}
		}
		if f.models != nil {
			if cur, ok := f.models["currentModelId"].(string); ok {
				f.modelDefault = cur
			}
		}
	}
	if !f.resetOnSession {
		return
	}
	for _, o := range f.configOptions {
		if id, ok := o["id"].(string); ok {
			if def, ok := f.optDefault[id]; ok {
				o["currentValue"] = def
			}
		}
	}
	if f.models != nil && f.modelDefault != "" {
		f.models["currentModelId"] = f.modelDefault
	}
}

// knownSession 校验 set_* 请求带的 sessionId 是这条连接上真实存在的会话。
func (f *fakeAgent) knownSession(sid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s == sid {
			return true
		}
	}
	return false
}

func (f *fakeAgent) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns
}

func (f *fakeAgent) promptBlocks() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.prompts))
	copy(out, f.prompts)
	return out
}

func (f *fakeAgent) setRecorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.setCalls...)
}

// record 存下 session/new / session/load 的 params（用例据此核对 cwd 与扩展根）。
func (f *fakeAgent) record(dst *map[string]any, raw json.RawMessage) {
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	f.mu.Lock()
	*dst = p
	f.mu.Unlock()
}

// sessionNewParams / sessionLoadParams 返回最近一次对应请求的 params。
func (f *fakeAgent) sessionNewParams() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newReq
}

func (f *fakeAgent) sessionLoadParams() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadReq
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
			caps := map[string]any{"loadSession": f.loadSession}
			if f.extraRoots {
				caps["sessionCapabilities"] = map[string]any{"additionalDirectories": map[string]any{}}
			}
			fc.w.respond(msg.ID, map[string]any{
				"protocolVersion":   1,
				"agentCapabilities": caps,
				"authMethods":       []any{},
			})
		case "session/new":
			f.record(&f.newReq, msg.Params)
			f.resetOptions()
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
			f.record(&f.loadReq, msg.Params)
			f.resetOptions()
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
				ConfigId  string `json:"configId"`
				Value     string `json:"value"`
				SessionId string `json:"sessionId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if !f.knownSession(p.SessionId) {
				fc.w.respondErr(msg.ID, "unknown session: "+p.SessionId)
				break
			}
			f.mu.Lock()
			f.setCalls = append(f.setCalls, "config:"+p.ConfigId+"="+p.Value)
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
				ModelId   string `json:"modelId"`
				SessionId string `json:"sessionId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if !f.knownSession(p.SessionId) {
				fc.w.respondErr(msg.ID, "unknown session: "+p.SessionId)
				break
			}
			f.mu.Lock()
			f.setCalls = append(f.setCalls, "model:"+p.ModelId)
			if f.models != nil {
				f.models["currentModelId"] = p.ModelId
			}
			f.mu.Unlock()
			fc.w.respond(msg.ID, map[string]any{})
		case "session/set_mode":
			var p struct {
				ModeId    string `json:"modeId"`
				SessionId string `json:"sessionId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if !f.knownSession(p.SessionId) {
				fc.w.respondErr(msg.ID, "unknown session: "+p.SessionId)
				break
			}
			f.mu.Lock()
			f.setCalls = append(f.setCalls, "mode:"+p.ModeId)
			if f.modes != nil {
				f.modes["currentModeId"] = p.ModeId
			}
			f.mu.Unlock()
			fc.w.respond(msg.ID, map[string]any{})
		case "session/prompt":
			sid := f.currentSession()
			promptID := msg.ID
			var pp struct {
				Prompt []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(msg.Params, &pp)
			f.mu.Lock()
			var texts []string
			for _, b := range pp.Prompt {
				if b.Type == "text" {
					texts = append(texts, b.Text)
				}
			}
			f.prompts = append(f.prompts, texts)
			f.mu.Unlock()
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
	return func(cfg config.AcpAgentConfig, cwd string, handshake time.Duration,
		onPermission permissionHook, onExit func()) (*agentProc, error) {
		f.mu.Lock()
		f.spawns++
		f.mu.Unlock()

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
			agent: cfg.Name, cwd: cwd,
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
		p.extraRoots = initRes.AgentCapabilities.SessionCapabilities.AdditionalDirectories != nil
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

// mustWorkspace 建一个工作区（不传目录就用一个新的临时目录）。
func mustWorkspace(t *testing.T, m *Manager, dirs ...string) Workspace {
	t.Helper()
	if len(dirs) == 0 {
		dirs = []string{t.TempDir()}
	}
	ws, err := m.CreateWorkspace("", dirs)
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}
	return ws
}

func mustThread(t *testing.T, m *Manager) Thread {
	t.Helper()
	return mustThreadIn(t, m, mustWorkspace(t, m))
}

// mustThreadIn 在指定工作区里建会话（cwd 留空 → 工作区主目录）。
func mustThreadIn(t *testing.T, m *Manager, ws Workspace) Thread {
	t.Helper()
	th, err := m.CreateSession("fake", ws.ID, "", "")
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
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

func (c *collector) statuses() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.stats...)
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

func TestSendRecordsWorkspaceChanges(t *testing.T) {
	f := &fakeAgent{}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m)
	th := mustThreadIn(t, m, ws)
	f.promptScript = func(fc *fakeConn, sid string) string {
		if err := os.WriteFile(filepath.Join(ws.Dirs[0], "note.txt"), []byte("from agent\n"), 0o600); err != nil {
			t.Errorf("写测试文件失败: %v", err)
		}
		return defaultScript(fc, sid)
	}

	res, err := m.Send(context.Background(), th.ID, "写个文件", nil, TurnHooks{})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.Changes == nil || !res.Changes.Snapshot {
		t.Fatalf("本轮应有快照: %+v", res.Changes)
	}
	if len(res.Changes.Files) != 1 || res.Changes.Files[0].Path != "note.txt" || res.Changes.Files[0].Status != "added" {
		t.Fatalf("改动清单不符: %+v", res.Changes.Files)
	}
	got, _ := m.Get(th.ID)
	if len(got.Messages) < 2 || got.Messages[1].Changes == nil || got.Messages[1].Changes.TurnID != res.Changes.TurnID {
		t.Fatalf("投影没有带上改动摘要: %+v", got.Messages)
	}
	full, err := m.Changes(th.ID, res.Changes.TurnID)
	if err != nil {
		t.Fatalf("读取改动详情失败: %v", err)
	}
	if len(full.Files) != 1 || !strings.Contains(full.Files[0].Diff, "+from agent") {
		t.Fatalf("改动详情缺 diff: %+v", full.Files)
	}
	if _, err := m.Revert(th.ID, res.Changes.TurnID); err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dirs[0], "note.txt")); !os.IsNotExist(err) {
		t.Fatalf("还原后文件仍存在: %v", err)
	}
	got, _ = m.Get(th.ID)
	if got.Messages[1].Changes == nil || got.Messages[1].Changes.RevertedAt == nil {
		t.Fatalf("投影未标记已还原: %+v", got.Messages[1].Changes)
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
	c := &collector{}
	if _, err := m.Send(ctx2, th2.ID, "后", nil, c.hooks()); err == nil {
		t.Fatal("槽位满时应随 ctx 超时失败")
	} else if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "并发已满") {
		t.Fatalf("错误应为取消/并发满: %v", err)
	}
	// 排队不是静默等待：等槽位期间要先投一条状态占位，前端气泡才有话说。
	if st := c.statuses(); len(st) == 0 || !strings.Contains(st[0], "跑满") {
		t.Fatalf("槽位满时应先投排队状态: %v", st)
	}
	<-done1
}

func TestRestartMarksInterrupted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	// 预置一个 running 态的 thread 文件（模拟重启前崩溃）。这是 261029 之前的旧
	// 投影形态：会话直接带 workspace 目录、没有 workspaceId。
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
	if got.Cwd != "/tmp" {
		t.Fatalf("旧键 workspace 的目录应迁进 cwd，实际 %q", got.Cwd)
	}
	// 旧 thread 自动归入一个同目录工作区，并把迁移结果写回投影文件
	if got.WorkspaceID == "" {
		t.Fatal("应已归属工作区")
	}
	ws, ok := m.GetWorkspace(got.WorkspaceID)
	if !ok {
		t.Fatalf("工作区 %s 未建出", got.WorkspaceID)
	}
	if len(ws.Dirs) != 1 || ws.Dirs[0] != "/tmp" || ws.Name != "tmp" {
		t.Fatalf("迁移出的工作区不符: %+v", ws)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved threadFile
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("迁移后的投影应可解析: %v", err)
	}
	if len(saved.Workspaces) != 1 || len(saved.Threads) != 1 || saved.Threads[0].WorkspaceID != ws.ID {
		t.Fatalf("迁移结果未落盘: %s", data)
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

// 工作区一个目录都不给时回退到临时会话目录（<数据目录>/acp-sessions/<工作区ID>，
// 测试里替换根）；给了目录就以目录为准。会话的主目录始终取自工作区。
func TestCreateFallsBackToTempSessionsDir(t *testing.T) {
	base := t.TempDir()
	old := sessionsBaseDirFn
	sessionsBaseDirFn = func() (string, error) { return base, nil }
	t.Cleanup(func() { sessionsBaseDirFn = old })

	cfg := testConfig(t)
	m := newTestManager(t, &cfg, nil, nil)

	ws, err := m.CreateWorkspace("", nil)
	if err != nil {
		t.Fatalf("不加目录也应能建工作区（回退临时目录）: %v", err)
	}
	want := filepath.Join(base, ws.ID)
	if len(ws.Dirs) != 1 || ws.Dirs[0] != want {
		t.Fatalf("应回退到 %s，实际 %v", want, ws.Dirs)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("回退目录应已创建: %v", err)
	}
	if ws.Name != filepath.Base(want) {
		t.Fatalf("没起名应回落到目录名，实际 %q", ws.Name)
	}
	th, err := m.CreateSession("fake", ws.ID, "", "")
	if err != nil {
		t.Fatalf("在回退工作区里建会话失败: %v", err)
	}
	if th.Cwd != want {
		t.Fatalf("会话主目录应是工作区主目录，实际 %s", th.Cwd)
	}

	explicit := filepath.Join(t.TempDir(), "chosen")
	ws2, err := m.CreateWorkspace("我的项目", []string{explicit})
	if err != nil {
		t.Fatalf("显式目录创建失败: %v", err)
	}
	if ws2.Dirs[0] != explicit {
		t.Fatalf("显式目录应优先，实际 %v", ws2.Dirs)
	}
	if ws2.Name != "我的项目" {
		t.Fatalf("名字应照用户给的，实际 %q", ws2.Name)
	}
	// 目录还不存在也要能用（建出来），与旧版选目录的行为一致
	if info, err := os.Stat(explicit); err != nil || !info.IsDir() {
		t.Fatalf("显式目录应自动创建: %v", err)
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

// 单轮超时：先 cancel、标记脏、宽限后才回收（M-6 后连接是共享的，不能为一轮
// 立刻打断同组别的会话）；接近上限时会先投一条状态帧。
func TestTimeoutDefersKillWithGrace(t *testing.T) {
	oldGrace := timeoutKillGrace
	timeoutKillGrace = 60 * time.Millisecond
	t.Cleanup(func() { timeoutKillGrace = oldGrace })

	f := &fakeAgent{}
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	f.promptScript = func(*fakeConn, string) string {
		<-blocked // 模拟不理会客户端取消的长任务
		return "end_turn"
	}
	cfg := testConfig(t)
	cfg.TurnTimeoutSeconds = 2 // 1s 时出"接近上限"状态帧，2s 时中止
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)

	c := &collector{}
	res, err := m.Send(context.Background(), th.ID, "长任务", nil, c.hooks())
	if err != nil {
		t.Fatalf("超时不应算失败: %v", err)
	}
	if res.StopReason != "timeout" {
		t.Fatalf("stopReason 应为 timeout: %+v", res)
	}
	// 中止后立刻：连接还在（等 agent 自己停），标成脏
	if lv := m.liveOf(th.ID); lv == nil || !lv.dirty.Load() {
		t.Fatal("超时后应保留连接并标记脏，等宽限/下次使用再收")
	}
	// 投影里的注记写清楚原因与下一步
	got, _ := m.Get(th.ID)
	last := got.Messages[len(got.Messages)-1]
	if last.Role != "note" || !strings.Contains(last.Text, "超时") || !strings.Contains(last.Text, "继续") {
		t.Fatalf("中断注记应说清超时与可继续: %+v", last)
	}
	// 宽限到点、同组没有别的轮次 → 回收
	waitFor(t, 2*time.Second, "宽限后回收连接", func() bool { return m.liveOf(th.ID) == nil })

	found := false
	for _, st := range c.statuses() {
		if strings.Contains(st, "接近单轮上限") {
			found = true
		}
	}
	if !found {
		t.Fatalf("接近上限时应投一条状态帧，实际 %v", c.statuses())
	}
}

// 客户端断开（不是用户点停止）：停因记为 cancelled、注记写"连接已断开"，
// 并且立刻回收进程（这一轮没人接收结果了）。
func TestClientDisconnectCancelsTurn(t *testing.T) {
	f := &fakeAgent{}
	started := make(chan struct{}, 1)
	f.promptScript = func(*fakeConn, string) string {
		started <- struct{}{}
		time.Sleep(2 * time.Second) // 等客户端断开
		return "end_turn"
	}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel() // 等价于页面关闭/网络中断
	}()
	res, err := m.Send(ctx, th.ID, "干到一半断开", nil, TurnHooks{})
	if err != nil {
		t.Fatalf("断开不应算失败: %v", err)
	}
	if res.StopReason != "cancelled" {
		t.Fatalf("客户端断开的停因应为 cancelled（不是 timeout），实际 %q", res.StopReason)
	}
	got, _ := m.Get(th.ID)
	last := got.Messages[len(got.Messages)-1]
	if !strings.Contains(last.Text, "连接已断开") {
		t.Fatalf("注记应写明连接断开: %+v", last)
	}
	if m.liveOf(th.ID) != nil {
		t.Fatal("没人接收结果时应立刻回收进程")
	}
}

// 超时后 agent 一直不收尾（宽限内也没停）：下一次发消息前必须先把脏连接换掉，
// 保证同一 session 不会两轮并发。
func TestDirtyConnectionReplacedOnNextTurn(t *testing.T) {
	oldGrace := timeoutKillGrace
	timeoutKillGrace = 30 * time.Second // 让宽限不参与，纯粹验"下次使用"时换连接
	t.Cleanup(func() { timeoutKillGrace = oldGrace })

	f := &fakeAgent{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	f.promptScript = func(fc *fakeConn, sid string) string {
		if calls.Add(1) == 1 {
			<-release // 第一轮卡到超时也不收尾
			return "end_turn"
		}
		return defaultScript(fc, sid)
	}
	cfg := testConfig(t)
	cfg.TurnTimeoutSeconds = 1
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)

	if _, err := m.Send(context.Background(), th.ID, "第一轮卡住", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if !m.liveOf(th.ID).dirty.Load() {
		t.Fatal("超时后连接应是脏的")
	}
	spawnsBefore := f.spawnCount()
	res, err := m.Send(context.Background(), th.ID, "再试一次", nil, TurnHooks{})
	if err != nil {
		t.Fatalf("脏连接后的下一轮应重接后成功: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Fatalf("第二轮应正常收尾: %+v", res)
	}
	if f.spawnCount() != spawnsBefore+1 {
		t.Fatalf("脏连接应先回收再重连（process 数 +1），实际 %d → %d", spawnsBefore, f.spawnCount())
	}
	if m.liveOf(th.ID).dirty.Load() {
		t.Fatal("重接后的连接不应还是脏的")
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

// 手动标题：覆盖首条消息派生的标题；清空后下一轮按那时的消息重新派生。
func TestRenameThread(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	if _, err := m.Send(context.Background(), th.ID, "## 帮我看看登录跳转\n细节在下面", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(th.ID); got.Title != "帮我看看登录跳转" {
		t.Fatalf("首轮应派生标题，实际 %q", got.Title)
	}

	renamed, err := m.RenameThread(th.ID, "  手动   标题  ")
	if err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if renamed.Title != "手动 标题" {
		t.Fatalf("应折叠空白，实际 %q", renamed.Title)
	}
	if got, _ := m.Get(th.ID); got.Title != "手动 标题" {
		t.Fatalf("改名应落盘可读回，实际 %q", got.Title)
	}

	long := strings.Repeat("标", 80)
	if got, _ := m.RenameThread(th.ID, long); len([]rune(got.Title)) != 61 || !strings.HasSuffix(got.Title, "…") {
		t.Fatalf("过长标题应截到 60 个 rune 加省略号，实际 %q", got.Title)
	}

	// 清空 = 回到自动：下一轮按那时的消息重新取标题
	if _, err := m.RenameThread(th.ID, "  "); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(th.ID); got.Title != "" {
		t.Fatalf("清空后标题应为空，实际 %q", got.Title)
	}
	if _, err := m.Send(context.Background(), th.ID, "再来一轮的要点", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(th.ID); got.Title != "再来一轮的要点" {
		t.Fatalf("清空后下一轮应重新派生，实际 %q", got.Title)
	}

	if _, err := m.RenameThread("nope", "x"); err == nil {
		t.Fatal("不存在的会话改名应报错")
	}
}

// 用户手动选的能力要能扛过重开：agent 不把选项存进会话（resetOnSession 模拟），
// 重接/新开会话后 Friday 按投影里记的那份意图把它重放回去。
func TestCapabilityChoiceSurvivesReconnect(t *testing.T) {
	f := &fakeAgent{loadSession: true, loadOK: true, resetOnSession: true,
		configOptions: sessionOptions("builtin/a"), promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	if _, err := m.Send(context.Background(), th.ID, "开工", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetCapability(th.ID, CapKindModel, "builtin/b"); err != nil {
		t.Fatalf("切模型失败: %v", err)
	}

	// 杀掉进程 = 杀掉 app 重开 / 空闲回收：下次打开会 session/load，agent 回默认
	m.killLive(th.ID, "测试：模拟重开")
	caps, err := m.Capabilities(th.ID)
	if err != nil {
		t.Fatalf("重开后取能力失败: %v", err)
	}
	calls := f.setRecorded()
	if len(calls) < 2 || calls[0] != "config:model=builtin/b" || calls[1] != "config:model=builtin/b" {
		t.Fatalf("重连后应把选择重放一次，实际调用: %v", calls)
	}
	got := ""
	for _, c := range caps {
		if c.ID == CapKindModel {
			got = c.CurrentValue
		}
	}
	if got != "builtin/b" {
		t.Fatalf("重开后模型应仍是 builtin/b，实际 %q", got)
	}

	// 选择也进了投影：文件里能读回 capChoices
	data, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved threadFile
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Threads) != 1 || saved.Threads[0].CapChoices[CapKindModel] != "builtin/b" {
		t.Fatalf("能力选择应写进投影，实际 %+v", saved.Threads[0].CapChoices)
	}
}

// 协议原生 models 来源的能力同样要扛过重开（cc 这类只报 models/modes 的 agent）。
func TestSessionModelChoiceSurvivesReconnect(t *testing.T) {
	f := &fakeAgent{loadSession: true, loadOK: true, resetOnSession: true,
		models: sessionModels("m1"), promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	if _, err := m.Send(context.Background(), th.ID, "开工", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetCapability(th.ID, CapKindModel, "m2"); err != nil {
		t.Fatalf("切模型失败: %v", err)
	}
	m.killLive(th.ID, "测试：模拟重开")
	caps, err := m.Capabilities(th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls := f.setRecorded(); len(calls) < 2 || calls[1] != "model:m2" {
		t.Fatalf("重连后应按记录重放 set_session_model，实际 %v", calls)
	}
	for _, c := range caps {
		if c.ID == CapKindModel && c.CurrentValue != "m2" {
			t.Fatalf("重开后模型应仍是 m2，实际 %q", c.CurrentValue)
		}
	}
}

// 记录里有一项新版 agent 不再提供：跳过它，别让会话建不起来。
func TestUnknownRecordedChoiceIsSkipped(t *testing.T) {
	f := &fakeAgent{loadSession: true, loadOK: true, resetOnSession: true,
		configOptions: sessionOptions("builtin/a"), promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	if _, err := m.Send(context.Background(), th.ID, "开工", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	th2 := m.threads[th.ID]
	th2.CapChoices = map[string]string{"other:gone": "x", CapKindModel: "builtin/b"}
	_ = m.saveLocked()
	m.mu.Unlock()
	m.killLive(th.ID, "测试：模拟重开")
	if _, err := m.Capabilities(th.ID); err != nil {
		t.Fatalf("未知能力项应跳过而不是报错: %v", err)
	}
	// 这条用例没走过手动切换，重放应恰好一次；未知的 other:gone 不应发出任何调用
	if calls := f.setRecorded(); len(calls) != 1 || calls[0] != "config:model=builtin/b" {
		t.Fatalf("已知能力项应重放一次、未知项跳过，实际 %v", calls)
	}
}

// 工作区说明：agent 看不到"工作区"概念，每个（新/重开的）会话首轮带一次说明，
// 同会话后续轮次不再重复；目录清单变了会重新交代。说明不进投影时间线。
func TestWorkspaceBriefInjectedPerSession(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	f := &fakeAgent{extraRoots: true, promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m, dirA, dirB)
	th := mustThreadIn(t, m, ws)

	if _, err := m.Send(context.Background(), th.ID, "第一句", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	blocks := f.promptBlocks()
	if len(blocks) != 1 || len(blocks[0]) != 2 {
		t.Fatalf("首轮应是 [工作区说明, 用户消息] 两个文本块，实际 %+v", blocks)
	}
	brief := blocks[0][0]
	for _, want := range []string{"工作区「", dirA, "主目录", dirB, "扩展目录"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("说明里应包含 %q，实际: %s", want, brief)
		}
	}
	if !strings.Contains(brief, "以这份列表为准") && !strings.Contains(brief, "以上面这份列表为准") {
		t.Fatalf("说明应明确「以列表为准、别扫上级」，实际: %s", brief)
	}

	// 同一会话的第二轮：不再重复注入
	if _, err := m.Send(context.Background(), th.ID, "第二句", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if got := f.promptBlocks(); len(got) != 2 || len(got[1]) != 1 || got[1][0] != "第二句" {
		t.Fatalf("同会话后续轮次不该再带说明，实际 %+v", got)
	}

	// 投影里只有用户原文，说明不进时间线
	if got, _ := m.Get(th.ID); got.Messages[0].Text != "第一句" {
		t.Fatalf("投影第一条应是用户原文，实际 %q", got.Messages[0].Text)
	}

	// 进程回收后重开：新会话（对 agent 而言上下文重来）要重新交代
	m.killLive(th.ID, "测试：模拟重开")
	if _, err := m.Send(context.Background(), th.ID, "重开后的第一句", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	got := f.promptBlocks()
	if len(got) != 3 || len(got[2]) != 2 {
		t.Fatalf("重开后首轮应重新带说明，实际 %+v", got)
	}
}

// 目录清单变了（工作区加了文件夹）：同一会话的下一轮也要重新交代。
func TestWorkspaceBriefRefreshOnFolderChange(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	f := &fakeAgent{extraRoots: true, promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m, dirA)
	th := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "开始", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddWorkspaceDir(ws.ID, dirB); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(context.Background(), th.ID, "加了目录之后", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	blocks := f.promptBlocks()
	if len(blocks) != 2 || len(blocks[1]) != 2 {
		t.Fatalf("目录变化后应重新交代，实际 %+v", blocks)
	}
	if !strings.Contains(blocks[1][0], dirB) {
		t.Fatalf("重新交代应包含新目录，实际: %s", blocks[1][0])
	}
}

// agent 不支持多目录：说明里要写明那些文件夹没下发，别让 agent 以为能访问。
func TestWorkspaceBriefNotesUnsupportedMultiDir(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript} // extraRoots=false
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m, t.TempDir(), t.TempDir())
	th := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "开始", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	brief := f.promptBlocks()[0][0]
	if !strings.Contains(brief, "未下发") || !strings.Contains(brief, "不要尝试访问") {
		t.Fatalf("不支持多目录时说明应写明未下发，实际: %s", brief)
	}
}

// M-6：同 agent、同主目录的多个会话共享一个进程（一条连接承载多个 session），
// 各自的事件按 sessionId 分流，互不串台。
func TestSharedProcessAcrossThreads(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m, t.TempDir()) // 同 cwd
	th1 := mustThreadIn(t, m, ws)
	th2 := mustThreadIn(t, m, ws)

	if _, err := m.Send(context.Background(), th1.ID, "会话一说", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("第一个会话应拉起 1 个进程，实际 %d", got)
	}
	if _, err := m.Send(context.Background(), th2.ID, "会话二说", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("同 agent+目录的第二个会话应复用进程，实际拉起 %d 次", got)
	}
	// 每次 session/new 各建一个会话：两条 thread 的 sessionId 不应相同
	// （mustThreadIn 返回的是创建时的副本，运行后要重新取）
	a1, _ := m.Get(th1.ID)
	a2, _ := m.Get(th2.ID)
	if a1.SessionID == "" || a2.SessionID == "" || a1.SessionID == a2.SessionID {
		t.Fatalf("两个会话应有各自的 sessionId: %q / %q", a1.SessionID, a2.SessionID)
	}
	// 能力缓存是会话级的：各自都能读到清单
	for _, id := range []string{th1.ID, th2.ID} {
		if _, err := m.Capabilities(id); err != nil {
			t.Fatalf("会话 %s 取能力失败: %v", id, err)
		}
	}
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("取能力不该再拉进程，实际 %d 次", got)
	}
}

// 目录不同就不共用：复用键是 agent + 主目录（进程的 cwd 与工具范围都是进程级的）。
func TestDifferentCwdUsesDifferentProcess(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	cfg := testConfig(t)
	cfg.MaxConcurrent = 2
	m := newTestManager(t, &cfg, nil, f)
	wsA := mustWorkspace(t, m, t.TempDir())
	wsB := mustWorkspace(t, m, t.TempDir())
	thA := mustThreadIn(t, m, wsA)
	thB := mustThreadIn(t, m, wsB)
	if _, err := m.Send(context.Background(), thA.ID, "A", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(context.Background(), thB.ID, "B", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if got := f.spawnCount(); got != 2 {
		t.Fatalf("不同主目录应各起一个进程，实际 %d", got)
	}
}

// 并发上限按"进程数"计：上限 1 时，同一工作区的两个会话照样能对话（复用一条连接）。
func TestConcurrencyCountsProcessesNotThreads(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	cfg := testConfig(t)
	cfg.MaxConcurrent = 1
	m := newTestManager(t, &cfg, nil, f)
	ws := mustWorkspace(t, m, t.TempDir())
	th1 := mustThreadIn(t, m, ws)
	th2 := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th1.ID, "一", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(context.Background(), th2.ID, "二", nil, TurnHooks{}); err != nil {
		t.Fatalf("同连接上的第二个会话不该因为上限 1 被挡: %v", err)
	}
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("应只有 1 条连接，实际 %d", got)
	}
}

// 删会话只摘 session：同连接上还有别的会话时进程留着；删到最后一个才回收。
func TestDeleteOneSessionKeepsSharedProcess(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m, t.TempDir())
	th1 := mustThreadIn(t, m, ws)
	th2 := mustThreadIn(t, m, ws)
	for _, th := range []Thread{th1, th2} {
		if _, err := m.Send(context.Background(), th.ID, "hi", nil, TurnHooks{}); err != nil {
			t.Fatal(err)
		}
	}
	if !m.Delete(th1.ID) {
		t.Fatal("删除失败")
	}
	if m.procOf(procKey("fake", th1.Cwd)) == nil {
		t.Fatal("还有其它会话在用时不该回收进程")
	}
	if _, err := m.Send(context.Background(), th2.ID, "还在", nil, TurnHooks{}); err != nil {
		t.Fatalf("同连接上的会话应继续可用: %v", err)
	}
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("删掉一个会话后仍应复用原连接，实际拉起 %d 次", got)
	}
	if !m.Delete(th2.ID) {
		t.Fatal("删除失败")
	}
	if ps := m.procOf(procKey("fake", th1.Cwd)); ps != nil {
		t.Fatal("最后一个会话删掉后应回收进程")
	}
}

// 权限请求按 sessionId 路由：同一条连接上的两个会话，谁请求就进谁的审批卡。
func TestPermissionRoutedBySession(t *testing.T) {
	approver := approval.New("off", nil, time.Second)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	n := 0
	f.promptScript = func(fc *fakeConn, sid string) string {
		n++
		if n == 1 {
			return "end_turn" // 第一轮只为把会话建起来，不弹权限
		}
		fc.sendPermissionRequest(sid, "执行 rm")
		f.permSent <- struct{}{}
		time.Sleep(300 * time.Millisecond) // 等测试 goroutine 点卡
		fc.w.notify("session/update", update(sid, "agent_message_chunk",
			map[string]any{"content": textBlock("已获授权。")}))
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	ws := mustWorkspace(t, m, t.TempDir())
	th1 := mustThreadIn(t, m, ws)
	th2 := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th1.ID, "先建会话", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}

	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "审批入队", func() bool { return len(approver.Pending()) == 1 })
		approver.Decide(approver.Pending()[0].ID, true)
	}()
	res, err := m.Send(context.Background(), th2.ID, "需要授权的活", nil, TurnHooks{})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.Answer != "已获授权。" {
		t.Fatalf("回答不符: %q", res.Answer)
	}
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("两个会话应共用一条连接，实际拉起 %d 次", got)
	}
}

func TestHandoffCreatesContinuationThread(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	if _, err := m.Send(context.Background(), th.ID, "先看下这个项目", nil, TurnHooks{}); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}

	nt, err := m.Handoff(th.ID)
	if err != nil {
		t.Fatalf("交接失败: %v", err)
	}
	if nt.ID == th.ID || nt.WorkspaceID != th.WorkspaceID || nt.Agent != th.Agent || nt.Cwd != th.Cwd {
		t.Fatalf("续会话应复用工作区/agent/主目录: old=%+v new=%+v", th, nt)
	}
	if !strings.Contains(nt.Handoff, "先看下这个项目") || !strings.Contains(nt.Handoff, "你好，这是回答。") {
		t.Fatalf("交接摘要没带上可见对话: %q", nt.Handoff)
	}
	if !strings.Contains(nt.Title, "续") {
		t.Fatalf("续会话标题应带标记: %q", nt.Title)
	}
	if len(nt.Messages) != 1 || nt.Messages[0].Role != "note" {
		t.Fatalf("续会话应有交接说明 note: %+v", nt.Messages)
	}
	old, _ := m.Get(th.ID)
	if len(old.Messages) != 2 || old.Handoff != "" {
		t.Fatalf("旧会话应原样保留: %+v", old)
	}

	if _, err := m.Send(context.Background(), nt.ID, "继续", nil, TurnHooks{}); err != nil {
		t.Fatalf("续会话首轮失败: %v", err)
	}
	blocks := f.promptBlocks()
	if len(blocks) < 2 {
		t.Fatalf("应有两轮 prompt，实际 %d", len(blocks))
	}
	joined := strings.Join(blocks[len(blocks)-1], "\n")
	if !strings.Contains(joined, "交接摘要") || !strings.Contains(joined, "先看下这个项目") {
		t.Fatalf("续会话首轮没把摘要发给 agent: %q", joined)
	}
	got, _ := m.Get(nt.ID)
	if got.Handoff != "" {
		t.Fatalf("摘要送达后应清空，实际 %q", got.Handoff)
	}
}

func TestHandoffRejectsEmptyThread(t *testing.T) {
	m := newTestManager(t, nil, nil, &fakeAgent{promptScript: defaultScript})
	th := mustThread(t, m)
	if _, err := m.Handoff(th.ID); err == nil {
		t.Fatal("空会话不应允许交接")
	}
}

func TestWorkspaceBriefIncludesProjectMemory(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m)
	store := memory.NewWikiStore("", "")
	if _, _, err := store.SaveScoped(ws.ID, "", "项目约定", "这个仓库用 pnpm，不要用 npm", "admin"); err != nil {
		t.Fatal(err)
	}
	m.SetMemory(store)
	th := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "开始干活", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	blocks := f.promptBlocks()
	if len(blocks) == 0 {
		t.Fatal("没有记录 prompt")
	}
	joined := strings.Join(blocks[0], "\n")
	if !strings.Contains(joined, "项目记忆") || !strings.Contains(joined, "pnpm") {
		t.Fatalf("首轮没有注入项目记忆: %q", joined)
	}
	if _, _, err := store.SaveScoped(ws.ID, "", "项目约定", "这个仓库用 pnpm；Node 版本 22", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(context.Background(), th.ID, "继续", nil, TurnHooks{}); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	blocks = f.promptBlocks()
	joined = strings.Join(blocks[len(blocks)-1], "\n")
	if !strings.Contains(joined, "Node 版本 22") {
		t.Fatalf("项目记忆更新后下一轮应重新交代: %q", joined)
	}
}

// codex 形态的 modes：没有「每次都问」的等价档，工作区默认档位只能退到
// workspace-write（可写但受限），且必须把实际落点讲清楚（V-1/V-4）。
func codexSessionModes(current string) map[string]any {
	return map[string]any{
		"currentModeId": current,
		"availableModes": []any{
			map[string]any{"id": "read-only", "name": "Read Only"},
			map[string]any{"id": "workspace-write", "name": "Workspace Write"},
			map[string]any{"id": "agent", "name": "Auto review"},
			map[string]any{"id": "agent-full-access", "name": "Full access"},
		},
	}
}

func TestWorkspacePolicyLandedValue(t *testing.T) {
	cases := []struct {
		name         string
		policy       string
		wantValue    string
		wantFallback bool
	}{
		{"每次都问退到工作区内可写", WorkspacePolicyAsk, "workspace-write", true},
		{"只读命中等价档", WorkspacePolicyReadOnly, "read-only", false},
		{"自动命中全自动档", WorkspacePolicyAuto, "agent-full-access", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeAgent{modes: codexSessionModes("agent"), promptScript: defaultScript}
			m := newTestManager(t, nil, nil, f)
			ws := mustWorkspace(t, m)
			if err := m.SetPolicy(ws.ID, c.policy); err != nil {
				t.Fatalf("设档位失败: %v", err)
			}
			th := mustThreadIn(t, m, ws)
			if _, err := m.Send(context.Background(), th.ID, "改一下", nil, TurnHooks{}); err != nil {
				t.Fatalf("Send 失败: %v", err)
			}
			got, _ := m.Get(th.ID)
			if got.PolicyApplied != c.policy {
				t.Fatalf("档位应记为已下发 %s，实际 %q（note=%q）", c.policy, got.PolicyApplied, got.PolicyNote)
			}
			if got.PolicyValue != c.wantValue {
				t.Fatalf("实际落点应为 %s，实际 %q", c.wantValue, got.PolicyValue)
			}
			if got.PolicyFallback != c.wantFallback {
				t.Fatalf("退让标记应为 %v，实际 %v", c.wantFallback, got.PolicyFallback)
			}
			note := got.PolicyNote
			if c.wantFallback && (!strings.Contains(note, "workspace-write") || !strings.Contains(note, "逐条")) {
				t.Fatalf("退让时说明要写清实际落点与差别: %q", note)
			}
			if !c.wantFallback && note != "" {
				t.Fatalf("精确命中不该有提示: %q", note)
			}
			want := "mode:" + c.wantValue
			for _, call := range f.setCalls {
				if call == want {
					return
				}
			}
			t.Fatalf("应把 %s 下发给 agent，实际调用 %v", want, f.setCalls)
		})
	}
}

// 只有 build/plan 这种「能不能干活」的档时，「每次都问」宁可不碰设置也不悄悄把
// agent 关进只读；这时要把原因、可选项和「它自己的默认档可能更宽松」一起说清。
func TestWorkspacePolicyDeclinesAmbiguousAgent(t *testing.T) {
	// zcode 形态：只有「能不能干活」两档，没有审批强度可谈。
	f := &fakeAgent{modes: map[string]any{
		"currentModeId": "build",
		"availableModes": []any{
			map[string]any{"id": "build", "name": "Build"},
			map[string]any{"id": "plan", "name": "Plan"},
		},
	}, promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m) // 默认 ask
	th := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "改一下", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	got, _ := m.Get(th.ID)
	if got.PolicyApplied != "" || got.PolicyValue != "" {
		t.Fatalf("没有可用档时不该记成已下发: %+v", got)
	}
	for _, want := range []string{"没有能对应「每次都问」", "更宽松", "手动选一个"} {
		if !strings.Contains(got.PolicyNote, want) {
			t.Fatalf("提示应含 %q，实际 %q", want, got.PolicyNote)
		}
	}
	for _, call := range f.setCalls {
		if strings.HasPrefix(call, "mode:") {
			t.Fatalf("不该改 agent 的审批模式: %v", f.setCalls)
		}
	}
}

// waitThreadState 轮询直到会话进入目标状态（排队/运行这类瞬时状态只能靠轮询抓到）。
func waitThreadState(t *testing.T, m *Manager, id, want string) Thread {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last Thread
	for time.Now().Before(deadline) {
		th, ok := m.Get(id)
		if ok {
			last = th
			if th.State == want {
				return th
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待会话状态 %s 超时，最后为 %q", want, last.State)
	return Thread{}
}

// statusLog 收集一轮的 status 帧（并发安全）。
func statusLog() (TurnHooks, func() []string) {
	var mu sync.Mutex
	var got []string
	return TurnHooks{OnStatus: func(s string) {
			mu.Lock()
			got = append(got, s)
			mu.Unlock()
		}}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string{}, got...)
		}
}

// 并发槽满时不能只把消息吞掉：会话要标成「排队中」、时间线要说清谁占着，
// 空闲连接也要提前让位（V-3）。
func TestSendQueuesWhenSlotsExhausted(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	f := &fakeAgent{}
	f.promptScript = func(fc *fakeConn, sid string) string {
		if calls.Add(1) == 1 {
			<-gate // 第一轮占着这条连接不放
		}
		return defaultScript(fc, sid)
	}
	cfg := testConfig(t)
	cfg.MaxConcurrent = 1
	cfg.IdleKillSeconds = 1 // 有人排队时空闲连接很快让位
	m := newTestManager(t, &cfg, nil, f)
	thA := mustThreadIn(t, m, mustWorkspace(t, m, t.TempDir()))
	thB := mustThreadIn(t, m, mustWorkspace(t, m, t.TempDir()))

	hooksA, _ := statusLog()
	doneA := make(chan error, 1)
	go func() { _, err := m.Send(context.Background(), thA.ID, "A 占位", nil, hooksA); doneA <- err }()
	waitThreadState(t, m, thA.ID, StateRunning)

	hooksB, statusesB := statusLog()
	doneB := make(chan error, 1)
	go func() { _, err := m.Send(context.Background(), thB.ID, "B 排队", nil, hooksB); doneB <- err }()
	waitThreadState(t, m, thB.ID, StateQueued)

	// 排队状态要能被列表/看板看到，且时间线给出一句带占用的说明
	found := false
	for _, th := range m.List() {
		if th.ID == thB.ID && th.State == StateQueued {
			found = true
		}
	}
	if !found {
		t.Fatalf("List 应把排队会话列出来: %+v", m.List())
	}
	close(gate) // A 收尾，让出槽位
	if err := <-doneA; err != nil {
		t.Fatalf("A 轮结束失败: %v", err)
	}
	if err := <-doneB; err != nil {
		t.Fatalf("B 排队后应能跑完: %v", err)
	}
	var queued string
	for _, s := range statusesB() {
		if strings.Contains(s, "排队") {
			queued = s
		}
	}
	if !strings.Contains(queued, "1/1") {
		t.Fatalf("排队提示应带占用比，实际 %q（全部 %v）", queued, statusesB())
	}
	m.mu.Lock()
	waiting := m.waiting
	m.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("两轮结束后等待计数应归零，实际 %d", waiting)
	}
}

// 排队中也要能停：Stop 在没有 ACP 会话可取消时，撤掉的就是这次等待。
func TestStopCancelsQueuedTurn(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	f := &fakeAgent{}
	f.promptScript = func(fc *fakeConn, sid string) string {
		if calls.Add(1) == 1 {
			<-gate
		}
		return defaultScript(fc, sid)
	}
	cfg := testConfig(t)
	cfg.MaxConcurrent = 1
	cfg.IdleKillSeconds = 600 // 不靠空闲回收：这条测试只验取消
	m := newTestManager(t, &cfg, nil, f)
	thA := mustThreadIn(t, m, mustWorkspace(t, m, t.TempDir()))
	thB := mustThreadIn(t, m, mustWorkspace(t, m, t.TempDir()))

	doneA := make(chan error, 1)
	go func() { _, err := m.Send(context.Background(), thA.ID, "A 占位", nil, TurnHooks{}); doneA <- err }()
	waitThreadState(t, m, thA.ID, StateRunning)

	doneB := make(chan error, 1)
	go func() { _, err := m.Send(context.Background(), thB.ID, "B 排队", nil, TurnHooks{}); doneB <- err }()
	waitThreadState(t, m, thB.ID, StateQueued)

	if !m.Stop(thB.ID) {
		t.Fatalf("排队中的会话应该能停")
	}
	if err := <-doneB; err == nil || !strings.Contains(err.Error(), "取消") {
		t.Fatalf("B 应带着取消原因返回: %v", err)
	}
	if th, _ := m.Get(thB.ID); th.State == StateQueued {
		t.Fatalf("取消后不该还挂着排队中: %+v", th)
	}
	m.mu.Lock()
	waiting := m.waiting
	cancels := len(m.queueCancels)
	m.mu.Unlock()
	if waiting != 0 || cancels != 0 {
		t.Fatalf("取消后等待计数与句柄都要清掉: waiting=%d cancels=%d", waiting, cancels)
	}
	close(gate)
	if err := <-doneA; err != nil {
		t.Fatalf("A 收尾失败: %v", err)
	}
}

// Reload 换掉槽位表时，活下来的连接必须在新表里记账：否则它们回收时抽的是旧
// channel，新上限被悄悄放大，甚至把空 channel 抽干后卡死后续 spawn。
func TestReloadReaccountsConcurrencySlots(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	cfg := testConfig(t)
	cfg.MaxConcurrent = 2
	m := newTestManager(t, &cfg, nil, f)
	th := mustThreadIn(t, m, mustWorkspace(t, m, t.TempDir()))
	if _, err := m.Send(context.Background(), th.ID, "占位", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	m.mu.Lock()
	procs := len(m.procs)
	m.mu.Unlock()
	if procs != 1 {
		t.Fatalf("应留下一条存活连接，实际 %d", procs)
	}

	next := cfg
	next.MaxConcurrent = 1
	m.Reload(next)

	m.mu.Lock()
	held, capacity := len(m.sem), cap(m.sem)
	var slot chan struct{}
	for _, ps := range m.procs {
		slot = ps.slot
	}
	m.mu.Unlock()
	if capacity != 1 {
		t.Fatalf("槽位表应按新上限重建，实际 cap=%d", capacity)
	}
	if held != 1 {
		t.Fatalf("活下来的连接要在新表里占住自己的名额，实际 held=%d", held)
	}
	m.mu.Lock()
	if slot != m.sem {
		m.mu.Unlock()
		t.Fatalf("连接的 slot 应指向新的槽位表")
	}
	m.mu.Unlock()
}

// 归还槽位绝不能阻塞：老代码在 spawn 失败路径上写的是阻塞式 <-m.sem，
// 计数一旦被上面的 Reload/重复回收打乱，就会死在 spawnMu 里面，
// 之后所有会话都起不了进程——正是「只能重启」的那个现象。
func TestReleaseSlotNeverBlocks(t *testing.T) {
	done := make(chan struct{})
	go func() {
		releaseSlot(make(chan struct{}))
		releaseSlot(make(chan struct{}, 1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("空槽位表上归还令牌不该阻塞")
	}
}
