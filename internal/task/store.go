package task

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/autonomous"
)

// fileStore 以 JSON 文件持久化任务快照。时间用 RFC3339（纳秒可选，整秒时输出
// 保持整洁）、时长用 Go duration 字符串，便于人工查看与编辑；写回采用
// "临时文件 + rename" 原子替换，掉电不会留下半个文件，适合无人值守的家用服务器。
type fileStore struct {
	path string
}

// load 读取任务列表；文件不存在或损坏时返回 nil（空启动，不阻塞机器人）。
func (s *fileStore) load() []Task {
	if s == nil || s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var stored []storedTask
	if err := json.Unmarshal(b, &stored); err != nil {
		return nil
	}
	tasks := make([]Task, 0, len(stored))
	for _, st := range stored {
		if t, ok := st.toTask(); ok {
			tasks = append(tasks, t)
		}
	}
	return tasks
}

// save 原子写回任务快照；未启用持久化（nil 或空路径）时为空操作。
func (s *fileStore) save(tasks []Task) error {
	if s == nil || s.path == "" {
		return nil
	}
	stored := make([]storedTask, 0, len(tasks))
	for _, t := range tasks {
		stored = append(stored, newStoredTask(t))
	}
	b, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// storedTask 是 Task 的落盘形态（不含派生字段 Next，启动时重算）。
type storedTask struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Kind      Kind   `json:"kind"`
	When      string `json:"when,omitempty"`      // once：触发时刻（RFC3339）
	Interval  string `json:"interval,omitempty"`  // repeat：间隔（如 "10m0s"、"1h30m0s"）
	Times     int    `json:"times,omitempty"`     // repeat：总次数
	Remaining int    `json:"remaining,omitempty"` // repeat：剩余次数
	Cron      string `json:"cron,omitempty"`      // cron：表达式
	CreatedAt string `json:"createdAt"`           // RFC3339
	// agent 任务扩展字段。
	Heartbeat bool   `json:"heartbeat,omitempty"`
	Channel   string `json:"channel,omitempty"`
}

func newStoredTask(t Task) storedTask {
	st := storedTask{
		ID:        t.ID,
		Text:      t.Text,
		Kind:      t.Kind,
		Times:     t.Times,
		Remaining: t.Remaining,
		Cron:      t.Cron,
		Heartbeat: t.Heartbeat,
		Channel:   t.Channel,
	}
	if !t.When.IsZero() {
		st.When = t.When.Format(time.RFC3339Nano)
	}
	if t.Interval > 0 {
		st.Interval = t.Interval.String()
	}
	if !t.CreatedAt.IsZero() {
		st.CreatedAt = t.CreatedAt.Format(time.RFC3339Nano)
	}
	return st
}

// ─── GoalStore 适配器 ───

// goalStore 把 JSON 任务文件适配成框架的 autonomous.GoalStore：磁盘格式保持
// []storedTask 不变（人工可读、旧文件直接可用），内存侧缓存 Goal 指针供框架
// 调度器复用（LoadAll/Get 返回与调度器共享的指针，框架就地推进状态）。
//
// 落盘只保留"存活"目标（active/pending 且次数未耗尽）：终态与已触发完的
// 单次/多次任务即时从文件消失，等价旧引擎"触发即移除"的落盘时序。
// mu 同时承担旧引擎 saveMu 的职责：串行化"写 tmp + rename"，避免并发落盘互踩。
type goalStore struct {
	mu    sync.Mutex
	file  *fileStore // nil 表示仅内存（不持久化）
	goals map[string]*autonomous.Goal
}

func newGoalStore(file *fileStore) *goalStore {
	return &goalStore{file: file, goals: make(map[string]*autonomous.Goal)}
}

// LoadAll 从文件恢复全部目标（框架启动时调用）：逐条经 buildGoal 重建，
// 非法行记日志跳过，不阻塞启动。
func (s *goalStore) LoadAll() ([]*autonomous.Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := s.file.load()
	goals := make([]*autonomous.Goal, 0, len(tasks))
	for _, t := range tasks {
		g, err := buildGoal(t)
		if err != nil {
			log.Printf("[task] 恢复任务 %s 失败: %v", t.ID, err)
			continue
		}
		s.goals[g.ID] = g
		goals = append(goals, g)
	}
	return goals, nil
}

// Save 框架每次状态推进后的落盘入口：存活目标更新缓存并写盘，非存活
// （终态或次数耗尽）目标从缓存与文件中即时移除。
func (s *goalStore) Save(g *autonomous.Goal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if liveGoal(g.Snapshot()) {
		s.goals[g.ID] = g
	} else {
		delete(s.goals, g.ID)
	}
	s.persistLocked()
	return nil
}

func (s *goalStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.goals, id)
	s.persistLocked()
	return nil
}

func (s *goalStore) Get(id string) (*autonomous.Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.goals[id]
	if !ok {
		return nil, fmt.Errorf("任务 %q 不存在", id)
	}
	return g, nil
}

// persistLocked 把当前存活目标整体快照原子写回文件。
func (s *goalStore) persistLocked() {
	if s.file == nil {
		return
	}
	tasks := make([]Task, 0, len(s.goals))
	for _, g := range s.goals {
		if snap := g.Snapshot(); liveGoal(snap) {
			tasks = append(tasks, taskView(snap))
		}
	}
	if err := s.file.save(tasks); err != nil {
		log.Printf("[task] 持久化失败: %v", err)
	}
}

// toTask 校验并还原落盘条目；字段缺失或非法（如手工编辑出错）时返回 false 跳过。
func (st storedTask) toTask() (Task, bool) {
	t := Task{
		ID:        st.ID,
		Text:      st.Text,
		Kind:      st.Kind,
		Times:     st.Times,
		Remaining: st.Remaining,
		Cron:      st.Cron,
		Heartbeat: st.Heartbeat,
		Channel:   st.Channel,
	}
	var err error
	if st.When != "" {
		if t.When, err = time.Parse(time.RFC3339Nano, st.When); err != nil {
			return Task{}, false
		}
	}
	if st.Interval != "" {
		if t.Interval, err = time.ParseDuration(st.Interval); err != nil {
			return Task{}, false
		}
	}
	if st.CreatedAt != "" {
		if t.CreatedAt, err = time.Parse(time.RFC3339Nano, st.CreatedAt); err != nil {
			return Task{}, false
		}
	}
	switch t.Kind {
	case KindOnce:
		if t.When.IsZero() {
			return Task{}, false
		}
	case KindRepeat:
		if t.Interval <= 0 || t.Times < 2 || t.Remaining < 1 {
			return Task{}, false
		}
	case KindCron:
		if t.Cron == "" {
			return Task{}, false
		}
	case KindAgent:
		// agent 任务须带定时字段（When/Interval/Cron 之一），具体校验交给 schedule()。
		if t.When.IsZero() && t.Interval <= 0 && t.Cron == "" {
			return Task{}, false
		}
	default:
		return Task{}, false
	}
	return t, true
}
