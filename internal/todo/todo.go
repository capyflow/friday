// Package todo 实现闪念清单：统一存放待办（kind=todo）与随手笔记（kind=note），
// agent 通过 add_todo / add_note 等工具帮用户记录与处理，管理后台"闪念"页可直接增删改。
// 条目持久化为 JSON 文件（临时文件 + rename 原子替换，与任务/记忆同一容错策略），
// 重启不丢；旧版 todos.json（无 kind 字段）加载时自动视为待办。
package todo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/task"
)

// 条目类型：todo 是待办（可带截止/提醒），note 是随手笔记。
const (
	KindTodo = "todo"
	KindNote = "note"
)

// Todo 是闪念清单里的一条条目（待办或笔记）。
type Todo struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"` // note / todo；旧文件空值在加载时迁移为 todo
	Text           string     `json:"text"`
	Pinned         bool       `json:"pinned,omitempty"`
	Tags           []string   `json:"tags,omitempty"` // 从文本 #标签 派生，保存时刷新
	Done           bool       `json:"done"`           // 仅 todo 有意义，note 恒为 false
	CreatedAt      time.Time  `json:"createdAt"`
	DoneAt         *time.Time `json:"doneAt,omitempty"`
	DueAt          *time.Time `json:"dueAt,omitempty"`          // 仅 todo
	RemindAt       *time.Time `json:"remindAt,omitempty"`       // 仅 todo
	ReminderTaskID string     `json:"reminderTaskId,omitempty"` // 仅 todo
}

var tagPattern = regexp.MustCompile(`#([\p{L}\p{N}_/-]+)`)

// parseTags 从文本提取 #标签（按出现顺序去重；无标签返回 nil）。
func parseTags(text string) []string {
	matches := tagPattern.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool, len(matches))
	tags := make([]string, 0, len(matches))
	for _, m := range matches {
		if tag := m[1]; !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

// Store 是待办仓库。
type Store struct {
	mu    sync.Mutex
	opMu  sync.Mutex // 业务变更串行；绝不在持有 mu 时调用任务引擎，避免调度门卫锁反转。
	path  string     // 为空表示不持久化（测试用）
	todos []Todo
	// lastCreated 是最近一次入库的 CreatedAt（墙钟，已剥离单调钟）。Windows 的
	// 系统时钟粒度粗（~15.6ms 一跳）且可能被 NTP 回拨，直接用 time.Now() 会让
	// 「后创建的条目时间更大」不成立；AddEntry 据此强制严格递增。
	lastCreated time.Time
	loadErr     error // 已有文件不可读取时禁止覆盖，修复文件并重启后恢复
	engine      *task.Engine
	syncErr     error
	syncing     bool
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
		// 旧版文件没有 kind 字段：全部视为待办（迁移随下次写盘落盘）。
		if td.Kind == "" {
			ts[i].Kind = KindTodo
		}
		if ts[i].Kind != KindNote && ts[i].Kind != KindTodo {
			s.loadErr = fmt.Errorf("待办文件 %q 第 %d 条类型 %q 无效，已保留原文件；修复文件后重启", s.path, i+1, ts[i].Kind)
			return
		}
		if (td.DueAt != nil && td.DueAt.IsZero()) || (td.RemindAt != nil && td.RemindAt.IsZero()) ||
			(td.RemindAt == nil) != (td.ReminderTaskID == "") {
			s.loadErr = fmt.Errorf("待办文件 %q 第 %d 条提醒关联无效，已保留原文件；修复文件后重启", s.path, i+1)
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
	if s.loadErr != nil {
		return s.loadErr
	}
	return s.syncErr
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
	return s.AddWithDeadline(text, nil, nil)
}

// List 返回全部待办（不含笔记）：未完成在前（按创建时间正序），已完成在后（按完成时间倒序）。
func (s *Store) List() []Todo {
	s.mu.Lock()
	out := make([]Todo, 0, len(s.todos))
	for _, td := range s.todos {
		if td.Kind == KindTodo {
			out = append(out, cloneTodo(td))
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Done != out[j].Done {
			return !out[i].Done
		}
		if !out[i].Done {
			// 只比墙钟（剥离单调钟），与 ListEntries 时间线同一纪律。
			return out[i].CreatedAt.Round(0).Before(out[j].CreatedAt.Round(0))
		}
		return out[i].doneAt().Round(0).After(out[j].doneAt().Round(0))
	})
	return out
}

// Complete 标记/取消完成一条待办；未找到时返回 false，落盘失败不修改待办。
// ID 容忍 "#t..." 形式（list_todos 展示时带 # 前缀，模型会原样传回）。
func (s *Store) Complete(id string, done bool) (Todo, bool, error) {
	return s.complete(id, done)
}

// Delete 按 ID 删除一条待办，落盘失败时保留待办并返回错误。
func (s *Store) Delete(id string) (bool, error) {
	return s.delete(id)
}

// CountPending 返回未完成待办条数（不含笔记；管理后台状态页与工具提示用）。
func (s *Store) CountPending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.todos {
		if t.Kind == KindTodo && !t.Done {
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
