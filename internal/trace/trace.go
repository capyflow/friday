// Package trace 保存 agent 的调用轨迹：每次用户输入或任务触发分配一个请求 ID，
// 沿途的 LLM 调用（轮次/token/耗时）、工具调用与结果、最终回答、错误按请求归档。
// 数据存内存并防抖落盘到 JSON 文件——进程重启后从文件恢复。
// 生命周期跟随对话历史：不自动淘汰，用户清除对话上下文时一并清除（ClearSession）。
package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/logx"
)

const (
	// maxEntries 单个请求的轨迹条目上限（防御工具循环等异常场景）。
	maxEntries = 500
	// maxText 单条轨迹文本的最大长度（rune）。
	maxText = 2000
	// flushDelay 落盘防抖：脏标记后的等待窗口，合并高频写入。
	flushDelay = 400 * time.Millisecond
)

// Entry 是轨迹中的一条事件。In/Out/Ms 仅 LLM 相关事件使用：
// 本轮发给模型的输入 token、模型输出的 token、调用耗时（毫秒）。
type Entry struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // 事件类型，如 llm_in / tool / answer / skill
	Text string    `json:"text"`
	In   int64     `json:"in,omitempty"`
	Out  int64     `json:"out,omitempty"`
	Ms   int64     `json:"ms,omitempty"`
}

// Request 是一次完整调用的轨迹。In/Out 为该次请求所有 LLM 轮次的 token 累计。
type Request struct {
	ID      string    `json:"id"`
	Source  string    `json:"source"`            // web / repl / task / dingtalk / feishu ...
	Session string    `json:"session,omitempty"` // 会话 key（多会话区分归属）
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitempty"`
	Input   string    `json:"input"`
	Answer  string    `json:"answer,omitempty"`
	Error   string    `json:"error,omitempty"`
	In      int64     `json:"in,omitempty"`
	Out     int64     `json:"out,omitempty"`
	Entries []Entry   `json:"entries,omitempty"`

	// pend* 是 llm_in 记录后暂存的当轮用量，由紧随其后的 llm_out 带走。
	pendIn, pendOut, pendMs int64
}

var (
	mu    sync.Mutex
	reqs  = map[string]*Request{}
	order []string // 插入顺序（旧→新），用于淘汰与倒序列出
	// llmInCap：llm_in / llm_out 事件文本（完整上下文快照）的截断上限，
	// 比普通事件宽松——这是排查 agent 决策链的核心数据。
	llmTextCap = 6000

	// 持久化状态：path 非空时启用，脏标记 + notify 触发后台防抖写盘。
	path      string
	dirty     bool
	notify    chan struct{}
	flushOnce sync.Once
)

// snapshot 是持久化文件的顶层结构。
type snapshot struct {
	Requests []*Request `json:"requests"`
}

// Init 启用持久化：从 path 加载历史轨迹并启动后台写盘协程。
// 文件不存在或损坏时从空开始（不视为错误）。path 为空则保持纯内存。
func Init(p string) {
	mu.Lock()
	loaded := map[string]*Request{}
	var loadedOrder []string
	path = p
	if data, err := os.ReadFile(p); p != "" && err == nil {
		var snap snapshot
		if json.Unmarshal(data, &snap) == nil {
			for _, r := range snap.Requests {
				if r == nil || r.ID == "" {
					continue
				}
				loaded[r.ID] = r
				loadedOrder = append(loadedOrder, r.ID)
			}
		}
	}
	reqs = loaded
	order = loadedOrder
	dirty = false
	mu.Unlock()
	flushOnce.Do(func() {
		notify = make(chan struct{}, 1)
		go flusher()
	})
}

// markDirtyLocked 标记有未落盘的变更并唤醒写盘协程（调用方须持有 mu）。
func markDirtyLocked() {
	if path == "" {
		return
	}
	dirty = true
	select {
	case notify <- struct{}{}:
	default:
	}
}

