package cost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleTable = `{
  "currency": "CNY",
  "prices": [
    {"model": "deepseek-chat", "inputPerMTok": 2, "outputPerMTok": 8, "cacheReadPerMTok": 0.5}
  ]
}`

func newFilePricer(t *testing.T, content string) (*Pricer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewPricer(path)
	if err != nil {
		t.Fatalf("NewPricer: %v", err)
	}
	return p, path
}

// TestPricerLookupAndCurrency 正常表可查价。
func TestPricerLookupAndCurrency(t *testing.T) {
	p, _ := newFilePricer(t, sampleTable)
	pr, ok := p.Lookup("deepseek-chat")
	if !ok || pr.InputPerMTok != 2 || pr.CacheReadPerMTok != 0.5 {
		t.Fatalf("查价不符: %+v ok=%v", pr, ok)
	}
	if _, ok := p.Lookup("gpt-4o"); ok {
		t.Error("未配价模型不应命中")
	}
	if p.Currency() != "CNY" || !p.Configured() {
		t.Errorf("币种/已配价状态不符: %q configured=%v", p.Currency(), p.Configured())
	}
}

// TestPricerMissingFileIsUsable 新部署没有价格表：可用、不计价、不报错。
func TestPricerMissingFileIsUsable(t *testing.T) {
	p, _ := newFilePricer(t, "")
	if p.Configured() {
		t.Error("空表不应视为已配价")
	}
	if _, ok := p.Cost("any", 100, 10, 0); ok {
		t.Error("无价格表时不应给出金额")
	}
}

// TestPricerCorruptFilePreserved 损坏文件保留原内容、按未配价处理并返回错误——
// 与任务/待办账本同一套"损坏不覆盖"约定。
func TestPricerCorruptFilePreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.json")
	original := "{ not json"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := NewPricer(path)
	if err == nil {
		t.Fatal("损坏价格表应返回错误")
	}
	if p.Configured() {
		t.Error("损坏表应视为未配价")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != original {
		t.Errorf("原文件应被保留，实际: %q", data)
	}
}

// TestPricerCostSplitsCacheRead 输入按"非缓存原价 + 缓存读价"分档，输出单独计。
func TestPricerCostSplitsCacheRead(t *testing.T) {
	p, _ := newFilePricer(t, sampleTable)
	// 1000 输入（其中 400 命中缓存）+ 100 输出
	// = 600/1e6*2 + 400/1e6*0.5 + 100/1e6*8 = 0.0012 + 0.0002 + 0.0008 = 0.0022
	cost, ok := p.Cost("deepseek-chat", 1000, 100, 400)
	if !ok {
		t.Fatal("应命中价格")
	}
	if diff := cost - 0.0022; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("金额 = %v，期望 0.0022", cost)
	}
}

// TestPricerCostClampsCacheRead 异常数据（缓存读大于总输入）不得算出负金额。
func TestPricerCostClampsCacheRead(t *testing.T) {
	p, _ := newFilePricer(t, sampleTable)
	cost, ok := p.Cost("deepseek-chat", 100, 10, 9999)
	if !ok {
		t.Fatal("应命中价格")
	}
	if cost < 0 {
		t.Errorf("金额不应为负: %v", cost)
	}
}

// TestPricerSaveRejectsInvalidTable 非法表不落盘、不改变现有索引。
func TestPricerSaveRejectsInvalidTable(t *testing.T) {
	p, path := newFilePricer(t, sampleTable)

	err := p.Save(Table{Prices: []Price{{Model: "m", InputPerMTok: -1}}})
	if err == nil {
		t.Fatal("负单价应被拒绝")
	}
	err = p.Save(Table{Prices: []Price{{Model: "a", InputPerMTok: 1}, {Model: "a", InputPerMTok: 2}}})
	if err == nil {
		t.Fatal("重复模型应被拒绝")
	}
	err = p.Save(Table{Prices: []Price{{Model: "  ", InputPerMTok: 1}}})
	if err == nil {
		t.Fatal("空模型名应被拒绝")
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "deepseek-chat") {
		t.Errorf("非法保存不应覆盖原表: %q", data)
	}
	if _, ok := p.Lookup("deepseek-chat"); !ok {
		t.Error("校验失败后索引应保持原样")
	}
}

// TestPricerSaveAndReload 保存后热生效，且目录不留临时文件。
func TestPricerSaveAndReload(t *testing.T) {
	p, path := newFilePricer(t, sampleTable)
	if err := p.Save(Table{Currency: "USD", Prices: []Price{{Model: "gpt-4o", InputPerMTok: 2.5, OutputPerMTok: 10}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, ok := p.Lookup("gpt-4o"); !ok {
		t.Error("保存后应可查到新模型")
	}
	if _, ok := p.Lookup("deepseek-chat"); ok {
		t.Error("整表替换后旧模型应消失")
	}
	if p.Currency() != "USD" {
		t.Errorf("币种应随表更新，实际 %q", p.Currency())
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}
}

// TestPricerReloadPicksUpManualEdit 手改文件后 Reload 生效（后台与手工编辑共用一份真相）。
func TestPricerReloadPicksUpManualEdit(t *testing.T) {
	p, path := newFilePricer(t, sampleTable)
	manual := `{"currency":"CNY","prices":[{"model":"qwen-max","inputPerMTok":1,"outputPerMTok":2}]}`
	if err := os.WriteFile(path, []byte(manual), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := p.Lookup("qwen-max"); !ok {
		t.Error("手改内容应被加载")
	}
}
