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
		n.TurnTimeoutSeconds != AcpDefaultTurnSeconds ||
		n.HandshakeTimeoutSeconds != AcpDefaultHandshakeSeconds ||
		n.SessionTimeoutSeconds != AcpDefaultSessionSeconds {
		t.Fatalf("默认值不符: %+v", n)
	}
	// agent 条目超时覆盖优先
	n.Agents = []AcpAgentConfig{{Name: "x", Command: "c", TurnTimeoutSeconds: 42}}
	if got := n.TurnSeconds(n.Agents[0]); got != 42 {
		t.Fatalf("条目覆盖应优先生效，实际 %d", got)
	}
	// 建会话/握手各有自己的预算：opencode 首启列 skill 表就要 9s，拿 10s 握手值
	// 去等 session/new 必然误判（V-2）。
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
		{"负超时", AcpConfig{Enabled: true, TurnTimeoutSeconds: -1}, true},
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
	// 回读一致
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if !cfg.Acp.Enabled || cfg.Acp.MaxConcurrent != 3 || cfg.Acp.IdleKillSeconds != 300 {
		t.Fatalf("全局参数不符: %+v", cfg.Acp)
	}
	if len(cfg.Acp.Agents) != 2 {
		t.Fatalf("agent 数不符: %+v", cfg.Acp.Agents)
	}
	a := cfg.Acp.Agents[0]
	if a.Command != "npx" || len(a.Args) != 2 || a.Env["A"] != "B" || a.Workspace != "~/proj" {
		t.Fatalf("codex 条目不符: %+v", a)
	}
	// 握手/建会话覆盖也要能落盘回读：否则设置页调了个寂寞
	if a.HandshakeTimeoutSeconds != 90 || a.SessionTimeoutSeconds != 120 {
		t.Fatalf("codex 的超时覆盖未写回: %+v", a)
	}
	if !cfg.Acp.Agents[1].DegradeResume {
		t.Fatalf("degradeResume 未写回: %+v", cfg.Acp.Agents[1])
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
	if len(cfg.Acp.Agents) != 1 || cfg.Acp.Agents[0].Name != "only" {
		t.Fatalf("二次覆盖不符: %+v", cfg.Acp.Agents)
	}
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
