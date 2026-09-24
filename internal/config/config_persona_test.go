package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestUpdatePersona 验证 persona 配置的热写回：注释保留、persona 段自动追加、
// presets 整体替换、清空自定义后其余内容仍在。
func TestUpdatePersona(t *testing.T) {
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

	// 1. 首次保存：persona 段不存在 → 自动追加，active 与 presets 写入。
	in1 := []Persona{
		{ID: "playful", Name: "活泼调皮", Tagline: "轻快爱闹", Prompt: "- 元气满满：轻快爱开玩笑。"},
		{ID: "custom-1", Name: "毒舌损友", Tagline: "自定义人设", Prompt: "- 嘴上不饶人，心里最可靠。"},
	}
	if err := UpdatePersona(path, "playful", in1); err != nil {
		t.Fatalf("UpdatePersona: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# 顶部注释", "# 行内注释", "persona:", "active: playful", "毒舌损友", "memories.json"} {
		if !strings.Contains(text, want) {
			t.Fatalf("保存后应保留/包含 %q，实际:\n%s", want, text)
		}
	}
	var cfg1 Config
	if err := yaml.Unmarshal(data, &cfg1); err != nil {
		t.Fatal(err)
	}
	if cfg1.Persona.Active != "playful" || len(cfg1.Persona.Presets) != 2 ||
		cfg1.Persona.Presets[0].ID != "playful" || cfg1.Persona.Presets[1].Prompt != "- 嘴上不饶人，心里最可靠。" {
		t.Fatalf("回读配置不符: %+v", cfg1.Persona)
	}

	// 2. 再次保存为空列表：presets 清空，active 与其余配置/注释仍在。
	if err := UpdatePersona(path, "default", nil); err != nil {
		t.Fatalf("UpdatePersona(empty): %v", err)
	}
	data, _ = os.ReadFile(path)
	text = string(data)
	if !strings.Contains(text, "# 顶部注释") || !strings.Contains(text, "active: default") {
		t.Fatalf("清空后 active 与注释应保留:\n%s", text)
	}
	var cfg2 Config
	if err := yaml.Unmarshal(data, &cfg2); err != nil {
		t.Fatal(err)
	}
	if cfg2.Persona.Active != "default" || len(cfg2.Persona.Presets) != 0 {
		t.Fatalf("清空后 persona 应只剩 active: %+v", cfg2.Persona)
	}
}
