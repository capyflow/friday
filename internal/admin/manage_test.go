package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/notification"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/todo"
)

func manageTestServer(t *testing.T) (*Server, http.Handler, *http.Cookie, string, string) {
	t.Helper()
	s, _, _ := setupTestServer(t)
	dir := t.TempDir()
	tasksPath := filepath.Join(dir, "tasks.json")
	todosPath := filepath.Join(dir, "todos.json")
	s.engine = task.NewReliableEngineFile(nil, tasksPath)
	t.Cleanup(s.engine.Close)
	s.todos = todo.NewStore(todosPath)
	if err := s.todos.BindEngine(s.engine); err != nil {
		t.Fatal(err)
	}
	var err error
	s.notifications, err = notification.New(filepath.Join(dir, "notifications.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.Handler(), &http.Cookie{Name: sessionCookie, Value: s.newSession()}, tasksPath, todosPath
}

func TestTaskManagementHandlersPersistAndRejectUnownedFields(t *testing.T) {
	s, handler, cookie, tasksPath, _ := manageTestServer(t)
	when := time.Now().Add(time.Hour)
	old, err := s.engine.Schedule(task.Task{ID: "task", Text: "旧内容", Kind: task.KindOnce, When: when, NotificationParentID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"text": "新内容", "when": when.Format(time.RFC3339Nano), "channel": "web", "status": "paused", "id": "forged", "todoId": "forged", "notificationParentId": "forged"})
	unauth := setupTestRequest(handler, "PUT", "/api/tasks/task", body, "127.0.0.1:1234", "localhost", "", nil)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("编辑必须授权: %d", unauth.Code)
	}
	response := setupTestRequest(handler, "PUT", "/api/tasks/task", body, "127.0.0.1:1234", "localhost", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("编辑失败: %d %s", response.Code, response.Body)
	}
	got, _ := s.engine.Get(old.ID)
	if got.ID != old.ID || got.Status != task.StatusActive || got.TodoID != "" || got.NotificationParentID != "parent" || !got.CreatedAt.Equal(old.CreatedAt) || got.Text != "新内容" || got.Channel != "web" {
		t.Fatalf("HTTP编辑篡改身份或定义错误: %+v", got)
	}
	for _, action := range []string{"pause", "resume"} {
		r := setupTestRequest(handler, "POST", "/api/tasks/task/"+action, []byte(`{}`), "127.0.0.1:1234", "localhost", "", cookie)
		if r.Code != http.StatusOK {
			t.Fatalf("%s失败: %d %s", action, r.Code, r.Body)
		}
		got, _ := s.engine.Get(old.ID)
		want := task.StatusPaused
		if action == "resume" {
			want = task.StatusActive
		}
		if got.Status != want {
			t.Fatalf("%s状态错误: %+v", action, got)
		}
	}
	s.engine.Close()
	r := task.NewReliableEngineFile(nil, tasksPath)
	defer r.Close()
	got, ok := r.Get(old.ID)
	if !ok || got.Text != "新内容" || got.Channel != "web" || got.NotificationParentID != "parent" {
		t.Fatalf("HTTP变更未持久化: %+v", got)
	}
}

func TestTaskManagementHandlersPersistenceFailuresDoNotClaimSuccess(t *testing.T) {
	s, handler, cookie, path, _ := manageTestServer(t)
	when := time.Now().Add(time.Hour)
	old, err := s.engine.Schedule(task.Task{ID: "task", Text: "保留", Kind: task.KindOnce, When: when})
	if err != nil {
		t.Fatal(err)
	}
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"text": "不可保存", "when": when.Add(time.Hour).Format(time.RFC3339Nano)})
	for _, req := range []struct {
		method, path string
		body         []byte
	}{
		{"PUT", "/api/tasks/task", body}, {"POST", "/api/tasks/task/pause", []byte(`{}`)},
	} {
		r := setupTestRequest(handler, req.method, req.path, req.body, "127.0.0.1:1234", "localhost", "", cookie)
		if r.Code == http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte("error")) {
			t.Fatalf("保存失败不能HTTP成功: %d %s", r.Code, r.Body)
		}
	}
	got, _ := s.engine.Get(old.ID)
	if got.Text != old.Text || got.Status != old.Status || !got.When.Equal(old.When) {
		t.Fatalf("HTTP失败污染内存: %+v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
}

func TestTaskManagementHandlerRejectsLinkedTodoEditing(t *testing.T) {
	s, handler, cookie, _, _ := manageTestServer(t)
	when := time.Now().Add(time.Hour)
	linked, err := s.engine.Schedule(task.Task{ID: "linked", TodoID: "todo", Kind: task.KindOnce, Text: "源待办", When: when})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"text": "绕过待办修改", "when": when.Add(time.Hour).Format(time.RFC3339Nano)})
	r := setupTestRequest(handler, "PUT", "/api/tasks/linked", body, "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code == http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte("update_todo")) {
		t.Fatalf("关联任务应指向待办管理: %d %s", r.Code, r.Body)
	}
	got, _ := s.engine.Get(linked.ID)
	if got.Text != linked.Text || !got.When.Equal(linked.When) {
		t.Fatalf("拒绝编辑污染关联: %+v", got)
	}
}

