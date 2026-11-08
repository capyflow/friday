package acp

// facade_test.go 覆盖工具门面的两端：
//   - HTTP 端：initialize 版本回显（C12）、tools/list 白名单、tools/call 的
//     成功/未知工具/执行失败/超时（C1/C9）四种形状；
//   - 会话端：session/new 的 mcpServers 真的挂上门面（enabled）或保持空数组
//     （disabled），沿 manager_test.go 的 fakeAgent 全链路走一遍。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/config"
	vxagent "github.com/capyflow/vortexagent/agent"
)

// facadeStubTool 是测试用的最小 vxagent.Tool 实现。
type facadeStubTool struct {
	name   string
	desc   string
	schema map[string]any
	call   func(ctx context.Context, args map[string]any) (string, error)
}

func (t *facadeStubTool) Name() string                  { return t.name }
func (t *facadeStubTool) Description() string           { return t.desc }
func (t *facadeStubTool) Schema() map[string]any        { return t.schema }
func (t *facadeStubTool) Call(ctx context.Context, args map[string]any) (string, error) {
	return t.call(ctx, args)
}

type facadeResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *facadeError    `json:"error"`
}

// facadePost 向门面 handler 发一条 JSON-RPC 请求并解析响应。
func facadePost(t *testing.T, h http.Handler, method string, params any) (int, facadeResponse) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		t.Fatalf("编码请求失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, facadePath, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp facadeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	return rec.Code, resp
}

func newFacadeStubRegistry() *vxagent.Registry {
	reg := vxagent.NewRegistry()
	_ = reg.Add(&facadeStubTool{
		name: "get_time", desc: "报当前时间", schema: map[string]any{"type": "object"},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			return "NOW 2026-11-06", nil
		},
	})
	_ = reg.Add(&facadeStubTool{
		name: "read_file", desc: "读文件", schema: map[string]any{"type": "object"},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			return "FILE CONTENT", nil
		},
	})
	_ = reg.Add(&facadeStubTool{
		name: "write_file", desc: "写文件（审批类，护栏拒绝）", schema: map[string]any{"type": "object"},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			return "", nil
		},
	})
	return reg
}

func TestFacadeInitializeEchoesVersion(t *testing.T) {
	f := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"get_time"}, true)
	h := http.HandlerFunc(f.handle)

	// C12：对方要什么版本就回什么版本。
	_, resp := facadePost(t, h, "initialize", map[string]any{"protocolVersion": "2025-11-25"})
	if resp.Error != nil {
		t.Fatalf("initialize 报错: %+v", resp.Error)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("解析 result 失败: %v", err)
	}
	if res.ProtocolVersion != "2025-11-25" || res.Capabilities.Tools == nil {
		t.Fatalf("initialize 响应不符: %+v", res)
	}

	// 客户端没带版本：兜底默认值，不能拒。
	_, resp = facadePost(t, h, "initialize", map[string]any{})
	if err := json.Unmarshal(resp.Result, &res); err != nil || res.ProtocolVersion != facadeMCPVersion {
		t.Fatalf("无版本 initialize 应兜底 %s: %+v err=%v", facadeMCPVersion, res, err)
	}
}

func TestFacadeToolsListWhitelisted(t *testing.T) {
	f := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"get_time"}, true)
	_, resp := facadePost(t, http.HandlerFunc(f.handle), "tools/list", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("tools/list 报错: %+v", resp.Error)
	}
	var res struct {
		Tools []facadeTool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("解析 tools 失败: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "get_time" {
		t.Fatalf("白名单外不应出现: %+v", res.Tools)
	}
	if res.Tools[0].InputSchema == nil {
		t.Fatal("schema 不应为空")
	}
}

