package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/capyflow/friday/internal/todo"
)

// 闪念笔记工具：与待办工具共用一个 todo.Store（kind=note 的条目）。
// 笔记是随手记的想法与资料片段，没有完成状态与提醒；要做的事走 add_todo。

// AddNoteTool 让 agent 往闪念里记一条随手笔记。
type AddNoteTool struct {
	Store *todo.Store
}

func (t *AddNoteTool) Name() string { return "add_note" }

func (t *AddNoteTool) Description() string {
	return "随手记一条笔记（想法、灵感、资料片段、备忘；无需完成的都记这里），文本里可用 #标签 归类（如 \"#工作 周报思路\"）。" +
		"明确要做的事（有完成动作、可能有截止时间）请用 add_todo，不要记成笔记。"
}

func (t *AddNoteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "笔记内容，支持 #标签 内联标记"},
		},
		"required": []string{"text"},
	}
}

func (t *AddNoteTool) Call(_ context.Context, args map[string]any) (string, error) {
	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("缺少参数 text")
	}
	note, err := t.Store.AddEntry(todo.KindNote, text, nil, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已记入笔记 #%s：%s", note.ID, note.Text), nil
}

// ListNotesTool 检索用户的笔记（新在前）。
type ListNotesTool struct {
	Store *todo.Store
}

func (t *ListNotesTool) Name() string { return "list_notes" }

func (t *ListNotesTool) Description() string {
	return "检索用户的笔记（随手记的闪念，置顶在前、新在前）。用户问\"我之前记过什么/那条笔记写了啥\"时使用；" +
		"q 按关键词过滤，tag 按标签过滤（不含 #），limit 限制返回条数（默认 20）。"
}

func (t *ListNotesTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q":     map[string]any{"type": "string", "description": "关键词，按笔记文本包含匹配"},
			"tag":   map[string]any{"type": "string", "description": "标签过滤，不含 #（来自 #标签 标记）"},
			"limit": map[string]any{"type": "number", "description": "最多返回条数，默认 20"},
		},
	}
}

func (t *ListNotesTool) Call(_ context.Context, args map[string]any) (string, error) {
	if err := t.Store.Err(); err != nil {
		return "", err
	}
	f := todo.EntryFilter{Kind: todo.KindNote}
	if q, ok := args["q"].(string); ok {
		f.Q = q
	}
	if tag, ok := args["tag"].(string); ok {
		f.Tag = strings.TrimPrefix(strings.TrimSpace(tag), "#")
	}
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	notes := t.Store.ListEntries(f)
	if len(notes) == 0 {
		return "没有找到匹配的笔记。", nil
	}
	out := "笔记：\n"
	for i, note := range notes {
		if i >= limit {
			out += fmt.Sprintf("（其余 %d 条未展示，可用 q/tag/limit 缩小范围）\n", len(notes)-limit)
			break
		}
		marker := ""
		if note.Pinned {
			marker = "【置顶】"
		}
		tags := ""
		if len(note.Tags) > 0 {
			tags = "（标签：" + strings.Join(note.Tags, "、") + "）"
		}
		out += fmt.Sprintf("- #%s %s%s%s\n", note.ID, marker, note.Text, tags)
	}
	return out, nil
}

// noteEntry 取一条笔记并确认类型；todo 条目报错指向对应待办工具。
func noteEntry(store *todo.Store, id string, todoTool string) (todo.Todo, error) {
	entry, ok := store.Get(id)
	if !ok {
		return todo.Todo{}, fmt.Errorf("未找到条目 #%s（先用 list_notes 拿 ID）", id)
	}
	if entry.Kind == todo.KindTodo {
		return todo.Todo{}, fmt.Errorf("#%s 是待办，请用 %s", entry.ID, todoTool)
	}
	return entry, nil
}

// UpdateNoteTool 完整替换一条笔记的文本。
type UpdateNoteTool struct {
	Store *todo.Store
}

func (t *UpdateNoteTool) Name() string { return "update_note" }

func (t *UpdateNoteTool) Description() string {
	return "修改一条笔记的内容（完整替换原文）。先用 list_notes 拿到笔记 ID；待办请用 update_todo。"
}

func (t *UpdateNoteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":   map[string]any{"type": "string", "description": "笔记 ID（来自 list_notes）"},
			"text": map[string]any{"type": "string", "description": "新的完整内容"},
		},
		"required": []string{"id", "text"},
	}
}

func (t *UpdateNoteTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	text, _ := args["text"].(string)
	if strings.TrimSpace(id) == "" || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("缺少参数 id 或 text")
	}
	if _, err := noteEntry(t.Store, id, "update_todo"); err != nil {
		return "", err
	}
	note, ok, err := t.Store.UpdateEntry(id, text, nil, nil)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("未找到条目 #%s", id), nil
	}
	return fmt.Sprintf("已更新笔记 #%s：%s", note.ID, note.Text), nil
}

// DeleteNoteTool 删除一条笔记。
type DeleteNoteTool struct {
	Store *todo.Store
}

func (t *DeleteNoteTool) Name() string { return "delete_note" }

func (t *DeleteNoteTool) Description() string {
	return "删除一条笔记（用户说\"这条笔记不用了/删掉\"时用）。先用 list_notes 拿到笔记 ID；待办请用 delete_todo。"
}

func (t *DeleteNoteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "笔记 ID（来自 list_notes）"},
		},
		"required": []string{"id"},
	}
}

func (t *DeleteNoteTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	if _, err := noteEntry(t.Store, id, "delete_todo"); err != nil {
		return "", err
	}
	deleted, err := t.Store.Delete(id)
	if err != nil {
		return "", err
	}
	if deleted {
		return fmt.Sprintf("已删除笔记 #%s", id), nil
	}
	return fmt.Sprintf("未找到条目 #%s", id), nil
}

// PinEntryTool 置顶/取消置顶一条闪念（笔记与待办通用）。
type PinEntryTool struct {
	Store *todo.Store
}

func (t *PinEntryTool) Name() string { return "pin_entry" }

func (t *PinEntryTool) Description() string {
	return "把一条笔记或待办置顶（网页闪念页固定显示在顶部）；pinned=false 取消置顶。先用 list_notes/list_todos 拿到 ID。"
}

func (t *PinEntryTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":     map[string]any{"type": "string", "description": "条目 ID（来自 list_notes 或 list_todos）"},
			"pinned": map[string]any{"type": "boolean", "description": "true=置顶（默认），false=取消置顶"},
		},
		"required": []string{"id"},
	}
}

func (t *PinEntryTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	pinned := true
	if v, ok := args["pinned"].(bool); ok {
		pinned = v
	}
	entry, ok, err := t.Store.SetPinned(id, pinned)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("未找到条目 #%s", id), nil
	}
	action := "已置顶"
	if !pinned {
		action = "已取消置顶"
	}
	return fmt.Sprintf("%s #%s：%s", action, entry.ID, entry.Text), nil
}
