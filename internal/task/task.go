// Package task 实现机器人统一任务引擎：单次（once）、有限多次（repeat）与
// 无限循环（cron）三类任务共用一个 Task 模型与到点回调；agent 通过 schedule_task
// 工具注册，到点后引擎在独立 goroutine 触发回调（通常是"语音播报 + 屏幕显示"）。
//
// 设计：once/repeat 用可重挂的 time.Timer（repeat 按计划时刻推进 Next，不随执行
// 延迟漂移），cron 用 robfig/cron；回调在锁外、独立 goroutine 执行，因此回调内
// 可以安全地再次调用引擎的 Schedule/Cancel/List 而不死锁。
//
// 持久化：NewEngineFile 启用 JSON 文件落盘（见 store.go）——启动时恢复任务，
// 注册/取消/触发后原子写回，Close 前保留快照，重启不丢任务。
package task

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Kind 是任务的触发类型。
type Kind string

const (
	KindOnce   Kind = "once"   // 单次：到 When 触发一次
	KindRepeat Kind = "repeat" // 有限多次：从首次触发起每 Interval 一次，共 Times 次
	KindCron   Kind = "cron"   // 无限循环：按 Cron 表达式反复触发
)

// Task 是一条待触发的任务，Kind 决定哪些字段生效：
// once → When；repeat → Interval + Times；cron → Cron。
type Task struct {
	ID        string        `json:"id"`
	Text      string        `json:"text"`
	Kind      Kind          `json:"kind"`
	When      time.Time     `json:"when,omitempty"`      // once：触发时刻
	Interval  time.Duration `json:"interval,omitempty"`  // repeat：触发间隔
	Times     int           `json:"times,omitempty"`     // repeat：总次数（≥2）
	Remaining int           `json:"remaining,omitempty"` // repeat：剩余次数（引擎维护）
	Cron      string        `json:"cron,omitempty"`      // cron：表达式
	CreatedAt time.Time     `json:"createdAt"`
	// Next 是下次触发时刻，由引擎维护：Schedule 返回值中即可读，List 与回调中均已填充。
	Next time.Time `json:"next,omitempty"`
}

// Callback 在任务到点时被调用，fn 在独立 goroutine 中执行。
// 循环任务每次触发也会收到一条 Task（When 为本次实际触发时刻）。
type Callback func(t Task)

// entry 是引擎内部的一条调度条目。
type entry struct {
	task   Task          // 可变副本：repeat/cron 每次触发更新 Remaining/Next
	sched  cron.Schedule // cron：解析结果，用于推进 Next
	timer  *time.Timer   // once/repeat
	cronID cron.EntryID  // cron
}

// Engine 是统一任务排程器：单次/有限多次（time.Timer）+ 无限循环（cron）。
type Engine struct {
	mu      sync.Mutex
	saveMu  sync.Mutex // 串行化落盘：恢复/触发/取消可能并发写盘，避免 tmp 文件互踩
	entries map[string]*entry
	cron    *cron.Cron
	cb      Callback
	store   *fileStore // nil 表示不持久化
	now     func() time.Time
	closed  bool
}

// NewEngine 创建排程器（不持久化）。cb 为到点回调，可为 nil。
func NewEngine(cb Callback) *Engine {
	return newEngine(cb, nil)
}

// NewEngineFile 创建带 JSON 文件持久化的排程器：启动时从 path 恢复任务
// （停机期间过期的单次任务会立即补触发一次），此后每次注册/取消/触发都
// 原子写回，Close 前保留未触发任务的快照。
func NewEngineFile(cb Callback, path string) *Engine {
	return newEngine(cb, &fileStore{path: path})
}

func newEngine(cb Callback, store *fileStore) *Engine {
	e := &Engine{
		entries: make(map[string]*entry),
		cron:    cron.New(),
		cb:      cb,
		store:   store,
		now:     time.Now,
	}
	e.restore()
	e.cron.Start()
	return e
}