func TestFacadeCallPassesArguments(t *testing.T) {
	var gotArgs map[string]any
	reg := vxagent.NewRegistry()
	_ = reg.Add(&facadeStubTool{
		name: "read_file", desc: "读文件", schema: map[string]any{"type": "object"},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			gotArgs = args
			return "FILE CONTENT", nil
		},
	})
	f := newFacade(func() *vxagent.Registry { return reg }, []string{"read_file"}, true)

	_, resp := facadePost(t, http.HandlerFunc(f.handle), "tools/call",
		map[string]any{"name": "read_file", "arguments": map[string]any{"path": "a.txt"}})
	if resp.Error != nil {
		t.Fatalf("tools/call 报错: %+v", resp.Error)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("解析 result 失败: %v", err)
	}
	if res.IsError || len(res.Content) != 1 || res.Content[0].Text != "FILE CONTENT" {
		t.Fatalf("调用结果不符: %+v", res)
	}
	if gotArgs["path"] != "a.txt" {
		t.Fatalf("参数未透传: %+v", gotArgs)
	}
}

func TestFacadeCallUnknownTool(t *testing.T) {
	f := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"get_time"}, true)
	// 注册表里有但不在白名单：同样按未知工具处理（-32602）。
	_, resp := facadePost(t, http.HandlerFunc(f.handle), "tools/call",
		map[string]any{"name": "write_file", "arguments": map[string]any{}})
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("白名单外工具应报 -32602: %+v", resp.Error)
	}
	// 注册表里压根没有的名字亦然。
	_, resp = facadePost(t, http.HandlerFunc(f.handle), "tools/call",
		map[string]any{"name": "no_such_tool"})
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("未知工具应报 -32602: %+v", resp.Error)
	}
}

func TestFacadeCallToolError(t *testing.T) {
	reg := vxagent.NewRegistry()
	_ = reg.Add(&facadeStubTool{
		name: "read_file", desc: "读文件", schema: map[string]any{"type": "object"},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			return "", context.Canceled
		},
	})
	f := newFacade(func() *vxagent.Registry { return reg }, []string{"read_file"}, true)
	_, resp := facadePost(t, http.HandlerFunc(f.handle), "tools/call",
		map[string]any{"name": "read_file"})
	if resp.Error != nil {
		t.Fatalf("工具执行错误应走 isError result 而非 JSON-RPC error: %+v", resp.Error)
	}
	var res struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "调用失败") {
		t.Fatalf("错误形状不符（C9 要指令化文本）: %+v", res)
	}
}

func TestFacadeCallTimeout(t *testing.T) {
	old := facadeCallTimeout
	facadeCallTimeout = 30 * time.Millisecond
	t.Cleanup(func() { facadeCallTimeout = old })

	reg := vxagent.NewRegistry()
	_ = reg.Add(&facadeStubTool{
		name: "web_fetch", desc: "抓网页", schema: map[string]any{"type": "object"},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			time.Sleep(300 * time.Millisecond) // 远超 30ms 预算
			return "TOO LATE", nil
		},
	})
	f := newFacade(func() *vxagent.Registry { return reg }, []string{"web_fetch"}, true)
	_, resp := facadePost(t, http.HandlerFunc(f.handle), "tools/call",
		map[string]any{"name": "web_fetch"})
	var res struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "中止") {
		t.Fatalf("超时应按 C1/C9 回指令化错误: %+v", res)
	}
}

func TestFacadeRejectsGET(t *testing.T) {
	f := newFacade(nil, []string{"get_time"}, true)
	req := httptest.NewRequest(http.MethodGet, facadePath, nil)
	rec := httptest.NewRecorder()
	http.HandlerFunc(f.handle).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405（无 SSE 反向通道）: %d", rec.Code)
	}
}

// ---------- 会话端：mcpServers 的注入与关断 ----------

