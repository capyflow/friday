// Package todo 实现待办清单：agent 通过 add_todo / list_todos / complete_todo /
// delete_todo 工具帮用户记录与处理生活琐事，管理后台"待办"页可直接增删改。
// 待办持久化为 JSON 文件（临时文件 + rename 原子替换，与任务/记忆同一容错策略），
// 重启不丢。
package todo

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
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
	mu    sync.Mutex
	path  string // 为空表示不持久化（测试用）
	todos []Todo
}

// NewStore 创建待办仓库并从 path 加载；文件不存在或损坏时空启动。
func NewStore(path string) *Store {
	s := &Store{path: path}
	s.load()
	return s
}

// load 从文件加载待办；损坏时忽略（与任务/记忆落盘相同的容错策略）。
func (s *Store) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var ts []Todo
	if err := json.Unmarshal(b, &ts); err != nil {
		log.Printf("[todo] 待办文件损坏，已忽略: %v", err)
		return
	}
	s.todos = ts
}

// saveLocked 原子写回（调用方须持有 mu）。
func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.MarshalIndent(s.todos, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[todo] 持久化失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[todo] 持久化失败: %v", err)
	}
}

// Add 新增一条待办并落盘，返回新待办。
func (s *Store) Add(text string) (Todo, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Todo{}, fmt.Errorf("待办内容不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := Todo{
		ID:        "t" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Text:      text,
		CreatedAt: time.Now(),
	}
	s.todos = append(s.todos, t)
	s.saveLocked()
	return t, nil
}

// List 返回全部待办：未完成在前（按创建时间正序），已完成在后（按完成时间倒序）。
func (s *Store) List() []Todo {
	s.mu.Lock()
	out := make([]Todo, len(s.todos))
	copy(out, s.todos)
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

// Complete 标记/取消完成一条待办，返回更新后的待办；未找到时返回 false。
// ID 容忍 "#t..." 形式（list_todos 展示时带 # 前缀，模型会原样传回）。
func (s *Store) Complete(id string, done bool) (Todo, bool) {
	id = normalizeID(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.todos {
		if t.ID == id {
			s.todos[i].Done = done
			if done {
				now := time.Now()
				s.todos[i].DoneAt = &now
			} else {
				s.todos[i].DoneAt = nil
			}
			s.saveLocked()
			return s.todos[i], true
		}
	}
	return Todo{}, false
}

// Delete 按 ID 删除一条待办，返回是否删除了。
func (s *Store) Delete(id string) bool {
	id = normalizeID(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.todos {
		if t.ID == id {
			s.todos = append(s.todos[:i], s.todos[i+1:]...)
			s.saveLocked()
			return true
		}
	}
	return false
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
