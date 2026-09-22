// Package usage 记录 LLM token 用量：按天（本地时区）聚合调用次数与
// 输入/输出 token，同步落盘 JSON（原子替换），重启自动恢复，
// 供管理后台展示用量账本。与 trace 的滚动窗口不同，这里是长期累计。
package usage

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// Day 是一天的用量聚合。
type Day struct {
	Date  string `json:"date"`
	Calls int64  `json:"calls"`
	In    int64  `json:"in"`
	Out   int64  `json:"out"`
}

// Total 返回总量视图（Date/Calls 等字段复用：Date 为 "total"）。
func (d Day) Total() Day {
	return Day{Date: "total", Calls: d.Calls, In: d.In, Out: d.Out}
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

// Init 启用持久化：从 path 加载历史用量（重置内存状态，符合"启动恢复"语义）。
// 文件缺失或损坏时从空开始（不视为错误）；path 为空则保持纯内存。
func Init(p string) {
	mu.Lock()
	path = p
	days = map[string]*Day{}
	mu.Unlock()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var snap snapshot
	if json.Unmarshal(data, &snap) != nil {
		return
	}
	mu.Lock()
	for date, d := range snap.Days {
		if d != nil {
			d.Date = date
			days[date] = d
		}
	}
	mu.Unlock()
}

// Add 累加一次 LLM 调用的 token 用量（零用量不计），并同步落盘。
// 调用频率为 LLM 轮次级（低频），文件小，同步原子写足够。
func Add(in, out int64) {
	if in <= 0 && out <= 0 {
		return
	}
	date := time.Now().Format("2006-01-02")

	mu.Lock()
	d := days[date]
	if d == nil {
		d = &Day{Date: date}
		days[date] = d
	}
	d.Calls++
	d.In += in
	d.Out += out
	data, err := json.Marshal(snapshot{Days: days})
	p := path
	mu.Unlock()
	if err != nil || p == "" {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, p)
	}
}

// Summary 返回按天倒序的用量明细与总量。
func Summary() (list []Day, total Day) {
	mu.Lock()
	defer mu.Unlock()
	for _, d := range days {
		list = append(list, *d)
		total.Calls += d.Calls
		total.In += d.In
		total.Out += d.Out
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Date > list[j].Date })
	total.Date = "total"
	return list, total
}
