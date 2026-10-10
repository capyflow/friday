package task

import (
	"context"
	"fmt"
	"log"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/autonomous"
)

// NewReliableEngineFile 把到期任务先交给持久化接收器，接收/写盘失败不消费次数。
// accept 应仅完成快速、幂等的持久化入队；LLM/网络投递由独立队列处理。
// 旧 NewEngineFile 保持兼容，生产入口使用此交付顺序避免丢失单次提醒。
func NewReliableEngineFile(accept func(context.Context, Task) error, path string) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	r := &reliableScheduler{file: &fileStore{path: path}, tasks: map[string]Task{}, accept: accept,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
	loaded, err := r.file.load()
	r.err = err
	needsMigration := false
	if err == nil {
		for _, t := range loaded {
			if t.Next.IsZero() {
				needsMigration = true
				t.Next, err = firstRun(t, time.Now())
				if err != nil {
					r.err = fmt.Errorf("恢复任务 %s: %w", t.ID, err)
					break
				}
			}
			r.tasks[t.ID] = t
		}
	}
	// 旧文件必须先落盘固定 Next，再开始接受任务；否则首次推进失败后重启
	// 会重新计算触发时刻，使同次通知的去重键变化。
	if r.err == nil && needsMigration {
		if err := r.file.save(taskValues(r.tasks)); err != nil {
			r.err = fmt.Errorf("保存任务计划迁移失败（保留原文件）: %w", err)
		}
	}
	e := &Engine{reliable: r}
	if r.err != nil {
		close(r.done)
	} else {
		go r.run()
	}
	return e
}

type reliableScheduler struct {
	mu     sync.Mutex
	file   *fileStore
	tasks  map[string]Task
	accept func(context.Context, Task) error
	err    error
	closed bool
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
}

func firstRun(t Task, now time.Time) (time.Time, error) {
	g, err := buildGoal(t)
	if err != nil {
		return time.Time{}, err
	}
	switch g.Schedule.Type {
	case autonomous.ScheduleOneShot:
		return t.When, nil
	case autonomous.ScheduleInterval:
		return now.Add(t.Interval), nil
	default:
		s, err := cronParser.Parse(t.Cron)
		if err != nil {
			return time.Time{}, err
		}
		next := s.Next(now)
		if next.IsZero() {
			return time.Time{}, fmt.Errorf("cron 没有下次触发时间")
		}
		return next, nil
	}
}

func (r *reliableScheduler) schedule(t Task) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return Task{}, r.err
	}
	if r.closed {
		return Task{}, ErrClosed
	}
	if t.ID == "" {
		t.ID = newID()
	}
	if _, ok := r.tasks[t.ID]; ok {
		return Task{}, ErrDuplicateID
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
	if t.Remaining < 1 || t.Remaining > t.Times {
		t.Remaining = t.Times
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	if t.Status == "" {
		t.Status = StatusActive
	}
	var err error
	t.Next, err = firstRun(t, time.Now())
	if err != nil {
		return Task{}, err
	}
	next := maps.Clone(r.tasks)
	next[t.ID] = t
	if err := r.commit(next); err != nil {
		return Task{}, err
	}
	r.signal()
	return t, nil
}

func (r *reliableScheduler) update(id string, t Task) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return Task{}, r.err
	}
	if r.closed {
		return Task{}, ErrClosed
	}
	old, ok := r.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("未找到任务 #%s", id)
	}
	if old.TodoID != "" {
		return Task{}, fmt.Errorf("关联待办提醒请使用 update_todo 修改待办内容和时间")
	}
	t.ID, t.CreatedAt, t.TodoID, t.Status, t.Heartbeat = old.ID, old.CreatedAt, old.TodoID, old.Status, old.Heartbeat
	t.NotificationParentID = old.NotificationParentID
	if t.Status == "" {
		t.Status = StatusActive
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
	t.Remaining = t.Times
	var err error
	if t.Next, err = firstRun(t, time.Now()); err != nil {
		return Task{}, err
	}
	// 正文/通道编辑不重置剩余次数或已定计划；改定时规则才开始新的计划。
	if old.Kind == t.Kind && old.When.Equal(t.When) && old.Interval == t.Interval && old.Times == t.Times && old.Cron == t.Cron {
		t.Next, t.Remaining = old.Next, old.Remaining
	}
	next := maps.Clone(r.tasks)
	next[id] = t
	if err := r.commit(next); err != nil {
		return Task{}, err
	}
	r.signal()
	return t, nil
}