func TestMcpServersForFacadeStates(t *testing.T) {
	reg := newFacadeStubRegistry()
	m := newTestManager(t, nil, nil, nil)
	m.SetRegistry(func() *vxagent.Registry { return reg })

	cfg := testConfig(t).Normalized()
	agent := config.AcpAgentConfig{Name: "fake"}
	servers := m.mcpServersFor(cfg, agent)
	if len(servers) != 1 || servers[0].Http == nil || servers[0].Http.Name != facadeName {
		t.Fatalf("enabled 应挂 friday 门面一条: %+v", servers)
	}
	if !strings.HasPrefix(servers[0].Http.Url, "http://127.0.0.1:") {
		t.Fatalf("门面地址应是本机回环: %s", servers[0].Http.Url)
	}
	// 默认鉴权开启：条目里应带 Bearer token。
	if len(servers[0].Http.Headers) == 0 || !strings.HasPrefix(servers[0].Http.Headers[0].Value, "Bearer ") {
		t.Fatalf("应携带 Authorization header: %+v", servers[0].Http.Headers)
	}

	off := false
	cfg.Facade = &config.AcpFacadeConfig{Enabled: &off}
	if servers := m.mcpServersFor(cfg, agent); servers == nil || len(servers) != 0 {
		t.Fatalf("disabled 应为非 nil 空数组: %#v", servers)
	}

	// 未注入注册表：同样空数组（不建门面）。
	m2 := newTestManager(t, nil, nil, nil)
	if servers := m2.mcpServersFor(testConfig(t).Normalized(), agent); servers == nil || len(servers) != 0 {
		t.Fatalf("无注册表应为非 nil 空数组: %#v", servers)
	}
}

// facadeHeaderToken 从 mcpServers 条目里取 Bearer token（无则空串）。
func facadeHeaderToken(t *testing.T, srv acp.McpServer) string {
	t.Helper()
	if srv.Http == nil {
		t.Fatal("条目缺少 http 形态")
	}
	for _, h := range srv.Http.Headers {
		if strings.EqualFold(h.Name, "Authorization") {
			return strings.TrimPrefix(h.Value, "Bearer ")
		}
	}
	return ""
}

