// Package task 实现机器人统一任务引擎，调度内核基于 vortexagent 的 autonomous
// 包：单次（once）、有限多次（repeat）、无限循环（cron）与智能任务（agent）共用
// 一个 Task 模型与到点回调；agent 通过 schedule_task 工具注册，到点后触发回调
// （普通提醒由调用方直投通知渠道，智能任务交给 agent 自主决策）。
//
// 调度核心复用框架能力，不在本项目重复实现：
//   - autonomous.Scheduler：维护目标的下次执行时间，计算下次唤醒时刻
//     （SleepDuration：取最近目标的 NextRunAt，maxSleep 封顶）与到期集合
//     （DueGoals：到期目标按优先级排序）；
//   - autonomous 的触发器（OneShot/Interval/Cron）：各调度类型的下次执行时间
//     计算（Cron 用 robfig/cron 标准解析，含 @every 描述符）；
//   - autonomous.Goal：任务在调度层的统一形态，Engine 维护 Task ↔ Goal 映射，
//     agent/heartbeat/channel 等业务字段作为元数据挂在 Engine 的 entry 上
//     （Goal 的可变字段经其并发安全方法访问，规避包外锁）。
//
// Engine 自身只保留薄薄一层业务胶水：入参校验与 Task↔Goal 映射、唤醒循环
// （睡眠可被注册/取消打断）、到期分发与触发后的推进/落盘。回调在独立 goroutine
// 中执行，因此回调内可以安全地再次调用 Schedule/Cancel/List 而不死锁。
//
// 持久化：NewEngineFile 启用 JSON 文件落盘（见 store.go）——启动时恢复任务
// （停机期间过期的单次任务会立即补触发一次），此后每次注册/取消/触发都原子写回，
// 落盘先于回调，重启不会重复触发已触发的任务。
package task

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/autonomous"
	"github.com/robfig/cron/v3"
)

// Kind 是任务的触发类型。
type Kind string

const (
	KindOnce   Kind = "once"   // 单次：到 When 触发一次
	KindRepeat Kind = "repeat" // 有限多次：从首次触发起每 Interval 一次，共 Times 次
	KindCron   Kind = "cron"   // 无限循环：按 Cron 表达式反复触发
	KindAgent  Kind = "agent"  // 智能任务：到点把指令交给 agent 自主决策与行动
)

// Task 是一条待触发的任务，Kind 决定哪些字段生效：
// once → When；repeat → Interval + Times；cron → Cron；agent → 按
// When/Interval/Cron 之一定时，到点执行 Prompt/Text 指令而非播报文案。
// 对 Engine 而言 Task 是入参与对外的视图形态；调度层形态是 autonomous.Goal。
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
	// Heartbeat 标记该任务为心跳任务（到点把 HEARTBEAT.md 清单喂给 agent）。
	Heartbeat bool `json:"heartbeat,omitempty"`
	// Channel 可选推送通道名（agent 任务的输出投递过滤；空 = 全部可用通道）。
	Channel string `json:"channel,omitempty"`
}

// Callback 在任务到点时被调用，fn 在独立 goroutine 中执行。
// 循环任务每次触发也会收到一条 Task（When 为本次实际触发时刻）。
type Callback func(t Task)

// maxSleep 是无活跃任务时的最长睡眠间隔：到期计算全交 Scheduler，
// 空闲时每小时空醒一次检查（无到期目标时为空操作，代价可忽略）。
const maxSleep = time.Hour

// entry 是引擎内部的一条调度条目：调度形态（Goal）+ 业务元数据。
// runCount（repeat 已触发次数）等可变业务状态留在 entry、由 Engine.mu 保护，
// Goal 上对应字段（RunCount 等）不使用——包外无法持有 Goal.mu，
// 全部经 Goal 的并发安全方法（SetNextRunAt/SetStatus）或只读访问器访问。
type entry struct {
	goal      *autonomous.Goal
	agent     bool // KindAgent：到点交给 agent 决策（视图 Kind 覆盖时间类型）
	heartbeat bool // 心跳任务（到点附 HEARTBEAT.md 清单）
	channel   string
	runCount  int // repeat：已触发次数（Times-Remaining）
}

// Engine 是统一任务排程器：调度与唤醒时间计算委托 autonomous.Scheduler，
// 本包只做 Task 映射、到期分发与持久化。
type Engine struct {
	mu      sync.Mutex // 保护 entries / runCount / closed
	saveMu  sync.Mutex // 串行化落盘：恢复/触发/取消可能并发写盘，避免 tmp 文件互踩
	entries map[string]*entry
	sched   *autonomous.Scheduler
	cb      Callback
	store   *fileStore // nil 表示不持久化
	wake    chan struct{}
	done    chan struct{}
	timer   *time.Timer
	now     func() time.Time
	closed  bool
}

// NewEngine 创建排程器（不持久化）。cb 为到点回调，可为 nil。
func NewEngine(cb Callback) *Engine {
	return newEngine(cb, nil)
}

