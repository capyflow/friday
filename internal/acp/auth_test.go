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

func TestAuthStatusLoggedOutWhenAuthenticateFails(t *testing.T) {
	f := &fakeAgent{
		authMethods: []map[string]any{{"id": "login", "name": "登录", "type": "agent"}},
	}
	m := authTestManager(t, f)
	st, err := m.AuthStatus(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateLoggedOut {
		t.Fatalf("应判为未登录，实际 %q（%s）", st.State, st.Message)
	}
	if !strings.Contains(st.Message, "未登录") {
		t.Fatalf("应说明未登录，实际 %q", st.Message)
	}
}

func TestAuthStatusUnknownWhenAgentDeclaresNothing(t *testing.T) {
	f := &fakeAgent{}
	m := authTestManager(t, f)
	st, err := m.AuthStatus(context.Background(), "fake")
	if err != nil {
		t.Fatalf("AuthStatus 失败: %v", err)
	}
	if st.State != AuthStateUnknown {
		t.Fatalf("没声明认证方式时应判为未知，实际 %q", st.State)
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
	if st.State != AuthStateUnknown {
		t.Fatalf("agent 不回应时应判为未知，实际 %q（%s）", st.State, st.Message)
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

// codebuddy 的凭据不在文件里（大概在钥匙串），本地探测不出登录态：
// 它的登录方式由 agent 自己声明，登录指引要说得清有哪几种。
func TestLoginGuideForCodebuddy(t *testing.T) {
	g := loginGuideFor(config.AcpAgentConfig{Name: "codebuddy-code", Command: "npx"})
	if g.command != "codebuddy" {
		t.Fatalf("codebuddy 的登录命令应是 codebuddy，实际 %q", g.command)
	}
	if !strings.Contains(g.hint, "iOA") {
		t.Fatalf("指引里应说明有哪几种登录方式，实际 %q", g.hint)
	}
}
