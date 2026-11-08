package acp

// facade.go 是 Friday 工具门面：一个最小的 MCP streamable-HTTP server，把
// Friday 注册表里白名单内的（默认只读）工具暴露给外部 agent。Friday 作为
// ACP 客户端建会话时把它挂进 session/new 的 mcpServers（见 manager.go 的
// mcpServersFor），外部 agent（minimax/codex/opencode…）即可直接调用 Friday
// 工具——注入方向与 internal/mcp（外部 MCP 工具给 Friday 用）相反。
//
// 鉴权：门面对每个外部 agent 会话签发一个 Bearer token（绑定该 agent 的工具
// 白名单 + TTL），经 session/new 的 McpServerHttpInline.Headers 交给 agent，
// agent 调 tools/list、tools/call 时带回——一台门面由此同时服务不同权限的
// 多个 agent。配置 acp.facade.disableAuth 可关鉴权（逃生口：某家 agent 不转发
// 自定义 header 时用，docs/AGENT-TOOL-INJECTION.md §3.5）。
//
// 形状与 cmd/acp-probe/mockmcp.go 一致（那条链路已用真实 agent 实测走通），
// 但这是生产实现，遵循 docs/ACP-AGENTS.md 的实测硬约束：
//   - C1：单次 tools/call 必须在 facadeCallTimeout 内返回、永不阻塞等人类。
//     写类工具不进白名单，读类超时按「工具执行错误」回复并给出下一步指引；
//   - C9：返回文本就是给子 agent 的指令——失败/超时都写成可执行的下一步，
//     不回光秃秃的状态词；
//   - C12：initialize 回显客户端请求的 protocolVersion（缺省 2025-03-26），
//     不做版本协商拒绝（opencode 要 2025-11-25，codex 要 2025-06-18）。
//
// 监听 127.0.0.1 随机端口：外部可见面与 Admin 服务同级（仅本机），且不依赖
// Admin 是否装配。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	vxagent "github.com/capyflow/vortexagent/agent"
)

const (
	// facadeName 是挂给外部 agent 的 server 名（session/new 的 mcpServers[0].name）。
	facadeName = "friday"
	facadePath = "/mcp"
	// facadeMCPVersion 是客户端没声明版本时的兜底（Friday MCP 客户端同款声明）。
	facadeMCPVersion = "2025-03-26"
)

// facadeCallTimeout 上限取自 C1：opencode 的 MCP 客户端 60.1s 就放弃并发
// MCP error，这里再留一倍余量——超过即按错误回复，不让对方白等。
// 用 var 而非 const 便于测试注入更短的预算。
var facadeCallTimeout = 12 * time.Second

// facadeTokenTTL 是会话 token 的有效期。建会话/重接都会换新 token，所以实际
// 上只有「一轮跑超过 TTL」才会撞线；24h 足够宽。var 便于测试注入。
var facadeTokenTTL = 24 * time.Hour

// facadeGrant 是一次授权：绑定的 agent、该会话可用的工具白名单与过期时刻。
type facadeGrant struct {
	agent   string // Friday 侧配置名（非 agent 自报，C10），调用记录据此归因
	allowed []string
	expires time.Time // 零值 = 不过期（鉴权关闭时的兜底授权）
}

// facadeRecordCap 是调用记录的保留条数（内存环形，进程重启即清零——审计的
// 权威记账在门面日志里，这里只服务设置页的「最近调用」列表）。
const facadeRecordCap = 50

// facadeRecord 是一次 tools/call 的结果记录（S5 设置页展示用）。
type facadeRecord struct {
	At      time.Time `json:"at"`
	Agent   string    `json:"agent,omitempty"` // 授权绑定的配置名；关鉴权时为空
	Tool    string    `json:"tool"`
	Outcome string    `json:"outcome"` // ok | error | timeout | denied
	Detail  string    `json:"detail,omitempty"`
}

// FacadeGrantInfo 是活授权的对外快照（不含 token 值本身）。
type FacadeGrantInfo struct {
	Agent   string    `json:"agent"`
	Tools   []string  `json:"tools"`
	Expires time.Time `json:"expires"`
}

// FacadeStatus 是门面运行状态的对外快照（admin GET /api/acp/facade 用）。
type FacadeStatus struct {
	// Enabled 是配置层开关（门面开启且注册表已注入）。URL 为空表示还没被
	// 任何外部会话用到（惰性监听未触发）。
	Enabled      bool   `json:"enabled"`
	AuthDisabled bool   `json:"authDisabled"`
	URL          string `json:"url,omitempty"`
	// FallbackTools 是全局兜底白名单（逐 agent 的 allowedFacadeTools 覆盖它，
	// 清单在 /api/acp/config 的 agents 里）。
	FallbackTools []string `json:"fallbackTools"`
	Lists         int      `json:"lists"`   // tools/list 累计
	Calls         int      `json:"calls"`   // tools/call 累计
	Rejects       int      `json:"rejects"` // 401 累计（无/错/过期 token）
	Grants        []FacadeGrantInfo `json:"grants"`
	Records       []facadeRecord    `json:"records"` // 最新在前，至多 facadeRecordCap 条
}

