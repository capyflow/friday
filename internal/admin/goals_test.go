package admin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/task"
)

func goalsTestServer(t *testing.T) (*Server, http.Handler, *http.Cookie, *task.Engine) {
	t.Helper()
	s, _, cookie, tasksPath, _ := manageTestServer(t)
	eng := task.NewReliableEngineFile(nil, tasksPath)
	t.Cleanup(eng.Close)
	s.engine = eng
	s.goalRunner = agent.NewGoalRunner(nil, nil)
	s.goalRunner.ConfigureSlots(1)
	return s, s.Handler(), cookie, eng
}

func goalBody(t *testing.T, handler http.Handler, cookie *http.Cookie, method, path string, body []byte) (int, map[string]any) {
	t.Helper()
	rec := setupTestRequest(handler, method, path, body, "127.0.0.1:1234", "localhost", "", cookie)
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
	if envelope.Body == nil {
		envelope.Body = map[string]any{}
	}
	return rec.Code, envelope.Body
}

// TestGoalsListShowsProgressWithDefaults 目标列表带运行状态与生效中的预算
// （未显式配置时回落配置默认，前端才显示得出来）。
func TestGoalsListShowsProgressWithDefaults(t *testing.T) {
	_, handler, cookie, eng := goalsTestServer(t)
	if _, err := eng.Schedule(task.Task{ID: "g1", Text: "把年度报销办完", Kind: task.KindAgent,
		Goal: true, When: time.Now().Add(time.Hour), MaxSteps: 6}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Schedule(task.Task{ID: "r1", Text: "喝水", Kind: task.KindOnce,
		When: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	code, body := goalBody(t, handler, cookie, "GET", "/api/goals", nil)
	if code != http.StatusOK {
		t.Fatalf("列表失败: %d %v", code, body)
	}
	list, _ := body["goals"].([]any)
	if len(list) != 1 {
		t.Fatalf("只应列出目标: %+v", list)
	}
	g := list[0].(map[string]any)
	if g["id"] != "g1" || g["running"] != false {
		t.Errorf("目标视图不符: %+v", g)
	}
	if g["stepsLimit"].(float64) != 6 {
		t.Errorf("显式步数应显示: %+v", g)
	}
}

// TestGoalDetailRejectsNonGoal 拿普通任务的 ID 查目标要报错，不返回半成品。
func TestGoalDetailRejectsNonGoal(t *testing.T) {
	_, handler, cookie, eng := goalsTestServer(t)
	if _, err := eng.Schedule(task.Task{ID: "plain", Text: "喝水", Kind: task.KindOnce,
		When: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if code, _ := goalBody(t, handler, cookie, "GET", "/api/goals/plain", nil); code == http.StatusOK {
		t.Error("非目标任务不该出现在目标接口里")
	}
	if code, _ := goalBody(t, handler, cookie, "GET", "/api/goals/ghost", nil); code == http.StatusOK {
		t.Error("不存在的目标应报错")
	}
}

// TestStopGoalRequiresRunning 没在跑的目标点"停止这次推进"要明确拒绝，
// 并指引用户去用任务暂停/取消（两条语义不能混）。
func TestStopGoalRequiresRunning(t *testing.T) {
	_, handler, cookie, eng := goalsTestServer(t)
	if _, err := eng.Schedule(task.Task{ID: "g2", Text: "目标", Kind: task.KindAgent,
		Goal: true, When: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	code, body := goalBody(t, handler, cookie, "POST", "/api/goals/g2/stop", []byte(`{}`))
	if code == http.StatusOK {
		t.Fatal("未在跑的目标不该接受停止请求")
	}
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Fatalf("要给出可读原因: %+v", body)
	}
	// 详情接口此时可用（含该目标的执行轮次，空列表也要正常返回）
	if c2, b2 := goalBody(t, handler, cookie, "GET", "/api/goals/g2", nil); c2 != http.StatusOK {
		t.Errorf("详情应可查: %d %+v", c2, b2)
	}
}

// TestGoalRoutesAbsentWithoutRunner 目标运行器未装配时不挂载接口。
func TestGoalRoutesAbsentWithoutRunner(t *testing.T) {
	s, _, cookie, _, _ := manageTestServer(t)
	s.goalRunner = nil
	handler := s.Handler()
	if code, _ := goalBody(t, handler, cookie, "GET", "/api/goals", nil); code == http.StatusOK {
		t.Error("未装配目标运行器时 /api/goals 不应可用")
	}
}