// TestMcpServersForFacadePerAgentWhitelist 验证逐 agent 白名单经 token 生效：
// 直接拿 mcpServersFor 的产物，带着它的 token 真调一次 tools/list。
func TestMcpServersForFacadePerAgentWhitelist(t *testing.T) {
	m := newTestManager(t, nil, nil, nil)
	m.SetRegistry(func() *vxagent.Registry { return newFacadeStubRegistry() })
	cfg := testConfig(t).Normalized()

	// 该 agent 只授权 get_time：tools/list 里就不该出现 write_file。
	agent := config.AcpAgentConfig{Name: "fake", AllowedFacadeTools: []string{"get_time"}}
	servers := m.mcpServersFor(cfg, agent)
	if len(servers) != 1 {
		t.Fatalf("应挂一条门面: %#v", servers)
	}
	srv := servers[0]
	tok := facadeHeaderToken(t, srv)
	if tok == "" {
		t.Fatal("应携带 token")
	}

	req, _ := http.NewRequest(http.MethodPost, srv.Http.Url,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("调门面失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带 token 应放行: %d", resp.StatusCode)
	}
	var body struct {
		Result struct {
			Tools []facadeTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(body.Result.Tools) != 1 || body.Result.Tools[0].Name != "get_time" {
		t.Fatalf("逐 agent 白名单未生效: %+v", body.Result.Tools)
	}

	// 无 token 裸调：401。
	bare, _ := http.NewRequest(http.MethodPost, srv.Http.Url,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	resp2, err := http.DefaultClient.Do(bare)
	if err != nil {
		t.Fatalf("裸调失败: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401: %d", resp2.StatusCode)
	}
}

func TestSessionNewCarriesFacade(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	m.SetRegistry(func() *vxagent.Registry { return newFacadeStubRegistry() })

	th := mustThread(t, m)
	c := &collector{}
	if _, err := m.Send(context.Background(), th.ID, "打个招呼", nil, c.hooks()); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	params := f.sessionNewParams()
	arr, ok := params["mcpServers"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("session/new 应携带 1 条 mcpServers: %#v", params["mcpServers"])
	}
	first, _ := arr[0].(map[string]any)
	if first["name"] != facadeName {
		t.Fatalf("server 名应为 %s: %#v", facadeName, first)
	}
	if url, _ := first["url"].(string); !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("url 应是本机回环: %#v", first)
	}
}

func TestSessionNewFacadeDisabledLeavesEmptyArray(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	cfg := testConfig(t)
	off := false
	cfg.Facade = &config.AcpFacadeConfig{Enabled: &off}
	m := newTestManager(t, &cfg, nil, f)
	m.SetRegistry(func() *vxagent.Registry { return newFacadeStubRegistry() })

	th := mustThread(t, m)
	c := &collector{}
	if _, err := m.Send(context.Background(), th.ID, "打个招呼", nil, c.hooks()); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	params := f.sessionNewParams()
	arr, ok := params["mcpServers"].([]any)
	// 必须是 [] 而非 null（探针实测 opencode 的 zod 校验拒 null）。
	if !ok || arr == nil || len(arr) != 0 {
		t.Fatalf("disabled 时 mcpServers 应为非 nil 空数组: %#v", params["mcpServers"])
	}
}

// ---------- 鉴权（S2）：token 签发 / 401 / 按 token 白名单 / 过期 ----------

// facadePostAuth 带（或可空）token 发一条 JSON-RPC 请求。
func facadePostAuth(t *testing.T, h http.Handler, token, method string, params any) (int, facadeResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req := httptest.NewRequest(http.MethodPost, facadePath, strings.NewReader(string(body)))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp facadeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	return rec.Code, resp
}

func TestFacadeAuthRequired(t *testing.T) {
	f := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"get_time"}, false) // 鉴权开启
	h := http.HandlerFunc(f.handle)

	// 无 token / 错 token：401。
	if code, _ := facadePostAuth(t, h, "", "tools/list", nil); code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401: %d", code)
	}
	if code, _ := facadePostAuth(t, h, "bogus", "tools/list", nil); code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401: %d", code)
	}

	// ServerFor 签发的 token：放行且按签发白名单过滤。
	srv, ok := f.ServerFor([]string{"get_time"}, "tester")
	if !ok {
		t.Fatal("ServerFor 失败")
	}
	code, resp := facadePostAuth(t, h, facadeHeaderToken(t, srv), "tools/list", nil)
	if code != http.StatusOK || resp.Error != nil {
		t.Fatalf("有效 token 应放行: %d %+v", code, resp.Error)
	}
	var res struct {
		Tools []facadeTool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "get_time" {
		t.Fatalf("按 token 白名单过滤未生效: %+v", res.Tools)
	}
}

func TestFacadePerTokenWhitelists(t *testing.T) {
	f := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"get_time"}, false)
	h := http.HandlerFunc(f.handle)

	wide, _ := f.ServerFor([]string{"get_time", "read_file"}, "tester")
	narrow, _ := f.ServerFor([]string{"get_time"}, "tester")

	// 宽 token 能看到 read_file，窄 token 看不到——一台门面两种权限。
	code, resp := facadePostAuth(t, h, facadeHeaderToken(t, wide), "tools/list", nil)
	var res struct {
		Tools []facadeTool `json:"tools"`
	}
	_ = json.Unmarshal(resp.Result, &res)
	if code != http.StatusOK || len(res.Tools) != 2 {
		t.Fatalf("宽 token 应见 2 个工具: %d %+v", code, res.Tools)
	}
	_, resp = facadePostAuth(t, h, facadeHeaderToken(t, narrow), "tools/list", nil)
	res = struct {
		Tools []facadeTool `json:"tools"`
	}{}
	_ = json.Unmarshal(resp.Result, &res)
	if len(res.Tools) != 1 {
		t.Fatalf("窄 token 应只见 1 个工具: %+v", res.Tools)
	}
	// 窄 token 调清单外工具：-32602。
	if _, r := facadePostAuth(t, h, facadeHeaderToken(t, narrow), "tools/call",
		map[string]any{"name": "read_file"}); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("窄 token 调清单外工具应 -32602: %+v", r.Error)
	}
}