func TestTodoHandlersCreateUpdateCompleteAndReportSaveFailure(t *testing.T) {
	s, handler, cookie, _, todosPath := manageTestServer(t)
	due, remind := time.Now().Add(2*time.Hour), time.Now().Add(time.Hour)
	body, _ := json.Marshal(map[string]any{"text": "交房租", "dueAt": due, "remindAt": remind})
	r := setupTestRequest(handler, "POST", "/api/todos", body, "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code != http.StatusOK {
		t.Fatalf("创建待办失败: %d %s", r.Code, r.Body)
	}
	td := s.todos.List()[0]
	if td.ReminderTaskID == "" || s.engine.Len() != 1 {
		t.Fatal("HTTP未建立关联提醒")
	}
	var before task.Task
	for _, tk := range s.engine.List() {
		before = tk
	}
	if allowed, err := s.todos.ShouldRemind(before); !allowed || err != nil {
		t.Fatalf("已同步提醒门卫异常: %v %v", allowed, err)
	}
	body, _ = json.Marshal(map[string]any{"text": "已修改", "dueAt": due, "remindAt": remind.Add(time.Minute)})
	r = setupTestRequest(handler, "PUT", "/api/todos/"+td.ID, body, "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code != http.StatusOK {
		t.Fatalf("编辑待办失败: %d %s", r.Code, r.Body)
	}
	if allowed, _ := s.todos.ShouldRemind(before); allowed {
		t.Fatal("更新后旧提醒门卫仍放行")
	}
	backup := todosPath + ".saved"
	if err := os.Rename(todosPath, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(todosPath, 0o700); err != nil {
		t.Fatal(err)
	}
	r = setupTestRequest(handler, "POST", "/api/todos/"+td.ID, []byte(`{"done":true}`), "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code == http.StatusOK || s.todos.List()[0].Done {
		t.Fatal("待办完成写盘失败不能成功或污染内存")
	}
	if err := os.Remove(todosPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, todosPath); err != nil {
		t.Fatal(err)
	}
	r = setupTestRequest(handler, "POST", "/api/todos/"+td.ID, []byte(`{"done":true}`), "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code != http.StatusOK || s.engine.Len() != 0 {
		t.Fatalf("完成待办未取消未来提醒: %d %s", r.Code, r.Body)
	}
}

func TestNotificationHTTPReadSnoozeAndTaskHistoryFilter(t *testing.T) {
	s, handler, cookie, _, _ := manageTestServer(t)
	tk := task.Task{ID: "notice-task", Kind: task.KindOnce, Text: "提醒", When: time.Now(), Next: time.Now()}
	if err := s.notifications.Enqueue(tk); err != nil {
		t.Fatal(err)
	}
	if _, err := s.notifications.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := s.notifications.List()[0]
	r := setupTestRequest(handler, "POST", "/api/notifications/"+n.ID+"/read", []byte(`{}`), "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code != http.StatusOK || s.notifications.Unread() != 0 {
		t.Fatalf("已读HTTP失败: %d %s", r.Code, r.Body)
	}
	for _, delay := range []string{`{"minutes":0}`, `{"minutes":10081}`, `{"minutes":1.5}`} {
		r = setupTestRequest(handler, "POST", "/api/notifications/"+n.ID+"/snooze", []byte(delay), "127.0.0.1:1234", "localhost", "", cookie)
		if r.Code == http.StatusOK || s.engine.Len() != 0 {
			t.Fatalf("非法延后分钟数不应创建任务: %s %d", delay, r.Code)
		}
	}
	r = setupTestRequest(handler, "POST", "/api/notifications/"+n.ID+"/snooze", []byte(`{"minutes":60}`), "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code != http.StatusOK || s.engine.Len() != 1 {
		t.Fatalf("延后HTTP失败: %d %s", r.Code, r.Body)
	}
	r = setupTestRequest(handler, "GET", "/api/notifications?taskId=other", nil, "127.0.0.1:1234", "localhost", "", cookie)
	if r.Code != http.StatusOK || bytes.Contains(r.Body.Bytes(), []byte(`"taskId":"notice-task"`)) {
		t.Fatalf("taskId历史过滤失败: %d %s", r.Code, r.Body)
	}
}

func TestTodoCompleteRejectsMalformedPayloadWithoutMutation(t *testing.T) {
	s, handler, cookie, _, _ := manageTestServer(t)
	td, err := s.todos.Add("不能被畸形请求完成")
	if err != nil {
		t.Fatal(err)
	}
	r := setupTestRequest(handler, "POST", "/api/todos/"+td.ID, []byte(`{"done":`), "127.0.0.1:1234", "localhost", "", cookie)
	got, _ := s.todos.Get(td.ID)
	if r.Code == http.StatusOK || got.Done {
		t.Fatalf("畸形请求不能完成待办: http=%d todo=%+v body=%s", r.Code, got, r.Body)
	}
}

func TestDisabledTodoManagementReturnsExplicitError(t *testing.T) {
	s, handler, cookie, _, _ := manageTestServer(t)
	s.todos = nil
	for _, request := range []struct {
		method, path string
		body         []byte
	}{
		{"PUT", "/api/todos/missing", []byte(`{"text":"待办"}`)},
		{"POST", "/api/todos/reconcile", []byte(`{}`)},
	} {
		r := setupTestRequest(handler, request.method, request.path, request.body, "127.0.0.1:1234", "localhost", "", cookie)
		if r.Code != http.StatusBadRequest || !bytes.Contains(r.Body.Bytes(), []byte("待办功能未启用")) {
			t.Fatalf("未启用时不能panic或假报成功: %d %s", r.Code, r.Body)
		}
	}
}
