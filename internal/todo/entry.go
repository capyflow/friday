package todo

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// 闪念扩展：统一条目（待办 note/todo）的增改、置顶、过滤列表与标签集合。
// 待办的提醒语义全部留在 reminder.go；这里对 note 条目忽略时间参数，不产生提醒关联。

// EntryFilter 过滤闪念列表；零值表示不过滤。
type EntryFilter struct {
	Kind string // "" / KindNote / KindTodo
	Q    string // 文本包含匹配（大小写不敏感）
	Tag  string // 命中该标签（不含 #）
	Done *bool  // 完成状态（仅对 todo 生效；nil 不过滤）
}

func cloneTodoSlice(in []Todo) []Todo {
	out := make([]Todo, len(in))
	for i, td := range in {
		out[i] = cloneTodo(td)
	}
	return out
}

// newNoteID 生成笔记 ID（n 前缀；待办沿用 reminder.go 的 t 前缀）。
func newNoteID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("生成条目 ID 失败: %w", err)
	}
	return "n" + hex.EncodeToString(value[:]), nil
}

// AddEntry 记入一条闪念：kind=todo 可带截止/提醒时间，kind=note 忽略时间参数。
func (s *Store) AddEntry(kind, text string, dueAt, remindAt *time.Time) (Todo, error) {
	if kind == "" {
		kind = KindTodo
	}
	if kind != KindTodo && kind != KindNote {
		return Todo{}, fmt.Errorf("未知的条目类型 %q", kind)
	}
	if kind == KindNote {
		dueAt, remindAt = nil, nil
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.validate(text, dueAt, remindAt); err != nil {
		return Todo{}, err
	}
	if err := s.ready(); err != nil {
		return Todo{}, err
	}
	s.mu.Lock()
	now := time.Now().Round(0) // 剥离单调钟：Windows 上其读数同样可能回退
	if !now.After(s.lastCreated) {
		now = s.lastCreated.Add(time.Nanosecond)
	}
	s.lastCreated = now
	s.mu.Unlock()
	td := Todo{Kind: kind, Text: strings.TrimSpace(text), CreatedAt: now,
		DueAt: cloneTime(dueAt), RemindAt: cloneTime(remindAt), Tags: parseTags(strings.TrimSpace(text))}
	if kind == KindNote {
		id, err := newNoteID()
		if err != nil {
			return Todo{}, err
		}
		td.ID = id
	} else {
		id, err := newReminderID()
		if err != nil {
			return Todo{}, err
		}
		td.ID = strings.Replace(id, "todo-reminder-", "t", 1)
		if remindAt != nil {
			td.ReminderTaskID = id
		}
	}
	s.mu.Lock()
	candidate := append(cloneTodoSlice(s.todos), td)
	s.mu.Unlock()
	if err := s.commit(candidate); err != nil {
		return Todo{}, err
	}
	if err := s.reconcile(); err != nil {
		return cloneTodo(td), committedSyncError(err)
	}
	return cloneTodo(td), nil
}

// UpdateEntry 修改一条闪念的文本与时间字段。kind 不可变：note 忽略并清空时间；
// 待办语义（提醒关联重建）与 Update 一致。未找到时返回 (Todo{}, false, nil)。
func (s *Store) UpdateEntry(id, text string, dueAt, remindAt *time.Time) (Todo, bool, error) {
	if td, ok := s.Get(id); ok && td.Kind == KindNote {
		dueAt, remindAt = nil, nil
	}
	return s.Update(id, text, dueAt, remindAt)
}

// SetPinned 置顶/取消置顶一条闪念（笔记与待办通用）；未找到返回 false。
func (s *Store) SetPinned(id string, pinned bool) (Todo, bool, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.ready(); err != nil {
		return Todo{}, false, err
	}
	s.mu.Lock()
	candidate := cloneTodoSlice(s.todos)
	s.mu.Unlock()
	for i, td := range candidate {
		if td.ID != normalizeID(id) {
			continue
		}
		if td.Pinned == pinned {
			return cloneTodo(td), true, nil
		}
		td.Pinned = pinned
		candidate[i] = td
		if err := s.commit(candidate); err != nil {
			return Todo{}, false, err
		}
		// 置顶不影响提醒，但 commit 会关门，仍需 reconcile 恢复同步状态。
		if err := s.reconcile(); err != nil {
			return cloneTodo(td), true, committedSyncError(err)
		}
		return cloneTodo(td), true, nil
	}
	return Todo{}, false, nil
}

// ListEntries 返回过滤后的闪念列表：置顶在前，其余按创建时间倒序（时间线视图）。
func (s *Store) ListEntries(f EntryFilter) []Todo {
	s.mu.Lock()
	out := cloneTodoSlice(s.todos)
	s.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(f.Q))
	filtered := make([]Todo, 0, len(out))
	for _, td := range out {
		if f.Kind != "" && td.Kind != f.Kind {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(td.Text), q) {
			continue
		}
		if f.Tag != "" && !slices.Contains(td.Tags, f.Tag) {
			continue
		}
		if f.Done != nil && td.Kind == KindTodo && td.Done != *f.Done {
			continue
		}
		filtered = append(filtered, td)
	}
	// 只比墙钟（Round(0) 剥离单调钟）：Windows 上单调钟读数可能回退，
	// 且 AddEntry 已保证墙钟严格递增，时间线顺序由入库先后唯一决定。
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Pinned != filtered[j].Pinned {
			return filtered[i].Pinned
		}
		return filtered[i].CreatedAt.Round(0).After(filtered[j].CreatedAt.Round(0))
	})
	return filtered
}

// Tags 返回全部条目出现过的标签（字母序，去重；供前端筛选 chips 用）。
func (s *Store) Tags() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]bool)
	for _, td := range s.todos {
		for _, tag := range td.Tags {
			seen[tag] = true
		}
	}
	out := make([]string, 0, len(seen))
	for tag := range seen {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}
