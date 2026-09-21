package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestUpdateMcpServers 验证 mcp.servers 的热写回：注释保留、mcp 段自动追加、
// 多次保存为最新快照、env 顺序固定。
func TestUpdateMcpServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := `# 顶部注释
provider:
  name: openai
  baseURL: https://api.example.com/v1
  model: demo
  apiKeyEnv: TEST_KEY # 行内注释
memory:
  file: memories.json
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. 首次保存：mcp 段不存在 → 自动追加。
	in1 := []McpServerConfig{
		{Name: "fs", Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
			Env: map[string]string{"B_VAR": "2", "A_VAR": "1"}},
	}
	if err := UpdateMcpServers(path, in1); err != nil {
		t.Fatalf("UpdateMcpServers: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# 顶部注释", "# 行内注释", "mcp:", "server-filesystem", "A_VAR", "B_VAR"} {
		if !strings.Contains(text, want) {
			t.Fatalf("保存后应保留/包含 %q，实际:\n%s", want, text)
		}
	}
	// env 固定顺序：A_VAR 应出现在 B_VAR 之前。
	if strings.Index(text, "A_VAR") > strings.Index(text, "B_VAR") {
		t.Fatalf("env 应按 key 排序稳定输出:\n%s", text)
	}
	var cfg1 Config
	if err := yaml.Unmarshal(data, &cfg1); err != nil {
		t.Fatal(err)
	}
	if len(cfg1.Mcp.Servers) != 1 || cfg1.Mcp.Servers[0].Name != "fs" ||
		len(cfg1.Mcp.Servers[0].Args) != 3 || cfg1.Mcp.Servers[0].Env["A_VAR"] != "1" {
		t.Fatalf("回读配置不符: %+v", cfg1.Mcp.Servers)
	}

	// 2. 再次保存为空列表：servers 清空，其余内容与注释仍在。
	if err := UpdateMcpServers(path, nil); err != nil {
		t.Fatalf("UpdateMcpServers(empty): %v", err)
	}
	data, _ = os.ReadFile(path)
	text = string(data)
	if !strings.Contains(text, "# 顶部注释") || !strings.Contains(text, "memories.json") {
		t.Fatalf("清空后其余配置与注释应保留:\n%s", text)
	}
	var cfg2 Config
	if err := yaml.Unmarshal(data, &cfg2); err != nil {
		t.Fatal(err)
	}
	if len(cfg2.Mcp.Servers) != 0 {
		t.Fatalf("清空后 servers 应为空: %+v", cfg2.Mcp.Servers)
	}
}
