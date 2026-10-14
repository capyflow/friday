package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/cost"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/usage"
)

// costTestServer 装配一个带价格表与预算闸门的后台（日额度 1000 token，预警线 0.8）。
func costTestServer(t *testing.T) (*Server, http.Handler, *http.Cookie) {
	t.Helper()
	s, _, cookie, _, _ := manageTestServer(t)

	pricer, err := cost.NewPricer(filepath.Join(t.TempDir(), "pricing.json"))
	if err != nil {
		t.Fatalf("NewPricer: %v", err)
	}
	table := cost.Table{Currency: "CNY", Prices: []cost.Price{
		{Model: "priced-model", InputPerMTok: 1, OutputPerMTok: 2, CacheReadPerMTok: 0.5},
	}}
	if err := pricer.Save(table); err != nil {
		t.Fatalf("预置价格表: %v", err)
	}

	cfg := &config.Config{}
	cfg.Cost.Currency = "CNY"
	cfg.Cost.Budget.SoftRatio = 0.8
	cfg.Cost.Budget.DailyTokens = 1000

	s.pricer = pricer
	s.guard = cost.NewGuard(cfg, pricer)
	// /api/status 会读记忆条数，测试服需自备一份空仓库。
	s.memory = memory.NewWikiStore(filepath.Join(t.TempDir(), "memories"), "")
	// 路由在 Handler() 构建时按已装配的依赖挂载，故装配完成后才取 handler。
	return s, s.Handler(), cookie
}

// costBody 解出 { body: ... } 信封内容。
func costBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body)
	}
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析响应 %s: %v", rec.Body, err)
	}
	if envelope.Body == nil {
		t.Fatalf("响应缺少 body: %s", rec.Body)
	}
	return envelope.Body
}

func costRows(t *testing.T, body map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("%s 不是数组: %v", key, body[key])
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s 含非对象项: %v", key, item)
		}
		rows = append(rows, m)
	}
	return rows
}

func findRow(rows []map[string]any, key string) map[string]any {
	for _, r := range rows {
		if r["key"] == key {
			return r
		}
	}
	return nil
}

func getCost(handler http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	return setupTestRequest(handler, "GET", path, nil, "127.0.0.1:1234", "localhost", "", cookie)
}

// TestUsageHandlerGroupsByDimensions 用量接口给出多维拆分，且保留旧 days/total 字段。
func TestUsageHandlerGroupsByDimensions(t *testing.T) {
	_, handler, cookie := costTestServer(t)
	usage.Init("")
	usage.Record(usage.Call{Model: "priced-model", Origin: "web", Session: "web", In: 600, Out: 100, CacheRead: 200})
	usage.Record(usage.Call{Model: "mystery", Origin: "task", Session: "task", Run: "goal:g1", In: 200, Out: 20})
	usage.Record(usage.Call{Model: "priced-model", Origin: "web", Session: "web"}) // 零用量哨兵

	body := costBody(t, getCost(handler, cookie, "/api/usage"))
	if body["days"] == nil || body["total"] == nil {
		t.Fatalf("旧字段缺失（前端按天表依赖）: %v", body)
	}
	total := body["total"].(map[string]any)
	if total["calls"] != float64(3) {
		t.Errorf("总调用数 = %v，期望 3", total["calls"])
	}

	models := costRows(t, body, "byModel")
	priced := findRow(models, "priced-model")
	if priced == nil {
		t.Fatalf("byModel 缺 priced-model: %v", models)
	}
	if priced["priced"] != true {
		t.Errorf("已配价模型应标 priced=true: %v", priced)
	}
	// 600 输入（其中 200 命中缓存）+ 100 输出
	// = 400/1e6*1 + 200/1e6*0.5 + 100/1e6*2 = 0.0004 + 0.0001 + 0.0002
	costVal, _ := priced["cost"].(float64)
	if costVal < 0.00069 || costVal > 0.00071 {
		t.Errorf("金额 = %v，期望约 0.0007", costVal)
	}
	if priced["zeroCalls"] != float64(1) {
		t.Errorf("零用量哨兵未上报: %v", priced)
	}
	unpriced := findRow(models, "mystery")
	if unpriced == nil || unpriced["priced"] != false {
		t.Errorf("未配价模型应标 priced=false: %v", unpriced)
	}

	if findRow(costRows(t, body, "byOrigin"), "task") == nil {
		t.Error("byOrigin 缺 task")
	}
	if findRow(costRows(t, body, "byRun"), "goal:g1") == nil {
		t.Error("byRun 缺 goal:g1")
	}
	if findRow(costRows(t, body, "bySession"), "web") == nil {
		t.Error("bySession 缺 web")
	}
	if body["currency"] != "CNY" {
		t.Errorf("币种 = %v", body["currency"])
	}
}

