package todo

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/capyflow/friday/internal/task"
)

// todos.json 是待办和提醒关联的单一源事实。先提交期望状态，再同步任务文件；
// 同步失败时明确保留已提交状态并关闭提醒门卫，启动/人工 Reconcile 可幂等恢复。
// opMu 串行化业务操作；mu 只保护快照，调用 Engine 时必须释放它。

func (s *Store) LoadError() error { s.mu.Lock(); defer s.mu.Unlock(); return s.loadErr }
func (s *Store) SyncError() error { s.mu.Lock(); defer s.mu.Unlock(); return s.syncErr }

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneTodo(td Todo) Todo {
	td.DoneAt, td.DueAt, td.RemindAt = cloneTime(td.DoneAt), cloneTime(td.DueAt), cloneTime(td.RemindAt)
	return td
}
func newReminderID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("生成提醒 ID 失败: %w", err)
	}
	return "todo-reminder-" + hex.EncodeToString(value[:]), nil
}
func sameTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

func (s *Store) Get(id string) (Todo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, td := range s.todos {
		if td.ID == normalizeID(id) {
			return cloneTodo(td), true
		}
	}
	return Todo{}, false
}

// BindEngine 在启动时恢复任务关联；accept 必须先接好 ShouldRemind 门卫再放开 worker。
func (s *Store) BindEngine(engine *task.Engine) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	s.engine = engine
	s.mu.Unlock()
	return s.reconcile()
}

func (s *Store) Reconcile() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.reconcile()
}

// ShouldRemind 不取 opMu，也不调用 Engine，允许在持任务引擎锁的 accept 中调用。
// 不属于待办的任务可正常执行；过期代际、已完成或已删除的关联任务被静默丢弃。
func (s *Store) ShouldRemind(tk task.Task) (bool, error) {
	if tk.TodoID == "" {
		return true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return false, s.loadErr
	}
	if s.syncErr != nil {
		return false, s.syncErr
	}
	if s.syncing || s.engine == nil {
		return false, fmt.Errorf("待办提醒尚未完成同步")
	}
	for _, td := range s.todos {
		if td.ID == tk.TodoID {
			return !td.Done && td.RemindAt != nil && td.ReminderTaskID == tk.ID &&
				td.RemindAt.Equal(tk.When) && td.RemindAt.Equal(tk.Next) && tk.Text == reminderText(td), nil
		}
	}
	return false, nil
}

func (s *Store) validate(text string, dueAt, remindAt *time.Time) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("待办内容不能为空")
	}
	if dueAt != nil && dueAt.IsZero() {
		return fmt.Errorf("截止时间无效")
	}
	if remindAt != nil {
		if !remindAt.After(time.Now()) {
			return fmt.Errorf("提醒时间必须在未来")
		}
		if dueAt != nil && remindAt.After(*dueAt) {
			return fmt.Errorf("提醒时间不能晚于截止时间")
		}
		s.mu.Lock()
		engine := s.engine
		s.mu.Unlock()
		if engine == nil {
			return fmt.Errorf("提醒服务未就绪，未保存待办")
		}
	}
	return nil
}

// AddWithDeadline 新增一条待办；截止时间只是待办属性，仅显式 remindAt 创建关联提醒。
func (s *Store) AddWithDeadline(text string, dueAt, remindAt *time.Time) (Todo, error) {
	return s.AddEntry(KindTodo, text, dueAt, remindAt)
}

// Update 是完整替换：nil 时间清除该时间字段；已有 ID、创建时间和完成状态保留。
func (s *Store) Update(id, text string, dueAt, remindAt *time.Time) (Todo, bool, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.validate(text, dueAt, remindAt); err != nil {
		return Todo{}, false, err
	}
	if err := s.ready(); err != nil {
		return Todo{}, false, err
	}
	s.mu.Lock()
	candidate := slices.Clone(s.todos)
	s.mu.Unlock()
	for i, td := range candidate {
		if td.ID != normalizeID(id) {
			continue
		}
		changed := td.Text != strings.TrimSpace(text) || !sameTime(td.DueAt, dueAt) || !sameTime(td.RemindAt, remindAt)
		td.Text, td.DueAt, td.RemindAt = strings.TrimSpace(text), cloneTime(dueAt), cloneTime(remindAt)
		td.Tags = parseTags(td.Text)
		if remindAt == nil {
			td.ReminderTaskID = ""
		} else if changed {
			var err error
			td.ReminderTaskID, err = newReminderID()
			if err != nil {
				return Todo{}, false, err
			}
		}
		candidate[i] = td
		if err := s.commit(candidate); err != nil {
			return Todo{}, false, err
		}
		if err := s.reconcile(); err != nil {
			return cloneTodo(td), true, committedSyncError(err)
		}
		return cloneTodo(td), true, nil
	}
	return Todo{}, false, nil
}

