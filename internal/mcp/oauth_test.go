// oauth_test.go 用一台"要求 OAuth 的假 MCP 服务器"（同一进程内既当受保护资源、
// 又当授权服务器）验证 Friday 侧的接入层：401 挑战如何变成「待授权」、浏览器授权
// 回来后如何带 Bearer、令牌被吊销后如何刷新重试、刷新被拒后如何回到可重新授权的
// 状态，以及会话落盘与撤销。全程只走 127.0.0.1，不碰真实服务商。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	vxauth "github.com/capyflow/vortexagent/tools/mcp/auth"
)

// fakeOAuthMCP 是一台最小实现 MCP OAuth 全部角色的假服务器。
type fakeOAuthMCP struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	accepted      map[string]bool // 仍然有效的访问令牌（刷新后旧的作废，模拟轮换）
	refresh       string          // 当前可用的 refresh token
	rejectRefresh bool            // true 时刷新一律 invalid_grant（模拟登录态失效）
	rotateOnUse   bool            // true 时每次刷新都让旧令牌失效
	authCode      string
	challengeURL  string
	clientID      string

	revokeCalls int
	tokenCalls  int
	refreshOK   int
}

func newFakeOAuthMCP(t *testing.T) *fakeOAuthMCP {
	f := &fakeOAuthMCP{
		t:        t,
		accepted: map[string]bool{"access-1": true},
		refresh:  "refresh-1",
		authCode: "code-1",
		clientID: "friday-test",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"resource":              f.srv.URL + "/mcp",
			"authorization_servers": []string{f.srv.URL},
			"scopes_supported":      []string{"mcp"},
			"resource_name":         "假 MCP",
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"registration_endpoint":                 f.srv.URL + "/register",
			"revocation_endpoint":                   f.srv.URL + "/revoke",
			"scopes_supported":                      []string{"mcp"},
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported": []string{"none"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ClientName   string   `json:"client_name"`
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ClientName == "" || len(req.RedirectURIs) == 0 {
			writeFakeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
			return
		}
		writeFakeJSON(w, http.StatusCreated, map[string]any{
			"client_id":                  f.clientID,
			"token_endpoint_auth_method": "none",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			f.tokenCalls++
			if r.Form.Get("code") != f.authCode {
				writeFakeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
				return
			}
			writeFakeJSON(w, http.StatusOK, map[string]any{
				"access_token": "access-1", "token_type": "Bearer",
				"expires_in": 3600, "refresh_token": f.refresh, "scope": "mcp",
			})
		case "refresh_token":
			if f.rejectRefresh {
				writeFakeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
				return
			}
			if r.Form.Get("refresh_token") != f.refresh {
				writeFakeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
				return
			}
			f.refreshOK++
			if f.rotateOnUse {
				for tok := range f.accepted {
					delete(f.accepted, tok) // 旧访问令牌立刻失效：只有刷新后的新令牌能用
				}
			}
			f.accepted["access-2"] = true
			f.refresh = "refresh-2"
			writeFakeJSON(w, http.StatusOK, map[string]any{
				"access_token": "access-2", "token_type": "Bearer",
				"expires_in": 3600, "refresh_token": f.refresh, "scope": "mcp",
			})
		default:
			writeFakeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		}
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.revokeCalls++
		delete(f.accepted, r.Form.Get("token"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	// 受保护的 MCP 端点：无有效 Bearer 时按规范回 401 + resource_metadata 挑战。
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authz, "Bearer ")
		f.mu.Lock()
		ok := f.accepted[token]
		f.mu.Unlock()
		if !ok {
			hdr := fmt.Sprintf(`Bearer error="invalid_token", resource_metadata=%q, scope="mcp"`,
				f.srv.URL+"/.well-known/oauth-protected-resource/mcp")
			w.Header().Set("WWW-Authenticate", hdr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		respond := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}
		switch msg.Method {
		case "initialize":
			w.Header().Set("MCP-Session-Id", "sess-fake-1")
			respond(map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			respond(map[string]any{"tools": []any{
				map[string]any{"name": "ping", "description": "测试工具", "inputSchema": map[string]any{"type": "object"}},
			}})
		case "tools/call":
			respond(map[string]any{"content": []map[string]any{{"type": "text", "text": "pong"}}})
		default:
			http.Error(w, "unknown method", http.StatusNotFound)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// resource 返回受保护的 MCP 端点地址。
func (f *fakeOAuthMCP) resource() string { return f.srv.URL + "/mcp" }

// authorizeAndComplete 走一次真实授权：Authorize 起流程 → 从授权 URL 里取出
// redirect_uri 与 state，按浏览器回调的样子打一次 → 等状态变 authorized。
func (f *fakeOAuthMCP) authorizeAndComplete(o *OAuth, name string) {
	f.t.Helper()
	authURL, err := o.Authorize(context.Background(), name)
	if err != nil {
		f.t.Fatalf("Authorize: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		f.t.Fatalf("解析授权 URL: %v", err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("resource") != f.resource() {
		f.t.Fatalf("授权参数不符（PKCE/resource）: %v", q)
	}
	cb := q.Get("redirect_uri") + "?code=" + url.QueryEscape(f.authCode) + "&state=" + url.QueryEscape(q.Get("state"))
	resp, err := http.Get(cb)
	if err != nil {
		f.t.Fatalf("模拟浏览器回调: %v", err)
	}
	resp.Body.Close()
	if !waitAuthState(o, name, AuthStateAuthorized, 3*time.Second) {
		state, msg := o.StateOf(name)
		f.t.Fatalf("授权后状态不是 authorized（当前 %q %q）", state, msg)
	}
}

func waitAuthState(o *OAuth, name, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if state, _ := o.StateOf(name); state == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// configure 声明一台 OAuth 远程服务器。
func (o *OAuth) configureTest(name, resource string) {
	o.Configure(name, AuthOAuth, resource)
}

func TestOAuthAuthorizeThenConnect(t *testing.T) {
	f := newFakeOAuthMCP(t)
	oauth := NewOAuthWithClient("", f.srv.Client())
	oauth.configureTest("notion-like", f.resource())

	mgr := NewManager(nil)
	mgr.SetOAuth(oauth)
	defer mgr.Close()

	// 1) 未授权：连接失败要归成"需要授权"，而不是含糊的 HTTP 401。
	err := mgr.Apply(context.Background(), []ServerConfig{{Name: "notion-like", URL: f.resource(), Auth: AuthOAuth}})
	if err == nil || !strings.Contains(err.Error(), "需要授权") {
		t.Fatalf("未授权应报需要授权: %v", err)
	}
	st := mgr.StatusList()
	if len(st) != 1 || st[0].Running || st[0].Auth != AuthOAuth || st[0].AuthState != AuthStateRequired {
		t.Fatalf("未授权状态不符: %+v", st)
	}
	if !strings.Contains(st[0].LastError, "授权") {
		t.Fatalf("状态里没有可操作的提示: %+v", st[0])
	}

	// 2) 授权完成后自动重连（Manager 的 onAuthorized 回调），工具随之可用。
	f.authorizeAndComplete(oauth, "notion-like")
	if !waitStatusRunning(mgr, "notion-like", true, 3*time.Second) {
		t.Fatalf("授权后未自动重连: %+v", mgr.StatusList())
	}
	reg := vxagent.NewRegistry()
	if err := mgr.RegisterTools(reg); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	tool, found := reg.Get("mcp_notion-like_ping") // sanitize 保留连字符，前缀按服务器名拼
	if !found {
		t.Fatalf("应有 mcp_notion_like_ping 工具: %v", reg.Names())
	}
	got, err := tool.Call(context.Background(), map[string]any{})
	if err != nil || got != "pong" {
		t.Fatalf("经 OAuth 连接调用工具失败: %v %q", err, got)
	}
	if st := mgr.StatusList(); st[0].AuthState != AuthStateAuthorized || st[0].AuthMessage != "" {
		t.Fatalf("已授权状态不符: %+v", st[0])
	}
	// 令牌确实发了出去：授权 + 一次工具调用，服务商侧看到的都是有效令牌。
	f.mu.Lock()
	refreshes := f.refreshOK
	f.mu.Unlock()
	if refreshes != 0 {
		t.Fatalf("令牌仍在有效期内不应提前刷新: refresh=%d", refreshes)
	}
}

// TestOAuthPendingAuthURLExposed 设置页在做「复制授权链接」兜底：流程等待回调期间
// 后端要能随时取回同一个授权地址，流程结束（成功或失败）后必须立刻失效。
func TestOAuthPendingAuthURLExposed(t *testing.T) {
	f := newFakeOAuthMCP(t)
	oauth := NewOAuthWithClient("", f.srv.Client())
	oauth.configureTest("notion-like", f.resource())
	mgr := NewManager(nil)
	mgr.SetOAuth(oauth)
	defer mgr.Close()

	if got := mgr.AuthURL("notion-like"); got != "" {
		t.Fatalf("未发起授权时不应有授权地址: %q", got)
	}
	authURL, err := oauth.Authorize(context.Background(), "notion-like")
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := oauth.AuthURL("notion-like"); got != authURL {
		t.Fatalf("等待回调期间应暴露授权地址: got %q want %q", got, authURL)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("解析授权 URL: %v", err)
	}
	q := u.Query()
	cb := q.Get("redirect_uri") + "?code=" + url.QueryEscape(f.authCode) + "&state=" + url.QueryEscape(q.Get("state"))
	resp, err := http.Get(cb)
	if err != nil {
		t.Fatalf("模拟浏览器回调: %v", err)
	}
	resp.Body.Close()
	if !waitAuthState(oauth, "notion-like", AuthStateAuthorized, 3*time.Second) {
		state, msg := oauth.StateOf("notion-like")
		t.Fatalf("授权后状态不是 authorized（当前 %q %q）", state, msg)
	}
	if got := oauth.AuthURL("notion-like"); got != "" {
		t.Fatalf("授权完成后不应再暴露授权地址: %q", got)
	}
}

// waitStatusRunning 等一台服务器的连接状态变成期望值（授权后的重连是异步的）。
func waitStatusRunning(mgr *Manager, name string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, st := range mgr.StatusList() {
			if st.Name == name && st.Running == want {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestOAuthExpiredTokenRefreshesAndRetries(t *testing.T) {
	f := newFakeOAuthMCP(t)
	f.mu.Lock()
	f.rotateOnUse = true // 刷新后旧令牌失效：第一次带旧令牌的请求必然 401
	f.mu.Unlock()
	oauth := NewOAuthWithClient("", f.srv.Client())
	oauth.configureTest("remote", f.resource())
	f.authorizeAndComplete(oauth, "remote")

	// 人为把会话里的令牌换成"已被服务商作废"的那一枚，验证 401 → 刷新 → 重试。
	sess, err := oauth.Get(context.Background(), "remote")
	if err != nil {
		t.Fatalf("会话未入库: %v", err)
	}
	sess.Token.AccessToken = "access-dead"
	sess.Token.ExpiresAt = time.Now().Add(time.Hour) // 本地看还没过期，只能靠 401 发现
	if err := oauth.Put(context.Background(), "remote", sess); err != nil {
		t.Fatalf("写入会话: %v", err)
	}

	conn, err := StartHTTPAuth("remote", f.resource(), nil, oauth.Provider("remote"))
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer conn.Close()
	if _, err := conn.CallTool(context.Background(), "ping", nil); err != nil {
		t.Fatalf("401 后应刷新并重试成功: %v", err)
	}
	f.mu.Lock()
	refreshes := f.refreshOK
	f.mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("应恰好刷新一次，实际 %d 次", refreshes)
	}
	// 轮换后的令牌与 refresh token 都要写回会话（否则下次刷新必失败）。
	sess, err = oauth.Get(context.Background(), "remote")
	if err != nil || sess.Token.AccessToken != "access-2" || sess.Token.RefreshToken != "refresh-2" {
		t.Fatalf("刷新结果未持久化: %+v err=%v", sess, err)
	}
}

func TestOAuthRefreshRejectedAsksReauth(t *testing.T) {
	f := newFakeOAuthMCP(t)
	oauth := NewOAuthWithClient("", f.srv.Client())
	oauth.configureTest("remote", f.resource())
	f.authorizeAndComplete(oauth, "remote")

	sess, err := oauth.Get(context.Background(), "remote")
	if err != nil {
		t.Fatalf("会话未入库: %v", err)
	}
	sess.Token.AccessToken = "access-dead"
	sess.Token.ExpiresAt = time.Now().Add(time.Hour)
	if err := oauth.Put(context.Background(), "remote", sess); err != nil {
		t.Fatalf("写入会话: %v", err)
	}
	f.mu.Lock()
	f.rejectRefresh = true // 服务商侧登录态已失效
	f.mu.Unlock()

	conn, err := StartHTTPAuth("remote", f.resource(), nil, oauth.Provider("remote"))
	if err == nil {
		conn.Close()
		t.Fatal("刷新被拒时握手应失败")
	}
	if !strings.Contains(err.Error(), "需要授权") {
		t.Fatalf("错误应说明需要授权: %v", err)
	}
	state, msg := oauth.StateOf("remote")
	if state != AuthStateRequired || !strings.Contains(msg, "重新授权") {
		t.Fatalf("状态应回到待授权并说明原因: %q %q", state, msg)
	}
}

// TestOAuthSeedStateFromStoredToken 状态由令牌库推断，覆盖重启后的四种情形。
func TestOAuthSeedStateFromStoredToken(t *testing.T) {
	const res = "https://mcp.example.com/mcp"
	now := time.Now()
	write := func(t *testing.T, path string, sess vxauth.Session) {
		t.Helper()
		b, err := json.Marshal(map[string]vxauth.Session{"remote": sess})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stateAfterRestart := func(t *testing.T, sess vxauth.Session, resource string) (string, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "mcp-oauth.json")
		write(t, path, sess)
		o := NewOAuthWithClient(path, nil)
		o.Configure("remote", AuthOAuth, resource)
		return o.StateOf("remote")
	}

	base := vxauth.Session{Resource: res, AuthorizationServer: "https://auth.example.com",
		Token: vxauth.Token{AccessToken: "a1", RefreshToken: "r1", ExpiresAt: now.Add(time.Hour)}}

	// 1. 令牌有效：已授权
	if st, _ := stateAfterRestart(t, base, res); st != AuthStateAuthorized {
		t.Errorf("有效令牌应算已授权: %q", st)
	}
	// 2. 过期但有刷新令牌：仍算已授权（连上时自动换）
	expired := base
	expired.Token.ExpiresAt = now.Add(-time.Hour)
	if st, _ := stateAfterRestart(t, expired, res); st != AuthStateAuthorized {
		t.Errorf("过期但有 refresh_token 应算已授权: %q", st)
	}
	// 3. 过期且没有刷新令牌：需要重新授权
	dead := expired
	dead.Token.RefreshToken = ""
	if st, _ := stateAfterRestart(t, dead, res); st != AuthStateRequired {
		t.Errorf("彻底过期的令牌应回到待授权: %q", st)
	}
	// 4. 配置里的地址换过：旧会话不再算数（令牌按资源发放，换地址等于换资源）
	moved := base
	moved.Resource = "https://old.example.com/mcp"
	if st, msg := stateAfterRestart(t, moved, res); st != AuthStateRequired || !strings.Contains(msg, "变更") {
		t.Errorf("地址变更应回到待授权并说明原因: %q %q", st, msg)
	}
}

func TestOAuthStorePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-oauth.json")
	f := newFakeOAuthMCP(t)
	oauth := NewOAuthWithClient(path, f.srv.Client())
	oauth.configureTest("remote", f.resource())
	f.authorizeAndComplete(oauth, "remote")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("会话未落盘: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("会话文件权限应为 0600: %v %v", fi, err)
	}
	if !strings.Contains(string(data), "refresh-1") {
		t.Fatalf("落盘内容缺少令牌: %s", data)
	}

	// 重启语义：新实例从文件读回，不需要再走一次浏览器授权。
	reloaded := NewOAuthWithClient(path, f.srv.Client())
	reloaded.configureTest("remote", f.resource())
	// 状态也要跟着恢复：否则每次重启都显示「待授权」，把人引去重走一遍浏览器授权。
	if state, _ := reloaded.StateOf("remote"); state != AuthStateAuthorized {
		t.Fatalf("重启后应直接显示已授权: %q", state)
	}
	value, ok := reloaded.Provider("remote").Authorization(context.Background())
	if !ok || !strings.HasPrefix(value, "Bearer access-") {
		t.Fatalf("重载后应能直接取到令牌: %q ok=%v", value, ok)
	}

	// 断开：撤销 + 删除本地会话，状态回到待授权。
	if err := reloaded.Disconnect(context.Background(), "remote"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	f.mu.Lock()
	revoked := f.revokeCalls
	f.mu.Unlock()
	if revoked == 0 {
		t.Fatal("断开授权应调用服务商撤销端点")
	}
	if state, _ := reloaded.StateOf("remote"); state != AuthStateRequired {
		t.Fatalf("断开后状态应为待授权: %q", state)
	}
	if _, err := reloaded.Get(context.Background(), "remote"); err != vxauth.ErrNotFound {
		t.Fatalf("断开后会话应已删除: %v", err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "refresh") {
		t.Fatalf("断开后文件里仍留有令牌: %s", data)
	}
}

func TestOAuthIgnoresSessionForAnotherResource(t *testing.T) {
	f := newFakeOAuthMCP(t)
	oauth := NewOAuthWithClient("", f.srv.Client())
	oauth.configureTest("remote", f.resource())
	f.authorizeAndComplete(oauth, "remote")

	// 配置里换了地址（同名服务器指向别的资源）：RFC 8707 下令牌不跨资源复用。
	oauth.Configure("remote", AuthOAuth, f.srv.URL+"/other")
	if _, ok := oauth.Provider("remote").Authorization(context.Background()); ok {
		t.Fatal("资源变更后可用令牌应失效")
	}
	state, _ := oauth.StateOf("remote")
	if state != AuthStateRequired {
		t.Fatalf("资源变更应回到待授权: %q", state)
	}
}

func TestOAuthConfigureGuards(t *testing.T) {
	f := newFakeOAuthMCP(t)
	oauth := NewOAuthWithClient("", f.srv.Client())

	// 没声明 OAuth 的服务器拿不到令牌提供者，静态头路径不受影响。
	oauth.Configure("local", AuthHeader, f.resource())
	if p := oauth.Provider("local"); p != nil {
		t.Fatal("静态头服务器不应有令牌提供者")
	}
	if _, err := oauth.Authorize(context.Background(), "local"); err == nil {
		t.Fatal("未启用 OAuth 的服务器不应能发起授权")
	}

	// 授权进行中重复点击要拒绝，避免并发起两个 loopback 回调。
	oauth.configureTest("remote", f.resource())
	if _, err := oauth.Authorize(context.Background(), "remote"); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if _, err := oauth.Authorize(context.Background(), "remote"); err == nil ||
		!strings.Contains(err.Error(), "正在进行中") {
		t.Fatalf("重复授权应被拒绝: %v", err)
	}
	oauth.Forget("remote") // 结束本次测试留下的流程
}

func TestManagerOAuthGuards(t *testing.T) {
	mgr := NewManager(nil)
	defer mgr.Close()
	// 未 EnableOAuth 时声明 oauth：给出明确原因，而不是静默连不上。
	err := mgr.Apply(context.Background(), []ServerConfig{{Name: "remote", URL: "https://mcp.example/mcp", Auth: AuthOAuth}})
	if err == nil {
		t.Fatal("缺少授权存储时应连接失败")
	}
	if _, err := mgr.Authorize(context.Background(), "remote"); err == nil ||
		!strings.Contains(err.Error(), "授权存储") {
		t.Fatalf("Authorize 应说明未启用授权存储: %v", err)
	}
	if err := mgr.Disconnect(context.Background(), "remote"); err == nil ||
		!strings.Contains(err.Error(), "授权存储") {
		t.Fatalf("Disconnect 同样应报错: %v", err)
	}
	// stdio 服务器不存在授权概念。
	mgr2 := NewManager(nil)
	mgr2.SetOAuth(NewOAuth(""))
	defer mgr2.Close()
	if _, err := mgr2.Authorize(context.Background(), "ghost"); err == nil ||
		!strings.Contains(err.Error(), "未找到") {
		t.Fatalf("未配置的服务器应报未找到: %v", err)
	}
}
