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

	configOptions  []map[string]any // session/new、load 响应携带的会话配置项（nil = agent 不回报该能力）
	models         map[string]any   // 协议原生 models 状态（nil = agent 不回报）
	modes          map[string]any   // 协议原生 modes 状态（nil = agent 不回报）
	newSessionHang time.Duration    // >0：session/new 先挂这么久（验证建会话超时的文案与预算）

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
	permReplies  []string       // client 对权限请求的回包原文（查 selected 的 optionId）
	resolveCalls []string       // CodeBuddy resolveInterruption 的 params 原文

	promptScript func(fc *fakeConn, sessionID string) string // 返回 stopReason
	permSent     chan struct{}                               // 权限请求已发出（非 nil 时）
	cancelSeen   chan struct{}                               // 收到 session/cancel（非 nil 时）
}

func (f *fakeAgent) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.methods...)
}

// permReply 返回最近一次权限请求的回包原文（client → agent 的 result）。
func (f *fakeAgent) permReply() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.permReplies) == 0 {
		return ""
	}
	return f.permReplies[len(f.permReplies)-1]
}

// resolveCall 返回最近一次 CodeBuddy resolveInterruption 的 params。
func (f *fakeAgent) resolveCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resolveCalls) == 0 {
		return ""
	}
	return f.resolveCalls[len(f.resolveCalls)-1]
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
			// client 对我们请求的响应：权限答复要能断言（问答卡回的是 optionId）
			f.mu.Lock()
			f.permReplies = append(f.permReplies, string(msg.Result))
			f.mu.Unlock()
			continue
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
			if f.newSessionHang > 0 {
				time.Sleep(f.newSessionHang) // 模拟 opencode 首启列 skill 表那种慢
			}
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
		case "_codebuddy.ai/resolveInterruption":
			f.mu.Lock()
			f.resolveCalls = append(f.resolveCalls, string(msg.Params))
			f.mu.Unlock()
			fc.w.respond(msg.ID, map[string]any{"resolved": true, "queued": false})
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
	fc.w.send(permissionRequest(fc, sid, title, []any{
		map[string]any{"optionId": "allow", "kind": "allow_once", "name": "允许"},
		map[string]any{"optionId": "reject", "kind": "reject_once", "name": "拒绝"},
	}, map[string]any{"command": "执行 " + title}))
}

// sendQuestionRequest 模拟 agent 借权限通道问用户（zcode 的 AskUserQuestion 形状）：
// 选项是答案本身，rawInput 带 questions 正文。
func (fc *fakeConn) sendQuestionRequest(sid, title string, options []any, rawInput map[string]any) {
	fc.w.send(permissionRequest(fc, sid, title, options, rawInput))
}

// request 发一次任意 JSON-RPC 请求（扩展方法 / elicitation 用）。
func (fc *fakeConn) request(method string, params any) {
	fc.w.mu.Lock()
	fc.w.seq++
	reqID, _ := json.Marshal(fc.w.seq)
	fc.w.mu.Unlock()
	fc.w.send(map[string]any{"jsonrpc": "2.0", "id": reqID, "method": method, "params": params})
}

