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
	"time"

	"github.com/capyflow/vortexagent/ctxengine"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/llmx"
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
	// 窗口解析：全局显式覆盖 > 模型库激活条目 > 框架按模型名查表（engine 内默认）。
	windowTokens := cfg.Context.WindowTokens
	if windowTokens <= 0 {
		windowTokens = cfg.Provider.ContextWindow
	}
	engineCfg := ctxengine.Config{
		Model:            cfg.Provider.Model,
		WindowTokens:     windowTokens,
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
	// 摘要/归档调用绕过 agent Hooks，单独包一层计入 token 账本。
	counted := &llmx.Counted{Base: provider, Origin: OriginSummarizer, Model: cfg.Provider.Model}
	engine := ctxengine.New(archive, ctxengine.NewLLMSummarizer(counted, cfg.Provider.Model), engineCfg)
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

// ContextSnapshot 一次上下文体检：活跃历史的估算占用，以及能否手动压缩。
//
// 口径与框架自动归档的闸门完全一致——都只按 session.Messages() 估算，既不含系统
// 提示词，也不含 Build 时前置的摘要区，因此它是「活跃区占比」而非「这次请求实际
// 发送的 token 数」。故意不另起一套口径：CanCompact 为真时，本轮结束后框架本来
// 就会自动归档一次，面板与后台行为对得上，用户才不会被两个数字绕晕。
type ContextSnapshot struct {
	Enabled    bool    `json:"enabled"`    // 上下文治理是否启用（关闭时其余字段无意义）
	Session    string  `json:"session"`    // 归一化后的会话 key
	Used       int     `json:"used"`       // 活跃历史估算 token
	Window     int     `json:"window"`     // 模型上下文窗口
	Ratio      float64 `json:"ratio"`      // Used/Window
	SoftRatio  float64 `json:"softRatio"`  // 归档触发软阈值
	Blocks     int     `json:"blocks"`     // 活跃块数（一轮问答通常一块）
	KeepRecent int     `json:"keepRecent"` // 压缩后保留的最近块数
	Messages   int     `json:"messages"`   // 活跃历史消息条数
	CanCompact bool    `json:"canCompact"` // 是否已达压缩闸门
}

// engine 返回当前生效的上下文引擎；未装配管理器或治理关闭时为 nil。
func (b *Brain) engine() *ctxengine.Engine {
	if b.ctx == nil {
		return nil
	}
	return b.ctx.Current()
}

// ContextSnapshot 返回指定会话的上下文占用快照。只读：走 Pool.History，
// 不会因为查一次占用就把空会话写进池里。治理未启用时只回 {Enabled:false}
// ——没有窗口值可显示，也没有可压缩的对象，前端据此隐藏入口。
func (b *Brain) ContextSnapshot(key string) ContextSnapshot {
	fullKey := CurrentKey(key)
	eng := b.engine()
	if eng == nil {
		return ContextSnapshot{Session: fullKey}
	}
	msgs := b.pool.History(context.Background(), fullKey)
	cfg := eng.Config()
	sid := sessionIDPrefix + fullKey
	return ContextSnapshot{
		Enabled:    true,
		Session:    fullKey,
		Used:       eng.EstimateTokens(sid, msgs),
		Window:     cfg.WindowTokens,
		Ratio:      eng.ContextUsage(sid, msgs),
		SoftRatio:  cfg.SoftRatio,
		Blocks:     len(ctxengine.SplitBlocks(msgs)),
		KeepRecent: cfg.KeepRecentBlocks,
		Messages:   len(msgs),
		CanCompact: eng.ShouldArchive(sid, msgs),
	}
}

// CompactContext 手动触发一次归档压缩：Compact 是框架里同步的那条路径
// （落盘原始块与交接摘要 → ResetTo 重写活跃历史），轮末自动归档走的是它的
// 异步版本，两者做的是同一件事。
//
// 与 Ask 抢同一把会话锁，抢不到就直接报错而不是排队——压缩会改写历史，
// 不能跟在途请求交错。Compact 只改内存会话，落盘由这里补：不 Save 的话
// 重启后又变回未压缩的长历史，等于白压一次。
func (b *Brain) CompactContext(key string) (ContextSnapshot, error) {
	fullKey := CurrentKey(key)
	eng := b.engine()
	if eng == nil {
		return ContextSnapshot{}, fmt.Errorf("上下文治理未启用（config.context.enabled），没有可压缩的归档流程")
	}
	mu := b.pool.Lock(fullKey)
	if !mu.TryLock() {
		return ContextSnapshot{}, fmt.Errorf("会话 %s 正在回复中，等这一轮结束再压缩", fullKey)
	}
	defer mu.Unlock()
	b.rw.RLock()
	defer b.rw.RUnlock()

	sess := b.pool.Get(context.Background(), fullKey)
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := eng.Compact(cctx, sessionIDPrefix+fullKey, sess); err != nil {
		return ContextSnapshot{}, err
	}
	if err := b.pool.Save(context.WithoutCancel(context.Background()), sess); err != nil {
		return ContextSnapshot{}, fmt.Errorf("压缩已完成，但保存会话失败: %w", err)
	}
	return b.ContextSnapshot(fullKey), nil
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
