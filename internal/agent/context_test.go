package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/capyflow/vortexagent/ctxengine"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/memory"
)

// testContextCfg 构造一份可构建引擎的最小配置（provider 只需结构合法，
// 不发真实请求；摘要仅在会话超阈值归档时才会调用 LLM）。
func testContextCfg(t *testing.T, archiveDir string) *config.Config {
	t.Helper()
	t.Setenv("TEST_CTX_KEY", "sk-test")
	cfg := &config.Config{}
	cfg.Provider = config.ProviderConfig{Name: "openai", BaseURL: "https://api.example.com/v1",
		Model: "demo", APIKeyEnv: "TEST_CTX_KEY"}
	cfg.Context = config.ContextConfig{ArchiveDir: archiveDir}
	return cfg
}

// TestContextManagerDisabled 显式关闭时不建引擎，注册工具与 Ask 自动退化为旧行为。
func TestContextManagerDisabled(t *testing.T) {
	no := false
	cfg := testContextCfg(t, t.TempDir())
	cfg.Context.Enabled = &no
	m := NewContextManager(nil)
	if err := m.Rebuild(cfg); err != nil {
		t.Fatalf("关闭态 Rebuild 不应报错: %v", err)
	}
	if m.Current() != nil {
		t.Fatal("关闭态 Current 应为 nil")
	}
}

// TestContextManagerRebuildAndReset 启用后引擎就绪、归档目录建立；
// ResetSession 清运行态与归档——否则同 ID 新会话会带出旧摘要区。
func TestContextManagerRebuildAndReset(t *testing.T) {
	archiveDir := filepath.Join(t.TempDir(), "archive")
	cfg := testContextCfg(t, archiveDir)
	m := NewContextManager(nil)
	if err := m.Rebuild(cfg); err != nil {
		t.Fatalf("Rebuild 不应报错: %v", err)
	}
	if m.Current() == nil {
		t.Fatal("启用态 Current 不应为 nil")
	}
	if _, err := os.Stat(archiveDir); err != nil {
		t.Fatalf("归档目录应已建立: %v", err)
	}

	// 模拟已归档数据：<archive>/<sessionID>/block-*.json。
	sid := "friday-web"
	sessDir := filepath.Join(archiveDir, sid)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "block-bdeadbeef.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.ResetSession(context.Background(), sid)
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Fatalf("ResetSession 后会话归档应已删除: %v", err)
	}
	// 重复 Reset 不应 panic（幂等）。
	m.ResetSession(context.Background(), sid)
}

// TestWikiFactSink 候选事实落库为 wiki 页面：Source 标 agent、长内容截断标题。
func TestWikiFactSink(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewWikiStore(filepath.Join(dir, "memories"), filepath.Join(dir, "legacy.json"))
	sink := WikiFactSink(store)

	long := "这是一条特别长的候选事实内容，用来验证标题截断逻辑是否按 rune 边界工作正常，以及后续的文字不应出现在标题里。"
	sink(context.Background(), "friday-web", []ctxengine.CandidateFact{
		{Content: "用户偏好深色主题", SourceChunkID: "chunk-1"},
		{Content: "  ", SourceChunkID: "chunk-1"}, // 空白应跳过
		{Content: long, SourceChunkID: "chunk-2"},
	})

	if got := store.Count(); got != 2 {
		t.Fatalf("应写入 2 页（空白事实跳过），实际 %d", got)
	}
	pages := store.Pages()
	var agentPages []memory.PageMeta
	for _, p := range pages {
		if p.Source == "agent" {
			agentPages = append(agentPages, p)
		}
	}
	if len(agentPages) != 2 {
		t.Fatalf("全部页面 Source 应为 agent，实际 %d/%d", len(agentPages), len(pages))
	}
	for _, p := range agentPages {
		if rs := []rune(p.Title); len(rs) > 25 {
			t.Fatalf("标题应截断在 24 rune + 省略号，实际 %d rune: %q", len(rs), p.Title)
		}
	}
}