func TestFacadeTokenExpiry(t *testing.T) {
	old := facadeTokenTTL
	facadeTokenTTL = 30 * time.Millisecond
	t.Cleanup(func() { facadeTokenTTL = old })

	f := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"get_time"}, false)
	srv, _ := f.ServerFor([]string{"get_time"}, "tester")
	tok := facadeHeaderToken(t, srv)
	time.Sleep(60 * time.Millisecond)
	if code, _ := facadePostAuth(t, http.HandlerFunc(f.handle), tok, "tools/list", nil); code != http.StatusUnauthorized {
		t.Fatalf("过期 token 应 401: %d", code)
	}
}

func TestFacadeAuthDisabledServesFallback(t *testing.T) {
	f := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"get_time"}, true) // 鉴权关闭
	// 关鉴权时 ServerFor 不发 header（裸条目），无 token 直接可用。
	srv, ok := f.ServerFor([]string{"get_time"}, "tester")
	if !ok || len(srv.Http.Headers) != 0 {
		t.Fatalf("关鉴权应回裸条目: ok=%v headers=%+v", ok, srv.Http.Headers)
	}
	code, resp := facadePostAuth(t, http.HandlerFunc(f.handle), "", "tools/list", nil)
	if code != http.StatusOK || resp.Error != nil {
		t.Fatalf("关鉴权无 token 应放行: %d %+v", code, resp.Error)
	}
}

// TestFacadeServeAcceptsRealRequests 真监听冒烟：S2 曾把 ensureListen 里的
// Serve 启动弄丢（listener 活着、无人 accept，请求堆 backlog 直到客户端超时），
// httptest 回放式用例探不到这种坏法——这里对真实端口打一发收尾。
func TestFacadeServeAcceptsRealRequests(t *testing.T) {
	f := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"get_time"}, false)
	srv, ok := f.ServerFor([]string{"get_time"}, "tester")
	if !ok {
		t.Fatal("ServerFor 失败")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(srv.Http.Url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("真实端口请求失败（监听没起来）: %v", err)
	}
	defer resp.Body.Close()
	// 无 token：应快速回 401，而不是挂着不响应。
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401: %d", resp.StatusCode)
	}
}

// TestFacadeDeniesApprovalTools S3 护栏：审批类工具即使被白名单显式点名，
// 也不出现在 tools/list、tools/call 也到不了 Registry（C1/C2 结构性约束）。
func TestFacadeDeniesApprovalTools(t *testing.T) {
	f := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"get_time", "write_file"}, false) // 白名单点名 write_file
	h := http.HandlerFunc(f.handle)

	srv, ok := f.ServerFor([]string{"get_time", "write_file"}, "tester")
	if !ok {
		t.Fatal("ServerFor 失败")
	}
	tok := facadeHeaderToken(t, srv)

	code, resp := facadePostAuth(t, h, tok, "tools/list", nil)
	var res struct {
		Tools []facadeTool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if code != http.StatusOK || len(res.Tools) != 1 || res.Tools[0].Name != "get_time" {
		t.Fatalf("审批类工具不应出现在清单: code=%d tools=%+v", code, res.Tools)
	}
	if _, r := facadePostAuth(t, h, tok, "tools/call",
		map[string]any{"name": "write_file"}); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("审批类工具调用应 -32602: %+v", r.Error)
	}

	// 关鉴权模式同样被兜底白名单的构造期过滤挡住。
	g := newFacade(func() *vxagent.Registry { return newFacadeStubRegistry() },
		[]string{"write_file"}, true)
	if _, r := facadePostAuth(t, http.HandlerFunc(g.handle), "", "tools/call",
		map[string]any{"name": "write_file"}); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("关鉴权模式审批类工具仍应被拒: %+v", r.Error)
	}
}

