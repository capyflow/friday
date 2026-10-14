// Package task 实现机器人统一任务引擎：单次（once）、有限多次（repeat）、
// 无限循环（cron）与智能任务（agent）共用一个 Task 模型与到点回调；agent 通过
// schedule_task 工具注册，到点后普通提醒直投通知渠道，智能任务交给 agent 自主决策。
//
// 生产入口 NewReliableEngineFile 先完成可靠入队再推进任务，见 reliable.go。
// 以下框架说明适用于兼容入口 NewEngine / NewEngineFile。
// 调度与执行编排整体复用 vortexagent 的 autonomous.AutonomousAgent，不在本项目
// 重复实现：唤醒主循环、到期集合计算、各调度类型的推进（once 终态、repeat 锚定
// 推进不随执行延迟漂移、cron 推进）、MaxRuns 终态与失败熔断、"先落盘后回调"的
// 持久化时序，均由框架完成。本包只保留四块业务胶水：
//   - Task ↔ Goal 映射与入参校验（含哨兵错误）；业务字段（kind/heartbeat/channel）
//     挂在 Goal.Meta 上随目标持久化；
//   - goalStore 适配器（见 store.go）：磁盘保持 JSON 任务文件格式不变，落盘只
//     保留存活目标，等价旧引擎"触发即移除"的对外表现；
//   - directExecutor（见 executor.go）：零 token 产出结果，实际投递统一延迟到
//     框架的 OnGoalExecute 回调；
//   - 派发与终态清理两个回调。
//
// 业务回调在独立 goroutine 中执行，因此回调内可以安全地再次调用
// Schedule/Cancel/List 而不死锁。
//
// 持久化：NewEngineFile 启用 JSON 文件落盘——启动时由框架从适配器恢复任务
// （停机期间过期的单次任务会立即补触发一次），此后每次注册/取消/触发由框架
// 驱动适配器原子写回；落盘先于回调，重启不会重复触发已触发的任务。
package task

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
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

// 业务元数据在 Goal.Meta 中的键名，随目标持久化、重启恢复。
const (
	metaKind                 = "kind"
	metaHeartbeat            = "heartbeat"
	metaGoal                 = "goal"
	metaGoalMaxSteps         = "goalMaxSteps"
	metaGoalMaxTokens        = "goalMaxTokens"
	metaGoalSkill            = "goalSkill"
	metaChannel              = "channel"
	metaTodoID               = "todoId"
	metaNotificationParentID = "notificationParentId"
)

const (
	StatusActive = "active"
	StatusPaused = "paused"
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
	// Status 为空兼容旧数据，按 active 处理；paused 仅停止未来的入队。
	Status string `json:"status,omitempty"`
	// TodoID 由待办管理器维护，公开编辑任务时保留关联。
	TodoID               string `json:"todoId,omitempty"`
	NotificationParentID string `json:"notificationParentId,omitempty"`

	// Goal 把一条智能任务升级为"目标"：到点后不是一次问答，而是在隔离会话里
	// 多轮自主推进，直到达成、到达步数上限或 token 预算。
	Goal bool `json:"goal,omitempty"`
	// MaxSteps / MaxTokens 是单个目标的预算（0 = 用 cost.budget 的默认值）。
	MaxSteps  int   `json:"maxSteps,omitempty"`
	MaxTokens int64 `json:"maxTokens,omitempty"`
	// Skill 把目标钉在某个技能上执行（空 = 不启用技能，也不做自动匹配）。
	Skill string `json:"skill,omitempty"`
}

// Callback 在任务到点时被调用，fn 在独立 goroutine 中执行。
// 循环任务每次触发也会收到一条 Task（When 为本次实际触发时刻）。
type Callback func(t Task)

// maxSleep 是无活跃任务时的最长睡眠间隔（透传给框架）。
const maxSleep = time.Hour

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

