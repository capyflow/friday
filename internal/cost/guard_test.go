package cost

import (
	"path/filepath"
	"testing"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/usage"
)

// resetLedger 清空内存账本（用量是包级账本，每个用例从干净状态起算）。
func resetLedger(t *testing.T) {
	t.Helper()
	if err := usage.Init(""); err != nil {
		t.Fatalf("usage.Init: %v", err)
	}
}

// tokenCfg 构造只按天 token 限额的配置。
func tokenCfg(daily int64) *config.Config {
	c := &config.Config{}
	c.Cost.Currency = "CNY"
	c.Cost.Budget.SoftRatio = 0.8
	c.Cost.Budget.DailyTokens = daily
	return c
}

// emptyPricer 返回一个没有价格表的 Pricer（路径指向空目录）。
func emptyPricer(t *testing.T) *Pricer {
	t.Helper()
	p, err := NewPricer(filepath.Join(t.TempDir(), "pricing.json"))
	if err != nil {
		t.Fatalf("NewPricer: %v", err)
	}
	return p
}

// TestGuardNoBudgetMeansNoLimit 未配置额度即完全不限：不会误拦主动任务。
func TestGuardNoBudgetMeansNoLimit(t *testing.T) {
	resetLedger(t)
	usage.Record(usage.Call{Model: "m", Origin: "web", In: 5_000_000, Out: 1000})

	c := &config.Config{}
	c.Cost.Budget.SoftRatio = 0.8
	g := NewGuard(c, emptyPricer(t))
	v := g.Evaluate()
	if v.State != StateOK || v.Ratio != 0 {
		t.Fatalf("无限额时应为 ok，实际 %+v", v)
	}
	if ok, _ := g.ProactiveAllowed("心跳巡检"); !ok {
		t.Error("无限额时不应拦主动任务")
	}
}

// TestGuardTokenBudgetTransitions ok → warn → over 的档位切换与闸门拦截。
func TestGuardTokenBudgetTransitions(t *testing.T) {
	resetLedger(t)
	g := NewGuard(tokenCfg(1000), emptyPricer(t))

	usage.Record(usage.Call{Model: "m", In: 400, Out: 100}) // 500/1000 = 0.5
	if v := g.Evaluate(); v.State != StateOK {
		t.Fatalf("占比 0.5 应为 ok，实际 %+v", v)
	}
	if ok, reason := g.ProactiveAllowed("心跳巡检"); !ok {
		t.Fatalf("0.5 时不应拦截，reason=%q", reason)
	}

	usage.Record(usage.Call{Model: "m", In: 300, Out: 50}) // 850/1000 = 0.85
	if v := g.Evaluate(); v.State != StateWarn {
		t.Fatalf("占比 0.85 应为 warn，实际 %+v", v)
	}
	if ok, _ := g.ProactiveAllowed("心跳巡检"); !ok {
		t.Error("warn 档不应拦截主动任务（只有 over 才停手）")
	}

	usage.Record(usage.Call{Model: "m", In: 200, Out: 0}) // 1050/1000 = 1.05
	v := g.Evaluate()
	if v.State != StateOver || v.Tightest != "dailyTokens" {
		t.Fatalf("超限应为 over，实际 %+v", v)
	}
	ok, reason := g.ProactiveAllowed("心跳巡检")
	if ok {
		t.Fatal("over 时应拦主动任务")
	}
	if reason == "" {
		t.Error("拦截必须给出可展示的原因")
	}
}

// TestGuardHysteresisPreventsFlapping 从 warn 回落到 ok 需要跌破迟滞带下沿，
// 避免贴着预警线抖动。
func TestGuardHysteresisPreventsFlapping(t *testing.T) {
	g := NewGuard(tokenCfg(1000), emptyPricer(t))

	resetLedger(t)
	usage.Record(usage.Call{Model: "m", In: 850})
	if v := g.Evaluate(); v.State != StateWarn {
		t.Fatalf("0.85 应为 warn，实际 %v", v.State)
	}

	// 回落到 0.75：仍在 [0.72, 0.8) 迟滞带内 → 保持 warn
	resetLedger(t)
	usage.Record(usage.Call{Model: "m", In: 750})
	if v := g.Evaluate(); v.State != StateWarn {
		t.Fatalf("迟滞带内应保持 warn，实际 %v", v.State)
	}

	// 回落到 0.70：跌破 0.8*0.9 → 恢复 ok
	resetLedger(t)
	usage.Record(usage.Call{Model: "m", In: 700})
	if v := g.Evaluate(); v.State != StateOK {
		t.Fatalf("跌破迟滞带应恢复 ok，实际 %v", v.State)
	}
}

