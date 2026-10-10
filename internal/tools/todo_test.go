package tools

import (
	"context"
	"os"
	"path/filepath"
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
