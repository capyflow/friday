// auth_test.go 覆盖 agent 登录态探测与认证执行（设置页「认证」面板）。
//
// 判定口径有三条，先信 agent 自己（authenticate 成功 = 已登录），再看环境变量
// 与 HOME 下的凭据文件，都看不出来就说「状态未知」——不硬猜"未登录"。
package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/config"
)

// authTestManager 装一个带 fake agent 的管理器（清单里只有一台 fake）。
func authTestManager(t *testing.T, f *fakeAgent) *Manager {
	t.Helper()
	return newTestManager(t, nil, nil, f)
}

func TestAuthStatusLoggedInWhenAuthenticateSucceeds(t *testing.T) {
	f := &fakeAgent{
		authMethods: []map[string]any{{"id": "login", "name": "登录", "type": "agent"}},
		authOK:      map[string]bool{"login": true},
	}
	m := authTestManager(t, f)
	st, err := m.AuthStatus(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn {
		t.Fatalf("应判为已登录，实际 %q（%s）", st.State, st.Message)
	}
	if !strings.Contains(st.Message, "认证通过") {
		t.Fatalf("应说明认证通过，实际 %q", st.Message)
	}
	if len(st.Methods) != 1 || st.Methods[0].ID != "login" || st.Methods[0].Type != "agent" {
		t.Fatalf("认证方式应原样透出，实际 %+v", st.Methods)
	}
}

// authenticate 失败但建会话能成 = 凭据其实可用，以建会话为准。
func TestAuthStatusLoggedOutWhenAuthenticateFails(t *testing.T) {
	f := &fakeAgent{
		authMethods: []map[string]any{{"id": "login", "name": "登录", "type": "agent"}},
	}
	m := authTestManager(t, f)
	st, err := m.AuthStatus(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn {
		t.Fatalf("能建会话应判为已登录，实际 %q（%s）", st.State, st.Message)
	}
}

// 建会话报 Authentication required —— agent 自己说没登录，这是权威答案。
func TestAuthStatusLoggedOutWhenSessionNeedsAuth(t *testing.T) {
	f := &fakeAgent{authRequired: true}
	m := authTestManager(t, f)
	st, err := m.AuthStatus(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedOut {
		t.Fatalf("建会话报鉴权错应判为未登录，实际 %q（%s）", st.State, st.Message)
	}
	if !strings.Contains(st.Message, "Authentication required") {
		t.Fatalf("应把 agent 的原话带回来，实际 %q", st.Message)
	}
}

func TestAuthStatusUnknownWhenAgentDeclaresNothing(t *testing.T) {
	f := &fakeAgent{authRequired: true}
	m := authTestManager(t, f)
	st, err := m.AuthStatus(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedOut {
		t.Fatalf("建会话报鉴权错应判为未登录，实际 %q（%s）", st.State, st.Message)
	}
}

func TestAuthStatusBuiltinNeedsNoLogin(t *testing.T) {
	f := &fakeAgent{}
	m := authTestManager(t, f)
	st, err := m.AuthStatus(context.Background(), config.BuiltinAgentName)
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn || !strings.Contains(st.Message, "内置") {
		t.Fatalf("内置条目应说明无需登录，实际 %+v", st)
	}
}

// 拉不起来不等于"未登录"：命令写错、包没装、PATH 变了都会走到这里。
// 那种情况只能说"判不出来"，别让人去折腾本来正常的登录态。
func TestAuthStatusUnknownWhenAgentCannotStart(t *testing.T) {
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "ghost", Command: "/nonexistent/ghost-bin"},
	}}
	// 不注入 fake agent：走真实 spawn，命令不存在必然拉不起来。
	m := newTestManager(t, &cfg, nil, nil)
	st, err := m.AuthStatus(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateUnknown {
		t.Fatalf("拉不起来应判为未知，实际 %q（%s）", st.State, st.Message)
	}
	if !strings.Contains(st.Message, "拉起失败") {
		t.Fatalf("应说明拉起失败，实际 %q", st.Message)
	}
}

// 已知 agent 给得出可执行的登录命令（设置页「在终端里登录」用得上）。
func TestAuthStatusCarriesLoginCommand(t *testing.T) {
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "codex", Command: "npx", Args: []string{"-y", "@agentclientprotocol/codex-acp"}},
	}}
	f := &fakeAgent{}
	m := newTestManager(t, &cfg, nil, f)
	st, err := m.AuthStatus(context.Background(), "codex")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.Command != "codex login" {
		t.Fatalf("codex 的登录命令应是 codex login，实际 %q", st.Command)
	}
	if !strings.Contains(st.Hint, "codex login") {
		t.Fatalf("登录指引里应含命令，实际 %q", st.Hint)
	}
}

