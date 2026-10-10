// context.go 接入 vortexagent 的 ctxengine（单会话上下文引擎）：长会话不再
// 线性膨胀——活跃上下文 = 交接摘要区 + 最近块原文 + 按需检索
// （search_history / expand_chunk 两个工具，RegisterTools 注册），原始历史
// 不可变归档本地目录。Agent 挂上引擎后（robot.Build 的 Options.ContextEngine）
// Ask 全自动接线：注入会话 ID / 按预算组装 / 超窗裁剪重试 / 轮末异步归档。
//
// 引擎绑定 provider 与 model（摘要器构造后不可替换），因此与 Agent 一样走
// 「配置变更 → Rebuild」生命周期；引擎运行态（校准系数/摘要缓存）按会话 ID
// 惰性从归档加载，重建不丢数据。归档以会话 ID（friday-<key>，跨重启稳定）
// 为目录名，会话重置时须同步清归档，否则同 ID 新会话会带出旧摘要区。
package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/capyflow/vortexagent/ctxengine"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/memory"
)

// ContextManager 持有当前上下文引擎与归档库，供 Build（挂 Options）、
// RegisterTools（注册检索工具）、Brain.Reset（清会话）与热更新路径共用。
type ContextManager struct {
	mu      sync.Mutex
	engine  *ctxengine.Engine
	archive ctxengine.ArchiveStore
	cancel  context.CancelFunc // retention goroutine（RetentionDays>0 时存在）

	// factsSink 候选事实回调（NewContextManager 注入）；是否接线由 Rebuild
	// 时的 config.Context.FactsEnabled() 决定。
	factsSink func(context.Context, string, []ctxengine.CandidateFact)
}

// NewContextManager 创建管理器。factsSink 为候选事实落库回调（nil 表示丢弃，
// 见 WikiFactSink）；引擎本身经 Rebuild 按配置构建，未启用时 Current 返回 nil，
// 各使用方以 nil 引擎降级为「不治理上下文」的旧行为。
func NewContextManager(factsSink func(context.Context, string, []ctxengine.CandidateFact)) *ContextManager {
	return &ContextManager{factsSink: factsSink}
}

// Current 返回当前引擎（未启用返回 nil）。
func (m *ContextManager) Current() *ctxengine.Engine {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.engine
}

// Rebuild 按当前配置重建引擎：provider/model 变更（热更新供应商）或 context
// 配置变更后调用。失败时保留旧引擎并返回错误——摘要器模型可能落后一拍，
// 归档与检索不受影响。引擎构建失败最常见原因是 provider 配置非法。
func (m *ContextManager) Rebuild(cfg *config.Config) error {
	cfg = cfg.Snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !cfg.Context.IsEnabled() {
		m.stopRetentionLocked()
		m.engine = nil
		return nil
	}
	archive, err := ctxengine.NewFileSystemArchive(cfg.Context.ArchiveDir)
	if err != nil {
		return fmt.Errorf("创建归档目录: %w", err)
	}
	engineCfg := ctxengine.Config{
		Model:            cfg.Provider.Model,
		WindowTokens:     cfg.Context.WindowTokens,
		SoftRatio:        cfg.Context.SoftRatio,
		HardRatio:        cfg.Context.HardRatio,
		SummaryBudget:    cfg.Context.SummaryBudget,
		KeepRecentBlocks: cfg.Context.KeepRecentBlocks,
		RetrievalLimit:   cfg.Context.RetrievalLimit,
		RetentionDays:    cfg.Context.RetentionDays,
	}
	if m.factsSink != nil && cfg.Context.FactsEnabled() {
		engineCfg.OnCandidateFacts = m.factsSink
	}
	provider, err := newProvider(cfg)
	if err != nil {
		return fmt.Errorf("创建摘要器 provider: %w", err)
	}
	engine := ctxengine.New(archive, ctxengine.NewLLMSummarizer(provider, cfg.Provider.Model), engineCfg)
	m.stopRetentionLocked()
	m.engine = engine
	m.archive = archive
	if cfg.Context.RetentionDays > 0 {
		retCtx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		go engine.StartRetention(retCtx)
	}
	return nil
}

// ResetSession 清除会话的引擎运行态与已归档数据（Brain.Reset 调用）：
// 归档不清的话，同 ID 新会话首轮 Build 会带出旧摘要区，等于没清干净。
func (m *ContextManager) ResetSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	engine, archive := m.engine, m.archive
	m.mu.Unlock()
	if engine != nil {
		engine.DropState(sessionID)
	}
	if archive == nil {
		return nil
	}
	if err := archive.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("清除会话归档失败: %w", err)
	}
	return nil
}

// Close 停止 retention goroutine（预留：优雅退出时调用；不调也不泄漏进程外资源）。
func (m *ContextManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopRetentionLocked()
}

func (m *ContextManager) stopRetentionLocked() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// WikiFactSink 把归档抽取的候选事实写入记忆 wiki：标题取内容前 24 个 rune，
// Source 标 agent（与用户手写页面区分，后台可查可删）。同内容事实按标题
// 命中旧页更新，不重复堆积。失败仅记日志——候选事实是低置信补充，
// 显式 memory_save 仍是主通道，不能因它拖垮归档流水线。
func WikiFactSink(store *memory.WikiStore) func(context.Context, string, []ctxengine.CandidateFact) {
	return func(_ context.Context, _ string, facts []ctxengine.CandidateFact) {
		for _, f := range facts {
			content := strings.TrimSpace(f.Content)
			if content == "" {
				continue
			}
			title := content
			if rs := []rune(title); len(rs) > 24 {
				title = string(rs[:24]) + "…"
			}
			if _, _, err := store.Save("", title, content, "agent"); err != nil {
				log.Printf("[上下文] 候选事实写入记忆失败: %v", err)
			}
		}
	}
}
