// guard.go 实现成本软限额闸门：把 token 账本换算成"当前该用什么颜色显示、
// 主动型工作还能不能开工"的判定。
package cost

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/usage"
)

// State 是成本态势。
type State string

const (
	StateOK   State = "ok"
	StateWarn State = "warn"
	StateOver State = "over"
)

// okReleaseFactor 是 warn→ok 的迟滞系数：跌破预警线的这个比例才回落，
// 避免在阈值附近来回抖动、告警刷屏。
const okReleaseFactor = 0.9

// View 是一次成本判定的完整快照（管理后台与闸门判定共用）。
type View struct {
	State           State             `json:"state"`
	Ratio           float64           `json:"ratio"` // 最紧一档额度的占比
	Tightest        string            `json:"tightest"`
	TodayTokens     int64             `json:"todayTokens"`
	TodayCost       float64           `json:"todayCost"`
	MonthlyTokens   int64             `json:"monthlyTokens"`
	MonthlyCost     float64           `json:"monthlyCost"`
	Currency        string            `json:"currency"`
	Budget          config.CostBudget `json:"budget"`
	UnknownModels   []string          `json:"unknownModels"` // 有用量但未配价的模型
	ZeroCalls       int64             `json:"zeroCalls"`     // provider 未回传 usage 的调用数（账本可信度哨兵）
	ManualPaused    bool              `json:"manualPaused"`
	ProactivePaused bool              `json:"proactivePaused"` // 当下是否真的在拦主动任务
}

// Guard 按当前账本与预算给出判定。预算来自 config（重启生效），
// 手动暂停开关只存在内存里（不写配置，重启后恢复默认）。
type Guard struct {
	cfg    *config.Config
	pricer *Pricer

	mu         sync.Mutex
	state      State
	warnLatch  bool
	latchDay   string
	alertedDay string
	manual     bool
}

// NewGuard 创建闸门。
func NewGuard(cfg *config.Config, p *Pricer) *Guard {
	return &Guard{cfg: cfg, pricer: p, state: StateOK}
}

// Evaluate 依据当前用量账本重算态势。按需计算（账本只有少量天），
// 不设后台 tick——状态永远与账本一致。
func (g *Guard) Evaluate() View {
	cfg := g.cfg.Snapshot()
	b := cfg.Cost.Budget
	soft := b.SoftRatio
	if soft <= 0 {
		soft = 0.8
	}

	now := time.Now()
	today := now.Format("2006-01-02")
	month := now.Format("2006-01")

	var v View
	v.Budget = b
	unknown := map[string]bool{}

	days, _ := usage.Summary()
	for _, d := range days {
		isToday := d.Date == today
		inMonth := strings.HasPrefix(d.Date, month)
		if isToday {
			v.TodayTokens += d.In + d.Out
		}
		if inMonth {
			v.MonthlyTokens += d.In + d.Out
		}
		for model, dim := range d.ByModel {
			if dim == nil {
				continue
			}
			v.ZeroCalls += dim.ZeroCalls
			c, ok := g.pricer.Cost(model, dim.In, dim.Out, dim.CacheRead)
			if !ok {
				if dim.In+dim.Out > 0 || dim.Calls > 0 {
					unknown[model] = true
				}
				continue
			}
			if isToday {
				v.TodayCost += c
			}
			if inMonth {
				v.MonthlyCost += c
			}
		}
	}

	// 取最紧的一档额度做状态判定：任一额度触顶即 over。
	type limit struct {
		used, cap float64
		label     string
	}
	candidates := []limit{
		{float64(v.TodayTokens), float64(b.DailyTokens), "dailyTokens"},
		{float64(v.MonthlyTokens), float64(b.MonthlyTokens), "monthlyTokens"},
		{v.TodayCost, b.DailyCost, "dailyCost"},
		{v.MonthlyCost, b.MonthlyCost, "monthlyCost"},
	}
	ratio := 0.0
	for _, c := range candidates {
		if c.cap <= 0 {
			continue
		}
		if r := c.used / c.cap; r > ratio {
			ratio, v.Tightest = r, c.label
		}
	}
	v.Ratio = ratio
	if len(unknown) > 0 {
		v.UnknownModels = make([]string, 0, len(unknown))
		for m := range unknown {
			v.UnknownModels = append(v.UnknownModels, m)
		}
		sort.Strings(v.UnknownModels)
	}

	g.mu.Lock()
	dayChanged := g.latchDay != today
	if dayChanged {
		g.latchDay, g.warnLatch = today, false
	}
	switch {
	case ratio >= 1:
		g.state = StateOver
		g.warnLatch = true
	case ratio >= soft:
		g.state = StateWarn
		g.warnLatch = true
	case g.warnLatch && ratio >= soft*okReleaseFactor && !dayChanged:
		// 迟滞带：仍在 warn，避免贴着阈值抖动时反复告警
		g.state = StateWarn
	default:
		g.state = StateOK
		g.warnLatch = false
	}
	v.State = g.state
	v.ManualPaused = g.manual
	v.Currency = g.currencyOf(cfg)
	v.ProactivePaused = g.manual || (cfg.Cost.PauseProactiveEnabled() && g.state == StateOver)
	g.mu.Unlock()
	return v
}

// ProactiveAllowed 判断一类主动型工作（心跳巡检 / 智能任务 / 目标运行）现在能否
// 开工。返回的 reason 供轨迹与通知展示。用户对话与计划步骤不调用此闸门。
func (g *Guard) ProactiveAllowed(what string) (bool, string) {
	v := g.Evaluate()
	if v.ManualPaused {
		return false, "成本闸门已手动暂停主动任务（运行状态页可恢复）"
	}
	if v.ProactivePaused {
		return false, fmt.Sprintf("%s已暂停：额度用量 %.0f%%（%s）", what, v.Ratio*100, v.Tightest)
	}
	return true, ""
}

// SetManualPaused 手动开关主动任务闸门（管理后台按钮，不写配置）。
func (g *Guard) SetManualPaused(on bool) {
	g.mu.Lock()
	g.manual = on
	g.mu.Unlock()
}

// TakeOverAlert 在每天首次进入 over 时返回 true（同日去重，避免每轮都推送）。
func (g *Guard) TakeOverAlert() bool {
	v := g.Evaluate()
	if v.State != StateOver {
		return false
	}
	today := time.Now().Format("2006-01-02")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.alertedDay == today {
		return false
	}
	g.alertedDay = today
	return true
}

func (g *Guard) currencyOf(cfg *config.Config) string {
	if g.pricer.Configured() {
		return g.pricer.Currency()
	}
	if cfg.Cost.Currency != "" {
		return cfg.Cost.Currency
	}
	return "CNY"
}