// HOME 下有凭据文件 = 登录过（CLI 类 agent 不声明 authMethods，只有凭据可查）。
func TestAuthStatusDetectsCredFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatalf("造凭据目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("造凭据文件失败: %v", err)
	}
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "codex", Command: "npx"},
	}}
	f := &fakeAgent{}
	m := newTestManager(t, &cfg, nil, f)
	st, err := m.AuthStatus(context.Background(), "codex")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn {
		t.Fatalf("有凭据文件时应判为已登录，实际 %q（%s）", st.State, st.Message)
	}
	if len(st.Creds) != 1 || !strings.Contains(st.Creds[0], ".codex/auth.json") {
		t.Fatalf("应回显凭据文件路径，实际 %+v", st.Creds)
	}
}

// 认证动作要真的调到 agent 的 authenticate 方法上。
func TestAuthenticateCallsAgentMethod(t *testing.T) {
	f := &fakeAgent{
		authMethods: []map[string]any{{"id": "login", "name": "登录", "type": "agent"}},
		authOK:      map[string]bool{"login": true},
	}
	m := authTestManager(t, f)
	res, err := m.Authenticate(context.Background(), "fake", "login", nil)
	if err != nil {
		t.Fatalf("Authenticate 失败: %v", err)
	}
	if res.State != AuthStateLoggedIn {
		t.Fatalf("应认证成功，实际 %+v", res)
	}
	f.mu.Lock()
	calls := append([]string{}, f.authCalls...)
	f.mu.Unlock()
	if len(calls) == 0 || calls[len(calls)-1] != "login" {
		t.Fatalf("应调过 authenticate(login)，实际 %+v", calls)
	}
}

// 认证失败时把 agent 自己的话带回来（别只说"失败"）。
func TestAuthenticateReportsAgentError(t *testing.T) {
	f := &fakeAgent{
		authMethods: []map[string]any{{"id": "login", "name": "登录", "type": "agent"}},
	}
	m := authTestManager(t, f)
	res, err := m.Authenticate(context.Background(), "fake", "login", nil)
	if err != nil {
		t.Fatalf("Authenticate 失败: %v", err)
	}
	if res.State != AuthStateLoggedOut || !strings.Contains(res.Message, "not authenticated") {
		t.Fatalf("应带回 agent 的原话，实际 %+v", res)
	}
}

// env_var 型：变量填在配置里就算配好了（还没填 = 未登录）。
func TestAuthStatusEnvVarMethod(t *testing.T) {
	f := &fakeAgent{
		authMethods: []map[string]any{{
			"id": "key", "name": "API Key", "type": "env_var",
			"vars": []map[string]any{{"name": "OPENAI_API_KEY", "secret": true}},
		}},
	}
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "fake", Command: "fake-bin", Env: map[string]string{"OPENAI_API_KEY": "sk-x"}},
	}}
	m := newTestManager(t, &cfg, nil, f)
	st, err := m.AuthStatus(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn {
		t.Fatalf("配了变量应判为已登录，实际 %q（%s）", st.State, st.Message)
	}
	if len(st.Methods) != 1 || st.Methods[0].Type != "env_var" || len(st.Methods[0].Vars) != 1 {
		t.Fatalf("env_var 型应带出变量清单，实际 %+v", st.Methods)
	}
	if !st.Methods[0].Vars[0].Secret {
		t.Fatalf("密钥变量应标记为 secret，实际 %+v", st.Methods[0].Vars[0])
	}
}

// SDK 的联合体在"报文没有 type 字段"时会把方法判成 terminal，而 ACP 规范说
// 没有 type 即视为 agent——这里按规范纠正，别让界面把普通登录方式说成终端登录。
func TestAuthMethodViewTreatsBareMethodAsAgent(t *testing.T) {
	mv := acp.AuthMethod{Terminal: &acp.AuthMethodTerminalInline{Id: "login", Name: "登录"}}
	views := authMethodViews([]acp.AuthMethod{mv})
	if len(views) != 1 || views[0].Type != "agent" {
		t.Fatalf("没有 type 的方法应算 agent 型，实际 %+v", views)
	}
}

