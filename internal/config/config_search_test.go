package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestUpdateToolsSearch 验证 tools.search 的热写回：注释保留、tools/search 段
// 自动追加、provider/apiKey 覆写为最新值。
func TestUpdateToolsSearch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := `# 顶部注释
provider:
  name: openai
  apiKeyEnv: TEST_KEY # 行内注释
tools:
  exec:
    mode: confirm
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. 首次保存：search 段不存在 → 在 tools 下追加。
	if err := UpdateToolsSearch(path, SearchConfig{Provider: "tavily", APIKey: "tvly-abc"}); err != nil {
		t.Fatalf("UpdateToolsSearch: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# 顶部注释", "# 行内注释", "mode: confirm", "provider: tavily", "tvly-abc"} {
		if !strings.Contains(text, want) {
			t.Fatalf("保存后应保留/包含 %q，实际:\n%s", want, text)
		}
	}
	var cfg1 Config
	if err := yaml.Unmarshal(data, &cfg1); err != nil {
		t.Fatal(err)
	}
	if cfg1.Tools.Search.Provider != "tavily" || cfg1.Tools.Search.APIKey != "tvly-abc" {
		t.Fatalf("回读配置不符: %+v", cfg1.Tools.Search)
	}
	if cfg1.Tools.Exec.Mode != "confirm" {
		t.Fatalf("tools.exec 不应被破坏: %+v", cfg1.Tools.Exec)
	}

	// 2. 覆写为另一供应商并清空 key。
	if err := UpdateToolsSearch(path, SearchConfig{Provider: "brave"}); err != nil {
		t.Fatalf("UpdateToolsSearch(brave): %v", err)
	}
	var cfg2 Config
	data, _ = os.ReadFile(path)
	if err := yaml.Unmarshal(data, &cfg2); err != nil {
		t.Fatal(err)
	}
	if cfg2.Tools.Search.Provider != "brave" || cfg2.Tools.Search.APIKey != "" {
		t.Fatalf("覆写结果不符: %+v", cfg2.Tools.Search)
	}
	if !strings.Contains(string(data), "# 顶部注释") {
		t.Fatalf("覆写后注释应保留:\n%s", data)
	}
}

// TestSearchProviderOrDefault 验证未知/空供应商回落 bing。
func TestSearchProviderOrDefault(t *testing.T) {
	for in, want := range map[string]string{
		"": "bing", "unknown": "bing", "Bing": "bing",
		"tavily": "tavily", "brave": "brave", "bocha": "bocha",
	} {
		if got := (SearchConfig{Provider: in}).ProviderOrDefault(); got != want {
			t.Errorf("ProviderOrDefault(%q) = %q, want %q", in, got, want)
		}
	}
}
