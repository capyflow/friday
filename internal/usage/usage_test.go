package usage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRecordAggregatesAndPersists 记录 → 分桶 → 落盘 → 重新 Init 恢复。
func TestRecordAggregatesAndPersists(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token-usage.json")
	if err := Init(file); err != nil {
		t.Fatalf("Init: %v", err)
	}

	Record(Call{Model: "deepseek-chat", Session: "web", Origin: "web", In: 100, Out: 20})
	Record(Call{Model: "qwen-max", Session: "task", Origin: "task", Run: "goal:g1", In: 50, Out: 10})

	list, total := Summary()
	if len(list) != 1 {
		t.Fatalf("应有 1 天记录，实际 %d", len(list))
	}
	d := list[0]
	if total.Calls != 2 || total.In != 150 || total.Out != 30 {
		t.Fatalf("总量不符: %+v", total)
	}
	if d.ByModel["deepseek-chat"] == nil || d.ByModel["qwen-max"].In != 50 {
		t.Errorf("按模型分桶不符: %+v", d.ByModel)
	}
	if d.ByOrigin["web"] == nil || d.ByOrigin["web"].Calls != 1 {
		t.Errorf("按来源分桶不符: %+v", d.ByOrigin)
	}
	if d.BySession["task"] == nil || d.BySession["task"].Out != 10 {
		t.Errorf("按会话分桶不符: %+v", d.BySession)
	}
	if d.ByRun["goal:g1"] == nil {
		t.Errorf("按运行分桶不符: %+v", d.ByRun)
	}

	// 落盘是同步的，直接重新 Init 验证恢复。
	if err := Init(file); err != nil {
		t.Fatalf("重新 Init: %v", err)
	}
	list, total = Summary()
	if len(list) != 1 || total.Calls != 2 || total.In != 150 || total.Out != 30 {
		t.Fatalf("恢复后聚合不符: list=%d total=%+v", len(list), total)
	}
	if list[0].ByRun["goal:g1"] == nil {
		t.Errorf("恢复后运行分桶丢失: %+v", list[0].ByRun)
	}
	if list[0].Date == "" || list[0].Date == "total" {
		t.Fatalf("日期字段不符: %q", list[0].Date)
	}
}

// TestRecordCacheTokensCounted 缓存命中 token 单独累计（计价只认缓存读，见 internal/cost）。
func TestRecordCacheTokensCounted(t *testing.T) {
	Init("")
	Record(Call{Model: "m", Origin: "web", In: 1000, Out: 50, CacheRead: 800, CacheWrite: 200})
	list, _ := Summary()
	d := list[0].ByModel["m"]
	if d.CacheRead != 800 || d.CacheWrite != 200 {
		t.Fatalf("缓存 token 未累计: %+v", d)
	}
}

// TestRecordZeroUsageCountedAsSentinel provider 没回传 usage 时仍计调用数，
// 并把 ZeroCalls 顶起来——这是流式记账失效的哨兵，不能静默丢弃。
func TestRecordZeroUsageCountedAsSentinel(t *testing.T) {
	Init("")
	Record(Call{Model: "m", Origin: "web"})
	Record(Call{Model: "m", Origin: "web", In: 10, Out: 1})

	list, total := Summary()
	d := list[0]
	if total.Calls != 2 {
		t.Fatalf("零用量也应计调用数，实际 Calls=%d", total.Calls)
	}
	if d.ByModel["m"].ZeroCalls != 1 {
		t.Fatalf("ZeroCalls 应为 1，实际 %+v", d.ByModel["m"])
	}
}

// TestRecordMissingAttributionGoesUnknown 归属缺失统一进 unknown 桶；
// 普通对话无运行归属，不建 byRun 空键。
func TestRecordMissingAttributionGoesUnknown(t *testing.T) {
	Init("")
	Record(Call{In: 5, Out: 5})

	list, _ := Summary()
	d := list[0]
	if d.ByModel[UnknownBucket] == nil || d.ByOrigin[UnknownBucket] == nil || d.BySession[UnknownBucket] == nil {
		t.Fatalf("缺失归属应记作 unknown: %+v", d)
	}
	if _, ok := d.ByRun[""]; ok {
		t.Error("空运行不应建桶")
	}
	if len(d.ByRun) != 0 {
		t.Errorf("无运行归属时 byRun 应为空: %+v", d.ByRun)
	}
}