func permissionRequest(fc *fakeConn, sid, title string, options []any, rawInput map[string]any) map[string]any {
	fc.w.mu.Lock()
	fc.w.seq++
	reqID, _ := json.Marshal(fc.w.seq)
	fc.w.mu.Unlock()
	return map[string]any{"jsonrpc": "2.0", "id": reqID,
		"method": "session/request_permission", "params": map[string]any{
			"sessionId": sid,
			"options":   options,
			"toolCall": map[string]any{"toolCallId": "t9", "title": title,
				"kind": "execute", "rawInput": rawInput},
		}}
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
		HandshakeTimeoutSeconds: 5,
		Agents:                  []config.AcpAgentConfig{{Name: "fake", Command: "fake-bin"}},
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

	nt, err := m.Handoff(th.ID, "")
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
	if _, err := m.Handoff(th.ID, ""); err == nil {
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

// npx 型命令第一次拉起要联网装包，握手按冷启动预算给；起过一次之后回落到
// 常规值——不能为了第一次慢把每次等待都拉长（V-2）。
func TestHandshakeBudgetCoversColdStart(t *testing.T) {
	cfg := testConfig(t) // HandshakeTimeoutSeconds: 5
	cfg.Agents = []config.AcpAgentConfig{
		{Name: "fake", Command: "npx", Args: []string{"-y", "some-acp"}},
		{Name: "local", Command: "/opt/homebrew/bin/opencode", Args: []string{"acp"}},
	}
	m := newTestManager(t, &cfg, nil, nil)
	c := m.cfgFn().Normalized()
	// 按名字取条目：归一化后清单最前是注入的内置 friday，下标不再指向日志里那两台。
	fakeCfg := agentFromCfg(t, c, "fake")
	localCfg := agentFromCfg(t, c, "local")

	if got := m.handshakeBudget(fakeCfg); got != time.Duration(config.AcpColdStartHandshakeSeconds)*time.Second {
		t.Fatalf("npx 首次应按冷启动预算，实际 %v", got)
	}
	if got := m.handshakeBudget(localCfg); got != 5*time.Second {
		t.Fatalf("绝对路径命令不该吃冷启动预算，实际 %v", got)
	}
	// 内置条目不 exec，永远不该吃冷启动预算
	if got := m.handshakeBudget(agentFromCfg(t, c, config.BuiltinAgentName)); got != 5*time.Second {
		t.Fatalf("内置条目应按常规握手值，实际 %v", got)
	}
	m.markWarm("fake")
	if got := m.handshakeBudget(fakeCfg); got != 5*time.Second {
		t.Fatalf("起过一次后应回落到握手值，实际 %v", got)
	}
	// 显式写了更大的覆盖值时不能被"回落"缩掉
	big := fakeCfg
	big.HandshakeTimeoutSeconds = 120
	if got := m.handshakeBudget(big); got != 120*time.Second {
		t.Fatalf("条目覆盖比冷启动预算更大时应以覆盖为准，实际 %v", got)
	}
	// 总预算（Options 一次性拉起用）要盖住握手 + 建会话
	if got := m.dialBudget(localCfg); got <= 5*time.Second+time.Duration(cfg.SessionTimeoutSeconds)*time.Second {
		t.Fatalf("dialBudget 应大于握手+建会话之和，实际 %v", got)
	}
}

// agentFromCfg 按名字取一条 agent 配置（归一化后的清单最前是内置 friday，
// 老用例的 Agents[0]/[1] 下标已经不可靠）。
func agentFromCfg(t *testing.T, c config.AcpConfig, name string) config.AcpAgentConfig {
	t.Helper()
	a, ok := c.Agent(name)
	if !ok {
		t.Fatalf("清单里没有 %q: %+v", name, c.Agents)
	}
	return a
}

// 建会话超时不该再长得像"agent 坏了"：说清是谁、多久、去哪调（V-2）。
func TestSessionNewTimeoutNamesTheKnob(t *testing.T) {
	f := &fakeAgent{newSessionHang: 3 * time.Second, promptScript: defaultScript}
	cfg := testConfig(t)
	cfg.SessionTimeoutSeconds = 1
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)

	_, err := m.Send(context.Background(), th.ID, "在吗", nil, TurnHooks{})
	if err == nil {
		t.Fatalf("建会话超时应该报错")
	}
	for _, want := range []string{"没在 1 秒内建好会话", "建会话超时", "设置 → 智能体"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误应含 %q，实际 %q", want, err.Error())
		}
	}
	// 超时后不留半条连接占着槽位：再来一次（换成不挂的 fake）应该能正常跑
	f2 := &fakeAgent{promptScript: defaultScript}
	m2 := newTestManager(t, &cfg, nil, f2)
	th2 := mustThread(t, m2)
	if _, err := m2.Send(context.Background(), th2.ID, "在吗", nil, TurnHooks{}); err != nil {
		t.Fatalf("另一台实例应能正常建会话: %v", err)
	}
}

// agent 收下这一轮却什么都不回时，不能让人对着安静的界面干等：到点先说一声
// "没回音"并给出下一步（V-2 真机：opencode 卡在自己的模型调用上，三分钟零输出）。
func TestSilenceWatchdogNoticesQuietAgent(t *testing.T) {
	old := turnSilenceNotice
	turnSilenceNotice = 200 * time.Millisecond
	defer func() { turnSilenceNotice = old }()

	f := &fakeAgent{promptScript: func(fc *fakeConn, sid string) string {
		time.Sleep(700 * time.Millisecond) // 收下这一轮，但一条 update 都不发
		return "end_turn"
	}}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	hooks, statuses := statusLog()
	if _, err := m.Send(context.Background(), th.ID, "在吗", nil, hooks); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	joined := strings.Join(statuses(), "\n")
	if !strings.Contains(joined, "没有任何回音") {
		t.Fatalf("静默到点应给一条状态帧，实际 %q", joined)
	}
	if !strings.Contains(joined, "fake") {
		t.Fatalf("提示要点名是哪个 agent，实际 %q", joined)
	}
}

// 反例：agent 正常在吐字时不该弹这条提示（否则每次对话都多一句废话）。
func TestSilenceWatchdogStaysQuietWhenAgentTalks(t *testing.T) {
	old := turnSilenceNotice
	turnSilenceNotice = 50 * time.Millisecond
	defer func() { turnSilenceNotice = old }()

	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	hooks, statuses := statusLog()
	if _, err := m.Send(context.Background(), th.ID, "在吗", nil, hooks); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if strings.Contains(strings.Join(statuses(), "\n"), "没有任何回音") {
		t.Fatalf("有产出就不该报静默: %v", statuses())
	}
}

// ---------- 系统提示桥（审批待处理 / 一轮结束） ----------

// noticeLog 收集 notifier 投递的 key+text，供断言。
type noticeLog struct {
	mu    sync.Mutex
	keys  []string
	texts []string
}

func (n *noticeLog) fn() func(key, text string) {
	return func(key, text string) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.keys = append(n.keys, key)
		n.texts = append(n.texts, text)
	}
}

// find 返回第一条正文含 substr 的 key。
func (n *noticeLog) find(substr string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i, t := range n.texts {
		if strings.Contains(t, substr) {
			return n.keys[i], true
		}
	}
	return "", false
}

// body 按 key 取正文（找不到返回空串）。
func (n *noticeLog) body(key string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i, k := range n.keys {
		if k == key {
			return n.texts[i]
		}
	}
	return ""
}

func (n *noticeLog) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.keys)
}

