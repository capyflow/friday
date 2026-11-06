package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/notification"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/todo"
)

func manageTestServer(t *testing.T) (*Server, http.Handler, string, string, string) {
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
	return s, s.Handler(), testAuthToken(t, s), tasksPath, todosPath
}

// testAuthToken 为测试初始化管理员账号并签发一个 JWT（替代旧的会话 Cookie）。
func testAuthToken(t *testing.T, s *Server) string {
	t.Helper()
	if s.accounts == nil {
		t.Fatal("测试服务器缺少账号仓库")
	}
	if s.accounts.Current().SetupRequired {
		if err := s.accounts.Initialize("owner", "test-password"); err != nil {
			t.Fatal(err)
		}
	}
	token, err := s.issueToken(s.accounts.Current().Username)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestTaskManagementHandlersPersistAndRejectUnownedFields(t *testing.T) {
	s, handler, token, tasksPath, _ := manageTestServer(t)
	when := time.Now().Add(time.Hour)
	old, err := s.engine.Schedule(task.Task{ID: "task", Text: "旧内容", Kind: task.KindOnce, When: when, NotificationParentID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"text": "新内容", "when": when.Format(time.RFC3339Nano), "channel": "web", "status": "paused", "id": "forged", "todoId": "forged", "notificationParentId": "forged"})
	unauth := setupTestRequest(handler, "PUT", "/api/tasks/task", body, "127.0.0.1:1234", "localhost", "", "")
	if unauth.Code != http.StatusForbidden {
		t.Fatalf("编辑必须授权: %d", unauth.Code)
	}
	response := setupTestRequest(handler, "PUT", "/api/tasks/task", body, "127.0.0.1:1234", "localhost", "", token)
	if response.Code != http.StatusOK {
		t.Fatalf("编辑失败: %d %s", response.Code, response.Body)
	}
	got, _ := s.engine.Get(old.ID)
	if got.ID != old.ID || got.Status != task.StatusActive || got.TodoID != "" || got.NotificationParentID != "parent" || !got.CreatedAt.Equal(old.CreatedAt) || got.Text != "新内容" || got.Channel != "web" {
		t.Fatalf("HTTP编辑篡改身份或定义错误: %+v", got)
	}
	for _, action := range []string{"pause", "resume"} {
		r := setupTestRequest(handler, "POST", "/api/tasks/task/"+action, []byte(`{}`), "127.0.0.1:1234", "localhost", "", token)
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
	s, handler, token, path, _ := manageTestServer(t)
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
		r := setupTestRequest(handler, req.method, req.path, req.body, "127.0.0.1:1234", "localhost", "", token)
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
	s, handler, token, _, _ := manageTestServer(t)
	when := time.Now().Add(time.Hour)
	linked, err := s.engine.Schedule(task.Task{ID: "linked", TodoID: "todo", Kind: task.KindOnce, Text: "源待办", When: when})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"text": "绕过待办修改", "when": when.Add(time.Hour).Format(time.RFC3339Nano)})
	r := setupTestRequest(handler, "PUT", "/api/tasks/linked", body, "127.0.0.1:1234", "localhost", "", token)
	if r.Code == http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte("update_todo")) {
		t.Fatalf("关联任务应指向待办管理: %d %s", r.Code, r.Body)
	}
	got, _ := s.engine.Get(linked.ID)
	if got.Text != linked.Text || !got.When.Equal(linked.When) {
		t.Fatalf("拒绝编辑污染关联: %+v", got)
	}
}