// TestSummaryReturnsCopies 调用方改返回值不得污染内存账本。
func TestSummaryReturnsCopies(t *testing.T) {
	Init("")
	Record(Call{Model: "m", Origin: "web", In: 10, Out: 1})

	list, _ := Summary()
	list[0].ByModel["m"].In = 99999
	list[0].ByModel["hacked"] = &Dim{Calls: 1}

	again, _ := Summary()
	if again[0].ByModel["m"].In != 10 {
		t.Errorf("账本被外部改写: In=%d", again[0].ByModel["m"].In)
	}
	if _, ok := again[0].ByModel["hacked"]; ok {
		t.Error("账本被外部追加了桶")
	}
}

// TestInitPreservesCorruptFile 损坏文件保留原内容并禁用落盘——
// 绝不用空账本覆盖既有数据。
func TestInitPreservesCorruptFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token-usage.json")
	original := []byte("{ this is not json")
	if err := os.WriteFile(file, original, 0o600); err != nil {
		t.Fatal(err)
	}

	err := Init(file)
	if err == nil {
		t.Fatal("损坏文件应返回错误")
	}
	Record(Call{Model: "m", In: 1, Out: 1})

	got, readErr := os.ReadFile(file)
	if readErr != nil {
		t.Fatalf("读回文件: %v", readErr)
	}
	if string(got) != string(original) {
		t.Errorf("原文件被覆盖: %q", got)
	}
	// 内存账本仍可用（本次运行不落盘，但不影响展示与判定）。
	if _, total := Summary(); total.Calls != 1 {
		t.Errorf("内存账本应仍累计: %+v", total)
	}
}

// TestInitReadsLegacyFormat 旧版只有 calls/in/out 的账本无需迁移即可解析。
func TestInitReadsLegacyFormat(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token-usage.json")
	legacy := `{"days":{"2026-10-01":{"date":"2026-10-01","calls":3,"in":300,"out":60}}}`
	if err := os.WriteFile(file, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Init(file); err != nil {
		t.Fatalf("Init 旧格式: %v", err)
	}
	list, total := Summary()
	if len(list) != 1 || total.Calls != 3 || total.In != 300 {
		t.Fatalf("旧格式解析不符: list=%+v total=%+v", list, total)
	}
	// 旧记录没有分桶数据，补记新调用时不得因 nil map 崩溃。
	Record(Call{Model: "m", Origin: "web", In: 1, Out: 1})
	list, _ = Summary()
	if len(list) != 2 {
		t.Fatalf("应新增当天记录，实际 %d 天", len(list))
	}
}

// TestSummarySortedNewestFirst 多天记录按日期倒序。
func TestSummarySortedNewestFirst(t *testing.T) {
	Init("")
	for _, d := range []string{"2026-09-20", "2026-09-22", "2026-09-21"} {
		mu.Lock()
		days[d] = &Day{Date: d, Calls: 1, In: 1, Out: 1}
		mu.Unlock()
	}
	list, _ := Summary()
	if len(list) != 3 || list[0].Date != "2026-09-22" || list[2].Date != "2026-09-20" {
		t.Fatalf("应按日期倒序: %+v", list)
	}
	_, total := Summary()
	if total.In != 3 {
		t.Fatalf("总计不符: %+v", total)
	}
}

// TestInitMissingFile 缺失文件从空开始，且首次记账即建账本。
func TestInitMissingFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nope.json")
	if err := Init(file); err != nil {
		t.Fatalf("缺失文件不应报错: %v", err)
	}
	list, total := Summary()
	if len(list) != 0 || total.In != 0 {
		t.Fatalf("缺失文件应从空开始: %+v %v", list, total)
	}
	Record(Call{Model: "m", In: 2, Out: 2})
	if _, err := os.Stat(file); err != nil {
		t.Errorf("首次记账应落盘: %v", err)
	}
}