// SDK 把"报文里有 type 字段"的认证方式一律判成 terminal（哪怕值写的是 agent），
// 这里按 ACP 规范归一：type=agent 仍算 agent 型。
func TestNormalizeAuthMethodsTreatsTypeAgentAsAgent(t *testing.T) {
	var mv acp.AuthMethod
	if err := json.Unmarshal([]byte("{\"id\":\"login\",\"name\":\"登录\",\"type\":\"agent\"}"), &mv); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	views := authMethodViews([]acp.AuthMethod{mv})
	if len(views) != 1 || views[0].Type != "agent" {
		t.Fatalf("type=agent 应归一成 agent 型，实际 %+v", views)
	}
}

// 真带 args 的 terminal 型就是终端登录，别当成 agent。
func TestAuthMethodViewKeepsTerminalType(t *testing.T) {
	mv := acp.AuthMethod{Terminal: &acp.AuthMethodTerminalInline{
		Id: "login", Name: "登录", Args: []string{"login"},
	}}
	views := authMethodViews([]acp.AuthMethod{mv})
	if len(views) != 1 || views[0].Type != "terminal" {
		t.Fatalf("带 args 的应算 terminal 型，实际 %+v", views)
	}
}

// terminal 型没法在无头进程里做交互，只能把命令交出去。
func TestAuthenticateTerminalMethodAsksForTerminal(t *testing.T) {
	f := &fakeAgent{
		authMethods: []map[string]any{{
			"id": "login", "name": "登录", "type": "terminal", "args": []string{"login"},
		}},
	}
	m := authTestManager(t, f)
	_, err := m.Authenticate(context.Background(), "fake", "login", nil)
	if err == nil || !strings.Contains(err.Error(), "终端") {
		t.Fatalf("terminal 型应提示去终端登录，实际 %v", err)
	}
}

// 本地信号优先于问 agent：凭据文件在，就别去调 authenticate（它可能阻塞或
// 当场拉起登录流程，探测阶段不该有副作用）。
func TestAuthStatusPrefersLocalSignalOverAuthenticate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeAgent{
		authMethods: []map[string]any{{"id": "login", "name": "登录", "type": "agent"}},
	}
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "codex", Command: "npx"},
	}}
	m := newTestManager(t, &cfg, nil, f)
	st, err := m.AuthStatus(context.Background(), "codex")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn {
		t.Fatalf("有凭据文件应判为已登录，实际 %q（%s）", st.State, st.Message)
	}
	// 凭据命中就不该再去调 agent 的认证方法。
	f.mu.Lock()
	calls := len(f.authCalls)
	f.mu.Unlock()
	if calls != 0 {
		t.Fatalf("凭据命中时不该调 authenticate，实际调了 %d 次", calls)
	}
}

// agent 收下认证请求却不吭声（codebuddy 那样）：这是「判不出」，不是「未登录」——
// 它很可能正等着人完成登录。
func TestAuthStatusUnknownWhenAgentDoesNotAnswerAuthenticate(t *testing.T) {
	old := authProbeTimeout
	authProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { authProbeTimeout = old })
	f := &fakeAgent{
		authMethods: []map[string]any{{"id": "iOA", "name": "Login with iOA", "type": "agent"}},
		authSilent:  map[string]bool{"iOA": true},
	}
	m := authTestManager(t, f)
	st, err := m.AuthStatus(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn {
		t.Fatalf("能建会话应判为已登录，实际 %q（%s）", st.State, st.Message)
	}
}

// 探测最多试几个方式就收手（codebuddy 有四个，逐个试一遍太慢）。
func TestAuthStatusProbesAtMostFewMethods(t *testing.T) {
	old := authProbeTimeout
	authProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { authProbeTimeout = old })
	methods := []map[string]any{
		{"id": "a", "name": "A", "type": "agent"},
		{"id": "b", "name": "B", "type": "agent"},
		{"id": "c", "name": "C", "type": "agent"},
		{"id": "d", "name": "D", "type": "agent"},
	}
	f := &fakeAgent{authMethods: methods, authSilent: map[string]bool{
		"a": true, "b": true, "c": true, "d": true,
	}}
	m := authTestManager(t, f)
	if _, err := m.AuthStatus(context.Background(), "fake"); err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	f.mu.Lock()
	calls := len(f.authCalls)
	f.mu.Unlock()
	if calls > maxAuthProbeMethods {
		t.Fatalf("最多试 %d 个方式，实际试了 %d 个", maxAuthProbeMethods, calls)
	}
}