// Engine 是统一任务排程器：调度、唤醒与执行编排委托 autonomous.AutonomousAgent，
// 本包只做 Task 映射、校验与回调派发。
type Engine struct {
	mu         sync.Mutex // 保护 ids / closed
	ids        map[string]struct{}
	agent      *autonomous.AutonomousAgent
	store      *goalStore
	cb         Callback
	ready      chan struct{} // 框架主循环就绪（scheduler 已装载）后关闭
	cancel     context.CancelFunc
	closed     bool
	startupErr error
	readyOnce  sync.Once
	reliable   *reliableScheduler
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

func newEngine(cb Callback, file *fileStore) *Engine {
	e := &Engine{
		ids:   make(map[string]struct{}),
		store: newGoalStore(file),
		cb:    cb,
		ready: make(chan struct{}),
	}

	// 恢复：预填 ID 去重表（心跳等固定 ID 任务重启后不重复注册）并打日志；
	// 目标本身由框架启动时经 store.LoadAll 装载。
	if file != nil {
		tasks, err := file.load()
		e.startupErr = err
		if err == nil && len(tasks) > 0 {
			for _, t := range tasks {
				e.ids[t.ID] = struct{}{}
			}
			log.Printf("[task] 已从 %s 恢复 %d 条任务", file.path, len(tasks))
		}
	}

	e.agent = autonomous.New(autonomous.Config{
		GoalStore:     e.store,
		Executor:      directExecutor{},
		MaxSleep:      maxSleep,
		ReflectConfig: autonomous.ReflectConfig{Mode: autonomous.ReflectionLight},
		// Agent/Model/Registry 留空：执行走自定义 Executor、反思为轻量规则
		//（零 token），不注册框架自带的 add_goal/list_goals/remove_goal 工具。
		OnGoalExecute:      e.onFire,
		OnGoalStatusChange: e.onStatusChange,
	})

	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	if e.startupErr != nil {
		e.readyOnce.Do(func() { close(e.ready) })
		return e
	}
	go func() {
		if err := e.agent.Run(ctx); err != nil && ctx.Err() == nil {
			e.mu.Lock()
			e.startupErr = err
			e.mu.Unlock()
			e.readyOnce.Do(func() { close(e.ready) })
		}
	}()
	go func() {
		for e.agent.Goals() == nil { // Run 装载完目标并建好 scheduler 即就绪
			select {
			case <-ctx.Done():
				return
			case <-e.ready:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
		e.readyOnce.Do(func() { close(e.ready) })
	}()
	return e
}

// waitReady 等框架主循环就绪。就绪前 AddGoal 只落盘不入调度器（目标会被静默
// 漏触发），因此所有依赖调度器的公开方法先过这道门；就绪通常在构造后毫秒级。
func (e *Engine) waitReady() error {
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return ErrClosed
	}
	select {
	case <-e.ready:
		return e.Err()
	case <-time.After(5 * time.Second):
		return fmt.Errorf("任务引擎启动超时")
	}
}

// Err 返回恢复错误；损坏文件不能被空任务列表覆盖。
func (e *Engine) Err() error {
	if e.reliable != nil {
		return e.reliable.err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startupErr
}

// Schedule 注册一条任务，返回补全后的任务（含 ID/CreatedAt/Remaining/Next）。
// id 为空自动生成；Kind 为空时按已填字段推断（When→once、Interval→repeat、Cron→cron）。
// once 的 When 已过期时由框架立即补触发。注册成功即落盘（启用持久化时）。
func (e *Engine) Schedule(t Task) (Task, error) {
	if e.reliable != nil {
		return e.reliable.schedule(t)
	}
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
	// 恢复场景从文件里的剩余次数继续；新建/异常值从满次数开始。
	if t.Remaining < 1 || t.Remaining > t.Times {
		t.Remaining = t.Times
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	if t.Status == "" {
		t.Status = StatusActive
	}

	g, err := buildGoal(t)
	if err != nil {
		return Task{}, err
	}

	if err := e.waitReady(); err != nil {
		return Task{}, err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return Task{}, ErrClosed
	}
	if _, dup := e.ids[t.ID]; dup {
		e.mu.Unlock()
		return Task{}, ErrDuplicateID
	}
	e.ids[t.ID] = struct{}{}
	e.mu.Unlock()

	if err := e.agent.AddGoal(g); err != nil {
		e.mu.Lock()
		delete(e.ids, t.ID)
		e.mu.Unlock()
		return Task{}, err
	}
	t.Next = g.GetNextRunAt()
	return t, nil
}

// Get 返回未来任务（含暂停任务）；已入队的执行记录由通知中心查询。
func (e *Engine) Get(id string) (Task, bool) {
	for _, t := range e.List() {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

// Update 替换未来任务定义；ID、创建时间、待办关联与当前暂停状态保持不变。
// 仅可靠调度入口支持编辑，旧构造保留原注册/取消/回调协议。
func (e *Engine) Update(id string, replacement Task) (Task, error) {
	if e.reliable == nil {
		return Task{}, fmt.Errorf("任务编辑需要可靠调度入口")
	}
	return e.reliable.update(id, replacement)
}

func (e *Engine) Pause(id string) (Task, error) {
	if e.reliable == nil {
		return Task{}, fmt.Errorf("任务暂停需要可靠调度入口")
	}
	return e.reliable.setPaused(id, true)
}

func (e *Engine) Resume(id string) (Task, error) {
	if e.reliable == nil {
		return Task{}, fmt.Errorf("任务恢复需要可靠调度入口")
	}
	return e.reliable.setPaused(id, false)
}

// Cancel 取消一条任务（单次/多次/循环通用），返回是否真的取消了。
func (e *Engine) Cancel(id string) bool {
	ok, _ := e.CancelErr(id)
	return ok
}

func (e *Engine) CancelErr(id string) (bool, error) {
	if e.reliable != nil {
		return e.reliable.cancelTask(id)
	}
	if err := e.waitReady(); err != nil {
		return false, err
	}
	e.mu.Lock()
	if _, ok := e.ids[id]; !ok {
		e.mu.Unlock()
		return false, nil
	}
	e.mu.Unlock()
	// 先提交文件删除；失败时不碰仍然运行的调度器。
	if err := e.store.Delete(id); err != nil {
		return false, err
	}

	if err := e.agent.RemoveGoal(id); err != nil {
		log.Printf("[task] 取消任务 %s 失败: %v", id, err)
		e.mu.Lock()
		e.ids[id] = struct{}{}
		e.mu.Unlock()
		return false, err
	}
	e.mu.Lock()
	delete(e.ids, id)
	e.mu.Unlock()
	return true, nil
}

// List 返回当前所有待触发的任务（按下次触发时间升序）。
func (e *Engine) List() []Task {
	if e.reliable != nil {
		return e.reliable.list()
	}
	if e.waitReady() != nil {
		return nil
	}
	out := make([]Task, 0)
	for _, g := range e.agent.Goals() {
		if liveGoal(g) {
			out = append(out, taskView(g))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Next.Before(out[j].Next) })
	return out
}

// Len 返回当前待触发任务数量。
func (e *Engine) Len() int {
	if e.reliable != nil {
		return len(e.reliable.list())
	}
	if e.waitReady() != nil {
		return 0
	}
	n := 0
	for _, g := range e.agent.Goals() {
		if liveGoal(g) {
			n++
		}
	}
	return n
}

// Close 关闭引擎：停止框架主循环，之后 Schedule 返回 ErrClosed。
// 每次变更已实时落盘，无需额外快照。
func (e *Engine) Close() {
	if e.reliable != nil {
		e.reliable.close()
		return
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()
	e.readyOnce.Do(func() { close(e.ready) })
	e.cancel()
}

// onFire 是框架的 OnGoalExecute 回调：此刻框架已完成状态推进与落盘
// （先落盘后回调），在这里独立 goroutine 派发业务回调——回调可安全重入
// Schedule/Cancel/List，慢投递（通知渠道/LLM）也不会阻塞框架主循环。
func (e *Engine) onFire(g *autonomous.Goal, _ string) {
	snap := g.Snapshot()
	tk := taskView(snap)
	if snap.Schedule.Type == autonomous.ScheduleOneShot {
		tk.When = snap.NextRunAt // 计划触发时刻（此刻尚未被 reschedule 清零）
	} else {
		tk.When = time.Now() // 实际触发时刻
	}
	if e.cb != nil {
		go e.cb(tk)
	}
}

// onStatusChange 清理终态目标：框架终态只标记不删除，这里异步 RemoveGoal 把
// done/failed 目标移出调度器与存储（异步触发，避免与框架内部锁重入死锁）。
func (e *Engine) onStatusChange(g *autonomous.Goal, _, nu autonomous.GoalStatus) {
	switch nu {
	case autonomous.GoalStatusDone, autonomous.GoalStatusFailed, autonomous.GoalStatusCanceled:
		id := g.ID
		go func() {
			_ = e.agent.RemoveGoal(id)
			e.mu.Lock()
			delete(e.ids, id)
			e.mu.Unlock()
		}()
	}
}

// buildGoal 校验并把 Task 映射为框架 Goal。首次触发时刻不在此计算：AddGoal
// 依 Schedule 经框架触发器求得（once→At，允许过去时刻以立即补触发；repeat→
// now+Interval；cron→下一满足时刻）。
func buildGoal(t Task) (*autonomous.Goal, error) {
	if t.Status != "" && t.Status != StatusActive && t.Status != StatusPaused {
		return nil, fmt.Errorf("未知任务状态 %q", t.Status)
	}
	trigger := t.Kind
	switch t.Kind {
	case "":
		return nil, fmt.Errorf("未知任务类型 %q", t.Kind)
	case KindAgent:
		// agent 任务的定时方式由时间字段推断（三种触发器共用），Kind 保持 agent。
		switch {
		case !t.When.IsZero():
			trigger = KindOnce
		case t.Interval > 0:
			trigger = KindRepeat
		case t.Cron != "":
			trigger = KindCron
		default:
			return nil, ErrNoWhen
		}
	case KindOnce, KindRepeat, KindCron:
	default:
		return nil, fmt.Errorf("未知任务类型 %q", t.Kind)
	}

	g := autonomous.NewGoal()
	g.ID = t.ID
	g.Title = t.Text
	g.Description = t.Text // 普通提醒按 Description 直投、列表按 Title 展示
	g.Status = autonomous.GoalStatusActive
	if t.Status == StatusPaused {
		g.Status = autonomous.GoalStatusPaused
	}
	g.CreatedAt = t.CreatedAt
	g.SetMeta(metaKind, string(t.Kind))
	if t.Heartbeat {
		g.SetMeta(metaHeartbeat, "1")
	}
	if t.Channel != "" {
		g.SetMeta(metaChannel, t.Channel)
	}
	if t.Goal {
		g.SetMeta(metaGoal, "1")
	}
	if t.MaxSteps > 0 {
		g.SetMeta(metaGoalMaxSteps, strconv.Itoa(t.MaxSteps))
	}
	if t.MaxTokens > 0 {
		g.SetMeta(metaGoalMaxTokens, strconv.FormatInt(t.MaxTokens, 10))
	}
	if t.Skill != "" {
		g.SetMeta(metaGoalSkill, t.Skill)
	}
	if t.TodoID != "" {
		g.SetMeta(metaTodoID, t.TodoID)
	}
	if t.NotificationParentID != "" {
		g.SetMeta(metaNotificationParentID, t.NotificationParentID)
	}

	switch trigger {
	case KindOnce:
		if t.When.IsZero() {
			return nil, ErrNoWhen
		}
		// MaxRuns=1：触发过一次即视为已结束（落盘即时消失），重启绝不重发。
		g.Schedule = autonomous.Schedule{Type: autonomous.ScheduleOneShot, At: t.When, MaxRuns: 1}
	case KindRepeat:
		if t.Interval <= 0 {
			return nil, ErrNoInterval
		}
		if t.Times < 2 {
			return nil, ErrBadTimes
		}
		// Anchored：按计划时刻推进，不随执行延迟漂移（与旧引擎一致）；
		// RunCount=已触发次数，恢复时从文件里的剩余次数继续。
		g.Schedule = autonomous.Schedule{Type: autonomous.ScheduleInterval, Interval: t.Interval, MaxRuns: t.Times, Anchored: true}
		g.RunCount = t.Times - t.Remaining
	case KindCron:
		if _, err := cronParser.Parse(t.Cron); err != nil {
			return nil, fmt.Errorf("无效的 cron 表达式 %q: %w", t.Cron, err)
		}
		g.Schedule = autonomous.Schedule{Type: autonomous.ScheduleCron, Cron: t.Cron}
	}
	return g, nil
}

// liveGoal 判断目标是否仍保留：active/pending/paused 且次数未耗尽。repeat 刚触发完
// 最后一次、等待框架下轮标记 done 的窗口视为已结束，与"触发即移除"的对外
// 表现一致（List 不可见、落盘即时消失）。
func liveGoal(snap autonomous.Goal) bool {
	if snap.Status != autonomous.GoalStatusActive && snap.Status != autonomous.GoalStatusPending && snap.Status != autonomous.GoalStatusPaused {
		return false
	}
	return snap.Schedule.MaxRuns <= 0 || snap.RunCount < snap.Schedule.MaxRuns
}

// taskView 把目标快照投影成对外的 Task 视图（List、落盘与回调共用）。
func taskView(g autonomous.Goal) Task {
	tk := Task{
		ID:                   g.ID,
		Text:                 g.Title,
		Cron:                 g.Schedule.Cron,
		CreatedAt:            g.CreatedAt,
		Next:                 g.NextRunAt,
		Heartbeat:            g.Meta[metaHeartbeat] == "1",
		Channel:              g.Meta[metaChannel],
		TodoID:               g.Meta[metaTodoID],
		NotificationParentID: g.Meta[metaNotificationParentID],
		Goal:                 g.Meta[metaGoal] == "1",
		Skill:                g.Meta[metaGoalSkill],
		Status:               StatusActive,
	}
	// 预算字段解析失败按"未设置"处理（沿用旧数据缺字段的读法），
	// 执行器会回落到 cost.budget 的默认值，不会因为脏数据直接罢工。
	if n, err := strconv.Atoi(g.Meta[metaGoalMaxSteps]); err == nil && n > 0 {
		tk.MaxSteps = n
	}
	if n, err := strconv.ParseInt(g.Meta[metaGoalMaxTokens], 10, 64); err == nil && n > 0 {
		tk.MaxTokens = n
	}
	if g.Status == autonomous.GoalStatusPaused {
		tk.Status = StatusPaused
	}
	if k := Kind(g.Meta[metaKind]); k != "" {
		tk.Kind = k
	}
	switch g.Schedule.Type {
	case autonomous.ScheduleInterval:
		tk.Interval = g.Schedule.Interval
		tk.Times = g.Schedule.MaxRuns
		tk.Remaining = tk.Times - g.RunCount
		if tk.Kind == "" {
			tk.Kind = KindRepeat
		}
	case autonomous.ScheduleCron:
		if tk.Kind == "" {
			tk.Kind = KindCron
		}
	default:
		if tk.Kind == "" {
			tk.Kind = KindOnce
		}
		tk.When = g.NextRunAt
	}
	return tk
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
