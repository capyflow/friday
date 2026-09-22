package usage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAddAggregatesAndPersists 累加 → 落盘 → 重新 Init 恢复。
func TestAddAggregatesAndPersists(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token-usage.json")
	Init(file)

	Add(100, 20)
	Add(50, 10)
	Add(0, 0) // 零用量不计

	list, total := Summary()
	if len(list) != 1 {
		t.Fatalf("应有 1 天记录，实际 %d", len(list))
	}
	if total.Calls != 2 || total.In != 150 || total.Out != 30 {
		t.Fatalf("聚合不符: %+v", total)
	}

	// 落盘是同步的，直接重新 Init 验证恢复。
	Init(file)
	list, total = Summary()
	if len(list) != 1 {
		t.Fatalf("恢复后应有 1 天，实际 %d", len(list))
	}
	if total.Calls != 2 || total.In != 150 || total.Out != 30 {
		t.Fatalf("恢复后聚合不符: %+v", total)
	}
	if list[0].Date == "" || list[0].Date == "total" {
		t.Fatalf("日期字段不符: %q", list[0].Date)
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

// TestInitMissingFile 缺失文件从空开始。
func TestInitMissingFile(t *testing.T) {
	Init(filepath.Join(t.TempDir(), "nope.json"))
	list, total := Summary()
	if len(list) != 0 || total.In != 0 {
		t.Fatalf("缺失文件应从空开始: %+v %v", list, total)
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "nope.json")); !os.IsNotExist(err) {
		t.Log("stat err:", err)
	}
}
