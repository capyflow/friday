package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// load 读取任务列表；仅文件不存在时空启动，损坏或不可读文件保留并报错。
func (s *fileStore) load() ([]Task, error) {
	if s == nil || s.path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			if _, statErr := os.Lstat(s.path); os.IsNotExist(statErr) {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("读取任务文件 %q 失败，已保留原文件；修复文件后重启: %w", s.path, err)
	}
	var stored []storedTask
	if err := json.Unmarshal(b, &stored); err != nil || strings.TrimSpace(string(b)) == "null" {
		if err == nil {
			err = fmt.Errorf("必须是任务数组，不能为 null")
		}
		return nil, fmt.Errorf("任务文件 %q 损坏，已保留原文件；修复文件后重启: %w", s.path, err)
	}
	tasks := make([]Task, 0, len(stored))
	ids := make(map[string]bool, len(stored))
	for i, st := range stored {
		t, ok := st.toTask()
		if !ok || t.ID == "" || ids[t.ID] {
			return nil, fmt.Errorf("任务文件 %q 第 %d 条内容无效或 ID 重复，已保留原文件；修复文件后重启", s.path, i+1)
		}
		if _, err := buildGoal(t); err != nil {
			return nil, fmt.Errorf("任务文件 %q 第 %d 条无效，已保留原文件；修复文件后重启: %w", s.path, i+1, err)
		}
		ids[t.ID] = true
		tasks = append(tasks, t)
	}
	return tasks, nil
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
	f, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建任务临时文件失败: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("写入任务临时文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("同步任务临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭任务临时文件失败: %w", err)
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return fmt.Errorf("替换任务文件失败: %w", err)
	}
	return nil
}

// storedTask 是 Task 的落盘形态，保留 Next 以便重启后保持周期任务计划时刻。
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
	Next      string `json:"next,omitempty"`      // 下次计划触发时刻（旧文件可缺省）
	// agent 任务扩展字段。
	Heartbeat            bool   `json:"heartbeat,omitempty"`
	Channel              string `json:"channel,omitempty"`
	Status               string `json:"status,omitempty"`
	TodoID               string `json:"todoId,omitempty"`
	NotificationParentID string `json:"notificationParentId,omitempty"`
	// 目标模式扩展字段（goal 任务在 tasks.json 里与普通 agent 任务同处，
	// 避免"两处真相"：排程/暂停/编辑/重启补偿仍由同一个引擎负责）。
	Goal      bool   `json:"goal,omitempty"`
	MaxSteps  int    `json:"maxSteps,omitempty"`
	MaxTokens int64  `json:"maxTokens,omitempty"`
	Skill     string `json:"skill,omitempty"`
}