// TestGuardManualPauseOverridesEverything 手动暂停连"无限额"也照停。
func TestGuardManualPauseOverridesEverything(t *testing.T) {
	resetLedger(t)
	g := NewGuard(&config.Config{}, emptyPricer(t))
	g.SetManualPaused(true)

	v := g.Evaluate()
	if v.State != StateOK || !v.ManualPaused || !v.ProactivePaused {
		t.Fatalf("手动暂停状态不符: %+v", v)
	}
	if ok, reason := g.ProactiveAllowed("目标运行"); ok || reason == "" {
		t.Errorf("手动暂停应拦住主动任务, ok=%v reason=%q", ok, reason)
	}

	g.SetManualPaused(false)
	if ok, _ := g.ProactiveAllowed("目标运行"); !ok {
		t.Error("恢复后应放行")
	}
}

// TestGuardPauseProactiveDisabledOnlyWarns pauseProactive=false 时只警示不停手。
func TestGuardPauseProactiveDisabledOnlyWarns(t *testing.T) {
	resetLedger(t)
	off := false
	c := tokenCfg(100)
	c.Cost.PauseProactive = &off
	usage.Record(usage.Call{Model: "m", In: 500})

	g := NewGuard(c, emptyPricer(t))
	v := g.Evaluate()
	if v.State != StateOver {
		t.Fatalf("应判定为 over，实际 %v", v.State)
	}
	if v.ProactivePaused {
		t.Error("pauseProactive=false 时不应真的暂停")
	}
	if ok, _ := g.ProactiveAllowed("智能任务"); !ok {
		t.Error("只警示模式下应放行")
	}
}

// TestGuardMoneyBudget 金额额度判定：配价模型的消费计入 dailyCost。
func TestGuardMoneyBudget(t *testing.T) {
	resetLedger(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.json")
	pricer, err := NewPricer(path)
	if err != nil {
		t.Fatal(err)
	}
	// 输入 1/百万、输出 8/百万
	if err := pricer.Save(Table{Currency: "CNY", Prices: []Price{
		{Model: "priced", InputPerMTok: 1, OutputPerMTok: 8},
	}}); err != nil {
		t.Fatal(err)
	}

	c := &config.Config{}
	c.Cost.Budget.SoftRatio = 0.8
	c.Cost.Budget.DailyCost = 1.0 // 1 元
	g := NewGuard(c, pricer)

	// 100k 输入 + 25k 输出 = 0.1 + 0.2 = 0.3 元
	usage.Record(usage.Call{Model: "priced", In: 100_000, Out: 25_000})
	if v := g.Evaluate(); v.State != StateOK || v.TodayCost < 0.29 || v.TodayCost > 0.31 {
		t.Fatalf("金额判定不符: %+v", v)
	}

	// 再加 0.8 元 → 超 1 元
	usage.Record(usage.Call{Model: "priced", In: 800_000, Out: 0})
	v := g.Evaluate()
	if v.State != StateOver || v.Tightest != "dailyCost" {
		t.Fatalf("金额超限应为 over，实际 %+v", v)
	}
}

// TestGuardUnpricedModelSkippedInCost 未配价模型计入 token 但不计入金额，
// 并单列出来提醒补价，避免给出看似准确实则漏算的成本。
func TestGuardUnpricedModelSkippedInCost(t *testing.T) {
	resetLedger(t)
	g := NewGuard(tokenCfg(0), emptyPricer(t))
	usage.Record(usage.Call{Model: "mystery-llm", In: 1000, Out: 100})

	v := g.Evaluate()
	if v.TodayCost != 0 {
		t.Errorf("未配价不应算出金额: %v", v.TodayCost)
	}
	if v.TodayTokens != 1100 {
		t.Errorf("token 应照计: %d", v.TodayTokens)
	}
	if len(v.UnknownModels) != 1 || v.UnknownModels[0] != "mystery-llm" {
		t.Errorf("应列出未配价模型: %+v", v.UnknownModels)
	}
}

// TestGuardZeroCallsSentinel provider 未回传 usage 的调用数进快照——
// 它是"流式记账又失效了"的哨兵。
func TestGuardZeroCallsSentinel(t *testing.T) {
	resetLedger(t)
	g := NewGuard(&config.Config{}, emptyPricer(t))
	usage.Record(usage.Call{Model: "m", Origin: "web"})
	usage.Record(usage.Call{Model: "m", Origin: "web", In: 10, Out: 1})

	v := g.Evaluate()
	if v.ZeroCalls != 1 {
		t.Fatalf("ZeroCalls 应为 1，实际 %d", v.ZeroCalls)
	}
}