// TestNoticeOnApprovalAndTurnEnd 人在别的窗口时，这两件事必须各弹一次：
// 审批卡入队（带卡片编号做身份）、一轮结束（带轮 id 做身份）。
func TestNoticeOnApprovalAndTurnEnd(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.sendPermissionRequest(sid, "写 a.go")
		f.permSent <- struct{}{}
		time.Sleep(300 * time.Millisecond) // 等测试 goroutine 点批准
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)
	notices := &noticeLog{}
	m.SetNotifier(notices.fn())

	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "审批入队", func() bool { return len(approver.Pending()) == 1 })
		approver.Decide(approver.Pending()[0].ID, true)
	}()
	if _, err := m.Send(context.Background(), th.ID, "改一下 a.go", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	key, ok := notices.find("在等你批准")
	if !ok {
		t.Fatalf("审批卡入队应投一条提示: %+v", notices.texts)
	}
	if _, ok := notices.find("在等你批准"); !ok {
		t.Fatalf("审批卡入队应投一条提示: %+v", notices.texts)
	}
	if !strings.HasPrefix(key, "acp:"+th.ID+":approval:") {
		t.Errorf("审批提示的 key 应带会话与卡片身份，实际 %q", key)
	}
	if body := notices.body(key); !strings.Contains(body, "写 a.go") {
		t.Errorf("提示正文应带上要批准的操作摘要，实际 %q", body)
	}

	doneKey, ok := notices.find("这一轮跑完了")
	if !ok {
		t.Fatalf("轮末应投一条提示: %+v", notices.texts)
	}
	if !strings.HasPrefix(doneKey, "acp:"+th.ID+":t") || !strings.HasSuffix(doneKey, ":done") {
		t.Errorf("轮末提示的 key 应绑定这一轮，实际 %q", doneKey)
	}
	if got := notices.count(); got != 2 {
		t.Errorf("应只有审批+轮末两条，实际 %d 条: %+v", got, notices.texts)
	}
}

// TestNoticeSkippedWhenUserStops 「停止」是人自己按的，他就在页面上，别再弹一条。
func TestNoticeSkippedWhenUserStops(t *testing.T) {
	f := &fakeAgent{}
	f.cancelSeen = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.w.notify("session/update", update(sid, "agent_message_chunk",
			map[string]any{"content": textBlock("开头…")}))
		<-f.cancelSeen
		return "cancelled"
	}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	notices := &noticeLog{}
	m.SetNotifier(notices.fn())

	done := make(chan TurnResult, 1)
	go func() {
		res, err := m.Send(context.Background(), th.ID, "长任务", nil, TurnHooks{})
		if err != nil {
			t.Errorf("取消不应算失败: %v", err)
		}
		done <- res
	}()
	waitFor(t, 2*time.Second, "turn 已进入可停止状态", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		lv := m.live[th.ID]
		// turnDone 挂上才算"这一轮在跑"：在那之前 Stop 只会发取消、不会记 stopping，
		// 收尾就按连接断开结算（用例要验的是"用户按停止"那条路）。
		return lv != nil && lv.turnDone != nil
	})
	if !m.Stop(th.ID) {
		t.Fatal("Stop 应成功发出取消")
	}
	res := <-done
	if res.StopReason != "cancelled" {
		t.Fatalf("应为 cancelled: %+v", res)
	}
	if _, ok := notices.find("这一轮"); ok {
		t.Errorf("用户主动停止不该投系统提示: %+v", notices.texts)
	}
}

// TestNoticeOnClientDisconnect 页面关闭/连接断掉这一类收尾要提示，并把已经改动的
// 文件数带上——人回来时得知道盘上已经动了什么、结果没回到哪一条会话。
func TestNoticeOnClientDisconnect(t *testing.T) {
	f := &fakeAgent{}
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.w.notify("session/update", update(sid, "agent_message_chunk",
			map[string]any{"content": textBlock("跑一会儿…")}))
		time.Sleep(2 * time.Second)
		return "end_turn"
	}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	notices := &noticeLog{}
	m.SetNotifier(notices.fn())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.Send(ctx, th.ID, "别跑完", nil, TurnHooks{})
	}()
	waitFor(t, 2*time.Second, "turn 启动", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.live[th.ID] != nil
	})
	cancel() // 客户端断开（不是用户按停止）
	<-done

	key, ok := notices.find("这一轮没跑完")
	if !ok {
		t.Fatalf("断开收尾应投一条提示: %+v", notices.texts)
	}
	if !strings.HasPrefix(key, "acp:"+th.ID+":t") || !strings.HasSuffix(key, ":done") {
		t.Errorf("提示 key 应绑定这一轮，实际 %q", key)
	}
}

// ---------- 问答卡（agent 借权限通道问用户） ----------

// questionOptions 是 zcode AskUserQuestion 那种形状：选项就是答案。
var questionOptions = []any{
	map[string]any{"optionId": "opt_min", "kind": "allow_once", "name": "最小影响面"},
	map[string]any{"optionId": "opt_full", "kind": "allow_once", "name": "全量封堵"},
	map[string]any{"optionId": "reject", "kind": "reject_once", "name": "拒绝"},
}

var questionRawInput = map[string]any{"questions": []any{
	map[string]any{"header": "范围", "question": "本轮修复的范围怎么定？", "multiSelect": false,
		"options": []any{
			map[string]any{"label": "最小影响面", "description": "只拦新上传"},
			map[string]any{"label": "全量封堵", "description": "含存量盘点与校验"},
		}},
}}