// codebuddy 的凭据是它自己的会话文件，登录要人扫码，四条登录方式还各对应
// 不同站点：登录指引要说得清「网页端不等于 CLI」「国内选微信、国际选 Google」。
func TestLoginGuideForCodebuddy(t *testing.T) {
	g := loginGuideFor(config.AcpAgentConfig{Name: "codebuddy-code", Command: "npx"})
	if g.command != "codebuddy" {
		t.Fatalf("codebuddy 的登录命令应是 codebuddy，实际 %q", g.command)
	}
	if !strings.Contains(g.hint, "网页端登录") {
		t.Fatalf("指引里应点明网页端登录不等于 CLI 凭据，实际 %q", g.hint)
	}
	if !g.interactive {
		t.Fatal("codebuddy 的登录要人扫码，必须标成 interactive——探测时不能调它的 authenticate")
	}
	if len(g.vars) == 0 || g.vars[0] != "CODEBUDDY_API_KEY" {
		t.Fatalf("应推荐 CODEBUDDY_API_KEY 作为凭据入口，实际 %+v", g.vars)
	}
	// codebuddy 的 /login 只在交互界面里认（参数形式 `codebuddy /login` 实测
	// 仍报未登录），所以要带 follow，终端页起完界面再补敲这一行。界面起来先问
	// 「信任此文件夹吗」，不先确认它不收斜杠命令——所以 follow 第一步是回车。
	want := []string{"", "/login"}
	if len(g.follow) != len(want) {
		t.Fatalf("应按「先回车确认信任、再 /login」补敲，实际 %+v", g.follow)
	}
	for i, w := range want {
		if g.follow[i] != w {
			t.Fatalf("follow 第 %d 步应为 %q，实际 %q（整体 %+v）", i+1, w, g.follow[i], g.follow)
		}
	}
}

// agent 不声明 env_var 认证方式时，推荐的环境变量就是唯一能落地的入口：
// 填进该 agent 的环境变量就算配好了（codebuddy 只认这个）。
func TestAuthStatusSuggestedVarsFilledMeansConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "codebuddy-code", Command: "npx", Env: map[string]string{
			"CODEBUDDY_API_KEY": "cb-xxxx",
		}},
	}}
	f := &fakeAgent{}
	m := newTestManager(t, &cfg, nil, f)
	st, err := m.AuthStatus(context.Background(), "codebuddy-code")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn {
		t.Fatalf("配了推荐变量应判为已登录，实际 %q（%s）", st.State, st.Message)
	}
	if !strings.Contains(st.Message, "CODEBUDDY_API_KEY") {
		t.Fatalf("应说明配了哪个变量，实际 %q", st.Message)
	}
	if len(st.SuggestedVars) == 0 || st.SuggestedVars[0] != "CODEBUDDY_API_KEY" {
		t.Fatalf("应把推荐变量回给界面，实际 %+v", st.SuggestedVars)
	}
}

// 没填推荐变量时不能凭空说已登录。
func TestAuthStatusSuggestedVarsEmptyStaysUnknown(t *testing.T) {
	// codebuddy 的凭据现在能按文件探测了，HOME 指向一个空的临时目录，
	// 结果才不随开发机是否登录过 codebuddy 变化。
	t.Setenv("HOME", t.TempDir())
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "codebuddy-code", Command: "npx"},
	}}
	f := &fakeAgent{authRequired: true}
	m := newTestManager(t, &cfg, nil, f)
	st, err := m.AuthStatus(context.Background(), "codebuddy-code")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State == AuthStateLoggedIn {
		t.Fatalf("没填凭据不该判为已登录，实际 %q（%s）", st.State, st.Message)
	}
}