func TestTodoHandlersCreateUpdateCompleteAndReportSaveFailure(t *testing.T) {
	s, handler, token, _, todosPath := manageTestServer(t)
	due, remind := time.Now().Add(2*time.Hour), time.Now().Add(time.Hour)
	body, _ := json.Marshal(map[string]any{"text": "交房租", "dueAt": due, "remindAt": remind})
	r := setupTestRequest(handler, "POST", "/api/entries", body, "127.0.0.1:1234", "localhost", "", token)
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
	r = setupTestRequest(handler, "PUT", "/api/entries/"+td.ID, body, "127.0.0.1:1234", "localhost", "", token)
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
	r = setupTestRequest(handler, "POST", "/api/entries/"+td.ID, []byte(`{"done":true}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code == http.StatusOK || s.todos.List()[0].Done {
		t.Fatal("待办完成写盘失败不能成功或污染内存")
	}
	if err := os.Remove(todosPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, todosPath); err != nil {
		t.Fatal(err)
	}
	r = setupTestRequest(handler, "POST", "/api/entries/"+td.ID, []byte(`{"done":true}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK || s.engine.Len() != 0 {
		t.Fatalf("完成待办未取消未来提醒: %d %s", r.Code, r.Body)
	}
}

func TestNotificationHTTPReadSnoozeAndTaskHistoryFilter(t *testing.T) {
	s, handler, token, _, _ := manageTestServer(t)
	tk := task.Task{ID: "notice-task", Kind: task.KindOnce, Text: "提醒", When: time.Now(), Next: time.Now()}
	if err := s.notifications.Enqueue(tk); err != nil {
		t.Fatal(err)
	}
	if _, err := s.notifications.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := s.notifications.List()[0]
	r := setupTestRequest(handler, "POST", "/api/notifications/"+n.ID+"/read", []byte(`{}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK || s.notifications.Unread() != 0 {
		t.Fatalf("已读HTTP失败: %d %s", r.Code, r.Body)
	}
	for _, delay := range []string{`{"minutes":0}`, `{"minutes":10081}`, `{"minutes":1.5}`} {
		r = setupTestRequest(handler, "POST", "/api/notifications/"+n.ID+"/snooze", []byte(delay), "127.0.0.1:1234", "localhost", "", token)
		if r.Code == http.StatusOK || s.engine.Len() != 0 {
			t.Fatalf("非法延后分钟数不应创建任务: %s %d", delay, r.Code)
		}
	}
	r = setupTestRequest(handler, "POST", "/api/notifications/"+n.ID+"/snooze", []byte(`{"minutes":60}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK || s.engine.Len() != 1 {
		t.Fatalf("延后HTTP失败: %d %s", r.Code, r.Body)
	}
	r = setupTestRequest(handler, "GET", "/api/notifications?taskId=other", nil, "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK || bytes.Contains(r.Body.Bytes(), []byte(`"taskId":"notice-task"`)) {
		t.Fatalf("taskId历史过滤失败: %d %s", r.Code, r.Body)
	}
}

func TestTodoCompleteRejectsMalformedPayloadWithoutMutation(t *testing.T) {
	s, handler, token, _, _ := manageTestServer(t)
	td, err := s.todos.Add("不能被畸形请求完成")
	if err != nil {
		t.Fatal(err)
	}
	r := setupTestRequest(handler, "POST", "/api/entries/"+td.ID, []byte(`{"done":`), "127.0.0.1:1234", "localhost", "", token)
	got, _ := s.todos.Get(td.ID)
	if r.Code == http.StatusOK || got.Done {
		t.Fatalf("畸形请求不能完成待办: http=%d todo=%+v body=%s", r.Code, got, r.Body)
	}
}

func TestDisabledEntryManagementReturnsExplicitError(t *testing.T) {
	s, handler, token, _, _ := manageTestServer(t)
	s.todos = nil
	for _, request := range []struct {
		method, path string
		body         []byte
	}{
		{"PUT", "/api/entries/missing", []byte(`{"text":"待办"}`)},
		{"POST", "/api/entries/reconcile", []byte(`{}`)},
	} {
		r := setupTestRequest(handler, request.method, request.path, request.body, "127.0.0.1:1234", "localhost", "", token)
		if r.Code != http.StatusBadRequest || !bytes.Contains(r.Body.Bytes(), []byte("闪念功能未启用")) {
			t.Fatalf("未启用时不能panic或假报成功: %d %s", r.Code, r.Body)
		}
	}
}

func TestEntryHandlersNotePinAndFilters(t *testing.T) {
	_, handler, token, _, _ := manageTestServer(t)
	// 记一条笔记：kind 缺省才是 todo，笔记显式声明并自动派生标签。
	body, _ := json.Marshal(map[string]any{"kind": "note", "text": "#工作 周报思路"})
	r := setupTestRequest(handler, "POST", "/api/entries", body, "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(`"kind":"note"`)) {
		t.Fatalf("创建笔记失败: %d %s", r.Code, r.Body)
	}
	// 笔记不能完成。
	noteID := jsonGet(t, r.Body.Bytes(), "body", "entry", "id")
	r = setupTestRequest(handler, "POST", "/api/entries/"+noteID, []byte(`{"done":true}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusBadRequest || !bytes.Contains(r.Body.Bytes(), []byte("笔记没有完成状态")) {
		t.Fatalf("笔记完成应被拒绝: %d %s", r.Code, r.Body)
	}
	// 置顶。
	r = setupTestRequest(handler, "POST", "/api/entries/"+noteID+"/pin", []byte(`{"pinned":true}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte(`"pinned":true`)) {
		t.Fatalf("置顶失败: %d %s", r.Code, r.Body)
	}
	// 过滤：kind 与 tag。
	r = setupTestRequest(handler, "GET", "/api/entries?kind=note", nil, "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte("周报思路")) {
		t.Fatalf("kind 过滤失败: %d %s", r.Code, r.Body)
	}
	r = setupTestRequest(handler, "GET", "/api/entries?tag=%E5%B7%A5%E4%BD%9C", nil, "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK || !bytes.Contains(r.Body.Bytes(), []byte("周报思路")) {
		t.Fatalf("tag 过滤失败: %d %s", r.Code, r.Body)
	}
	// 删除。
	r = setupTestRequest(handler, "DELETE", "/api/entries/"+noteID, nil, "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK {
		t.Fatalf("删除笔记失败: %d %s", r.Code, r.Body)
	}
}

// jsonGet 从响应 JSON 里按路径取字符串字段（仅测试用，路径必须存在且为字符串）。
func jsonGet(t *testing.T, data []byte, path ...string) string {
	t.Helper()
	var current map[string]any
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	for i, key := range path {
		value, ok := current[key]
		if !ok {
			t.Fatalf("响应缺少字段 %q: %s", key, data)
		}
		if i == len(path)-1 {
			str, isStr := value.(string)
			if !isStr {
				t.Fatalf("字段 %q 不是字符串: %v", key, value)
			}
			return str
		}
		if current, ok = value.(map[string]any); !ok {
			t.Fatalf("字段 %q 不是对象: %v", key, value)
		}
	}
	return ""
}

// TestApprovalQuestionDecideHTTP 问答卡走同一条审批接口，但用 {"choose"} 回答：
// 列表要带出 kind/options（前端据此换卡片形状），乱填的选项要 400 且卡片仍在。
func TestApprovalQuestionDecideHTTP(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	s.approver = approval.New("confirm", nil, time.Minute)
	handler := s.Handler() // 审批路由按 approver 是否装配来挂，取 handler 前要先生好
	card := s.approver.RequestCard(approval.Card{
		Tool: "智能体提问", Summary: "范围怎么定？", Kind: approval.KindQuestion,
		Options: []approval.Option{{ID: "opt_a", Name: "方案 A", Kind: "allow_once"}},
		Questions: []approval.QuestionGroup{{Header: "范围", Question: "范围怎么定？",
			Options: []approval.QuestionOption{{Label: "方案 A", Description: "只拦新上传"}}}},
	}, 0)

	r := setupTestRequest(handler, "GET", "/api/approvals", nil, "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK {
		t.Fatalf("列表失败: %d %s", r.Code, r.Body)
	}
	for _, want := range []string{`"kind":"question"`, `"opt_a"`, `只拦新上传`} {
		if !bytes.Contains(r.Body.Bytes(), []byte(want)) {
			t.Errorf("列表缺少 %s: %s", want, r.Body)
		}
	}

	path := "/api/approvals/" + strconv.FormatInt(card.ID(), 10)
	r = setupTestRequest(handler, "POST", path, []byte(`{"choose":"nope"}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code == http.StatusOK {
		t.Fatalf("不存在的选项不该被接受: %s", r.Body)
	}
	if !bytes.Contains(r.Body.Bytes(), []byte("这张卡没有这个选项")) {
		t.Errorf("失效原因要说得清: %s", r.Body)
	}

	r = setupTestRequest(handler, "POST", path, []byte(`{"choose":"opt_a"}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK {
		t.Fatalf("作答失败: %d %s", r.Code, r.Body)
	}
	out, err := card.WaitDecision()
	if err != nil || out.OptionID != "opt_a" {
		t.Fatalf("应取回选中的选项: %+v %v", out, err)
	}
}

// TestApprovalQuestionTextHTTP 问答卡的自由文本走同一条接口的 {"text"}：
// 空文本拒绝，正常文本原样交给 agent。
func TestApprovalQuestionTextHTTP(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	s.approver = approval.New("confirm", nil, time.Minute)
	handler := s.Handler()
	card := s.approver.RequestCard(approval.Card{
		Tool: "智能体提问", Summary: "方案怎么定？", Kind: approval.KindQuestion,
		AllowText: true, TextPlaceholder: "也可以自己写",
		Options:   []approval.Option{{ID: "opt_a", Name: "方案 A"}},
		Questions: []approval.QuestionGroup{{Question: "方案怎么定？"}},
	}, 0)

	path := "/api/approvals/" + strconv.FormatInt(card.ID(), 10)
	r := setupTestRequest(handler, "POST", path, []byte(`{"text":"   "}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code == http.StatusOK {
		t.Fatalf("空文本不该被接受: %s", r.Body)
	}

	r = setupTestRequest(handler, "POST", path, []byte(`{"text":"自定义方案"}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK {
		t.Fatalf("文本作答失败: %d %s", r.Code, r.Body)
	}
	out, err := card.WaitDecision()
	if err != nil || !out.Allow || out.Text != "自定义方案" {
		t.Fatalf("应取回自由文本: %+v %v", out, err)
	}
}

// TestApprovalQuestionMultiChoiceHTTP 多选问答卡用 {"choices":[…]} 一次提交多条：
// 按提交顺序回到 Outcome.OptionIDs；空勾选与单选卡的 choices 都要 400。
func TestApprovalQuestionMultiChoiceHTTP(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	s.approver = approval.New("confirm", nil, time.Minute)
	handler := s.Handler()
	card := s.approver.RequestCard(approval.Card{
		Tool: "智能体提问", Summary: "带哪些模块？", Kind: approval.KindQuestion,
		MultiSelect: true,
		Options: []approval.Option{
			{ID: "opt_a", Name: "网关"}, {ID: "opt_b", Name: "存储"}, {ID: "opt_c", Name: "前端"},
		},
	}, 0)

	path := "/api/approvals/" + strconv.FormatInt(card.ID(), 10)
	r := setupTestRequest(handler, "POST", path, []byte(`{"choices":[]}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code == http.StatusOK {
		t.Fatalf("空勾选不该被接受: %s", r.Body)
	}
	r = setupTestRequest(handler, "POST", path, []byte(`{"choices":["opt_c","opt_a"]}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusOK {
		t.Fatalf("多选作答失败: %d %s", r.Code, r.Body)
	}
	out, err := card.WaitDecision()
	if err != nil || len(out.OptionIDs) != 2 || out.OptionIDs[0] != "opt_c" || out.OptionIDs[1] != "opt_a" {
		t.Fatalf("应按提交顺序取回两条勾选: %+v %v", out, err)
	}

	single := s.approver.RequestCard(approval.Card{
		Tool: "智能体提问", Kind: approval.KindQuestion,
		Options: []approval.Option{{ID: "opt_x", Name: "X"}},
	}, 0)
	path = "/api/approvals/" + strconv.FormatInt(single.ID(), 10)
	r = setupTestRequest(handler, "POST", path, []byte(`{"choices":["opt_x"]}`), "127.0.0.1:1234", "localhost", "", token)
	if r.Code == http.StatusOK {
		t.Fatalf("单选卡不该接受 choices: %s", r.Body)
	}
	s.approver.Decide(single.ID(), false)
}