func (r *reliableScheduler) setPaused(id string, paused bool) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return Task{}, r.err
	}
	if r.closed {
		return Task{}, ErrClosed
	}
	t, ok := r.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("未找到任务 #%s", id)
	}
	status := StatusActive
	if paused {
		status = StatusPaused
	}
	if t.Status == status || t.Status == "" && !paused {
		return t, nil
	}
	t.Status = status
	if !paused {
		var err error
		if t.Next, err = resumeRun(t, time.Now()); err != nil {
			return Task{}, err
		}
	}
	next := maps.Clone(r.tasks)
	next[id] = t
	if err := r.commit(next); err != nil {
		return Task{}, err
	}
	r.signal()
	return t, nil
}

// resumeRun: 过去的单次保留原时刻立即补触发；interval 跳到下一未来锚点，
// cron 重算下一未来时刻。跳过暂停期间的周期不消费剩余次数。
func resumeRun(t Task, now time.Time) (time.Time, error) {
	g, err := buildGoal(t)
	if err != nil {
		return time.Time{}, err
	}
	if t.Next.After(now) {
		return t.Next, nil
	}
	switch g.Schedule.Type {
	case autonomous.ScheduleOneShot:
		return t.When, nil
	case autonomous.ScheduleInterval:
		if t.Next.IsZero() {
			return now.Add(t.Interval), nil
		}
		next := t.Next.Add((now.Sub(t.Next)/t.Interval + 1) * t.Interval)
		return next, nil
	default:
		return firstRun(t, now)
	}
}

func (r *reliableScheduler) cancelTask(id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return false, r.err
	}
	if r.closed {
		return false, ErrClosed
	}
	if _, ok := r.tasks[id]; !ok {
		return false, nil
	}
	next := maps.Clone(r.tasks)
	delete(next, id)
	if err := r.commit(next); err != nil {
		return false, err
	}
	r.signal()
	return true, nil
}

func (r *reliableScheduler) commit(next map[string]Task) error {
	if err := r.file.save(taskValues(next)); err != nil {
		return err
	}
	r.tasks = next
	return nil
}

func taskValues(tasks map[string]Task) []Task {
	values := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		values = append(values, t)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Next.Equal(values[j].Next) {
			return values[i].ID < values[j].ID
		}
		return values[i].Next.Before(values[j].Next)
	})
	return values
}

func (r *reliableScheduler) list() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return taskValues(r.tasks)
}

func (r *reliableScheduler) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *reliableScheduler) close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	<-r.done
}

func (r *reliableScheduler) run() {
	defer close(r.done)
	for {
		wait := r.tick(time.Now())
		timer := time.NewTimer(wait)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-r.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (r *reliableScheduler) tick(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	wait := maxSleep
	for _, t := range taskValues(r.tasks) {
		if t.Status == StatusPaused {
			continue
		}
		if t.Next.After(now) {
			if d := t.Next.Sub(now); d < wait {
				wait = d
			}
			continue
		}
		if r.ctx.Err() != nil {
			return maxSleep
		}
		if r.accept != nil {
			if err := r.accept(r.ctx, t); err != nil {
				log.Printf("[task] 任务 %s 未可靠入队（保留待触发状态）: %v", t.ID, err)
				return 2 * time.Second
			}
		}
		next := maps.Clone(r.tasks)
		g, err := buildGoal(t)
		if err != nil {
			log.Printf("[task] 无法推进任务 %s: %v", t.ID, err)
			return 2 * time.Second
		}
		switch g.Schedule.Type {
		case autonomous.ScheduleOneShot:
			delete(next, t.ID)
		case autonomous.ScheduleInterval:
			t.Remaining--
			if t.Remaining <= 0 {
				delete(next, t.ID)
			} else {
				t.Next = t.Next.Add((now.Sub(t.Next)/t.Interval + 1) * t.Interval)
				next[t.ID] = t
			}
		default:
			s, _ := cronParser.Parse(t.Cron)
			t.Next = s.Next(now)
			next[t.ID] = t
		}
		if err := r.commit(next); err != nil {
			log.Printf("[task] 任务 %s 推进写盘失败（下一轮幂等重试）: %v", t.ID, err)
			return 2 * time.Second
		}
	}
	// 刚推进的循环任务也参与下次唤醒时间计算。
	for _, t := range r.tasks {
		if t.Status == StatusPaused {
			continue
		}
		if d := t.Next.Sub(now); d < wait {
			wait = d
		}
	}
	if wait <= 0 {
		return time.Millisecond
	}
	return wait
}
