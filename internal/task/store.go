package task

import (
	"encoding/json"
	"os"
	"time"
)

// fileStore 以 JSON 文件持久化任务快照。时间用 RFC3339（纳秒可选，整秒时输出
// 保持整洁）、时长用 Go duration 字符串，便于人工查看与编辑；写回采用
// "临时文件 + rename" 原子替换，掉电不会留下半个文件（目标环境是开发板）。
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
}

func newStoredTask(t Task) storedTask {
	st := storedTask{
		ID:        t.ID,
		Text:      t.Text,
		Kind:      t.Kind,
		Times:     t.Times,
		Remaining: t.Remaining,
		Cron:      t.Cron,
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

// toTask 校验并还原落盘条目；字段缺失或非法（如手工编辑出错）时返回 false 跳过。
func (st storedTask) toTask() (Task, bool) {
	t := Task{
		ID:        st.ID,
		Text:      st.Text,
		Kind:      st.Kind,
		Times:     st.Times,
		Remaining: st.Remaining,
		Cron:      st.Cron,
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
	default:
		return Task{}, false
	}
	return t, true
}
