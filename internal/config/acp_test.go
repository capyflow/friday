package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcpNormalized(t *testing.T) {
	c := AcpConfig{Enabled: true}
	n := c.Normalized()
	if n.MaxConcurrent != AcpDefaultMaxConcurrent ||
		n.IdleKillSeconds != AcpDefaultIdleKillSeconds ||
		n.TurnTimeoutSeconds != AcpDefaultTurnSeconds ||
		n.HandshakeTimeoutSeconds != AcpDefaultHandshakeSeconds {
		t.Fatalf("默认值不符: %+v", n)
	}
	// agent 条目超时覆盖优先
	n.Agents = []AcpAgentConfig{{Name: "x", Command: "c", TurnTimeoutSeconds: 42}}
	if got := n.TurnSeconds(n.Agents[0]); got != 42 {
		t.Fatalf("条目覆盖应优先生效，实际 %d", got)
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
				Env: map[string]string{"A": "B"}, Workspace: "~/proj"},
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