func TestQuestionCardShapeAndOptionReply(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.sendQuestionRequest(sid, "本轮修复的范围怎么定？", questionOptions, questionRawInput)
		f.permSent <- struct{}{}
		time.Sleep(400 * time.Millisecond) // 等测试 goroutine 选项作答
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	seen := make(chan struct{}, 1)
	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "问答卡入队", func() bool { return len(approver.Pending()) == 1 })
		it := approver.Pending()[0]
		if it.Kind != approval.KindQuestion {
			t.Errorf("这类请求应入队成问答卡，实际 kind=%q", it.Kind)
		}
		if len(it.Options) != 3 {
			t.Errorf("按钮应是 agent 给的 3 个选项，实际 %+v", it.Options)
		}
		if len(it.Questions) != 1 || it.Questions[0].Header != "范围" ||
			len(it.Questions[0].Options) != 2 {
			t.Errorf("问题正文应结构化（含 header 与每项说明），实际 %+v", it.Questions)
		}
		if strings.Contains(it.Detail, "{") {
			t.Errorf("卡片正文不该是 JSON 原文: %q", it.Detail)
		}
		if !approver.DecideOption(it.ID, "opt_full") {
			t.Error("按 optionId 作答应被接受")
		}
		seen <- struct{}{}
	}()
	if _, err := m.Send(context.Background(), th.ID, "定个范围", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-seen

	reply := f.permReply()
	if !strings.Contains(reply, `"optionId":"opt_full"`) {
		t.Fatalf("应把选中的 optionId 原样回给 agent，实际 %s", reply)
	}
}

// 没答（本轮先结束、卡被撤）时回 cancelled，不替用户挑一条——agent 那边那三条
// 选项的含义只有它自己知道，随便选一条等于编答案。
func TestQuestionCardUnansweredRepliesCancelled(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.sendQuestionRequest(sid, "这个怎么处理？", questionOptions, questionRawInput)
		time.Sleep(150 * time.Millisecond) // 等人作答前本轮先收尾（撤卡路径）
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	if _, err := m.Send(context.Background(), th.ID, "快点结束", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, 2*time.Second, "权限答复已发出", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.permReplies) > 0
	})
	if !strings.Contains(f.permReply(), "cancelled") {
		t.Fatalf("未回答应回 cancelled，实际 %s", f.permReply())
	}
	if len(approver.Pending()) != 0 {
		t.Error("问答卡也应随本轮结束被撤下")
	}
}

// 真的授权请求（只有规范 allow/reject 选项、正文是命令）不得被误判成问答卡，
// 否则普通授权会变成一堆按钮。
func TestPlainPermissionStaysApprovalCard(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.sendPermissionRequest(sid, "执行 rm -rf ./tmp")
		f.permSent <- struct{}{}
		time.Sleep(400 * time.Millisecond)
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-f.permSent
		waitFor(t, 2*time.Second, "授权卡入队", func() bool { return len(approver.Pending()) == 1 })
		it := approver.Pending()[0]
		if it.Kind != "" {
			t.Errorf("普通授权不该带 kind=%q", it.Kind)
		}
		if len(it.Options) != 0 {
			t.Errorf("授权卡不该带选项按钮，实际 %+v", it.Options)
		}
		approver.Decide(it.ID, true)
	}()
	if _, err := m.Send(context.Background(), th.ID, "清理临时目录", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-done
}

// CodeBuddy 的 AskUserQuestion 不走 request_permission，而是私有扩展方法
// `_codebuddy.ai/question`；问答卡答完要按 q_0/q_1 回 answers。
func TestCodeBuddyQuestionExtensionAnswers(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.request(codeBuddyQuestionMethod, map[string]any{
			"sessionId": sid, "toolCallId": "t1", "inputType": "question",
			"schema": map[string]any{"questions": []any{
				map[string]any{
					"id": "q_0", "header": "方案", "question": "选哪个方案？",
					"options": []any{
						map[string]any{"label": "方案 A", "description": "改动小"},
						map[string]any{"label": "方案 B"},
					},
				},
			}},
		})
		f.permSent <- struct{}{}
		time.Sleep(400 * time.Millisecond)
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	seen := make(chan struct{}, 1)
	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "CodeBuddy 问答卡入队", func() bool { return len(approver.Pending()) == 1 })
		it := approver.Pending()[0]
		if it.Kind != approval.KindQuestion {
			t.Errorf("扩展方法提问应入队成问答卡，实际 kind=%q", it.Kind)
		}
		if len(it.Questions) != 1 || it.Questions[0].Question != "选哪个方案？" {
			t.Errorf("问题正文应结构化，实际 %+v", it.Questions)
		}
		if !approver.DecideOption(it.ID, "q:o0") {
			t.Error("按选项 ID 作答应被接受")
		}
		seen <- struct{}{}
	}()
	if _, err := m.Send(context.Background(), th.ID, "你决定", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-seen

	reply := f.permReply()
	for _, want := range []string{`"outcome":"submitted"`, `"q_0":"方案 A"`} {
		if !strings.Contains(reply, want) {
			t.Fatalf("回包缺少 %s，实际 %s", want, reply)
		}
	}
}

