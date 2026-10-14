// Package usage 记录 LLM token 用量：按天（本地时区）聚合调用次数与输入/输出
// token，并按模型/来源/会话/运行四个维度分桶，同步落盘 JSON（原子替换），
// 重启自动恢复，供管理后台展示用量账本与成本预算判定。与 trace 的滚动窗口
// 不同，这里是长期累计。金额不在此计算（价格表见 internal/cost）。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// UnknownBucket 是归属维度缺失时的桶名（老数据与未经入口标注的调用）。
const UnknownBucket = "unknown"

// Call 是一次 LLM 调用的记账输入。归属维度由调用方从 ctx 取好传入——
// 本包不认识 context，保持纯计数器。
type Call struct {
	Model      string // 实际请求的模型名（技能覆盖后的结果）
	Session    string // 会话 key（web / task / plan-xxx / goal-xxx）
	Origin     string // 来源渠道（web / repl / dingtalk / feishu / task / summarizer）
	Run        string // 所属运行："plan:<id>" / "goal:<id>"，普通对话为空
	In         int64
	Out        int64
	CacheRead  int64
	CacheWrite int64
}

// Dim 是某一维度取值下的累计计数。
type Dim struct {
	Calls      int64 `json:"calls"`
	In         int64 `json:"in"`
	Out        int64 `json:"out"`
	CacheRead  int64 `json:"cacheRead,omitempty"`
	CacheWrite int64 `json:"cacheWrite,omitempty"`
	// ZeroCalls 是 provider 未回传 usage 的调用数。它非零通常意味着流式记账
	// 失效（网关不吃 stream_options），是账本可信度的哨兵指标。
	ZeroCalls int64 `json:"zeroCalls,omitempty"`
}

func (d *Dim) add(c Call) {
	d.Calls++
	d.In += c.In
	d.Out += c.Out
	d.CacheRead += c.CacheRead
	d.CacheWrite += c.CacheWrite
	if c.In == 0 && c.Out == 0 {
		d.ZeroCalls++
	}
}

// Day 是一天的用量聚合。顶层 date/calls/in/out 沿用旧字段名，
// 旧版 token-usage.json 无需迁移即可解析；分桶维度缺省为空。
type Day struct {
	Date  string `json:"date"`
	Calls int64  `json:"calls"`
	In    int64  `json:"in"`
	Out   int64  `json:"out"`

	ByModel   map[string]*Dim `json:"byModel,omitempty"`
	ByOrigin  map[string]*Dim `json:"byOrigin,omitempty"`
	BySession map[string]*Dim `json:"bySession,omitempty"`
	ByRun     map[string]*Dim `json:"byRun,omitempty"`
}

// Total 返回总量视图（Date 置为 "total"，分桶不合并）。
func (d Day) Total() Day {
	return Day{Date: "total", Calls: d.Calls, In: d.In, Out: d.Out}
}

func (d *Day) ensureMaps() {
	if d.ByModel == nil {
		d.ByModel = map[string]*Dim{}
	}
	if d.ByOrigin == nil {
		d.ByOrigin = map[string]*Dim{}
	}
	if d.BySession == nil {
		d.BySession = map[string]*Dim{}
	}
	if d.ByRun == nil {
		d.ByRun = map[string]*Dim{}
	}
}

// clone 深拷贝（含分桶），避免调用方改到内存账本。
func (d Day) clone() Day {
	out := d
	out.ByModel = cloneDims(d.ByModel)
	out.ByOrigin = cloneDims(d.ByOrigin)
	out.BySession = cloneDims(d.BySession)
	out.ByRun = cloneDims(d.ByRun)
	return out
}

func cloneDims(src map[string]*Dim) map[string]*Dim {
	if src == nil {
		return nil
	}
	dst := make(map[string]*Dim, len(src))
	for k, v := range src {
		if v != nil {
			c := *v
			dst[k] = &c
		}
	}
	return dst
}

// dimOf 取（或建）某维度桶；空归属统一记作 unknown，避免账本出现空键。
func dimOf(buckets map[string]*Dim, key string) *Dim {
	if key == "" {
		key = UnknownBucket
	}
	d, ok := buckets[key]
	if !ok {
		d = &Dim{}
		buckets[key] = d
	}
	return d
}

var (
	mu   sync.Mutex
	days = map[string]*Day{}
	path string // 为空时不落盘（纯内存）
)

// snapshot 是持久化文件的顶层结构。
type snapshot struct {
	Days map[string]*Day `json:"days"`
}

// Init 启用持久化：从 p 加载历史用量（重置内存状态，符合"启动恢复"语义）。
//
// 文件缺失视为空账本；损坏或不可读时**保留原文件**、退回纯内存模式（不再写回，
// 避免用空数据覆盖既有账本）并返回错误。p 为空表示纯内存（测试或未配路径）。
func Init(p string) error {
	mu.Lock()
	defer mu.Unlock()
	days = map[string]*Day{}
	path = ""
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if _, statErr := os.Lstat(p); os.IsNotExist(statErr) {
			path = p // 新部署：尚无账本，首次记账起落盘
			return nil
		}
		return fmt.Errorf("读取用量文件 %q 失败，已保留原文件并暂停落盘；修复文件后重启: %w", p, err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("用量文件 %q 损坏，已保留原文件并暂停落盘；修复文件后重启: %w", p, err)
	}
	for date, d := range snap.Days {
		if d == nil {
			continue
		}
		d.Date = date
		d.ensureMaps()
		days[date] = d
	}
	path = p
	return nil
}

// Record 累加一次 LLM 调用并同步落盘。
// 调用频率为 LLM 轮次级（低频）、文件小，同步原子写足够。
func Record(c Call) {
	date := time.Now().Format("2006-01-02")

	mu.Lock()
	d := days[date]
	if d == nil {
		d = &Day{Date: date}
		days[date] = d
	}
	d.ensureMaps()
	dimOf(d.ByModel, c.Model).add(c)
	dimOf(d.ByOrigin, c.Origin).add(c)
	dimOf(d.BySession, c.Session).add(c)
	// 普通对话没有运行归属，不建桶——byRun 只用于计划/目标这类多步运行。
	if c.Run != "" {
		dimOf(d.ByRun, c.Run).add(c)
	}
	d.Calls++
	d.In += c.In
	d.Out += c.Out
	data, err := json.Marshal(snapshot{Days: days})
	p := path
	mu.Unlock()

	if err != nil || p == "" {
		return
	}
	if err := save(p, data); err != nil {
		// 落盘失败不影响内存账本，但必须可见：账本关系到成本判定。
		log.Printf("[usage] 用量落盘失败（内存账本仍准确）: %v", err)
	}
}

// save 以"临时文件 + rename"原子替换落盘，掉电不留半个文件。
func save(p string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建用量临时文件失败: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("写入用量临时文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("同步用量临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭用量临时文件失败: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		return fmt.Errorf("设置用量文件权限失败: %w", err)
	}
	if err := os.Rename(f.Name(), p); err != nil {
		return fmt.Errorf("替换用量文件失败: %w", err)
	}
	return nil
}

// Summary 返回按天倒序的用量明细与总量（均为拷贝）。
func Summary() (list []Day, total Day) {
	mu.Lock()
	defer mu.Unlock()
	for _, d := range days {
		list = append(list, d.clone())
		total.Calls += d.Calls
		total.In += d.In
		total.Out += d.Out
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Date > list[j].Date })
	total.Date = "total"
	return list, total
}