func (s *Store) complete(id string, done bool) (Todo, bool, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.ready(); err != nil {
		return Todo{}, false, err
	}
	s.mu.Lock()
	candidate := slices.Clone(s.todos)
	s.mu.Unlock()
	for i, td := range candidate {
		if td.ID != normalizeID(id) {
			continue
		}
		if td.Kind == KindNote {
			return Todo{}, false, fmt.Errorf("笔记没有完成状态，请直接编辑或删除")
		}
		if td.Done != done {
			td.Done = done
			if done {
				now := time.Now()
				td.DoneAt = &now
			} else {
				td.DoneAt = nil
				// 已过提醒时刻时重新打开不补发；新的未来提醒可通过编辑显式设定。
				if td.RemindAt != nil && td.RemindAt.After(time.Now()) {
					var err error
					td.ReminderTaskID, err = newReminderID()
					if err != nil {
						return Todo{}, false, err
					}
				} else {
					td.RemindAt, td.ReminderTaskID = nil, ""
				}
			}
		}
		candidate[i] = td
		if err := s.commit(candidate); err != nil {
			return Todo{}, false, err
		}
		if err := s.reconcile(); err != nil {
			return cloneTodo(td), true, committedSyncError(err)
		}
		return cloneTodo(td), true, nil
	}
	return Todo{}, false, nil
}

func (s *Store) delete(id string) (bool, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.ready(); err != nil {
		return false, err
	}
	s.mu.Lock()
	candidate := slices.Clone(s.todos)
	s.mu.Unlock()
	for i, td := range candidate {
		if td.ID != normalizeID(id) {
			continue
		}
		candidate = append(candidate[:i], candidate[i+1:]...)
		if err := s.commit(candidate); err != nil {
			return false, err
		}
		if err := s.reconcile(); err != nil {
			return true, committedSyncError(err)
		}
		return true, nil
	}
	return false, nil
}

func committedSyncError(err error) error {
	return fmt.Errorf("待办已保存，但提醒同步失败；修复任务存储后重试同步: %w", err)
}

func (s *Store) ready() error {
	s.mu.Lock()
	loadErr, syncErr := s.loadErr, s.syncErr
	s.mu.Unlock()
	if loadErr != nil {
		return loadErr
	}
	if syncErr != nil {
		return s.reconcile()
	}
	return nil
}

func (s *Store) commit(candidate []Todo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range candidate {
		candidate[i].Tags = parseTags(candidate[i].Text)
	}
	if err := s.saveLocked(candidate); err != nil {
		return err
	}
	s.todos = candidate
	s.syncing = true // 原子地发布新状态并关门，随后不持数据锁同步任务。
	return nil
}

func (s *Store) reconcile() (err error) {
	s.mu.Lock()
	if s.loadErr != nil {
		s.mu.Unlock()
		return s.loadErr
	}
	s.syncing = true
	engine := s.engine
	todos := slices.Clone(s.todos)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.syncing = false; s.syncErr = err; s.mu.Unlock() }()
	desired := make(map[string]Todo)
	for _, td := range todos {
		if !td.Done && td.RemindAt != nil {
			desired[td.ReminderTaskID] = td
		}
	}
	if engine == nil {
		if len(desired) > 0 {
			return fmt.Errorf("待办提醒服务未绑定")
		}
		return nil
	}
	if err := engine.Err(); err != nil {
		return err
	}
	existing := make(map[string]task.Task)
	for _, tk := range engine.List() {
		if tk.TodoID == "" {
			continue
		}
		td, wanted := desired[tk.ID]
		if !wanted || td.ID != tk.TodoID || !td.RemindAt.Equal(tk.When) || !td.RemindAt.Equal(tk.Next) || tk.Text != reminderText(td) {
			if _, err := engine.CancelErr(tk.ID); err != nil {
				return err
			}
			continue
		}
		existing[tk.ID] = tk
	}
	for id, td := range desired {
		if _, exists := existing[id]; exists {
			continue
		}
		_, err := engine.Schedule(task.Task{ID: id, TodoID: td.ID, Kind: task.KindOnce,
			Text: reminderText(td), When: *td.RemindAt, Channel: ""})
		if err != nil {
			return err
		}
	}
	return nil
}

func reminderText(td Todo) string {
	text := "待办提醒：" + td.Text
	if td.DueAt != nil {
		text += "（截止 " + td.DueAt.Format(time.RFC3339) + "）"
	}
	return text
}
