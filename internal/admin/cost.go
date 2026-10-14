// cost.go 提供管理后台的成本相关接口：用量账本（多维）、预算态势、
// 手动暂停开关与模型价格表编辑。
package admin

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/cost"
	"github.com/capyflow/friday/internal/usage"
)

// usageRow 是一个维度桶的展示行。
//
// 金额只有"按模型"维度能准确算出：账本按维度分别累计，某个来源/运行里可能
// 混着已配价与未配价的模型，跨维度的金额无法还原，故只给 token。
type usageRow struct {
	Key        string  `json:"key"`
	Calls      int64   `json:"calls"`
	In         int64   `json:"in"`
	Out        int64   `json:"out"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	ZeroCalls  int64   `json:"zeroCalls"`
	Cost       float64 `json:"cost"`
	Priced     bool    `json:"priced"`
}

// handleUsage 返回 token 用量账本：按天明细 + 总量 + 各维度合并视图。
func (s *Server) handleUsage(ctx *vhttp.Context) error {
	days, total := usage.Summary()
	if days == nil {
		days = []usage.Day{}
	}
	pricer := s.pricer
	rows := func(buckets map[string]*usage.Dim, withCost bool) []usageRow {
		out := make([]usageRow, 0, len(buckets))
		for key, d := range buckets {
			if d == nil {
				continue
			}
			row := usageRow{Key: key, Calls: d.Calls, In: d.In, Out: d.Out,
				CacheRead: d.CacheRead, CacheWrite: d.CacheWrite, ZeroCalls: d.ZeroCalls}
			if withCost && pricer != nil {
				c, ok := pricer.Cost(key, d.In, d.Out, d.CacheRead)
				row.Cost, row.Priced = c, ok
			}
			out = append(out, row)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].In+out[i].Out != out[j].In+out[j].Out {
				return out[i].In+out[i].Out > out[j].In+out[j].Out
			}
			return out[i].Key < out[j].Key
		})
		return out
	}

	merged := map[string]map[string]*usage.Dim{
		"byModel":   mergeDims(days, func(d usage.Day) map[string]*usage.Dim { return d.ByModel }),
		"byOrigin":  mergeDims(days, func(d usage.Day) map[string]*usage.Dim { return d.ByOrigin }),
		"bySession": mergeDims(days, func(d usage.Day) map[string]*usage.Dim { return d.BySession }),
		"byRun":     mergeDims(days, func(d usage.Day) map[string]*usage.Dim { return d.ByRun }),
	}
	payload := map[string]any{
		"days":      days,
		"total":     total,
		"byModel":   rows(merged["byModel"], true),
		"byOrigin":  rows(merged["byOrigin"], false),
		"bySession": rows(merged["bySession"], false),
		"byRun":     rows(merged["byRun"], false),
		"currency":  s.costCurrency(),
	}
	return ctx.JsonResponse(vhttp.Codes.Success, payload)
}

// handleCost 返回预算态势（颜色横幅、被暂停项、恢复时刻提示）。
func (s *Server) handleCost(ctx *vhttp.Context) error {
	if s.guard == nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "成本闸门未装配"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"cost": s.guard.Evaluate()})
}

// handleCostPause 手动暂停主动型工作（心跳/智能任务/目标启动）。
// 只翻内存开关，不写 config.yaml——重启后回到配置决定的默认状态。
func (s *Server) handleCostPause(ctx *vhttp.Context) error {
	return s.setCostPaused(ctx, true)
}

// handleCostResume 解除手动暂停。
func (s *Server) handleCostResume(ctx *vhttp.Context) error {
	return s.setCostPaused(ctx, false)
}

func (s *Server) setCostPaused(ctx *vhttp.Context, on bool) error {
	if s.guard == nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "成本闸门未装配"})
	}
	s.guard.SetManualPaused(on)
	msg := "已恢复主动任务（心跳巡检 / 智能任务 / 目标启动）"
	if on {
		msg = "已暂停主动任务；用户对话与定时提醒不受影响"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message":      msg,
		"manualPaused": on,
	})
}

// handlePricingGet 返回当前价格表与有用量但未配价的模型。
func (s *Server) handlePricingGet(ctx *vhttp.Context) error {
	if s.pricer == nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "价格表未装配"})
	}
	table := s.pricer.Snapshot()
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"currency":      table.Currency,
		"prices":        table.Prices,
		"unknownModels": unpricedModels(s.pricer),
	})
}

// handlePricingSet 校验并保存价格表（原子写 + 热加载，不经配置热更新链路）。
func (s *Server) handlePricingSet(ctx *vhttp.Context) error {
	if s.pricer == nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "价格表未装配"})
	}
	var body struct {
		Currency string       `json:"currency"`
		Prices   []cost.Price `json:"prices"`
	}
	if err := ctx.UnmarshalBody(&body); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	table := cost.Table{Currency: strings.TrimSpace(body.Currency), Prices: body.Prices}
	if err := s.pricer.Save(table); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	saved := s.pricer.Snapshot()
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message":       fmt.Sprintf("价格表已保存并热生效（%d 个模型，币种 %s）", len(saved.Prices), saved.Currency),
		"prices":        saved.Prices,
		"unknownModels": unpricedModels(s.pricer),
	})
}

// registerCostRoutes 挂载成本接口（闸门/价格表未装配时自动跳过）。
func (s *Server) registerCostRoutes(root *vhttp.VortexHttpRouterGroup) {
	if s.guard == nil && s.pricer == nil {
		return
	}
	root.AddRouter([]string{http.MethodGet}, "/api/cost", s.wrap(s.handleCost))
	root.AddRouter([]string{http.MethodPost}, "/api/cost/pause", s.wrap(s.handleCostPause))
	root.AddRouter([]string{http.MethodPost}, "/api/cost/resume", s.wrap(s.handleCostResume))
	if s.pricer != nil {
		root.AddRouter([]string{http.MethodGet}, "/api/cost/pricing", s.wrap(s.handlePricingGet))
		root.AddRouter([]string{http.MethodPost}, "/api/cost/pricing", s.wrap(s.handlePricingSet))
	}
}

// mergeDims 跨天合并某一维度的累计值（Summary 返回的是拷贝，可安全累加）。
func mergeDims(days []usage.Day, pick func(usage.Day) map[string]*usage.Dim) map[string]*usage.Dim {
	out := map[string]*usage.Dim{}
	for _, d := range days {
		for key, v := range pick(d) {
			if v == nil {
				continue
			}
			dst, ok := out[key]
			if !ok {
				dst = &usage.Dim{}
				out[key] = dst
			}
			dst.Calls += v.Calls
			dst.In += v.In
			dst.Out += v.Out
			dst.CacheRead += v.CacheRead
			dst.CacheWrite += v.CacheWrite
			dst.ZeroCalls += v.ZeroCalls
		}
	}
	return out
}

// unpricedModels 列出"账本里有用量、但价格表没覆盖"的模型，供后台提示补价。
func unpricedModels(p *cost.Pricer) []string {
	days, _ := usage.Summary()
	seen := map[string]bool{}
	for _, d := range days {
		for model, dim := range d.ByModel {
			if dim == nil || model == "" || model == usage.UnknownBucket {
				continue
			}
			if _, ok := p.Lookup(model); !ok {
				seen[model] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// costCurrency 返回展示币种。
func (s *Server) costCurrency() string {
	if s.pricer != nil && s.pricer.Configured() {
		return s.pricer.Currency()
	}
	if s.cfg != nil {
		if c := s.cfg.Snapshot().Cost.Currency; c != "" {
			return c
		}
	}
	return "CNY"
}