// flusher 后台协程：防抖合并高频写入，全量快照到临时文件后原子替换。
func flusher() {
	for range notify {
		time.Sleep(flushDelay)
		mu.Lock()
		if !dirty {
			mu.Unlock()
			continue
		}
		dirty = false
		snap := snapshot{Requests: make([]*Request, 0, len(order))}
		for _, id := range order {
			if r := reqs[id]; r != nil {
				snap.Requests = append(snap.Requests, r)
			}
		}
		p := path
		mu.Unlock()

		if data, err := json.Marshal(snap); err == nil {
			tmp := p + ".tmp"
			if os.WriteFile(tmp, data, 0o644) == nil {
				os.Rename(tmp, p)
			}
		}
	}
}

// recordWithCap 按 cap 截断并追加事件（调用方须持有 mu）。
func recordWithCap(r *Request, kind, text string, in, out, ms int64) {
	if len(r.Entries) >= maxEntries {
		return
	}
	r.Entries = append(r.Entries, Entry{Time: time.Now(), Kind: kind, Text: text, In: in, Out: out, Ms: ms})
}

// Begin 登记一次调用的开始（来源 + 会话 key + 原始输入）。
func Begin(id, source, session, input string) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := reqs[id]; ok {
		return
	}
	reqs[id] = &Request{
		ID:      id,
		Source:  source,
		Session: session,
		Started: time.Now(),
		Input:   logx.Truncate(input, maxText),
	}
	order = append(order, id)
	markDirtyLocked()
}

// Record 追加一条轨迹事件；ctx 携带请求 ID（logx.WithID）时生效。
// kind 为 answer / error 时同步填充 Request 的对应字段（留空不覆盖）。
func Record(ctx context.Context, kind, format string, args ...any) {
	id := logx.IDFrom(ctx)
	if id == "" {
		return
	}
	text := logx.Truncate(fmt.Sprintf(format, args...), maxText)
	mu.Lock()
	defer mu.Unlock()
	r, ok := reqs[id]
	if !ok {
		return
	}
	if len(r.Entries) >= maxEntries {
		return
	}
	r.Entries = append(r.Entries, Entry{Time: time.Now(), Kind: kind, Text: text})
	switch kind {
	case "answer":
		if r.Answer == "" {
			r.Answer = text
		}
	case "error":
		if r.Error == "" {
			r.Error = text
		}
	}
	markDirtyLocked()
}

// RecordLLMCall 记录一轮 LLM 调用的输入快照（发给模型的完整 messages JSON）
// 与 token 用量。快照在 OnLLMCall 时机截取：此刻会话内容恰好等于本轮输入。
func RecordLLMCall(ctx context.Context, round int, inputJSON string, in, out, ms int64) {
	id := logx.IDFrom(ctx)
	if id == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	r, ok := reqs[id]
	if !ok {
		return
	}
	r.In += in
	r.Out += out
	r.pendIn, r.pendOut, r.pendMs = in, out, ms
	if len(r.Entries) < maxEntries {
		recordWithCap(r, "llm_in", fmt.Sprintf("第 %d 轮输入\n%s", round, logx.Truncate(inputJSON, llmTextCap)), in, 0, ms)
	}
	markDirtyLocked()
}

// RecordLLMMessage 记录模型本轮的原始输出（文本 + 工具调用意图 JSON），
// 并带上刚记录的 llm_in 暂存的 token 用量。
func RecordLLMMessage(ctx context.Context, outJSON string) {
	id := logx.IDFrom(ctx)
	if id == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	r, ok := reqs[id]
	if !ok {
		return
	}
	if len(r.Entries) < maxEntries {
		recordWithCap(r, "llm_out", logx.Truncate(outJSON, llmTextCap), r.pendIn, r.pendOut, r.pendMs)
	}
	r.pendIn, r.pendOut, r.pendMs = 0, 0, 0
	markDirtyLocked()
}

// End 登记调用结束：盖时间戳；errMsg 非空时补记错误。
func End(id, errMsg string) {
	mu.Lock()
	r, ok := reqs[id]
	mu.Unlock()
	if !ok {
		return
	}
	if errMsg != "" {
		Record(logx.WithID(context.Background(), id), "error", "%s", errMsg)
	}
	mu.Lock()
	r.Ended = time.Now()
	markDirtyLocked()
	mu.Unlock()
}

