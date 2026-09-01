package reminder

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// Recurring 是一条循环任务（如"每天早上 8 点提醒喝水"）。
type Recurring struct {
	ID        string    `json:"id"`
	Cron      string    `json:"cron"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}

// ParseCron 把 cron 表达式解析为标准 5 段 cron（分 时 日 月 周）。
func ParseCron(spec string) (string, error) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(spec); err != nil {
		return "", fmt.Errorf("无效的 cron 表达式 %q: %w", spec, err)
	}
	return spec, nil
}

// ScheduleRecurring 注册一条循环任务。cb 每次触发都会收到一条 Reminder（When=触发时刻）。
func (e *Engine) ScheduleRecurring(id, cronSpec, text string) (string, error) {
	if id == "" {
		id = newID()
	}
	if cronSpec == "" {
		return "", fmt.Errorf("缺少 cron 表达式")
	}
	if e.cron == nil {
		return "", ErrClosed
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", ErrClosed
	}
	if _, dup := e.recurrings[id]; dup {
		return "", ErrDuplicateID
	}

	entryID, err := e.cron.AddFunc(cronSpec, func() { e.fireRecurring(id) })
	if err != nil {
		return "", fmt.Errorf("无效的 cron 表达式 %q: %w", cronSpec, err)
	}
	e.recurrings[id] = &recurringEntry{
		recurring: Recurring{ID: id, Cron: cronSpec, Text: text, CreatedAt: e.now()},
		cronID:    entryID,
	}
	return id, nil
}

// CancelRecurring 取消一条循环任务，返回是否真的取消了。
func (e *Engine) CancelRecurring(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent, ok := e.recurrings[id]
	if !ok {
		return false
	}
	e.cron.Remove(ent.cronID)
	delete(e.recurrings, id)
	return true
}

// ListRecurring 返回所有循环任务。
func (e *Engine) ListRecurring() []Recurring {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Recurring, 0, len(e.recurrings))
	for _, ent := range e.recurrings {
		out = append(out, ent.recurring)
	}
	return out
}

// fireRecurring 由 cron 触发，在锁外执行回调。
func (e *Engine) fireRecurring(id string) {
	e.mu.Lock()
	ent, ok := e.recurrings[id]
	e.mu.Unlock()
	if !ok {
		return
	}
	if e.cb != nil {
		e.cb(Reminder{ID: ent.recurring.ID, When: e.now(), Text: ent.recurring.Text, CreatedAt: ent.recurring.CreatedAt})
	}
}
