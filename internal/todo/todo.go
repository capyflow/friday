// Package todo 实现待办清单：agent 通过 add_todo / list_todos / complete_todo /
// delete_todo 工具帮用户记录与处理生活琐事，管理后台"待办"页可直接增删改。
// 待办持久化为 JSON 文件（临时文件 + rename 原子替换，与任务/记忆同一容错策略），
// 重启不丢。
package todo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Todo 是一条待办事项。
type Todo struct {
	ID        string     `json:"id"`
	Text      string     `json:"text"`
	Done      bool       `json:"done"`
	CreatedAt time.Time  `json:"createdAt"`
	DoneAt    *time.Time `json:"doneAt,omitempty"`
}

// Store 是待办仓库。
type Store struct {
	mu      sync.Mutex
	path    string // 为空表示不持久化（测试用）
	todos   []Todo
	loadErr error // 已有文件不可读取时禁止覆盖，修复文件并重启后恢复
}

// NewStore 创建待办仓库并从 path 加载；仅文件不存在时以空仓库启动。
// 已有文件损坏或不可读时保留原文件，Err 和所有修改操作返回恢复错误。
func NewStore(path string) *Store {
	s := &Store{path: path}
	s.load()
	return s
}

// load 从文件加载待办，不把损坏或不可读文件误当作首次启动。
func (s *Store) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		_, statErr := os.Lstat(s.path)
		if !os.IsNotExist(err) || !os.IsNotExist(statErr) {
			s.loadErr = fmt.Errorf("读取待办文件 %q 失败，已保留原文件；修复文件后重启: %w", s.path, err)
		}
		return
	}
	var ts []Todo
	if err := json.Unmarshal(b, &ts); err != nil || strings.TrimSpace(string(b)) == "null" {
		if err == nil {
			err = fmt.Errorf("必须是待办数组，不能为 null")
		}
		s.loadErr = fmt.Errorf("待办文件 %q 损坏，已保留原文件；修复文件后重启: %w", s.path, err)
		return
	}
	ids := make(map[string]bool, len(ts))
	for i, td := range ts {
		if td.ID == "" || strings.TrimSpace(td.Text) == "" || ids[td.ID] {
			s.loadErr = fmt.Errorf("待办文件 %q 第 %d 条内容无效或 ID 重复，已保留原文件；修复文件后重启", s.path, i+1)
			return
		}
		ids[td.ID] = true
	}
	s.todos = ts
}

// Err 返回加载错误；列表调用方应检查它，避免把恢复失败显示为空列表。
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

// saveLocked 原子写回候选快照；成功后调用方才提交内存变更（须持有 mu）。
func (s *Store) saveLocked(todos []Todo) error {
	if s.loadErr != nil {
		return s.loadErr
	}
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(todos, "", "  ")
	if err != nil {
		return fmt.Errorf("编码待办失败: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建待办临时文件失败: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("写入待办临时文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("同步待办临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭待办临时文件失败: %w", err)
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return fmt.Errorf("替换待办文件失败: %w", err)
	}
	return nil
}

// Add 新增一条待办并落盘，返回新待办。
func (s *Store) Add(text string) (Todo, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Todo{}, fmt.Errorf("待办内容不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return Todo{}, s.loadErr
	}
	t := Todo{
		ID:        "t" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Text:      text,
		CreatedAt: time.Now(),
	}
	candidate := make([]Todo, len(s.todos)+1)
	copy(candidate, s.todos)
	candidate[len(s.todos)] = t
	if err := s.saveLocked(candidate); err != nil {
		return Todo{}, err
	}
	s.todos = candidate
	return t, nil
}

// List 返回全部待办：未完成在前（按创建时间正序），已完成在后（按完成时间倒序）。
func (s *Store) List() []Todo {
	s.mu.Lock()
	out := make([]Todo, len(s.todos))
	copy(out, s.todos)
	for i := range out {
		if out[i].DoneAt != nil {
			doneAt := *out[i].DoneAt
			out[i].DoneAt = &doneAt
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Done != out[j].Done {
			return !out[i].Done
		}
		if !out[i].Done {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].doneAt().After(out[j].doneAt())
	})
	return out
}

// Complete 标记/取消完成一条待办；未找到时返回 false，落盘失败不修改待办。
// ID 容忍 "#t..." 形式（list_todos 展示时带 # 前缀，模型会原样传回）。
func (s *Store) Complete(id string, done bool) (Todo, bool, error) {
	id = normalizeID(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return Todo{}, false, s.loadErr
	}
	for i, t := range s.todos {
		if t.ID == id {
			t.Done = done
			if done {
				now := time.Now()
				t.DoneAt = &now
			} else {
				t.DoneAt = nil
			}
			candidate := make([]Todo, len(s.todos))
			copy(candidate, s.todos)
			candidate[i] = t
			if err := s.saveLocked(candidate); err != nil {
				return Todo{}, false, err
			}
			s.todos = candidate
			if t.DoneAt != nil {
				doneAt := *t.DoneAt
				t.DoneAt = &doneAt
			}
			return t, true, nil
		}
	}
	return Todo{}, false, nil
}

// Delete 按 ID 删除一条待办，落盘失败时保留待办并返回错误。
func (s *Store) Delete(id string) (bool, error) {
	id = normalizeID(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return false, s.loadErr
	}
	for i, t := range s.todos {
		if t.ID == id {
			candidate := make([]Todo, 0, len(s.todos)-1)
			candidate = append(candidate, s.todos[:i]...)
			candidate = append(candidate, s.todos[i+1:]...)
			if err := s.saveLocked(candidate); err != nil {
				return false, err
			}
			s.todos = candidate
			return true, nil
		}
	}
	return false, nil
}

// CountPending 返回未完成待办条数（管理后台状态页展示用）。
func (s *Store) CountPending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.todos {
		if !t.Done {
			n++
		}
	}
	return n
}

// doneAt 返回完成时刻（空指针时退化为零值，排序用）。
func (t Todo) doneAt() time.Time {
	if t.DoneAt != nil {
		return *t.DoneAt
	}
	return time.Time{}
}

// normalizeID 去掉 ID 前的 "#" 与空白。
func normalizeID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "#")
}