// facadeDeniedTools 结构性拒绝经门面暴露的工具：它们内部依赖 Friday 的审批
// 队列/权限层（confirm 模式下同步等人），一旦被外部 agent 调用必然撞 C1 的
// 15s 上限——审批卡弹着、agent 早已超时离开（docs/ACP-AGENTS.md C1/C2 的实测
// 教训）。写文件/执行命令类需求由外部 agent 用自己的工具走 ACP 原生
// request_permission 审批卡（一期 P1-D 已上线），不经门面；两拍审批 + 唤醒轮
// 属作业模型范围（M6 §5.1），对话空间不做。见 docs/AGENT-TOOL-INJECTION.md §3.4。
var facadeDeniedTools = map[string]bool{
	"write_file":   true,
	"edit_file":    true,
	"exec_command": true,
	"mcp_edit":     true,
}

// FacadeToolDenied 返回该工具是否属审批类（门面结构性拒绝暴露，C1 护栏）。
// admin 设置页渲染勾选框用。
func FacadeToolDenied(name string) bool { return facadeDeniedTools[name] }

// facadeFilterDenied 从白名单里剔除拒绝暴露的工具，返回清洗后的清单与被剔除
// 的名字（发 grant 前调用；日志最多每会话一条，不刷屏）。
func facadeFilterDenied(allowed []string) (clean []string, dropped []string) {
	clean = make([]string, 0, len(allowed))
	for _, n := range allowed {
		if facadeDeniedTools[n] {
			dropped = append(dropped, n)
			continue
		}
		clean = append(clean, n)
	}
	return clean, dropped
}

// Facade 把 Friday 注册表以 MCP server 形态暴露给外部 agent。惰性起监听：
// 只有第一个外部 agent 要建会话时才占端口，内置 Friday 会话完全不经过它。
type Facade struct {
	regFn        func() *vxagent.Registry
	fallback     []string // 鉴权关闭时的固定白名单
	authDisabled bool

	mu      sync.Mutex
	ln      net.Listener
	server  *http.Server
	url     string
	nonce   string
	grants  map[string]*facadeGrant
	lists   int            // tools/list 累计次数（观测用）
	calls   int            // tools/call 累计次数（观测用）
	rejects int            // 401 次数（观测用）
	records []facadeRecord // 最近调用（最新在尾，Status 时倒序给出）
}

// newFacade 构造门面（不立即监听）。兜底白名单先过一遍 C1 护栏（审批类工具
// 无条件剔除）；authDisabled=true 时忽略发 token 流程，一律按该兜底清单放行。
func newFacade(regFn func() *vxagent.Registry, fallbackAllowed []string, authDisabled bool) *Facade {
	clean, dropped := facadeFilterDenied(fallbackAllowed)
	if len(dropped) > 0 {
		log.Printf("[MCP门面] 兜底白名单含审批类工具 %s，已剔除（这类工具不能经门面暴露，见 C1/C2）",
			joinNames(dropped))
	}
	return &Facade{
		regFn:        regFn,
		fallback:     clean,
		authDisabled: authDisabled,
		grants:       map[string]*facadeGrant{},
		nonce:        randHex(6),
	}
}

// ServerFor 返回挂给 session/new 的 MCP server 条目：按传入白名单（绑定 agent
// 名以便调用记录归因）签发一个会话 token 并放进 Authorization header（鉴权
// 开启时），或裸条目（鉴权关闭）。监听失败返回 false——调用方据此回退到空
// 清单，不影响建会话。
func (f *Facade) ServerFor(allowed []string, agent string) (acp.McpServer, bool) {
	if f == nil {
		return acp.McpServer{}, false
	}
	url, ok := f.ensureListen()
	if !ok {
		return acp.McpServer{}, false
	}
	headers := []acp.HttpHeader{}
	if !f.authDisabled {
		tok, ok := f.issueGrant(allowed, agent)
		if !ok {
			return acp.McpServer{}, false
		}
		headers = append(headers, acp.HttpHeader{
			Name: "Authorization", Value: "Bearer " + tok,
		})
	}
	return acp.McpServer{
		Http: &acp.McpServerHttpInline{Name: facadeName, Url: url, Headers: headers},
	}, true
}

// URL 返回门面的 MCP 端点地址；尚未起监听返回空串（测试与日志用）。
func (f *Facade) URL() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.url
}

