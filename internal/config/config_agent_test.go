package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAgentMaxIterationsDefault 未配置时默认 100：框架缺省只有 10 轮，
// 真实的多工具活儿半路就断了（"工具调用超过 10 轮仍未结束"）。
func TestAgentMaxIterationsDefault(t *testing.T) {
	if DefaultAgentMaxIterations != 100 {
		t.Fatalf("默认值应为 100，实际 %d", DefaultAgentMaxIterations)
	}
	if got := (&Config{}).Agent.MaxIterationsOrDefault(); got != DefaultAgentMaxIterations {
		t.Fatalf("未配置应回落到 %d，实际 %d", DefaultAgentMaxIterations, got)
	}
	if got := (AgentConfig{MaxIterations: 60}).MaxIterationsOrDefault(); got != 60 {
		t.Fatalf("显式配置应原样返回，实际 %d", got)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "provider:\n  name: openai\n  baseURL: https://a.com/v1\n  model: m\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.MaxIterations != DefaultAgentMaxIterations {
		t.Fatalf("加载缺省应补成 %d，实际 %d", DefaultAgentMaxIterations, cfg.Agent.MaxIterations)
	}
}

// TestValidateAgentMaxIterations 超过上限的配置启动即报错（多打一个 0 就是十倍 token）。
func TestValidateAgentMaxIterations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "provider:\n  name: openai\n  baseURL: https://a.com/v1\n  model: m\n" +
		"agent:\n  maxIterations: 501\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "agent.maxIterations") {
		t.Fatalf("越界轮数上限应导致加载失败: %v", err)
	}
}

// TestNormalizeAgentMaxIterations 区间校验：0 表示回到默认，越界给出可填范围。
func TestNormalizeAgentMaxIterations(t *testing.T) {
	cases := map[int]int{
		0:                     DefaultAgentMaxIterations,
		1:                     1,
		200:                   200,
		MaxAgentMaxIterations: MaxAgentMaxIterations,
	}
	for in, want := range cases {
		got, err := NormalizeAgentMaxIterations(in)
		if err != nil || got != want {
			t.Errorf("NormalizeAgentMaxIterations(%d) = %d, %v；应为 %d", in, got, err, want)
		}
	}
	for _, bad := range []int{-1, MaxAgentMaxIterations + 1} {
		_, err := NormalizeAgentMaxIterations(bad)
		if err == nil {
			t.Fatalf("%d 越界应报错", bad)
		}
		if !strings.Contains(err.Error(), "1-500") {
			t.Errorf("报错应带出可填区间，实际 %q", err)
		}
	}
}

// TestUpdateAgentMaxIterations 热写回：保留注释与其他键，agent 段缺失时自动追加，
// 落盘的是 !!int（不是字符串），回读后能直接进 int 字段。
func TestUpdateAgentMaxIterations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := `# 顶部注释
provider:
  name: openai
  baseURL: https://api.example.com/v1
  model: demo
agent:
  maxIterations: 10 # 行内注释
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAgentMaxIterations(path, 120); err != nil {
		t.Fatalf("UpdateAgentMaxIterations: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# 顶部注释", "# 行内注释", "maxIterations: 120"} {
		if !strings.Contains(text, want) {
			t.Fatalf("写回后应保留/包含 %q，实际:\n%s", want, text)
		}
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.MaxIterations != 120 {
		t.Fatalf("回读轮数上限应为 120，实际 %d", cfg.Agent.MaxIterations)
	}

	// agent 段不存在的配置：自动追加。
	path2 := filepath.Join(dir, "no-agent.yaml")
	if err := os.WriteFile(path2, []byte("provider:\n  name: openai\n  baseURL: https://a.com/v1\n  model: m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAgentMaxIterations(path2, 30); err != nil {
		t.Fatalf("UpdateAgentMaxIterations(无段): %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if cfg2.Agent.MaxIterations != 30 {
		t.Fatalf("应自动追加 agent 段并写回: %+v", cfg2.Agent)
	}
}