// TurnUsage 是一次调用（一整轮问答）的 token 与耗时汇总。SSE 的 done 帧与历史
// 回显共用它：两条路径上的数字必须同源，否则刷新前后同一轮会报出两个结果。
type TurnUsage struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
	Ms  int64 `json:"ms,omitempty"` // 0 = 还没结束（被暂停的现场没有结束时刻）
}

// Usage 返回本次请求的轮末汇总。
func (r *Request) Usage() *TurnUsage {
	if r == nil {
		return nil
	}
	u := &TurnUsage{In: r.In, Out: r.Out}
	if ms := r.Ended.Sub(r.Started).Milliseconds(); ms > 0 {
		u.Ms = ms
	}
	return u
}

// List 返回全部轨迹摘要（不含逐条事件），最新的在前。
func List() []Request {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Request, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		r := reqs[order[i]]
		if r == nil {
			continue
		}
		cp := *r
		cp.Entries = nil
		out = append(out, cp)
	}
	return out
}

// Get 返回一条完整轨迹（含逐条事件）。
func Get(id string) (Request, bool) {
	mu.Lock()
	defer mu.Unlock()
	r, ok := reqs[id]
	if !ok {
		return Request{}, false
	}
	return *r, true
}

// Event 是全局事件流中的一条调用事件（已展开请求归属信息）。
type Event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"` // 事件类型，如 llm_in / tool / answer / skill
	Text    string    `json:"text"`
	In      int64     `json:"in,omitempty"`
	Out     int64     `json:"out,omitempty"`
	Ms      int64     `json:"ms,omitempty"`
	ReqID   string    `json:"reqId"`
	Source  string    `json:"source,omitempty"`
	Session string    `json:"session,omitempty"` // 所属会话（plan-<id> / goal-<id> 即一次多步运行）
	Input   string    `json:"input"`             // 所属请求的用户输入（展示调用上下文用，已截断）
}

// Stats 是全局统计：请求次数与 token 累计（不受 limit 影响，全量统计）。
type Stats struct {
	Requests int   `json:"requests"`
	In       int64 `json:"in"`
	Out      int64 `json:"out"`
}

// Events 返回全局事件流：所有请求的调用事件按时间倒序平铺成一条流水
// （最新在前），limit 限制返回条数（<=0 返回全部），统计始终全量。
// Events 返回最新在前的事件流与统计。传 session 时只给该会话的请求，
// 统计也随筛选重算——计划（plan-<id>）与目标（goal-<id>）各自一条会话，
// 用它就能回看一次多步运行的全部轮次。
func Events(limit int, session ...string) (events []Event, stats Stats) {
	mu.Lock()
	defer mu.Unlock()
	want := ""
	if len(session) > 0 {
		want = strings.TrimSpace(session[0])
	}
	stats.Requests = len(order)
	capEvts := limit > 0
	for i := len(order) - 1; i >= 0; i-- {
		r := reqs[order[i]]
		if r == nil {
			continue
		}
		if want != "" && r.Session != want {
			continue
		}
		stats.In += r.In
		stats.Out += r.Out
		input := logx.Truncate(r.Input, 120)
		for j := len(r.Entries) - 1; j >= 0; j-- {
			e := r.Entries[j]
			if capEvts && len(events) >= limit {
				continue
			}
			events = append(events, Event{
				Time: e.Time, Kind: e.Kind, Text: e.Text,
				In: e.In, Out: e.Out, Ms: e.Ms,
				ReqID: r.ID, Source: r.Source, Session: r.Session, Input: input,
			})
		}
	}
	return events, stats
}

// ClearSession 删除指定会话的全部轨迹（生命周期跟随对话历史：
// 用户清除对话上下文时调用），返回删除的请求数。
func ClearSession(session string) int {
	mu.Lock()
	defer mu.Unlock()
	removed := 0
	kept := order[:0]
	for _, id := range order {
		r := reqs[id]
		if r != nil && r.Session == session {
			delete(reqs, id)
			removed++
		} else {
			kept = append(kept, id)
		}
	}
	order = kept
	markDirtyLocked()
	return removed
}
