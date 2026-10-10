package tools

import (
	"context"
	"fmt"

	"github.com/capyflow/friday/internal/todo"
)

// AddTodoTool 让 agent 往待办清单里记一件事。
type AddTodoTool struct {
	Store *todo.Store
}

func (t *AddTodoTool) Name() string { return "add_todo" }

func (t *AddTodoTool) Description() string {
	return "往用户的待办清单里记一件事（无明确截止时间的琐事，如“周三前交房租”“记得买牛奶”）。" +
		"需要在特定时间提醒的事项不要用本工具，用 schedule_task；两者可配合：先记待办、再定提醒。"
}

func (t *AddTodoTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "待办内容，如：周三前交房租"},
		},
		"required": []string{"text"},
	}
}

func (t *AddTodoTool) Call(_ context.Context, args map[string]any) (string, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return "", fmt.Errorf("缺少参数 text")
	}
	td, err := t.Store.Add(text)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已记入待办 #%s：%s（当前未完成 %d 项）", td.ID, td.Text, t.Store.CountPending()), nil
}

// ListTodosTool 列出待办清单（未完成在前）。
type ListTodosTool struct {
	Store *todo.Store
}

func (t *ListTodosTool) Name() string { return "list_todos" }

func (t *ListTodosTool) Description() string {
	return "列出用户的待办清单（未完成在前，已完成附在末尾）。用户问“我有什么待办/今天要做什么”时使用。"
}

func (t *ListTodosTool) Schema() map[string]any { return map[string]any{"type": "object"} }

func (t *ListTodosTool) Call(_ context.Context, _ map[string]any) (string, error) {
	if err := t.Store.Err(); err != nil {
		return "", err
	}
	todos := t.Store.List()
	if len(todos) == 0 {
		return "待办清单是空的。", nil
	}
	out := "待办清单：\n"
	pending := 0
	for _, td := range todos {
		if td.Done {
			out += fmt.Sprintf("- #%s（已完成）%s\n", td.ID, td.Text)
			continue
		}
		pending++
		out += fmt.Sprintf("- #%s %s\n", td.ID, td.Text)
	}
	return out, nil
}

// CompleteTodoTool 标记（或取消）完成一条待办。
type CompleteTodoTool struct {
	Store *todo.Store
}

func (t *CompleteTodoTool) Name() string { return "complete_todo" }

func (t *CompleteTodoTool) Description() string {
	return "把一条待办标记为已完成。done=false 可重新打开（用户说“那个还没做完”时用）。" +
		"先用 list_todos 拿到待办 ID。"
}

func (t *CompleteTodoTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":   map[string]any{"type": "string", "description": "待办 ID（来自 list_todos）"},
			"done": map[string]any{"type": "boolean", "description": "true=完成（默认），false=重新打开"},
		},
		"required": []string{"id"},
	}
}

func (t *CompleteTodoTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	done := true
	if v, ok := args["done"].(bool); ok {
		done = v
	}
	td, ok, err := t.Store.Complete(id, done)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("未找到待办 #%s", id), nil
	}
	if done {
		return fmt.Sprintf("已完成待办 %q，还剩 %d 项未完成", td.Text, t.Store.CountPending()), nil
	}
	return fmt.Sprintf("已重新打开待办 %q", td.Text), nil
}

// DeleteTodoTool 按 ID 删除一条待办。
type DeleteTodoTool struct {
	Store *todo.Store
}

func (t *DeleteTodoTool) Name() string { return "delete_todo" }

func (t *DeleteTodoTool) Description() string {
	return "从待办清单删除一条待办（用户说“这条不用了/划掉”时用）。先用 list_todos 拿到待办 ID。"
}

func (t *DeleteTodoTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "待办 ID（来自 list_todos）"},
		},
		"required": []string{"id"},
	}
}

func (t *DeleteTodoTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	deleted, err := t.Store.Delete(id)
	if err != nil {
		return "", err
	}
	if deleted {
		return fmt.Sprintf("已删除待办 #%s", id), nil
	}
	return fmt.Sprintf("未找到待办 #%s", id), nil
}
