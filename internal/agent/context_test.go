package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/capyflow/vortexagent/ctxengine"
	"github.com/capyflow/vortexagent/llm"

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

// seedTurns 往会话池灌 n 轮问答（一轮 = 一块：user + assistant）。
func seedTurns(b *Brain, key string, turns int) {
	sess := b.pool.Get(context.Background(), key)
	for i := 0; i < turns; i++ {
		sess.Add(llm.Message{Role: llm.RoleUser, Content: []llm.Content{{
			Type: llm.ContentText,
			Text: fmt.Sprintf("第 %d 个问题：请介绍一下这个会话上下文压缩的判定口径是怎样的", i),
		}}})
		sess.Add(llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{{
			Type: llm.ContentText, Text: fmt.Sprintf("第 %d 个回答", i),
		}}})
	}
}

func enabledCtxMgr(t *testing.T, window int) *ContextManager {
	t.Helper()
	m := NewContextManager(nil)
	cfg := testContextCfg(t, filepath.Join(t.TempDir(), "archive"))
	cfg.Context.WindowTokens = window
	if err := m.Rebuild(cfg); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	return m
}

// TestBrainContextSnapshotDisabled 未启用上下文治理时快照只表态"不可用"，
// 压缩直接报错——前端据此隐藏 chip：没有窗口值可显示，也没有可压缩的对象。
func TestBrainContextSnapshotDisabled(t *testing.T) {
	b := NewBrain(nil, NewSessionPool(nil), NewContextManager(nil))
	if got := b.ContextSnapshot("web"); got.Enabled {
		t.Fatalf("未 Rebuild 时 Enabled 应为 false: %+v", got)
	}
	if _, err := b.CompactContext("web"); err == nil {
		t.Fatal("未启用时压缩应报错而不是静默返回")
	}
}

// TestBrainContextSnapshot 快照给出占用/窗口/块数并归一化 key；闸门用「窗口压到
// 100 token」来翻转，比堆很多消息更稳（不依赖估算系数的绝对值）。
func TestBrainContextSnapshot(t *testing.T) {
	mgr := enabledCtxMgr(t, 128000)
	b := NewBrain(nil, NewSessionPool(nil), mgr)
	seedTurns(b, "web", 3)

	snap := b.ContextSnapshot("") // 空 key 归到主会话 web
	if !snap.Enabled || snap.Session != "web" {
		t.Fatalf("快照应归一化到主会话: %+v", snap)
	}
	if snap.Window != 128000 || snap.KeepRecent != 12 {
		t.Fatalf("窗口/保留块数应取生效配置: %+v", snap)
	}
	if snap.Blocks != 3 || snap.Messages != 6 {
		t.Fatalf("3 轮问答应是 3 块 6 条: %+v", snap)
	}
	if snap.Used <= 0 || snap.Ratio <= 0 {
		t.Fatalf("占用与占比应为正数: %+v", snap)
	}
	if snap.CanCompact {
		t.Fatalf("占比极低且块数未超保留数，不该放行压缩: %+v", snap)
	}

	tiny := enabledCtxMgr(t, 100)
	b2 := NewBrain(nil, NewSessionPool(nil), tiny)
	seedTurns(b2, "web", 3)
	if got := b2.ContextSnapshot("web"); !got.CanCompact || got.Ratio < got.SoftRatio {
		t.Fatalf("窗口 100 时应越过软阈值: %+v", got)
	}
}

// TestBrainCompactContextNoop 活跃块没超过保留数时，压缩是"无事可做"而不是错误：
// 框架的 archiveSession 直接返回 nil，快照前后一致（也不该因此去打 LLM）。
func TestBrainCompactContextNoop(t *testing.T) {
	b := NewBrain(nil, NewSessionPool(nil), enabledCtxMgr(t, 128000))
	seedTurns(b, "web", 2)
	before := b.ContextSnapshot("web")

	after, err := b.CompactContext("web")
	if err != nil {
		t.Fatalf("无历史可归档时不应报错: %v", err)
	}
	if after.Messages != before.Messages || after.Blocks != before.Blocks {
		t.Fatalf("无事可做时不该改写活跃历史: %+v → %+v", before, after)
	}
}