// 交互式登录的 agent（codebuddy）探测时一次都不能调 authenticate：
// 它的 authenticate 会先登出再拉起浏览器（CLI 的 logout-origin.json 里
// reason=acp_reauthenticate 就是这条路径写的），探测等于替用户登出，
// 而且每探一个方式就多开一个登录页。
func TestAuthStatusNeverCallsAuthenticateForInteractiveAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	methods := []map[string]any{
		{"id": "iOA", "name": "Login with iOA", "type": "agent"},
		{"id": "external", "name": "Login with Google/Github", "type": "agent"},
		{"id": "internal", "name": "Login with WeChat", "type": "agent"},
		{"id": "selfhosted", "name": "Login with Enterprise Domain", "type": "agent"},
	}
	f := &fakeAgent{authMethods: methods, authRequired: true}
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "codebuddy", Command: "codebuddy"},
	}}
	m := newTestManager(t, &cfg, nil, f)
	st, err := m.AuthStatus(context.Background(), "codebuddy")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	f.mu.Lock()
	calls := append([]string{}, f.authCalls...)
	f.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("交互式登录的 agent 探测时不应调 authenticate，实际调了 %+v", calls)
	}
	if st.State != AuthStateLoggedOut {
		t.Fatalf("建会话报鉴权错应判未登录，实际 %q（%s）", st.State, st.Message)
	}
}

// codebuddy 登录成功后会把会话写到 Application Support 下；那份文件就是
// 它唯一的本地凭据（不是钥匙串）。认它，面板才能不碰 authenticate 就说已登录。
func TestAuthStatusDetectsCodebuddySessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rel := "Library/Application Support/CodeBuddyExtension/Data/Public/auth/Tencent-Cloud.coding-copilot.info"
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("造会话目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatalf("造会话文件失败: %v", err)
	}
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "codebuddy-code", Command: "codebuddy"},
	}}
	f := &fakeAgent{authRequired: true}
	m := newTestManager(t, &cfg, nil, f)
	st, err := m.AuthStatus(context.Background(), "codebuddy-code")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn {
		t.Fatalf("会话文件在应判已登录，实际 %q（%s）", st.State, st.Message)
	}
	if len(st.Creds) != 1 || !strings.Contains(st.Creds[0], "Tencent-Cloud.coding-copilot.info") {
		t.Fatalf("应回显会话文件路径，实际 %+v", st.Creds)
	}
	// 面板要据此收起「由 agent 自己认证」那组按钮：CLI 的 authenticate 会先登出
	// 再登录，已登录时点一次就掉线，还会多开登录页。
	if !st.Interactive {
		t.Fatalf("codebuddy 这类要人完成登录的 agent 应标记 interactive，实际 %+v", st)
	}
	f.mu.Lock()
	calls := append([]string{}, f.authCalls...)
	f.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("有会话文件就不该再调 authenticate，实际 %+v", calls)
	}
}

// 登出标记在 = 明确登出过：这时残留的凭据文件不能当已登录。
func TestAuthStatusLogoutMarkerBeatsLeftoverSessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rel := "Library/Application Support/CodeBuddyExtension/Data/Public/auth/Tencent-Cloud.coding-copilot.info"
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("造会话目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"stale"}}`), 0o600); err != nil {
		t.Fatalf("造会话文件失败: %v", err)
	}
	if err := os.WriteFile(path+".logged-out", []byte("marker"), 0o600); err != nil {
		t.Fatalf("造登出标记失败: %v", err)
	}
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "codebuddy", Command: "codebuddy"},
	}}
	f := &fakeAgent{authRequired: true}
	m := newTestManager(t, &cfg, nil, f)
	st, err := m.AuthStatus(context.Background(), "codebuddy")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State == AuthStateLoggedIn {
		t.Fatalf("登出标记在时不该判已登录，实际 %q（%s）", st.State, st.Message)
	}
	if len(st.Creds) != 0 {
		t.Fatalf("登出标记在时不该把残留文件当凭据，实际 %+v", st.Creds)
	}
}