// MiniMax 的 ask_user 在客户端声明 elicitation.form 后走 elicitation/create；
// 表单字段要翻成问答卡，答复按 requestedSchema 的键回填。
func TestElicitationFormAnswers(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.request("elicitation/create", map[string]any{
			"mode": "form", "sessionId": sid, "message": "MiniMax 需要你的选择",
			"requestedSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"q1": map[string]any{
						"type": "string", "title": "选哪个方案？",
						"oneOf": []any{
							map[string]any{"const": "a", "title": "方案 A"},
							map[string]any{"const": "b", "title": "方案 B"},
						},
					},
				},
				"required": []any{"q1"},
			},
		})
		f.permSent <- struct{}{}
		time.Sleep(400 * time.Millisecond)
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	seen := make(chan struct{}, 1)
	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "elicitation 问答卡入队", func() bool { return len(approver.Pending()) == 1 })
		it := approver.Pending()[0]
		if it.Kind != approval.KindQuestion {
			t.Errorf("elicitation 应入队成问答卡，实际 kind=%q", it.Kind)
		}
		if len(it.Options) != 2 || it.Options[0].Name != "方案 A" {
			t.Errorf("选项按钮应来自 schema.oneOf，实际 %+v", it.Options)
		}
		if !approver.DecideOption(it.ID, "q:o0") {
			t.Error("按选项 ID 作答应被接受")
		}
		seen <- struct{}{}
	}()
	if _, err := m.Send(context.Background(), th.ID, "你决定", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-seen

	reply := f.permReply()
	for _, want := range []string{`"action":"accept"`, `"q1":"a"`} {
		if !strings.Contains(reply, want) {
			t.Fatalf("回包缺少 %s，实际 %s", want, reply)
		}
	}
}

// CodeBuddy 的 AskUserQuestion 实际链路：先发工具授权（这里应自动放行），
// 再发 session_info_update/interruptionRequest 承载问题，答完调 resolveInterruption。
func TestCodeBuddyInterruptionCardAnswers(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.request("session/request_permission", map[string]any{
			"sessionId": sid,
			"options": []any{
				map[string]any{"optionId": "allow_always", "kind": "allow_always", "name": "Always Allow"},
				map[string]any{"optionId": "allow", "kind": "allow_once", "name": "Allow"},
				map[string]any{"optionId": "reject", "kind": "reject_once", "name": "Reject"},
			},
			"toolCall": map[string]any{
				"toolCallId": "t1",
				"rawInput": map[string]any{"questions": []any{
					map[string]any{"question": "要选方案 A 还是方案 B？", "header": "方案选择",
						"options": []any{
							map[string]any{"label": "方案 A", "description": "选择方案 A"},
							map[string]any{"label": "方案 B", "description": "选择方案 B"},
						}},
				}},
				"_meta": map[string]any{"codebuddy.ai/toolName": "AskUserQuestion"},
			},
		})
		fc.w.notify("session/update", update(sid, "session_info_update", map[string]any{
			"_meta": map[string]any{"codebuddy.ai/interruptionRequest": map[string]any{
				"interruptionId": "ir-t1", "toolCallId": "t1", "toolName": "AskUserQuestion",
				"toolInput": map[string]any{"questions": []any{
					map[string]any{"id": "q_0", "question": "要选方案 A 还是方案 B？", "header": "方案选择",
						"options": []any{
							map[string]any{"label": "方案 A", "description": "选择方案 A"},
							map[string]any{"label": "方案 B", "description": "选择方案 B"},
						}},
				}},
			}},
		}))
		f.permSent <- struct{}{}
		time.Sleep(600 * time.Millisecond)
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	seen := make(chan struct{}, 1)
	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "CodeBuddy interruption 问答卡入队", func() bool {
			return len(approver.Pending()) == 1
		})
		it := approver.Pending()[0]
		if it.Kind != approval.KindQuestion {
			t.Errorf("interruption 应入队成问答卡，实际 kind=%q", it.Kind)
		}
		if it.Summary != "方案选择 · 要选方案 A 还是方案 B？" {
			t.Errorf("卡片摘要应带 header 与题干，实际 %q", it.Summary)
		}
		if len(it.Options) != 2 || it.Options[0].Name != "方案 A" {
			t.Errorf("按钮应是真正的问题选项，实际 %+v", it.Options)
		}
		if !approver.DecideOption(it.ID, "q:o0") {
			t.Error("按选项 ID 作答应被接受")
		}
		seen <- struct{}{}
	}()
	if _, err := m.Send(context.Background(), th.ID, "你决定", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-seen

	f.mu.Lock()
	replies := strings.Join(f.permReplies, "\n")
	f.mu.Unlock()
	if !strings.Contains(replies, `"optionId":"allow"`) {
		t.Errorf("提问工具授权应自动放行，实际回包 %s", replies)
	}
	resolve := f.resolveCall()
	for _, want := range []string{`"decision":"allow"`, `"toolCallId":"t1"`, `"q_0":"方案 A"`} {
		if !strings.Contains(resolve, want) {
			t.Fatalf("resolveInterruption 缺少 %s，实际 %s", want, resolve)
		}
	}
}