// ---------- 调用记录与状态快照（S5 admin API 的数据面） ----------

func TestFacadeRecordsAndStatus(t *testing.T) {
	reg := vxagent.NewRegistry()
	_ = reg.Add(&facadeStubTool{
		name: "get_time", desc: "报时", schema: map[string]any{"type": "object"},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			return "NOW", nil
		},
	})
	_ = reg.Add(&facadeStubTool{
		name: "web_search", desc: "会失败的工具", schema: map[string]any{"type": "object"},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			return "", context.Canceled
		},
	})
	f := newFacade(func() *vxagent.Registry { return reg }, []string{"get_time", "web_search"}, false)
	h := http.HandlerFunc(f.handle)
	srv, ok := f.ServerFor([]string{"get_time", "web_search"}, "minimax")
	if !ok {
		t.Fatal("ServerFor 失败")
	}
	tok := facadeHeaderToken(t, srv)

	// ok 一条、error 一条、denied 一条。
	facadePostAuth(t, h, tok, "tools/call", map[string]any{"name": "get_time"})
	facadePostAuth(t, h, tok, "tools/call", map[string]any{"name": "web_search"})
	facadePostAuth(t, h, tok, "tools/call", map[string]any{"name": "read_file"}) // 不在 grant

	st := f.Status()
	if st.Calls != 3 || len(st.Records) != 3 {
		t.Fatalf("应有 3 条记录: calls=%d records=%d", st.Calls, len(st.Records))
	}
	if st.Records[0].Outcome != "denied" || st.Records[0].Tool != "read_file" {
		t.Fatalf("最新应为 denied（最新在前）: %+v", st.Records[0])
	}
	if st.Records[1].Outcome != "error" || st.Records[2].Outcome != "ok" {
		t.Fatalf("中间 error、最旧 ok: %+v", st.Records)
	}
	for _, r := range st.Records {
		if r.Agent != "minimax" {
			t.Fatalf("记录应归因到 grant 的 agent: %+v", r)
		}
	}
	if len(st.Grants) != 1 || st.Grants[0].Agent != "minimax" || len(st.Grants[0].Tools) != 2 {
		t.Fatalf("活授权快照不符: %+v", st.Grants)
	}
	// URL 已因真实调用起监听。
	if !strings.HasPrefix(st.URL, "http://127.0.0.1:") {
		t.Fatalf("status 应带门面地址: %q", st.URL)
	}
}

// TestFacadeStatusViaManager 验证 Manager.FacadeStatus：配置层开关 + 运行态
// 归并，且不为看状态而起监听。
func TestFacadeStatusViaManager(t *testing.T) {
	m := newTestManager(t, nil, nil, nil)
	m.SetRegistry(func() *vxagent.Registry { return newFacadeStubRegistry() })

	// 门面还没被任何会话用过：enabled=true、URL 为空、无授权无记录。
	st := m.FacadeStatus()
	if !st.Enabled || st.URL != "" || len(st.Grants) != 0 || len(st.Records) != 0 {
		t.Fatalf("初始状态不符: %+v", st)
	}

	cfg := testConfig(t).Normalized()
	agent := config.AcpAgentConfig{Name: "fake", AllowedFacadeTools: []string{"get_time"}}
	if servers := m.mcpServersFor(cfg, agent); len(servers) != 1 {
		t.Fatalf("应挂一条门面: %#v", servers)
	}
	st = m.FacadeStatus()
	if !strings.HasPrefix(st.URL, "http://127.0.0.1:") {
		t.Fatalf("发过 grant 后应有监听地址: %q", st.URL)
	}
	if len(st.Grants) != 1 || st.Grants[0].Agent != "fake" {
		t.Fatalf("授权应归因到 fake: %+v", st.Grants)
	}
	if len(st.FallbackTools) != len(config.FacadeDefaultTools) {
		t.Fatalf("兜底白名单应回显: %+v", st.FallbackTools)
	}
}
