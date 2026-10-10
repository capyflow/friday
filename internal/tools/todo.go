package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/todo"
)

// AddTodoTool 让 agent 往待办清单里记一件事。
type AddTodoTool struct {
	Store *todo.Store
}

func (t *AddTodoTool) Name() string { return "add_todo" }

func (t *AddTodoTool) Description() string {
	return "记录待办，可设置 dueAt 截止时间及 remindAt/when/in 关联提醒。截止时间不会自动提醒；明确要求提醒时同时设置提醒时间。" +
		"完成或删除待办会取消关联的未来提醒。遇到“明天前/周三”等相对日期，先调用 get_time 获取当前时间和本地时区，再生成 RFC3339，不能猜测日期。"
}

func (t *AddTodoTool) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": todoTimeProperties(),
		"required":   []string{"text"},
	}
}

func (t *AddTodoTool) Call(_ context.Context, args map[string]any) (string, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return "", fmt.Errorf("缺少参数 text")
	}
	dueAt, remindAt, err := parseTodoTimes(args)
	if err != nil {
		return "", err
	}
	td, err := t.Store.AddWithDeadline(text, dueAt, remindAt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已记入待办 #%s：%s%s（当前未完成 %d 项）", td.ID, td.Text, todoTiming(td), t.Store.CountPending()), nil
}

func todoTimeProperties() map[string]any {
	return map[string]any{
		"text":     map[string]any{"type": "string", "description": "待办内容"},
		"dueAt":    map[string]any{"type": []string{"string", "null"}, "description": "可选截止时间，带时区 RFC3339；仅截止，不自动创建提醒"},
		"remindAt": map[string]any{"type": []string{"string", "null"}, "description": "可选关联提醒时间，带时区 RFC3339，不能晚于截止时间"},
		"when":     map[string]any{"type": "string", "description": "remindAt 的别名；RFC3339，与 remindAt/in 互斥"},
		"in":       map[string]any{"type": "string", "description": "多久后提醒，如 10分钟、1h，与 remindAt/when 互斥"},
	}
}

func parseTodoTimes(args map[string]any) (*time.Time, *time.Time, error) {
	parse := func(key string) (*time.Time, error) {
		value, exists := args[key]
		if !exists || value == nil {
			return nil, nil
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s 必须是带时区的 RFC3339 字符串", key)
		}
		if strings.TrimSpace(text) == "" {
			return nil, nil
		}
		when, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return nil, fmt.Errorf("%s 必须是带时区的 RFC3339 时间: %w", key, err)
		}
		return &when, nil
	}
	dueAt, err := parse("dueAt")
	if err != nil {
		return nil, nil, err
	}
	var remindAt *time.Time
	count := 0
	for _, key := range []string{"remindAt", "when"} {
		when, err := parse(key)
		if err != nil {
			return nil, nil, err
		}
		if when != nil {
			count++
			remindAt = when
		}
	}
	if value, exists := args["in"]; exists && value != nil {
		text, ok := value.(string)
		if !ok {
			return nil, nil, fmt.Errorf("in 必须是时长字符串")
		}
		if strings.TrimSpace(text) != "" {
			count++
			duration, err := task.ParseHumanDuration(text)
			if err != nil || duration <= 0 {
				return nil, nil, fmt.Errorf("in 必须是正数时长，如 10分钟")
			}
			when := time.Now().Add(duration)
			remindAt = &when
		}
	}
	if count > 1 {
		return nil, nil, fmt.Errorf("remindAt、when、in 只能指定一个提醒时间")
	}
	return dueAt, remindAt, nil
}

func todoTiming(td todo.Todo) string {
	out := ""
	if td.DueAt != nil {
		out += "（截止 " + td.DueAt.Format(time.RFC3339) + "）"
	}
	if td.RemindAt != nil && !td.Done {
		out += "（提醒 " + td.RemindAt.Format(time.RFC3339) + "）"
	}
	return out
}

// UpdateTodoTool 完整替换内容与时间；未提供的时间被清除，先 list_todos 再保留需要的值。
type UpdateTodoTool struct{ Store *todo.Store }

func (t *UpdateTodoTool) Name() string { return "update_todo" }
func (t *UpdateTodoTool) Description() string {
	return "修改待办内容、截止和关联提醒。完整替换：dueAt/remindAt 不提供或 null 会移除对应时间；先 list_todos 读取并保留不需修改的时间。" +
		"可用 when/in 设置提醒，修改后旧关联提醒自动取消。相对日期先用 get_time 确认本地时间与时区。"
}
func (t *UpdateTodoTool) Schema() map[string]any {
	props := todoTimeProperties()
	props["id"] = map[string]any{"type": "string", "description": "待办 ID"}
	return map[string]any{"type": "object", "properties": props, "required": []string{"id", "text"}}
}
func (t *UpdateTodoTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	text, _ := args["text"].(string)
	if id == "" || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("缺少参数 id 或 text")
	}
	dueAt, remindAt, err := parseTodoTimes(args)
	if err != nil {
		return "", err
	}
	td, ok, err := t.Store.Update(id, text, dueAt, remindAt)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("未找到待办 #%s", id), nil
	}
	return fmt.Sprintf("已更新待办 #%s：%s%s", td.ID, td.Text, todoTiming(td)), nil
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
			out += fmt.Sprintf("- #%s（已完成）%s%s\n", td.ID, td.Text, todoTiming(td))
			continue
		}
		pending++
		out += fmt.Sprintf("- #%s %s%s\n", td.ID, td.Text, todoTiming(td))
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