// CodeBuddy 的自由输入：AskUserQuestion 的 answers 可以是任意文本，不走 optionId。
func TestCodeBuddyInterruptionTextAnswer(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.request("session/request_permission", map[string]any{
			"sessionId": sid,
			"options": []any{
				map[string]any{"optionId": "allow", "kind": "allow_once", "name": "Allow"},
				map[string]any{"optionId": "reject", "kind": "reject_once", "name": "Reject"},
			},
			"toolCall": map[string]any{
				"toolCallId": "t2",
				"rawInput": map[string]any{"questions": []any{
					map[string]any{"question": "方案怎么定？", "options": []any{
						map[string]any{"label": "方案 A"}, map[string]any{"label": "方案 B"},
					}},
				}},
				"_meta": map[string]any{"codebuddy.ai/toolName": "AskUserQuestion"},
			},
		})
		fc.w.notify("session/update", update(sid, "session_info_update", map[string]any{
			"_meta": map[string]any{"codebuddy.ai/interruptionRequest": map[string]any{
				"interruptionId": "ir-t2", "toolCallId": "t2", "toolName": "AskUserQuestion",
				"toolInput": map[string]any{"questions": []any{
					map[string]any{"id": "q_0", "question": "方案怎么定？", "options": []any{
						map[string]any{"label": "方案 A"}, map[string]any{"label": "方案 B"},
					}},
				}},
			}},
		}))
		f.permSent <- struct{}{}
		time.Sleep(600 * time.Millisecond)
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	seen := make(chan struct{}, 1)
	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "CodeBuddy 文本问答卡入队", func() bool {
			return len(approver.Pending()) == 1
		})
		it := approver.Pending()[0]
		if !it.AllowText {
			t.Error("CodeBuddy 的问答卡应允许自由文本")
		}
		if !approver.DecideText(it.ID, "方案 C") {
			t.Error("自由文本作答应被接受")
		}
		seen <- struct{}{}
	}()
	if _, err := m.Send(context.Background(), th.ID, "你决定", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-seen

	resolve := f.resolveCall()
	if !strings.Contains(resolve, `"q_0":"方案 C"`) {
		t.Fatalf("resolveInterruption 应带自由文本答案，实际 %s", resolve)
	}
}

// MiniMax 的 Other 字段：文本要回填到 <stepId>__other，而不是主选项字段。
func TestElicitationOtherTextAnswer(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.request("elicitation/create", map[string]any{
			"mode": "form", "sessionId": sid, "message": "选择",
			"requestedSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"q1": map[string]any{
						"type": "string", "title": "方案怎么定？",
						"oneOf": []any{
							map[string]any{"const": "a", "title": "方案 A"},
							map[string]any{"const": "b", "title": "方案 B"},
						},
					},
					"q1__other": map[string]any{"type": "string", "title": "方案怎么定？ — Other"},
				},
				"required": []any{"q1"},
			},
		})
		f.permSent <- struct{}{}
		time.Sleep(600 * time.Millisecond)
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	seen := make(chan struct{}, 1)
	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "MiniMax Other 问答卡入队", func() bool {
			return len(approver.Pending()) == 1
		})
		it := approver.Pending()[0]
		if !it.AllowText {
			t.Error("带 Other 字段的问题应允许自由文本")
		}
		if !approver.DecideText(it.ID, "方案 C") {
			t.Error("自由文本作答应被接受")
		}
		seen <- struct{}{}
	}()
	if _, err := m.Send(context.Background(), th.ID, "你决定", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-seen

	reply := f.permReply()
	for _, want := range []string{`"action":"accept"`, `"q1__other":"方案 C"`} {
		if !strings.Contains(reply, want) {
			t.Fatalf("回包缺少 %s，实际 %s", want, reply)
		}
	}
}

// TestCodeBuddyInterruptionMultiSelect 多选题：卡面要标 multiSelect，
// 勾选的两条按值数组回给 resolveInterruption（只勾一条也得是数组）。
func TestCodeBuddyInterruptionMultiSelect(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.request("session/request_permission", map[string]any{
			"sessionId": sid,
			"options": []any{
				map[string]any{"optionId": "allow", "kind": "allow_once", "name": "Allow"},
				map[string]any{"optionId": "reject", "kind": "reject_once", "name": "Reject"},
			},
			"toolCall": map[string]any{
				"toolCallId": "m1",
				"rawInput": map[string]any{"questions": []any{
					map[string]any{"question": "带哪些模块？", "multiSelect": true, "options": []any{
						map[string]any{"label": "网关"}, map[string]any{"label": "存储"}, map[string]any{"label": "前端"},
					}},
				}},
				"_meta": map[string]any{"codebuddy.ai/toolName": "AskUserQuestion"},
			},
		})
		fc.w.notify("session/update", update(sid, "session_info_update", map[string]any{
			"_meta": map[string]any{"codebuddy.ai/interruptionRequest": map[string]any{
				"interruptionId": "ir-m1", "toolCallId": "m1", "toolName": "AskUserQuestion",
				"toolInput": map[string]any{"questions": []any{
					map[string]any{"id": "q_0", "question": "带哪些模块？", "multiSelect": true, "options": []any{
						map[string]any{"label": "网关"}, map[string]any{"label": "存储"}, map[string]any{"label": "前端"},
					}},
				}},
			}},
		}))
		f.permSent <- struct{}{}
		time.Sleep(600 * time.Millisecond)
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	seen := make(chan struct{}, 1)
	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "CodeBuddy 多选卡入队", func() bool {
			return len(approver.Pending()) == 1
		})
		it := approver.Pending()[0]
		if !it.MultiSelect {
			t.Error("多选题的卡应标 multiSelect")
		}
		if !approver.DecideOptions(it.ID, []string{"q:o2", "q:o0"}) {
			t.Error("多选作答应被接受")
		}
		seen <- struct{}{}
	}()
	if _, err := m.Send(context.Background(), th.ID, "你决定", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-seen

	resolve := f.resolveCall()
	if !strings.Contains(resolve, `"q_0":["前端","网关"]`) {
		t.Fatalf("resolveInterruption 应按勾选顺序回数组，实际 %s", resolve)
	}
}