// Close 停掉 HTTP 服务（Manager.Close 与配置重建时调用；幂等）。
func (f *Facade) Close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	ln, server := f.ln, f.server
	f.ln, f.server, f.url = nil, nil, ""
	f.mu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
	if ln != nil {
		_ = ln.Close()
	}
}

// ensureListen 惰性起监听（127.0.0.1 随机端口）。已起或起成功返回地址。
func (f *Facade) ensureListen() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.url != "" {
		return f.url, true
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Printf("[MCP门面] 监听失败（本次建会话不带 Friday 工具）: %v", err)
		return "", false
	}
	mux := http.NewServeMux()
	mux.HandleFunc(facadePath, f.handle)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	f.server = srv
	f.ln = ln
	f.url = fmt.Sprintf("http://127.0.0.1:%d%s", ln.Addr().(*net.TCPAddr).Port, facadePath)
	auth := "token 鉴权"
	if f.authDisabled {
		auth = "鉴权关闭"
	}
	// goroutine 必须用局部 srv 而不是回读 f.server：Close 会把它置 nil，若
	// Close 恰好插在 go 与 goroutine 首次读之间，nil 接收者直接 SIGSEGV。
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[MCP门面] Serve 异常退出: %v", err)
		}
	}()
	log.Printf("[MCP门面] 已就绪 %s（%s，兜底白名单 %d 个工具）", f.url, auth, len(f.fallback))
	return f.url, true
}

// issueGrant 签发绑定 agent 与白名单的会话 token；剔除拒绝暴露的工具（C1
// 护栏），顺手清掉过期授权。
func (f *Facade) issueGrant(allowed []string, agent string) (string, bool) {
	clean, dropped := facadeFilterDenied(allowed)
	if len(dropped) > 0 {
		log.Printf("[MCP门面] 白名单含审批类工具 %s，已剔除（这类工具不能经门面暴露，见 C1/C2）",
			joinNames(dropped))
	}
	tok := randHex(16)
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for k, g := range f.grants {
		if !g.expires.IsZero() && now.After(g.expires) {
			delete(f.grants, k)
		}
	}
	f.grants[tok] = &facadeGrant{
		agent:   agent,
		allowed: clean,
		expires: now.Add(facadeTokenTTL),
	}
	return tok, true
}

// grantFor 从请求里解析授权。鉴权开启时缺 token / 未知 token / 过期 token 都
// 返回 nil（调用方回 401）。
func (f *Facade) grantFor(r *http.Request) *facadeGrant {
	if f.authDisabled {
		return &facadeGrant{allowed: f.fallback}
	}
	tok := bearerToken(r.Header.Get("Authorization"))
	if tok == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.grants[tok]
	if g == nil {
		return nil
	}
	if !g.expires.IsZero() && time.Now().After(g.expires) {
		delete(f.grants, tok)
		return nil
	}
	return g
}

// bearerToken 解析 "Bearer <token>" 形式的 Authorization 值。
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// ---------- JSON-RPC 信封 ----------

type facadeRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// isNotification 没带 id（或 id 为 null）的是通知：无响应体，202 即可。
func (r *facadeRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

type facadeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------- HTTP 入口 ----------

func (f *Facade) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete: // 会话释放：无状态实现，直接确认
		w.WriteHeader(http.StatusOK)
		return
	default:
		// 不提供 GET SSE 反向通道：规范要求此时返回 405（mock 探针同款）。
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	grant := f.grantFor(r)
	if grant == nil {
		f.mu.Lock()
		f.rejects++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,` +
			`"message":"unauthorized: 缺少或无效的 Bearer token（由 Friday 建会话时下发）"}}`))
		return
	}

	var req facadeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32700,"message":"parse error"}}`))
		return
	}
	if req.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := f.dispatch(r.Context(), req, grant)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", "friday-"+f.nonce)
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		out["error"] = rpcErr
	} else {
		out["result"] = result
	}
	_ = json.NewEncoder(w).Encode(out)
}

// dispatch 按 method 分发；返回 result 或 JSON-RPC error（二选一）。
// 工具清单与调用都以 grant.allowed 为准——不同 agent 的会话各见各的白名单。
func (f *Facade) dispatch(ctx context.Context, req facadeRequest, grant *facadeGrant) (any, *facadeError) {
	switch req.Method {
	case "initialize":
		// C12：回显客户端要的版本，不拒绝——opencode/codex 的版本各不相同。
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := p.ProtocolVersion
		if v == "" {
			v = facadeMCPVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": facadeName, "version": "1.0.0"},
			"instructions":    facadeInstructions(grant.allowed),
		}, nil
	case "tools/list":
		f.mu.Lock()
		f.lists++
		f.mu.Unlock()
		return map[string]any{"tools": facadeToolsFromRegistry(f.regFn(), grant.allowed)}, nil
	case "tools/call":
		return f.handleCall(ctx, req.Params, grant)
	case "ping":
		return map[string]any{}, nil
	}
	return nil, &facadeError{Code: -32601, Message: "method not found: " + req.Method}
}

