package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAcpNormalized(t *testing.T) {
	c := AcpConfig{Enabled: true}
	n := c.Normalized()
	if n.MaxConcurrent != AcpDefaultMaxConcurrent ||
		n.IdleKillSeconds != AcpDefaultIdleKillSeconds ||
		n.HandshakeTimeoutSeconds != AcpDefaultHandshakeSeconds ||
		n.SessionTimeoutSeconds != AcpDefaultSessionSeconds {
		t.Fatalf("默认值不符: %+v", n)
	}
	// 建会话/握手各有自己的预算：opencode 首启列 skill 表就要 9s，拿 10s 握手值
	// 去等 session/new 必然误判（V-2）。条目覆盖优先。
	n.Agents = []AcpAgentConfig{{Name: "x", Command: "c"}}
	n.Agents[0].HandshakeTimeoutSeconds = 30
	n.Agents[0].SessionTimeoutSeconds = 120
	if got := n.HandshakeSeconds(n.Agents[0]); got != 30 {
		t.Fatalf("握手覆盖应优先生效，实际 %d", got)
	}
	if got := n.SessionSeconds(n.Agents[0]); got != 120 {
		t.Fatalf("建会话覆盖应优先生效，实际 %d", got)
	}
	plain := AcpAgentConfig{Name: "y", Command: "c"}
	if got := n.SessionSeconds(plain); got != AcpDefaultSessionSeconds {
		t.Fatalf("未覆盖时应回落到全局建会话超时，实际 %d", got)
	}
	// 缺省启用/思考流显示
	defaultAgent := AcpAgentConfig{Name: "x"}
	if !defaultAgent.IsEnabled() {
		t.Fatal("agent 缺省应启用")
	}
	if !(AcpConfig{}.ThoughtEnabled()) {
		t.Fatal("思考流缺省应显示（D5）")
	}
}

func TestAcpValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     AcpConfig
		wantErr bool
	}{
		{"关闭时跳过检查", AcpConfig{}, false},
		{"缺 command", AcpConfig{Enabled: true, Agents: []AcpAgentConfig{{Name: "x"}}}, true},
		{"缺 name", AcpConfig{Enabled: true, Agents: []AcpAgentConfig{{Command: "c"}}}, true},
		{"name 重复", AcpConfig{Enabled: true, Agents: []AcpAgentConfig{
			{Name: "x", Command: "c"}, {Name: "x", Command: "c"}}}, true},
		{"负建会话超时", AcpConfig{Enabled: true, SessionTimeoutSeconds: -5}, true},
		{"条目负握手超时", AcpConfig{Enabled: true, Agents: []AcpAgentConfig{
			{Name: "x", Command: "c", HandshakeTimeoutSeconds: -1}}}, true},
		{"条目负建会话超时", AcpConfig{Enabled: true, Agents: []AcpAgentConfig{
			{Name: "x", Command: "c", SessionTimeoutSeconds: -1}}}, true},
		{"合法", AcpConfig{Enabled: true, Agents: []AcpAgentConfig{{Name: "x", Command: "c"}}}, false},
	}
	for _, tc := range cases {
		if err := tc.cfg.Validate(); (err != nil) != tc.wantErr {
			t.Fatalf("%s: err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

func TestUpdateAcpRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := `# 顶部注释
provider:
  baseURL: https://api.deepseek.com/v1
  model: deepseek-chat # 行内注释
acp:
  enabled: false
mcp:
  servers: []
`
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	next := AcpConfig{
		Enabled:         true,
		MaxConcurrent:   3,
		IdleKillSeconds: 300,
		Agents: []AcpAgentConfig{
			{Name: "codex", Command: "npx", Args: []string{"-y", "@zed-industries/codex-acp"},
				Env: map[string]string{"A": "B"}, Workspace: "~/proj",
				HandshakeTimeoutSeconds: 90, SessionTimeoutSeconds: 120},
			{Name: "opencode", Command: "opencode", Args: []string{"acp"}, DegradeResume: true},
		},
	}
	if err := UpdateAcp(path, next); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	// 注释与其余段保留
	if !contains(text, "# 顶部注释") || !contains(text, "# 行内注释") {
		t.Fatalf("注释丢失:\n%s", text)
	}
	if !contains(text, "provider:") || !contains(text, "mcp:") {
		t.Fatalf("其他段丢失:\n%s", text)
	}
	// 内置条目不落盘：它是代码给的（Normalized 注入），config.yaml 里每一条都该是
	// "要拉起的外部进程"，留下 builtin 那条会让下次读取出现两份 friday。
	if contains(text, "builtin") || contains(text, "friday") {
		t.Fatalf("内置条目被写进 config.yaml:\n%s", text)
	}
	// 回读一致
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if !cfg.Acp.Enabled || cfg.Acp.MaxConcurrent != 3 || cfg.Acp.IdleKillSeconds != 300 {
		t.Fatalf("全局参数不符: %+v", cfg.Acp)
	}
	// Load 已经归一化（清单最前是注入的内置 friday），所以按名字取那两台外部 agent，
	// 不按下标——内置排最前是这次有意定的顺序，用例不该假装它不在。
	if len(cfg.Acp.Agents) != 3 || !cfg.Acp.Agents[0].Builtin {
		t.Fatalf("agent 清单不符（内置应注入且只注入一次）: %+v", cfg.Acp.Agents)
	}
	a := agentByName(t, cfg.Acp.Agents, "codex")
	if a.Command != "npx" || len(a.Args) != 2 || a.Env["A"] != "B" || a.Workspace != "~/proj" {
		t.Fatalf("codex 条目不符: %+v", a)
	}
	// 握手/建会话覆盖也要能落盘回读：否则设置页调了个寂寞
	if a.HandshakeTimeoutSeconds != 90 || a.SessionTimeoutSeconds != 120 {
		t.Fatalf("codex 的超时覆盖未写回: %+v", a)
	}
	if !agentByName(t, cfg.Acp.Agents, "opencode").DegradeResume {
		t.Fatalf("degradeResume 未写回: %+v", cfg.Acp.Agents)
	}
	// 二次覆盖（改动 agent 清单后旧条目不残留）
	next.Agents = []AcpAgentConfig{{Name: "only", Command: "x"}}
	if err := UpdateAcp(path, next); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Acp.Agents) != 2 || agentByName(t, cfg.Acp.Agents, "only").Command != "x" {
		t.Fatalf("二次覆盖不符: %+v", cfg.Acp.Agents)
	}
}

func TestAcpBuiltinAgentInjection(t *testing.T) {
	c := AcpConfig{Enabled: true, Agents: []AcpAgentConfig{{Name: "codex", Command: "npx"}}}
	n := c.Normalized()
	if len(n.Agents) != 2 || n.Agents[0].Name != BuiltinAgentName || !n.Agents[0].Builtin {
		t.Fatalf("内置条目应注入到清单最前: %+v", n.Agents)
	}
	if _, ok := n.Agent(BuiltinAgentName); !ok {
		t.Fatal("内置条目应可按名字查到（会话绑定与孤儿判定都靠它）")
	}
	// 归一化要幂等：cfgFn 拿到的清单常已经被归一化过一次，再注入就滚成两条 friday。
	if again := n.Normalized(); len(again.Agents) != 2 {
		t.Fatalf("归一化应幂等: %+v", again.Agents)
	}
	// 不改调用者那份（Normalized 在共享快照上跑）。
	if len(c.Agents) != 1 {
		t.Fatalf("Normalized 不该改调用者的清单: %+v", c.Agents)
	}
	// 内置条目没有 command，Validate 不该因此报错。
	if err := n.Validate(); err != nil {
		t.Fatalf("内置条目不该被当成缺 command: %v", err)
	}
	if got := len(n.WithoutBuiltin().Agents); got != 1 {
		t.Fatalf("WithoutBuiltin 应剔掉内置条目: %+v", n.WithoutBuiltin().Agents)
	}
	// 用户自己配了一台叫 friday 的外部 agent：以配置为准，不再注入内置条目。
	user := AcpConfig{Enabled: true,
		Agents: []AcpAgentConfig{{Name: BuiltinAgentName, Command: "my-friday"}}}.Normalized()
	if len(user.Agents) != 1 || user.Agents[0].Builtin || user.Agents[0].Command != "my-friday" {
		t.Fatalf("同名配置条目应压过内置注入: %+v", user.Agents)
	}
}

// agentByName 从清单里取一条。内置条目排在最前是有意的顺序，所以测试按名字找
// 外部那几台，别按下标。
func agentByName(t *testing.T, list []AcpAgentConfig, name string) AcpAgentConfig {
	t.Helper()
	for _, a := range list {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("清单里没有 %q: %+v", name, list)
	return AcpAgentConfig{}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

func TestUpdateAcpVersionStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("accounts: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := AcpConfig{Enabled: true, Agents: []AcpAgentConfig{
		{Name: "codex", Command: "npx", Version: "0.1.0"},
	}}
	if err := UpdateAcp(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Acp struct {
			Agents []struct {
				Name    string `yaml:"name"`
				Version string `yaml:"version"`
			} `yaml:"agents"`
		} `yaml:"acp"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Acp.Agents) != 1 || doc.Acp.Agents[0].Version != "0.1.0" {
		t.Fatalf("版本戳未写回: %s", data)
	}
	// 无版本的条目不写 version 键（保持配置文件干净）。
	cfg.Agents[0].Version = ""
	if err := UpdateAcp(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if bytes.Contains(data, []byte("version")) {
		t.Fatalf("空版本不应写 version 键: %s", data)
	}
}

// ---------- 工具门面（acp.facade）----------

func TestAcpFacadeDefaults(t *testing.T) {
	n := AcpConfig{}.Normalized()
	if !n.FacadeEnabled() {
		t.Fatal("门面缺省应开启")
	}
	if len(n.FacadeTools()) != len(FacadeDefaultTools) || n.FacadeTools()[0] != "read_file" {
		t.Fatalf("缺省白名单应为只读清单: %v", n.FacadeTools())
	}
	off := false
	d := AcpConfig{Facade: &AcpFacadeConfig{Enabled: &off}}.Normalized()
	if d.FacadeEnabled() {
		t.Fatal("显式关闭应生效")
	}
	if len(d.Facade.Tools) != len(FacadeDefaultTools) {
		t.Fatalf("显式关闭但未配清单时仍应补默认清单: %v", d.Facade.Tools)
	}
	// 未归一化的裸配置同样回退默认清单（门面读数不依赖调用方先 Normalized）。
	if got := (AcpConfig{}).FacadeTools(); len(got) != len(FacadeDefaultTools) {
		t.Fatalf("裸配置应回退默认清单: %v", got)
	}
}

func TestAcpFacadeValidate(t *testing.T) {
	bad := AcpConfig{Enabled: true, Facade: &AcpFacadeConfig{Tools: []string{"get_time", ""}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("白名单里的空工具名应报错")
	}
	ok := AcpConfig{Enabled: true, Facade: &AcpFacadeConfig{Tools: []string{"get_time"}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法白名单不应报错: %v", err)
	}
}

func TestUpdateAcpFacadeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := "provider:\n  baseURL: https://api.deepseek.com/v1\n  model: deepseek-chat\nacp:\n  enabled: false\n"
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}

	// 关闭 + 自定义清单（异于默认）：两处都该落盘并回读。
	off := false
	custom := AcpConfig{Enabled: true, Facade: &AcpFacadeConfig{Enabled: &off, Tools: []string{"get_time"}}}
	if err := UpdateAcp(path, custom); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if cfg.Acp.Facade == nil || cfg.Acp.Facade.Enabled == nil || *cfg.Acp.Facade.Enabled {
		t.Fatalf("facade.enabled=false 未写回: %+v", cfg.Acp.Facade)
	}
	if len(cfg.Acp.Facade.Tools) != 1 || cfg.Acp.Facade.Tools[0] != "get_time" {
		t.Fatalf("facade.tools 未写回: %+v", cfg.Acp.Facade.Tools)
	}

	// 回到默认形态：facade 段应整体消失（默认不写进 config.yaml）。
	if err := UpdateAcp(path, AcpConfig{Enabled: true}); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("facade")) {
		t.Fatalf("默认形态不应落 facade 段:\n%s", data)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if !cfg2.Acp.FacadeEnabled() || len(cfg2.Acp.FacadeTools()) != len(FacadeDefaultTools) {
		t.Fatalf("默认形态回读应回到缺省开启 + 只读清单: %+v", cfg2.Acp.Facade)
	}
}

func TestUpdateAcpFacadeAuthAndAgentWhitelist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := "provider:\n  baseURL: https://api.deepseek.com/v1\n  model: deepseek-chat\nacp:\n  enabled: false\n"
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}

	authOff := true
	cfg := AcpConfig{
		Enabled: true,
		Facade:  &AcpFacadeConfig{DisableAuth: &authOff},
		Agents: []AcpAgentConfig{
			{Name: "codex", Command: "codex-acp", AllowedFacadeTools: []string{"get_time", "recall"}},
			{Name: "opencode", Command: "opencode"},
		},
	}
	if err := UpdateAcp(path, cfg); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if !got.Acp.FacadeAuthDisabled() {
		t.Fatal("disableAuth 未写回")
	}
	codex := got.Acp.Agents[1] // Load 已归一化，内置 friday 排最前
	if codex.Name != "codex" {
		t.Fatalf("条目错位: %+v", got.Acp.Agents)
	}
	if len(codex.AllowedFacadeTools) != 2 || codex.AllowedFacadeTools[0] != "get_time" {
		t.Fatalf("allowedFacadeTools 未写回: %+v", codex.AllowedFacadeTools)
	}
	for _, a := range got.Acp.Agents {
		if a.Name == "opencode" && len(a.AllowedFacadeTools) != 0 {
			t.Fatalf("未配置的 agent 不应有白名单: %+v", a)
		}
	}

	// 回到默认：disableAuth / allowedFacadeTools 键都应消失。
	if err := UpdateAcp(path, AcpConfig{Enabled: true}); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("disableAuth")) || bytes.Contains(data, []byte("allowedFacadeTools")) {
		t.Fatalf("默认形态不应落这些键:\n%s", data)
	}
}