// 列表里的状态徽章走本地快查：只看环境变量与凭据文件，不拉起 agent 进程。
// 八台 agent 每台拉一次进程既慢（2–16 秒）又有副作用（非交互式的 authenticate
// 会开登录页），列表不该付这个代价。
func TestAuthQuickStatusReadsLocalSignalsOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
		{Name: "codex", Command: "npx"},
		{Name: "codebuddy-code", Command: "codebuddy"},
	}}
	f := &fakeAgent{authRequired: true}
	m := newTestManager(t, &cfg, nil, f)

	// 本地什么都没有：判不出来，但不能一口咬定「未登录」。
	st, err := m.AuthQuickStatus("codex")
	if err != nil {
		t.Fatalf("AuthQuickStatus 失败: %v", err)
	}
	if st.State != AuthStateUnknown {
		t.Fatalf("本地无凭据时应判 unknown，实际 %q（%s）", st.State, st.Message)
	}

	// 放一份 codebuddy 凭据文件：快查应当直接认出已登录，并带 interactive 标记。
	rel := "Library/Application Support/CodeBuddyExtension/Data/Public/auth/Tencent-Cloud.coding-copilot.info"
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("造会话目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatalf("造会话文件失败: %v", err)
	}
	cb, err := m.AuthQuickStatus("codebuddy-code")
	if err != nil {
		t.Fatalf("AuthQuickStatus 失败: %v", err)
	}
	if cb.State != AuthStateLoggedIn || len(cb.Creds) != 1 {
		t.Fatalf("凭据文件在应判已登录并回显路径，实际 %q / %+v", cb.State, cb.Creds)
	}
	if !cb.Interactive {
		t.Fatalf("codebuddy 应带 interactive 标记，实际 %+v", cb)
	}

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("快查不该拉起 agent 进程，实际拉起 %d 次", got)
	}
}

// minimax / qoder / zcode 的凭据不在「一眼看得出」的位置，但确实各有落点：
// 列表徽章走本地快查，路径填错这几台就永远显示「状态未知」，而点「认证」
// 又立刻变已登录——所以路径要有测试盯着。
func TestQuickStatusKnowsCredPathsOfCommonAgents(t *testing.T) {
	cases := []struct {
		agent string
		cmd   string
		rel   string
	}{
		{"minimax-code", "npx", ".minimax/auth/prod/cn/mcode-public/auth.json"},
		{"qoder", "npx", ".qoder/.auth/user"},
		{"zcode", "zcode-acp-server", ".zcode/v2/credentials.json"},
	}
	for _, c := range cases {
		t.Run(c.agent, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, c.rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("造凭据目录失败: %v", err)
			}
			if err := os.WriteFile(path, []byte(`{"token":"x"}`), 0o600); err != nil {
				t.Fatalf("造凭据文件失败: %v", err)
			}
			cfg := config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{
				{Name: c.agent, Command: c.cmd},
			}}
			m := newTestManager(t, &cfg, nil, &fakeAgent{authRequired: true})
			st, err := m.AuthQuickStatus(c.agent)
			if err != nil {
				t.Fatalf("AuthQuickStatus 失败: %v", err)
			}
			if st.State != AuthStateLoggedIn {
				t.Fatalf("凭据文件在应判已登录，实际 %q（%s）", st.State, st.Message)
			}
		})
	}
}

// safe 巡检：只握手 + 建会话，绝不调 authenticate。
// 依据是 authenticate 对不少 agent 有副作用（登出、开浏览器登录页），
// 列表后台把每台都问一遍时不能碰它。
func TestAuthStatusSafeNeverCallsAuthenticate(t *testing.T) {
	f := &fakeAgent{
		authMethods:  []map[string]any{{"id": "login", "name": "登录", "type": "agent"}},
		authOK:       map[string]bool{"login": true},
		authRequired: true, // 建会话报鉴权错 = 未登录
	}
	m := authTestManager(t, f)
	st, err := m.AuthStatusSafe(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatusSafe 失败: %v", err)
	}
	f.mu.Lock()
	calls := append([]string{}, f.authCalls...)
	f.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("safe 巡检不该调 authenticate，实际 %+v", calls)
	}
	if st.State != AuthStateLoggedOut {
		t.Fatalf("建会话报鉴权错应判未登录，实际 %q（%s）", st.State, st.Message)
	}
}

// 能建出会话 = 凭据可用：safe 巡检不调 authenticate 也能给出「已登录」的结论，
// 这是它在列表里替代「认证」按钮的底气。
func TestAuthStatusSafeLogsInOnSessionProbe(t *testing.T) {
	f := &fakeAgent{
		authMethods: []map[string]any{{"id": "login", "name": "登录", "type": "agent"}},
	}
	m := authTestManager(t, f)
	st, err := m.AuthStatusSafe(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatusSafe 失败: %v", err)
	}
	if st.State != AuthStateLoggedIn {
		t.Fatalf("能建会话应判已登录，实际 %q（%s）", st.State, st.Message)
	}
	f.mu.Lock()
	calls := append([]string{}, f.authCalls...)
	f.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("safe 巡检不该调 authenticate，实际 %+v", calls)
	}
}
