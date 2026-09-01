// Package reminder 实现机器人定时提醒引擎：agent 通过 schedule_reminder 工具
// 注册提醒，到点后引擎在独立 goroutine 触发回调（通常是"语音播报 + 屏幕显示"）。
//
// 设计：无外部依赖，仅用 time.Timer；回调在锁外、独立 goroutine 执行，
// 因此回调内可以安全地再次调用引擎的 Schedule/Cancel/List 而不死锁。
package reminder

import (
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Reminder 是一条待触发的提醒。
type Reminder struct {
	ID        string    `json:"id"`
	When      time.Time `json:"when"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}

// Callback 在提醒到点时被调用。fn 在独立 goroutine 中执行。
type Callback func(r Reminder)

type entry struct {
	reminder Reminder
	timer    *time.Timer
}

type recurringEntry struct {
	recurring Recurring
	cronID    cron.EntryID
}

// Engine 是提醒排程器：一次性提醒（time.Timer）+ 循环任务（cron）。
type Engine struct {
	mu         sync.Mutex
	timers     map[string]*entry
	recurrings map[string]*recurringEntry
	cron       *cron.Cron
	cb         Callback
	now        func() time.Time
	closed     bool
}

// NewEngine 创建排程器。cb 为到点回调，可为 nil。
func NewEngine(cb Callback) *Engine {
	e := &Engine{
		timers:     make(map[string]*entry),
		recurrings: make(map[string]*recurringEntry),
		cron:       cron.New(),
		cb:         cb,
		now:        time.Now,
	}
	e.cron.Start()
	return e
}

// Schedule 注册一条提醒：when 为触发时间点，text 为提醒内容。
// 返回 (id, error)。若 id 为空则自动生成。when 已过期时立即异步触发。
func (e *Engine) Schedule(id string, when time.Time, text string) (string, error) {
	if id == "" {
		id = newID()
	}
	if when.IsZero() {
		return "", ErrNoWhen
	}
	r := Reminder{ID: id, When: when, Text: text, CreatedAt: e.now()}

	delay := when.Sub(e.now())
	if delay < 0 {
		delay = 0
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return "", ErrClosed
	}
	if _, dup := e.timers[id]; dup {
		e.mu.Unlock()
		return "", ErrDuplicateID
	}
	entry := &entry{reminder: r}
	entry.timer = time.AfterFunc(delay, func() { e.fire(id) })
	e.timers[id] = entry
	e.mu.Unlock()

	return id, nil
}

// Cancel 取消一条提醒，返回是否真的取消了。
func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.timers[id]
	if !ok {
		return false
	}
	delete(e.timers, id)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	return true
}

// List 返回当前所有未触发的提醒（按触发时间升序）。
func (e *Engine) List() []Reminder {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Reminder, 0, len(e.timers))
	for _, ent := range e.timers {
		out = append(out, ent.reminder)
	}
	sortByWhen(out)
	return out
}

// Len 返回当前未触发提醒数量。
func (e *Engine) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.timers)
}

// Close 关闭引擎：停止所有定时器与循环任务，之后 Schedule 返回 ErrClosed。
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	for _, ent := range e.timers {
		if ent.timer != nil {
			ent.timer.Stop()
		}
	}
	e.timers = make(map[string]*entry)
	e.cron.Stop()
	e.recurrings = make(map[string]*recurringEntry)
}

// fire 由 time.AfterFunc 触发：先移除条目（锁内），再在锁外执行回调。
func (e *Engine) fire(id string) {
	e.mu.Lock()
	ent, ok := e.timers[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	delete(e.timers, id)
	e.mu.Unlock()

	if e.cb != nil {
		e.cb(ent.reminder)
	}
}

var (
	ErrDuplicateID = errKind("reminder: 该提醒 ID 已存在")
	ErrNoWhen      = errKind("reminder: 缺少触发时间 when")
	ErrClosed      = errKind("reminder: 引擎已关闭")
)

type errKind string

func (e errKind) Error() string { return string(e) }