// NewEngineFile 创建带 JSON 文件持久化的排程器：启动时从 path 恢复任务
// （停机期间过期的单次任务会立即补触发一次），此后每次注册/取消/触发都
// 原子写回。
func NewEngineFile(cb Callback, path string) *Engine {
	return newEngine(cb, &fileStore{path: path})
}

func newEngine(cb Callback, store *fileStore) *Engine {
	e := &Engine{
		entries: make(map[string]*entry),
		sched:   autonomous.NewScheduler(maxSleep),
		cb:      cb,
		store:   store,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		timer:   time.NewTimer(time.Hour),
		now:     time.Now,
	}
	e.timer.Stop() // 初始不触发，loop 首轮按调度结果重置
	if store != nil {
		e.restore()
	}
	go e.loop()
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

// loop 是唤醒主循环：睡眠时长由 Scheduler 计算（最近目标的 NextRunAt，
// maxSleep 封顶；无活跃目标时空醒一次），注册/取消经 wake 立即打断重算，
// 到点则分发全部到期任务后继续睡眠。
func (e *Engine) loop() {
	e.timer.Reset(e.sched.SleepDuration())
	for {
		select {
		case <-e.done:
			return
		case <-e.wake:
		case <-e.timer.C:
			e.fireDue()
		}
		if !e.timer.Stop() {
			select {
			case <-e.timer.C: // 排空已触发但未消费的值
			default:
			}
		}
		e.timer.Reset(e.sched.SleepDuration())
	}
}

// poke 通知唤醒循环重新计算睡眠（注册/取消后下次唤醒时间可能变化）。
// 缓冲为 1 的信号语义：循环正忙时合并为一次唤醒即可。
func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// cronParser 接受标准 5 段 cron（分 时 日 月 周）与描述符（@every 1h30m、@daily 等），
// 描述符支撑心跳 interval 与相对周期循环，保证解析与调度行为一致。
var cronParser = cron.NewParser(cron.Descriptor | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ParseCron 校验 cron 表达式（标准 5 段：分 时 日 月 周）。
func ParseCron(spec string) (string, error) {
	if _, err := cronParser.Parse(spec); err != nil {
		return "", fmt.Errorf("无效的 cron 表达式 %q: %w", spec, err)
	}
	return spec, nil
}

// Schedule 注册一条任务，返回补全后的任务（含 ID/CreatedAt/Remaining/Next）。
// id 为空自动生成；Kind 为空时按已填字段推断（When→once、Interval→repeat、Cron→cron）。
// once 的 When 已过期时由唤醒循环立即补触发。注册成功即落盘（启用持久化时）。
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

	// agent 任务的定时方式由时间字段推断（once/repeat/cron 三种触发器共用），
	// Kind 保持 agent，到点回调按 Kind 分流处理。
	trigger := t.Kind
	if t.Kind == KindAgent {
		switch {
		case !t.When.IsZero():
			trigger = KindOnce
		case t.Interval > 0:
			trigger = KindRepeat
		case t.Cron != "":
			trigger = KindCron
		default:
			return Task{}, ErrNoWhen
		}
	}

	// 校验 + 计算首次触发时刻。
	var next time.Time
	switch trigger {
	case KindOnce:
		if t.When.IsZero() {
			return Task{}, ErrNoWhen
		}
		next = t.When // 允许过去时刻：恢复/注册后由唤醒循环立即补触发
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
		next = e.now().Add(t.Interval)
	case KindCron:
		if _, err := cronParser.Parse(t.Cron); err != nil {
			return Task{}, fmt.Errorf("无效的 cron 表达式 %q: %w", t.Cron, err)
		}
		var err error
		if next, err = (&autonomous.CronTrigger{Expr: t.Cron}).NextRun(e.now()); err != nil {
			return Task{}, fmt.Errorf("无效的 cron 表达式 %q: %w", t.Cron, err)
		}
	default:
		return Task{}, fmt.Errorf("未知任务类型 %q", t.Kind)
	}
	if !restore {
		t.CreatedAt = e.now()
	}

	// Task → Goal 映射：调度规则与首次触发时刻交给框架形态；
	// Title/Description 均存原文（普通提醒按 Description 投递、列表按 Title 展示）。
	g := autonomous.NewGoal()
	g.ID = t.ID
	g.Title = t.Text
	g.Description = t.Text
	g.Status = autonomous.GoalStatusActive
	g.CreatedAt = t.CreatedAt
	switch trigger {
	case KindOnce:
		g.Schedule = autonomous.Schedule{Type: autonomous.ScheduleOneShot}
	case KindRepeat:
		g.Schedule = autonomous.Schedule{Type: autonomous.ScheduleInterval, Interval: t.Interval, MaxRuns: t.Times}
	case KindCron:
		g.Schedule = autonomous.Schedule{Type: autonomous.ScheduleCron, Cron: t.Cron}
	}
	g.SetNextRunAt(next)

	ent := &entry{goal: g, agent: t.Kind == KindAgent, heartbeat: t.Heartbeat, channel: t.Channel}
	if restore && trigger == KindRepeat {
		ent.runCount = t.Times - t.Remaining
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
	e.entries[t.ID] = ent
	e.sched.Add(g)
	e.mu.Unlock()

	if !restore {
		e.save()
	}
	e.poke()

	t.Next = next
	return t, nil
}

// Cancel 取消一条任务（单次/多次/循环通用），返回是否真的取消了。
func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	if _, ok := e.entries[id]; !ok {
		e.mu.Unlock()
		return false
	}
	delete(e.entries, id)
	e.sched.Remove(id)
	tasks := e.snapshotLocked()
	e.mu.Unlock()

	e.storeSave(tasks)
	e.poke()
	return true
}

// List 返回当前所有待触发的任务（按下次触发时间升序）。
func (e *Engine) List() []Task {
	e.mu.Lock()
	out := make([]Task, 0, len(e.entries))
	for _, ent := range e.entries {
		out = append(out, e.viewTask(ent))
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

// Close 关闭引擎：停止唤醒循环，之后 Schedule 返回 ErrClosed。
// 每次变更已实时落盘，无需额外快照。
func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()
	close(e.done)
}

// fireDue 分发全部到期任务（DueGoals 已按优先级排序）。
func (e *Engine) fireDue() {
	for _, g := range e.sched.DueGoals() {
		e.fire(g.ID)
	}
}

// fireCron 手动触发一条循环任务（测试辅助）：回调收到任务、条目常驻且 Next 推进。
func (e *Engine) fireCron(id string) {
	e.fire(id)
}

// fire 触发一条任务：推进调度状态（once 移除 / repeat 计数推进 / cron 推进 Next），
// 先落盘再异步回调——即使紧接掉电，重启也不会重复触发已触发的任务。
func (e *Engine) fire(id string) {
	e.mu.Lock()
	ent, ok := e.entries[id]
	if !ok {
		e.mu.Unlock()
		return
	}
	g := ent.goal
	now := e.now()

	fired := e.viewTask(ent)
	remove := false
	switch g.Schedule.Type {
	case autonomous.ScheduleOneShot:
		// 单次（含 agent 单次）：触发即移除；回调的 When 为计划触发时刻。
		fired.When = g.GetNextRunAt()
		remove = true
	case autonomous.ScheduleInterval:
		// 有限多次：计数推进，Next 按计划时刻推进（不随执行延迟漂移）；
		// 用尽后移除（对应框架 preCheck 的 MaxRuns 终态，这里直接删除条目）。
		ent.runCount++
		fired = e.viewTask(ent)
		fired.When = now
		if ent.runCount >= g.Schedule.MaxRuns {
			remove = true
		} else {
			g.SetNextRunAt(g.GetNextRunAt().Add(g.Schedule.Interval))
		}
	default: // cron
		fired.When = now
		if next, err := (&autonomous.CronTrigger{Expr: g.Schedule.Cron}).NextRun(now); err == nil {
			g.SetNextRunAt(next)
		} else {
			log.Printf("[task] 推进任务 %s 的 cron 失败: %v", id, err)
			remove = true
		}
	}
	if remove {
		delete(e.entries, id)
		e.sched.Remove(id)
	}
	tasks := e.snapshotLocked()
	e.mu.Unlock()

	e.storeSave(tasks)
	if e.cb != nil {
		go e.cb(fired)
	}
}

// viewTask 把 entry 投影成对外的 Task 视图（List 与落盘快照用；When 仅 once 填充，
// 触发回调的触发时刻由 fire 单独覆盖）。
func (e *Engine) viewTask(ent *entry) Task {
	g := ent.goal
	next := g.GetNextRunAt()
	tk := Task{
		ID:        g.ID,
		Text:      g.Title,
		Cron:      g.Schedule.Cron,
		CreatedAt: g.CreatedAt,
		Next:      next,
		Heartbeat: ent.heartbeat,
		Channel:   ent.channel,
	}
	switch {
	case ent.agent:
		tk.Kind = KindAgent
	case g.Schedule.Type == autonomous.ScheduleInterval:
		tk.Kind = KindRepeat
		tk.Interval = g.Schedule.Interval
		tk.Times = g.Schedule.MaxRuns
		tk.Remaining = tk.Times - ent.runCount
	case g.Schedule.Type == autonomous.ScheduleCron:
		tk.Kind = KindCron
	default:
		tk.Kind = KindOnce
		tk.When = next
	}
	return tk
}

// save 在锁外把当前待触发任务快照原子写回文件；未启用持久化时为空操作。
func (e *Engine) save() {
	e.storeSave(e.snapshot())
}

// snapshot 取当前全部任务的视图快照。
func (e *Engine) snapshot() []Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

func (e *Engine) snapshotLocked() []Task {
	out := make([]Task, 0, len(e.entries))
	for _, ent := range e.entries {
		out = append(out, e.viewTask(ent))
	}
	return out
}

// storeSave 串行化写盘：saveMu 保证同一时刻只有一次"写 tmp + rename"，
// 否则并发落盘会互相覆盖 tmp 文件导致 rename 失败。
func (e *Engine) storeSave(tasks []Task) {
	if e.store == nil || e.store.path == "" {
		return
	}
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	if err := e.store.save(tasks); err != nil {
		log.Printf("[task] 持久化失败: %v", err)
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
