package tools

import (
	"context"
	"strings"
	"testing"

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