// TestCostHandlerTracksBudget 预算态势接口随用量推进 ok → warn → over。
func TestCostHandlerTracksBudget(t *testing.T) {
	_, handler, cookie := costTestServer(t)
	usage.Init("")

	check := func(want string) {
		t.Helper()
		v := costBody(t, getCost(handler, cookie, "/api/cost"))["cost"].(map[string]any)
		if v["state"] != want {
			t.Fatalf("state = %v，期望 %s（完整视图 %v）", v["state"], want, v)
		}
	}

	usage.Record(usage.Call{Model: "priced-model", In: 100})
	check("ok")
	usage.Record(usage.Call{Model: "priced-model", In: 800}) // 900/1000
	check("warn")
	usage.Record(usage.Call{Model: "priced-model", In: 200}) // 1100/1000
	v := costBody(t, getCost(handler, cookie, "/api/cost"))["cost"].(map[string]any)
	if v["state"] != "over" || v["tightest"] != "dailyTokens" {
		t.Fatalf("超限视图不符: %v", v)
	}
	if v["proactivePaused"] != true {
		t.Error("over 时应标记主动任务被暂停")
	}
}

// TestCostPauseResumeHandlers 手动暂停开关（不写配置，只影响闸门）。
func TestCostPauseResumeHandlers(t *testing.T) {
	s, handler, cookie := costTestServer(t)
	usage.Init("")
	usage.Record(usage.Call{Model: "priced-model", In: 10}) // 远低于预算

	r := setupTestRequest(handler, "POST", "/api/cost/pause", []byte(`{}`), "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code != http.StatusOK {
		t.Fatalf("暂停失败: %d %s", r.Code, r.Body)
	}
	view := costBody(t, getCost(handler, cookie, "/api/cost"))["cost"].(map[string]any)
	if view["manualPaused"] != true || view["proactivePaused"] != true {
		t.Errorf("暂停状态不符: %v", view)
	}
	if ok, _ := s.guard.ProactiveAllowed("心跳巡检"); ok {
		t.Error("手动暂停时闸门应拦下主动任务")
	}

	r = setupTestRequest(handler, "POST", "/api/cost/resume", []byte(`{}`), "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code != http.StatusOK {
		t.Fatalf("恢复失败: %d %s", r.Code, r.Body)
	}
	view = costBody(t, getCost(handler, cookie, "/api/cost"))["cost"].(map[string]any)
	if view["manualPaused"] != false {
		t.Errorf("恢复后仍标记暂停: %v", view)
	}
	if ok, _ := s.guard.ProactiveAllowed("心跳巡检"); !ok {
		t.Error("恢复后闸门应放行")
	}
}

// TestPricingHandlersApplyAndReject 价格表保存后热生效；非法表被拒且不覆盖原表。
func TestPricingHandlersApplyAndReject(t *testing.T) {
	_, handler, cookie := costTestServer(t)
	usage.Init("")
	usage.Record(usage.Call{Model: "mystery", Origin: "web", In: 10, Out: 1})

	bad, _ := json.Marshal(map[string]any{"currency": "CNY", "prices": []map[string]any{
		{"model": "mystery", "inputPerMTok": -1},
	}})
	r := setupTestRequest(handler, "POST", "/api/cost/pricing", bad, "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code == http.StatusOK {
		t.Fatalf("负单价应被拒绝，响应: %s", r.Body)
	}

	good, _ := json.Marshal(map[string]any{"currency": "CNY", "prices": []map[string]any{
		{"model": "mystery", "inputPerMTok": 3, "outputPerMTok": 6},
	}})
	r = setupTestRequest(handler, "POST", "/api/cost/pricing", good, "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code != http.StatusOK {
		t.Fatalf("保存价格表失败: %d %s", r.Code, r.Body)
	}

	body := costBody(t, getCost(handler, cookie, "/api/cost/pricing"))
	if len(body["prices"].([]any)) != 1 {
		t.Fatalf("价格表未热生效: %v", body)
	}
	// 补价后不再出现在"未配价"清单里
	if unknown, ok := body["unknownModels"].([]any); ok && len(unknown) != 0 {
		t.Errorf("已配价模型仍被列为缺价: %v", unknown)
	}
}

// TestStatusHandlerIncludesCostSummary 运行状态页依赖的 cost 摘要。
func TestStatusHandlerIncludesCostSummary(t *testing.T) {
	_, handler, cookie := costTestServer(t)
	usage.Init("")
	usage.Record(usage.Call{Model: "priced-model", Origin: "web", In: 50, Out: 5})

	body := costBody(t, getCost(handler, cookie, "/api/status"))
	costView, ok := body["cost"].(map[string]any)
	if !ok {
		t.Fatalf("status 缺 cost 摘要: %v", body)
	}
	if costView["state"] != "ok" || costView["todayTokens"] != float64(55) {
		t.Errorf("cost 摘要不符: %v", costView)
	}
}

// TestCostRoutesAbsentWithoutGuard 未装配闸门/价格表时不挂载成本接口
// （老部署零变化），但用量接口仍在。
func TestCostRoutesAbsentWithoutGuard(t *testing.T) {
	s, handler, cookie, _, _ := manageTestServer(t)
	s.pricer, s.guard = nil, nil

	r := getCost(handler, cookie, "/api/cost")
	if r.Code == http.StatusOK {
		t.Fatal("未装配闸门时 /api/cost 不应可用")
	}
	if r2 := getCost(handler, cookie, "/api/usage"); r2.Code != http.StatusOK {
		t.Errorf("用量接口应始终可用: %d", r2.Code)
	}
}