// TestElicitationMultiSelect MiniMax 数组字段（items.anyOf）：勾选多条按数组
// 回 content，单选卡与多选卡互不混淆。
func TestElicitationMultiSelect(t *testing.T) {
	approver := approval.New("confirm", nil, time.Minute)
	f := &fakeAgent{}
	f.permSent = make(chan struct{}, 1)
	f.promptScript = func(fc *fakeConn, sid string) string {
		fc.request("elicitation/create", map[string]any{
			"mode": "form", "sessionId": sid, "message": "选择",
			"requestedSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"mods": map[string]any{
						"type": "array", "title": "带哪些模块？",
						"items": map[string]any{"anyOf": []any{
							map[string]any{"const": "gateway", "title": "网关"},
							map[string]any{"const": "storage", "title": "存储"},
							map[string]any{"const": "web", "title": "前端"},
						}},
					},
				},
			},
		})
		f.permSent <- struct{}{}
		time.Sleep(600 * time.Millisecond)
		return "end_turn"
	}
	m := newTestManager(t, nil, approver, f)
	th := mustThread(t, m)

	seen := make(chan struct{}, 1)
	go func() {
		<-f.permSent
		waitFor(t, 2*time.Second, "MiniMax 多选卡入队", func() bool {
			return len(approver.Pending()) == 1
		})
		it := approver.Pending()[0]
		if !it.MultiSelect {
			t.Error("数组字段的卡应标 multiSelect")
		}
		if !approver.DecideOptions(it.ID, []string{"q:o0", "q:o2"}) {
			t.Error("多选作答应被接受")
		}
		seen <- struct{}{}
	}()
	if _, err := m.Send(context.Background(), th.ID, "你决定", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-seen

	reply := f.permReply()
	for _, want := range []string{`"action":"accept"`, `"mods":["gateway","web"]`} {
		if !strings.Contains(reply, want) {
			t.Fatalf("回包缺少 %s，实际 %s", want, reply)
		}
	}
}

// TestHandoffTargetAgent 交接可以换 agent：摘要只是文本，跨 agent 带得动；
// 模型/审批档位的取值各家不同，换人时不搬（同 agent 才沿用）。
func TestHandoffTargetAgent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents = append(cfg.Agents, config.AcpAgentConfig{Name: "other", Command: "other-bin"})
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, &cfg, nil, f)
	th := mustThread(t, m)
	if _, err := m.Send(context.Background(), th.ID, "先看下这个项目", nil, TurnHooks{}); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	m.mu.Lock()
	m.threads[th.ID].CapChoices = map[string]string{"model": "m-a"}
	m.mu.Unlock()

	// 留空 = 交给原来那台，选择照搬
	same, err := m.Handoff(th.ID, "")
	if err != nil {
		t.Fatalf("同 agent 交接失败: %v", err)
	}
	if same.Agent != th.Agent || same.CapChoices["model"] != "m-a" {
		t.Fatalf("同 agent 应沿用选择: %+v", same)
	}

	// 换 agent：摘要带过去，选择不带，note 说清交给谁。
	// 目标 agent 自己上一次的选择也不带：交接明说「按工作区默认档」，
	// 不能被 CreateSession 的「沿用上次选择」偷偷播种进来。
	oth, err := m.CreateSession("other", th.WorkspaceID, th.Cwd, "other 的历史会话")
	if err != nil {
		t.Fatalf("建 other 历史会话失败: %v", err)
	}
	m.mu.Lock()
	m.threads[oth.ID].CapChoices = map[string]string{"model": "m-other"}
	m.mu.Unlock()
	got, err := m.Handoff(th.ID, " other ")
	if err != nil {
		t.Fatalf("换 agent 交接失败: %v", err)
	}
	if got.Agent != "other" || got.Cwd != th.Cwd || got.WorkspaceID != th.WorkspaceID {
		t.Fatalf("续会话应换 agent、留同一主目录: old=%+v new=%+v", th, got)
	}
	if _, ok := got.CapChoices["model"]; ok {
		t.Errorf("换 agent 不该搬模型选择: %+v", got.CapChoices)
	}
	if len(got.CapChoices) != 0 {
		t.Errorf("换 agent 应落回工作区默认档（连 other 上次的继承也不带）: %+v", got.CapChoices)
	}
	if !strings.Contains(got.Handoff, "先看下这个项目") {
		t.Errorf("摘要应照样带过去: %q", got.Handoff)
	}
	if len(got.Messages) != 1 || !strings.Contains(got.Messages[0].Text, "上一个由 fake 跑，这次交给 other") {
		t.Errorf("交接说明要说清换了 agent: %+v", got.Messages)
	}

	if _, err := m.Handoff(th.ID, "ghost"); err == nil ||
		!strings.Contains(err.Error(), "没有叫") {
		t.Fatalf("未知 agent 要给可读原因，实际 %v", err)
	}
}

// ---------- 新会话沿用这台 agent 上次的能力选择 ----------