// handleCall 执行白名单内的工具。执行失败/超时按 MCP 约定走 isError:true 的
// result（工具级错误），只有「名字未知/不在白名单」走 JSON-RPC error——前者
// 模型能看到并可自行调整，后者只是协议层的坏调用。
func (f *Facade) handleCall(ctx context.Context, raw json.RawMessage, grant *facadeGrant) (any, *facadeError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(raw, &p)
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	reg := f.regFn()
	if reg == nil {
		f.record(grant.agent, p.Name, "error", "工具注册表尚未就绪")
		return f.toolResult(fmt.Sprintf(
			"Friday 工具注册表尚未就绪，本次调用未执行。请稍后重试，或改用你自己的工具完成。"), true), nil
	}
	t, ok := resolveFacadeTool(reg, grant.allowed, p.Name)
	if !ok {
		f.record(grant.agent, p.Name, "denied", "不在该会话白名单")
		return nil, &facadeError{Code: -32602,
			Message: fmt.Sprintf("未知工具 %q（不在 Friday 门面清单内；可用清单见 tools/list）", p.Name)}
	}
	log.Printf("[MCP门面] 外部 agent 调用 %s", p.Name)

	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, facadeCallTimeout)
	defer cancel()
	out, err := t.Call(cctx, p.Arguments)
	if cctx.Err() == context.DeadlineExceeded {
		// C1/C9：超时不挂死连接，回指令化文本让对方自行决策。
		f.record(grant.agent, p.Name, "timeout", fmt.Sprintf("超过 %s 被中止", facadeCallTimeout))
		return f.toolResult(fmt.Sprintf(
			"Friday 工具 %s 超过 %s 未完成，已被门面中止。不要原样重试同一调用；"+
				"如仍需要该信息，请换更小的查询范围，或如实告知调用方该步骤未完成。",
			p.Name, facadeCallTimeout), true), nil
	}
	if err != nil {
		f.record(grant.agent, p.Name, "error", err.Error())
		return f.toolResult(fmt.Sprintf(
			"Friday 工具 %s 调用失败：%v。可修正参数后重试，或改用其它工具完成任务。",
			p.Name, err), true), nil
	}
	f.record(grant.agent, p.Name, "ok", time.Since(start).Round(time.Millisecond).String())
	return f.toolResult(out, false), nil
}

// record 追加一条调用记录（内存环形，封顶 facadeRecordCap 条，最新在尾）。
// 只服务设置页展示；审计权威在门面日志。
func (f *Facade) record(agent, tool, outcome, detail string) {
	detail = strings.TrimSpace(detail)
	if len(detail) > 200 {
		detail = detail[:200]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, facadeRecord{
		At: time.Now(), Agent: agent, Tool: tool, Outcome: outcome, Detail: detail,
	})
	if len(f.records) > facadeRecordCap {
		f.records = f.records[len(f.records)-facadeRecordCap:]
	}
}

// Status 返回门面运行状态快照（设置页用）：活授权按 agent 名排序（输出稳定，
// 不暴露 token 值本身），records 最新在前。
func (f *Facade) Status() FacadeStatus {
	if f == nil {
		return FacadeStatus{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := FacadeStatus{
		URL:           f.url,
		AuthDisabled:  f.authDisabled,
		FallbackTools: append([]string(nil), f.fallback...),
		Lists:         f.lists,
		Calls:         f.calls,
		Rejects:       f.rejects,
	}
	grants := make([]FacadeGrantInfo, 0, len(f.grants))
	for _, g := range f.grants {
		grants = append(grants, FacadeGrantInfo{
			Agent:   g.agent,
			Tools:   append([]string(nil), g.allowed...),
			Expires: g.expires,
		})
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].Agent < grants[j].Agent })
	st.Grants = grants
	for i := len(f.records) - 1; i >= 0; i-- {
		st.Records = append(st.Records, f.records[i])
	}
	return st
}

// toolResult 组装 MCP tools/call 的 result（文本单块 + isError 标记）。
func (f *Facade) toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}

// facadeInstructions 是 initialize 的 instructions 字段：告诉对方 agent 这台
// MCP server 的边界（只读、无审批），避免它对写类能力做无谓尝试。
func facadeInstructions(allowed []string) string {
	return fmt.Sprintf(
		"Friday 工具门面：只提供 Friday 的只读工具（%s）。"+
			"写入文件、执行命令、修改待办/记忆等能力未开放——那类需求请转告调用方走 Friday 对话流程。",
		joinNames(allowed))
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += "、"
		}
		out += n
	}
	return out
}