// restore 从持久化文件恢复任务；恢复期间不逐条写盘，结束后整体快照一次。
func (e *Engine) restore() {
	tasks := e.store.load()
	for _, t := range tasks {
		if _, err := e.schedule(t, true); err != nil {
			log.Printf("[task] 恢复任务 %s 失败: %v", t.ID, err)
		}
	}
	if len(tasks) > 0 {
		e.save()
		log.Printf("[task] 已从 %s 恢复 %d 条任务", e.store.path, len(tasks))
	}
}

// save 在锁外把当前待触发任务快照原子写回文件；未启用持久化时为空操作。
func (e *Engine) save() {
	e.saveWith(func() []Task {
		e.mu.Lock()
		defer e.mu.Unlock()
		tasks := make([]Task, 0, len(e.entries))
		for _, ent := range e.entries {
			tasks = append(tasks, ent.task)
		}
		return tasks
	})
}

// saveWith 串行化写盘：saveMu 保证同一时刻只有一次"写 tmp + rename"，
// 否则并发落盘会互相覆盖 tmp 文件导致 rename 失败。snapshot 由调用方提供
// （普通路径在锁内取引擎快照，Close 直接复用关闭前的快照）。
func (e *Engine) saveWith(snapshot func() []Task) {
	if e.store == nil || e.store.path == "" {
		return
	}
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	if err := e.store.save(snapshot()); err != nil {
		log.Printf("[task] 持久化失败: %v", err)
	}
}

// cronParser 仅接受标准 5 段 cron（分 时 日 月 周），保证解析与调度行为一致。
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ParseCron 校验 cron 表达式（标准 5 段：分 时 日 月 周）。
func ParseCron(spec string) (string, error) {
	if _, err := cronParser.Parse(spec); err != nil {
		return "", fmt.Errorf("无效的 cron 表达式 %q: %w", spec, err)
	}
	return spec, nil
}

// Schedule 注册一条任务，返回补全后的任务（含 ID/CreatedAt/Remaining/Next）。
// id 为空自动生成；Kind 为空时按已填字段推断（When→once、Interval→repeat、Cron→cron）。
// once 的 When 已过期时立即异步触发。注册成功即落盘（启用持久化时）。
func (e *Engine) Schedule(t Task) (Task, error) {
	return e.schedule(t, false)
}

// schedule 是 Schedule 的内部实现；restore=true 表示从持久化文件恢复：
// 保留原始 ID/CreatedAt/Remaining，且不触发逐条落盘。
func (e *Engine) schedule(t Task, restore bool) (Task, error) {
	if t.ID == "" {
		t.ID = newID()
	}
	if t.Kind == "" {
		switch {
		case !t.When.IsZero():
			t.Kind = KindOnce
		case t.Interval > 0:
			t.Kind = KindRepeat
		case t.Cron != "":
			t.Kind = KindCron
		}
	}

	var sched cron.Schedule
	switch t.Kind {
	case KindOnce:
		if t.When.IsZero() {
			return Task{}, ErrNoWhen
		}
	case KindRepeat:
		if t.Interval <= 0 {
			return Task{}, ErrNoInterval
		}
		if t.Times < 2 {
			return Task{}, ErrBadTimes
		}
		// 恢复时保留文件里的剩余次数（异常值除外），新建时从满次数开始。
		if !restore || t.Remaining < 1 || t.Remaining > t.Times {
			t.Remaining = t.Times
		}
	case KindCron:
		var err error
		if sched, err = cronParser.Parse(t.Cron); err != nil {
			return Task{}, fmt.Errorf("无效的 cron 表达式 %q: %w", t.Cron, err)
		}
	default:
		return Task{}, fmt.Errorf("未知任务类型 %q", t.Kind)
	}
	if !restore {
		t.CreatedAt = e.now()
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return Task{}, ErrClosed
	}
	if _, dup := e.entries[t.ID]; dup {
		e.mu.Unlock()
		return Task{}, ErrDuplicateID
	}
	ent := &entry{task: t, sched: sched}
	switch t.Kind {
	case KindOnce:
		ent.task.Next = t.When
		ent.timer = time.AfterFunc(t.When.Sub(e.now()), func() { e.fireOnce(t.ID) })
	case KindRepeat:
		ent.task.Next = e.now().Add(t.Interval)
		ent.timer = time.AfterFunc(t.Interval, func() { e.fireRepeat(t.ID) })
	case KindCron:
		id, err := e.cron.AddFunc(t.Cron, func() { e.fireCron(t.ID) })
		if err != nil {
			e.mu.Unlock()
			return Task{}, fmt.Errorf("无效的 cron 表达式 %q: %w", t.Cron, err)
		}
		ent.cronID = id
		ent.task.Next = sched.Next(e.now())
	}
	e.entries[t.ID] = ent
	out := ent.task
	e.mu.Unlock()

	if !restore {
		e.save()
	}
	return out, nil
}