// TestNewSessionInheritsLastChoices 新建会话直接沿用上次用过的选择：模型这类 chips
// 以前每条新会话都要重新点一遍——那份记录本来就存在老会话身上，只是新建时给的是空表。
// 现在建会话就抄最近一条的，会话一建立照常按这份记录重放（agent 侧不保存选项）。
func TestNewSessionInheritsLastChoices(t *testing.T) {
	f := &fakeAgent{resetOnSession: true, configOptions: sessionOptions("builtin/a"), promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m)
	first := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), first.ID, "开工", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetCapability(first.ID, CapKindModel, "builtin/b"); err != nil {
		t.Fatalf("切模型失败: %v", err)
	}

	second := mustThreadIn(t, m, ws)
	if second.CapChoices[CapKindModel] != "builtin/b" {
		t.Fatalf("新会话应沿用上次选过的模型: %+v", second.CapChoices)
	}
	caps, err := m.Capabilities(second.ID)
	if err != nil {
		t.Fatalf("取能力清单失败: %v", err)
	}
	if cap := findCap(caps, CapKindModel); cap == nil || cap.CurrentValue != "builtin/b" {
		t.Fatalf("新会话建立时就该把选择重放上去: %+v", cap)
	}

	// 沿用是持续生效的：第三条拿的是第二条（更新的那条）那份
	third := mustThreadIn(t, m, ws)
	if third.CapChoices[CapKindModel] != "builtin/b" {
		t.Fatalf("沿用应持续有效: %+v", third.CapChoices)
	}
}

// TestNewSessionInheritsFromMostRecent 两条老会话选过不同取值时，沿用最近动过的那条。
func TestNewSessionInheritsFromMostRecent(t *testing.T) {
	f := &fakeAgent{configOptions: sessionOptions("builtin/a"), promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m)
	older := mustThreadIn(t, m, ws)
	if _, err := m.SetCapability(older.ID, CapKindModel, "builtin/b"); err != nil {
		t.Fatal(err)
	}
	newer := mustThreadIn(t, m, ws)
	if _, err := m.SetCapability(newer.ID, CapKindModel, "builtin/a"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock() // 同一秒建出来的两条按秒分不出先后，用例自己拉开
	m.threads[older.ID].UpdatedAt = m.threads[older.ID].UpdatedAt.Add(-time.Hour)
	m.mu.Unlock()

	next := mustThreadIn(t, m, ws)
	if next.CapChoices[CapKindModel] != "builtin/a" {
		t.Fatalf("应沿用最近一条的选择: %+v", next.CapChoices)
	}
}

// TestNewSessionApprovalModeYieldsToWorkspacePolicy 审批档位的归属：工作区定成「只读」
// 时新会话听工作区的，不继承上次手选的档——把只读工作区的新会话静默升成「全自动」
// 等于替用户撤了这道设置。「每次都问」（含没动过的默认）谈的是打扰程度不是安全边界，
// 那里沿用手选的档就是用户要的效果。
func TestNewSessionApprovalModeYieldsToWorkspacePolicy(t *testing.T) {
	t.Run("工作区只读就不继承", func(t *testing.T) {
		f := &fakeAgent{modes: codexSessionModes("agent"), promptScript: defaultScript}
		m := newTestManager(t, nil, nil, f)
		ws := mustWorkspace(t, m)
		if err := m.SetPolicy(ws.ID, WorkspacePolicyReadOnly); err != nil {
			t.Fatal(err)
		}
		first := mustThreadIn(t, m, ws)
		if _, err := m.SetCapability(first.ID, CapKindApprovalMode, "agent-full-access"); err != nil {
			t.Fatalf("手动升档失败: %v", err)
		}
		second := mustThreadIn(t, m, ws)
		if _, ok := second.CapChoices[CapKindApprovalMode]; ok {
			t.Fatalf("工作区定了只读，新会话不该继承上次的全自动: %+v", second.CapChoices)
		}
		if _, err := m.Send(context.Background(), second.ID, "开工", nil, TurnHooks{}); err != nil {
			t.Fatal(err)
		}
		got, _ := m.Get(second.ID)
		if got.PolicyApplied != WorkspacePolicyReadOnly || got.PolicyValue != "read-only" {
			t.Fatalf("新会话应按工作区档位落地，实际 applied=%q value=%q note=%q",
				got.PolicyApplied, got.PolicyValue, got.PolicyNote)
		}
	})

	t.Run("每次都问的档位就继承", func(t *testing.T) {
		f := &fakeAgent{modes: codexSessionModes("agent"), promptScript: defaultScript}
		m := newTestManager(t, nil, nil, f)
		ws := mustWorkspace(t, m) // 默认 ask
		first := mustThreadIn(t, m, ws)
		if _, err := m.SetCapability(first.ID, CapKindApprovalMode, "workspace-write"); err != nil {
			t.Fatal(err)
		}
		second := mustThreadIn(t, m, ws)
		if second.CapChoices[CapKindApprovalMode] != "workspace-write" {
			t.Fatalf("工作区没定档位时应继承: %+v", second.CapChoices)
		}
	})
}

// TestNewSessionDoesNotInheritOtherAgent 换一台 agent 不沿用别台的取值：各家词汇表
// 不同，把 codex 的模型 id 播给 codebuddy 只会重放失败。
func TestNewSessionDoesNotInheritOtherAgent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents = append(cfg.Agents, config.AcpAgentConfig{Name: "other", Command: "other-bin"})
	f := &fakeAgent{configOptions: sessionOptions("builtin/a"), promptScript: defaultScript}
	m := newTestManager(t, &cfg, nil, f)
	ws := mustWorkspace(t, m)
	first := mustThreadIn(t, m, ws) // agent=fake
	if _, err := m.SetCapability(first.ID, CapKindModel, "builtin/b"); err != nil {
		t.Fatal(err)
	}

	other, err := m.CreateSession("other", ws.ID, "", "")
	if err != nil {
		t.Fatalf("换 agent 建会话失败: %v", err)
	}
	if len(other.CapChoices) != 0 {
		t.Fatalf("别台 agent 的选择不该搬过来: %+v", other.CapChoices)
	}
}