func newStoredTask(t Task) storedTask {
	st := storedTask{
		ID:                   t.ID,
		Text:                 t.Text,
		Kind:                 t.Kind,
		Times:                t.Times,
		Remaining:            t.Remaining,
		Cron:                 t.Cron,
		Heartbeat:            t.Heartbeat,
		Channel:              t.Channel,
		Status:               t.Status,
		TodoID:               t.TodoID,
		NotificationParentID: t.NotificationParentID,
		Goal:                 t.Goal,
		MaxSteps:             t.MaxSteps,
		MaxTokens:            t.MaxTokens,
		Skill:                t.Skill,
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
	if !t.Next.IsZero() {
		st.Next = t.Next.Format(time.RFC3339Nano)
	}
	return st
}

// ─── GoalStore 适配器 ───

// goalStore 把 JSON 任务文件适配成框架的 autonomous.GoalStore：磁盘格式保持
// []storedTask 不变（人工可读、旧文件直接可用），缓存已成功落盘的独立快照。
// LoadAll/Get 返回副本，框架推进 Goal 后 Save 失败不会污染已提交的内存状态。
//
// 落盘只保留"存活"目标（active/pending/paused 且次数未耗尽）：终态与已触发完的
// 单次/多次任务即时从文件消失，等价旧引擎"触发即移除"的落盘时序。
// mu 同时承担旧引擎 saveMu 的职责：串行化"写 tmp + rename"，避免并发落盘互踩。
type goalStore struct {
	mu      sync.Mutex
	file    *fileStore // nil 表示仅内存（不持久化）
	goals   map[string]*autonomous.Goal
	loaded  bool
	loadErr error
}

func newGoalStore(file *fileStore) *goalStore {
	return &goalStore{file: file, goals: make(map[string]*autonomous.Goal)}
}

// LoadAll 首次从文件恢复全部目标，此后返回已提交快照的副本。
func (s *goalStore) LoadAll() ([]*autonomous.Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	goals := make([]*autonomous.Goal, 0, len(s.goals))
	for _, g := range s.goals {
		snap := g.Snapshot()
		goals = append(goals, &snap)
	}
	return goals, nil
}

func (s *goalStore) loadLocked() error {
	if s.loaded {
		return s.loadErr
	}
	s.loaded = true
	tasks, err := s.file.load()
	if err != nil {
		s.loadErr = err
		return err
	}
	for _, t := range tasks {
		g, err := buildGoal(t)
		if err != nil {
			s.loadErr = fmt.Errorf("恢复任务 %q 失败，修复文件后重启: %w", t.ID, err)
			return s.loadErr
		}
		g.SetNextRunAt(t.Next)
		s.goals[g.ID] = g
	}
	return nil
}

func (s *goalStore) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Save 框架每次状态推进后的落盘入口：存活目标更新缓存并写盘，非存活
// （终态或次数耗尽）目标从缓存与文件中即时移除。
func (s *goalStore) Save(g *autonomous.Goal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	snap := g.Snapshot()
	previous, existed := s.goals[snap.ID]
	if liveGoal(snap) {
		s.goals[snap.ID] = &snap
	} else {
		delete(s.goals, snap.ID)
	}
	if err := s.persistLocked(); err != nil {
		if existed {
			s.goals[snap.ID] = previous
		} else {
			delete(s.goals, snap.ID)
		}
		return err
	}
	return nil
}

func (s *goalStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	previous, existed := s.goals[id]
	if !existed {
		return nil // 框架 RemoveGoal 在预先落盘删除后再次调用；无需重复写盘
	}
	delete(s.goals, id)
	if err := s.persistLocked(); err != nil {
		s.goals[id] = previous
		return err
	}
	return nil
}

func (s *goalStore) Get(id string) (*autonomous.Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	g, ok := s.goals[id]
	if !ok {
		return nil, fmt.Errorf("任务 %q 不存在", id)
	}
	snap := g.Snapshot()
	return &snap, nil
}

// persistLocked 把当前存活目标整体快照原子写回文件。
func (s *goalStore) persistLocked() error {
	if s.file == nil {
		return nil
	}
	tasks := make([]Task, 0, len(s.goals))
	for _, g := range s.goals {
		if snap := g.Snapshot(); liveGoal(snap) {
			if snap.Schedule.Type == autonomous.ScheduleOneShot && snap.NextRunAt.IsZero() {
				snap.NextRunAt = snap.Schedule.At
			}
			tasks = append(tasks, taskView(snap))
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return s.file.save(tasks)
}

// toTask 校验并还原落盘条目；字段缺失或非法时返回 false，load 据此拒绝覆盖文件。
func (st storedTask) toTask() (Task, bool) {
	t := Task{
		ID:                   st.ID,
		Text:                 st.Text,
		Kind:                 st.Kind,
		Times:                st.Times,
		Remaining:            st.Remaining,
		Cron:                 st.Cron,
		Heartbeat:            st.Heartbeat,
		Channel:              st.Channel,
		Status:               st.Status,
		TodoID:               st.TodoID,
		NotificationParentID: st.NotificationParentID,
		Goal:                 st.Goal,
		MaxSteps:             st.MaxSteps,
		MaxTokens:            st.MaxTokens,
		Skill:                st.Skill,
	}
	if t.Status == "" {
		t.Status = StatusActive
	}
	var err error
	if st.Next != "" {
		if t.Next, err = time.Parse(time.RFC3339Nano, st.Next); err != nil {
			return Task{}, false
		}
	}
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
	// 目标是"长程自主"的智能任务：脱离 agent 类型就没有多轮执行器接管；
	// 预算为负是配错，宁可拒绝也不要在运行时按 0 处理（那等于一步就停）。
	if t.Goal && t.Kind != KindAgent {
		return Task{}, false
	}
	if t.MaxSteps < 0 || t.MaxTokens < 0 {
		return Task{}, false
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