// Cancel 取消一条任务（单次/多次/循环通用），返回是否真的取消了。
func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	ent, ok := e.entries[id]
	if !ok {
		e.mu.Unlock()
		return false
	}
	delete(e.entries, id)
	if ent.timer != nil {
		ent.timer.Stop()
	}
	if ent.task.Kind == KindCron {
		e.cron.Remove(ent.cronID)
	}
	e.mu.Unlock()

	e.save()
	return true
}

// List 返回当前所有待触发的任务（按下次触发时间升序）。
func (e *Engine) List() []Task {
	e.mu.Lock()
	out := make([]Task, 0, len(e.entries))
	for _, ent := range e.entries {
		out = append(out, ent.task)
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Next.Before(out[j].Next) })
	return out
}

// Len 返回当前待触发任务数量。
func (e *Engine) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.entries)
}

// Close 关闭引擎：停止所有定时器与循环任务，之后 Schedule 返回 ErrClosed。
// 关闭前保留未触发任务的快照，供下次启动恢复。
func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	tasks := make([]Task, 0, len(e.entries))
	for _, ent := range e.entries {
		tasks = append(tasks, ent.task)
		if ent.timer != nil {
			ent.timer.Stop()
		}
	}
	e.entries = make(map[string]*entry)
	e.cron.Stop()
	e.mu.Unlock()

	// 关闭前保留未触发任务快照，供下次启动恢复。
	e.saveWith(func() []Task { return tasks })
}

// fireOnce 由 time.AfterFunc 触发：先移除条目（锁内），再在锁外执行回调。
func (e *Engine) fireOnce(id string) {
	e.mu.Lock()
	ent, ok := e.entries[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	delete(e.entries, id)
	fired := ent.task
	e.mu.Unlock()

	e.save()
	if e.cb != nil {
		e.cb(fired)
	}
}

// fireRepeat 由 time.AfterFunc 触发：扣减剩余次数；未用完则基于计划时刻 Next
// 重挂定时器（而非 now+Interval，避免累积漂移），用尽后移除条目。
func (e *Engine) fireRepeat(id string) {
	e.mu.Lock()
	ent, ok := e.entries[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	ent.task.Remaining--
	ent.task.Next = ent.task.Next.Add(ent.task.Interval)
	fired := ent.task
	if fired.Remaining > 0 {
		ent.timer = time.AfterFunc(time.Until(ent.task.Next), func() { e.fireRepeat(id) })
	} else {
		delete(e.entries, id)
	}
	e.mu.Unlock()

	// 先落盘再回调：即使紧接掉电，重启也不会重复触发已触发的任务。
	e.save()
	if e.cb != nil {
		e.cb(fired)
	}
}

// fireCron 由 cron 触发：推进 Next 供 List 展示，条目常驻直至 Cancel/Close。
func (e *Engine) fireCron(id string) {
	e.mu.Lock()
	ent, ok := e.entries[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	fired := ent.task
	fired.When = e.now()
	ent.task.Next = ent.sched.Next(e.now())
	e.mu.Unlock()

	if e.cb != nil {
		e.cb(fired)
	}
}

var (
	ErrDuplicateID = errKind("task: 该任务 ID 已存在")
	ErrNoWhen      = errKind("task: 单次任务缺少触发时间 when")
	ErrNoInterval  = errKind("task: 多次任务缺少间隔 interval")
	ErrBadTimes    = errKind("task: 多次任务次数 times 至少为 2（单次请用 once）")
	ErrClosed      = errKind("task: 引擎已关闭")
)

type errKind string

func (e errKind) Error() string { return string(e) }
