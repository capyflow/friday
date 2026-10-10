package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/todo"
)

func TestTodoToolsFlow(t *testing.T) {
	store := todo.NewStore("")
	add := &AddTodoTool{Store: store}
	list := &ListTodosTool{Store: store}
	done := &CompleteTodoTool{Store: store}
	del := &DeleteTodoTool{Store: store}

	res, err := add.Call(context.Background(), map[string]any{"text": "交房租"})
	if err != nil || !strings.Contains(res, "已记入待办") {
		t.Fatalf("add_todo: %v %q", err, res)
	}
	res, err = list.Call(context.Background(), nil)
	if err != nil || !strings.Contains(res, "交房租") {
		t.Fatalf("list_todos: %v %q", err, res)
	}
	// 未指明 ID 报错；错误 ID 提示未找到。
	if _, err := done.Call(context.Background(), nil); err == nil {
		t.Fatal("complete_todo 缺 id 应报错")
	}
	if res, _ := done.Call(context.Background(), map[string]any{"id": "#nope"}); !strings.Contains(res, "未找到") {
		t.Fatalf("complete_todo 错误 ID: %q", res)
	}
	// list 拿 ID → 完成 → 再删除。
	id := store.List()[0].ID
	if res, err := done.Call(context.Background(), map[string]any{"id": id}); err != nil || !strings.Contains(res, "已完成") {
		t.Fatalf("complete_todo: %v %q", err, res)
	}
	if res, _ := list.Call(context.Background(), nil); !strings.Contains(res, "（已完成）") {
		t.Fatalf("完成后列表应带标记: %q", res)
	}
	if res, _ := done.Call(context.Background(), map[string]any{"id": id, "done": false}); !strings.Contains(res, "重新打开") {
		t.Fatalf("重新打开: %q", res)
	}
	if res, _ := del.Call(context.Background(), map[string]any{"id": id}); !strings.Contains(res, "已删除") {
		t.Fatalf("delete_todo: %q", res)
	}
}

func TestTodoToolDeadlineAndAssociatedReminderFlow(t *testing.T) {
	dir := t.TempDir()
	store := todo.NewStore(filepath.Join(dir, "todos.json"))
	engine := task.NewReliableEngineFile(nil, filepath.Join(dir, "tasks.json"))
	defer engine.Close()
	if err := store.BindEngine(engine); err != nil {
		t.Fatal(err)
	}
	dueAt := time.Now().Add(3 * time.Hour).Format(time.RFC3339)
	result, err := (&AddTodoTool{Store: store}).Call(context.Background(), map[string]any{"text": "交房租", "dueAt": dueAt, "in": "10分钟"})
	if err != nil || !strings.Contains(result, "截止") || !strings.Contains(result, "提醒") {
		t.Fatalf("有截止和提醒的工具返回错误: %q %v", result, err)
	}
	td := store.List()[0]
	tk, found := engine.Get(td.ReminderTaskID)
	if !found || tk.TodoID != td.ID {
		t.Fatalf("工具没有关联同一待办: %+v", tk)
	}
	listed, err := (&ListTodosTool{Store: store}).Call(context.Background(), nil)
	if err != nil || !strings.Contains(listed, dueAt) || !strings.Contains(listed, "提醒") {
		t.Fatalf("列表缺少时间: %q %v", listed, err)
	}
	updated, err := (&UpdateTodoTool{Store: store}).Call(context.Background(), map[string]any{"id": td.ID, "text": "交水费", "dueAt": dueAt, "when": time.Now().Add(time.Hour).Format(time.RFC3339)})
	if err != nil || !strings.Contains(updated, "已更新") {
		t.Fatalf("更新工具失败: %q %v", updated, err)
	}
	if _, found := engine.Get(td.ReminderTaskID); found {
		t.Fatal("工具更新未取消旧提醒")
	}
	if _, err := (&CompleteTodoTool{Store: store}).Call(context.Background(), map[string]any{"id": td.ID}); err != nil || engine.Len() != 0 {
		t.Fatalf("完成工具未取消未来提醒: %v %+v", err, engine.List())
	}
}

func TestTodoToolRejectsAmbiguousOrGuessedTimes(t *testing.T) {
	tool := &AddTodoTool{Store: todo.NewStore("")}
	for _, args := range []map[string]any{
		{"text": "待办", "dueAt": "明天前"},
		{"text": "待办", "dueAt": "2026-10-11T08:00:00"},
		{"text": "待办", "remindAt": true},
		{"text": "待办", "in": "-10分钟"},
		{"text": "待办", "in": "1小时", "when": "2030-01-01T08:00:00+08:00"},
	} {
		if result, err := tool.Call(context.Background(), args); err == nil || result != "" {
			t.Fatalf("无效时间被接受: args=%v result=%q err=%v", args, result, err)
		}
	}
	if tool.Store.CountPending() != 0 {
		t.Fatal("无效时间创建了待办")
	}
}

func TestTodoToolDeadlineOnlyAndFullUpdateClearReminder(t *testing.T) {
	store := todo.NewStore("")
	engine := task.NewReliableEngineFile(nil, "")
	defer engine.Close()
	if err := store.BindEngine(engine); err != nil {
		t.Fatal(err)
	}
	tool := &AddTodoTool{Store: store}
	due := time.Now().Add(time.Hour).Format(time.RFC3339)
	if _, err := tool.Call(context.Background(), map[string]any{"text": "只有截止", "dueAt": due}); err != nil || engine.Len() != 0 {
		t.Fatalf("只有截止不应提醒: %v %+v", err, engine.List())
	}
	if _, err := tool.Call(context.Background(), map[string]any{"text": "提醒", "in": "10分钟"}); err != nil {
		t.Fatal(err)
	}
	for _, td := range store.List() {
		if td.ReminderTaskID != "" {
			if _, err := (&UpdateTodoTool{Store: store}).Call(context.Background(), map[string]any{"id": td.ID, "text": "取消提醒"}); err != nil {
				t.Fatal(err)
			}
			updated, _ := store.Get(td.ID)
			if updated.RemindAt != nil || updated.ReminderTaskID != "" || engine.Len() != 0 {
				t.Fatalf("完整更新未清除提醒: %+v %+v", updated, engine.List())
			}
		}
	}
}

func TestTodoToolsReportPersistenceFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	store := todo.NewStore(path)
	td, err := store.Add("必须保留")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() (string, error){
		"add": func() (string, error) {
			return (&AddTodoTool{Store: store}).Call(context.Background(), map[string]any{"text": "未保存"})
		},
		"complete": func() (string, error) {
			return (&CompleteTodoTool{Store: store}).Call(context.Background(), map[string]any{"id": td.ID})
		},
		"delete": func() (string, error) {
			return (&DeleteTodoTool{Store: store}).Call(context.Background(), map[string]any{"id": td.ID})
		},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := call()
			if err == nil || res != "" {
				t.Fatalf("失败不得返回成功文案: result=%q err=%v", res, err)
			}
		})
	}
	if got := store.List(); len(got) != 1 || got[0].Done || got[0].Text != "必须保留" {
		t.Fatalf("失败后待办应保持不变: %+v", got)
	}
}

func TestListTodosReportsLoadFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	if err := os.WriteFile(path, []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (&ListTodosTool{Store: todo.NewStore(path)}).Call(context.Background(), nil)
	if err == nil || res != "" {
		t.Fatalf("恢复失败不得显示空列表: result=%q err=%v", res, err)
	}
}
