// Package cost 把 token 账本换算成金额并做预算判定：价格表（pricing.go）
// 与软限额闸门（guard.go）。
//
// 设计边界：本包只做"观测 + 软限"——超预算时停止主动型工作（心跳/智能任务/
// 目标启动），绝不取消进行中的用户对话轮。硬取消需要框架侧的事前 LLM 钩子
// （vortexagent 目前只有事后 OnLLMCall），本轮按决策不做。
package cost

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Price 是一个模型的单价（每百万 token 计，币种由表内 currency 决定）。
type Price struct {
	Model            string  `json:"model"`
	InputPerMTok     float64 `json:"inputPerMTok"`
	OutputPerMTok    float64 `json:"outputPerMTok"`
	CacheReadPerMTok float64 `json:"cacheReadPerMTok,omitempty"`
}

// Table 是价格表文件（data/pricing.json）的顶层结构。
//
// 单独成文件而不进 config.yaml：后台编辑频繁，且热加载只需重读一个小 JSON，
// 不必占用配置热更新链路（config.Apply 的发布白名单）。
type Table struct {
	Currency string  `json:"currency"`
	Prices   []Price `json:"prices"`
}

const millionTokens = 1e6

// Pricer 持有价格表并提供换算。零值不可用，请用 NewPricer。
type Pricer struct {
	mu    sync.RWMutex
	path  string
	table Table
	index map[string]Price
}

// NewPricer 创建价格表读取器。文件不存在视为"未配价"（可用但不计价）；
// 内容损坏时保留原文件、记为未配价并返回错误供启动日志提示。
func NewPricer(path string) (*Pricer, error) {
	p := &Pricer{path: path, table: Table{Currency: "CNY"}, index: map[string]Price{}}
	if path == "" {
		return p, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return p, fmt.Errorf("读取价格表 %q 失败，已保留原文件并按未配价处理: %w", path, err)
	}
	if err := p.parse(data); err != nil {
		return p, fmt.Errorf("价格表 %q 损坏，已保留原文件并按未配价处理: %w", path, err)
	}
	return p, nil
}

// Reload 重新读取价格表（Save 之后或手改文件后调用）。
func (p *Pricer) Reload() error {
	p.mu.RLock()
	path := p.path
	p.mu.RUnlock()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("重载价格表失败: %w", err)
	}
	return p.parse(data)
}

func (p *Pricer) parse(data []byte) error {
	t, index, err := validateTable(parseBytes(data))
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.table, p.index = t, index
	p.mu.Unlock()
	return nil
}

// parseBytes 只做 JSON 反序列化，校验交给 validateTable。
func parseBytes(data []byte) (Table, error) {
	var t Table
	err := json.Unmarshal(data, &t)
	return t, err
}

// validateTable 校验价格表：币种兜底、模型名非空不重复、单价非负。
func validateTable(t Table, err error) (Table, map[string]Price, error) {
	if err != nil {
		return Table{}, nil, err
	}
	if t.Currency == "" {
		t.Currency = "CNY"
	}
	index := make(map[string]Price, len(t.Prices))
	for i, pr := range t.Prices {
		name := strings.TrimSpace(pr.Model)
		if name == "" {
			return Table{}, nil, fmt.Errorf("第 %d 条价格缺少 model", i+1)
		}
		if _, dup := index[name]; dup {
			return Table{}, nil, fmt.Errorf("模型 %q 重复配价", name)
		}
		if pr.InputPerMTok < 0 || pr.OutputPerMTok < 0 || pr.CacheReadPerMTok < 0 {
			return Table{}, nil, fmt.Errorf("模型 %q 的单价不能为负", name)
		}
		pr.Model = name
		index[name] = pr
	}
	return t, index, nil
}

// Save 原子写回价格表并热加载。
func (p *Pricer) Save(t Table) error {
	if _, _, err := validateTable(t, nil); err != nil {
		return fmt.Errorf("价格表无效: %w", err)
	}
	p.mu.RLock()
	path := p.path
	p.mu.RUnlock()
	if path == "" {
		return fmt.Errorf("未配置价格表路径（cost.pricingFile），无法保存")
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建价格表临时文件失败: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("写入价格表临时文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("同步价格表临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭价格表临时文件失败: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		return fmt.Errorf("设置价格表权限失败: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("替换价格表文件失败: %w", err)
	}
	return p.Reload()
}

// Lookup 返回某模型的单价。
func (p *Pricer) Lookup(model string) (Price, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pr, ok := p.index[strings.TrimSpace(model)]
	return pr, ok
}

// Currency 返回价格表币种。
func (p *Pricer) Currency() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.table.Currency == "" {
		return "CNY"
	}
	return p.table.Currency
}

// Snapshot 返回当前价格表（后台编辑用）。
func (p *Pricer) Snapshot() Table {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := Table{Currency: p.table.Currency, Prices: make([]Price, len(p.table.Prices))}
	copy(out.Prices, p.table.Prices)
	return out
}

// Configured 表示价格表里是否已有配价条目。没有时币种以 cost.currency 配置为准。
func (p *Pricer) Configured() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.index) > 0
}

// Cost 计算一组 token 的金额；模型未配价时返回 ok=false（不计入合计，
// 由调用方单列"价格未配置"提示，避免给出看似准确实则漏算的成本）。
//
// 计费的输入 token 按"非缓存 + 缓存读"分档：厂商返回的 prompt_tokens 含缓存
// 命中部分，故先扣掉 cacheRead 再按原价计。缓存写不单独计价（框架把
// prompt_cache_miss_tokens 当 CacheWrite 上报，语义各家不一致，只展示不计费）。
func (p *Pricer) Cost(model string, in, out, cacheRead int64) (float64, bool) {
	pr, ok := p.Lookup(model)
	if !ok {
		return 0, false
	}
	if cacheRead > in {
		cacheRead = in
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	cost := float64(in-cacheRead) / millionTokens * pr.InputPerMTok
	cost += float64(cacheRead) / millionTokens * pr.CacheReadPerMTok
	cost += float64(out) / millionTokens * pr.OutputPerMTok
	return cost, true
}

// Models 返回已配价的模型名（后台展示用）。
func (p *Pricer) Models() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	names := make([]string, 0, len(p.index))
	for name := range p.index {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
