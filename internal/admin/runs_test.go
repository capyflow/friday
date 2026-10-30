package admin

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/plan"
	"github.com/capyflow/friday/internal/task"
)

func TestRunsDashboardAggregatesActiveWork(t *testing.T) {
	s, handler, token, _, _ := manageTestServer(t)
	store, err := plan.NewStore(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create("把项目文档补全", "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(p.ID, func(q *plan.Plan) error {
		q.State = plan.StateAwaiting
		q.Title = "补全项目文档"
		q.Steps = []plan.Step{{N: 1, Title: "梳理缺口", State: plan.StepPending}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.plans = store
	if _, err := s.engine.Schedule(task.Task{Kind: task.KindOnce, Text: "明天提醒我交房租", When: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	code, body := acpCall(t, handler, token, "GET", "/api/runs", nil)
	if code != http.StatusOK {
		t.Fatalf("运行看板应 200，实际 %d: %v", code, body)
	}
	plans, _ := body["plans"].([]any)
	if len(plans) != 1 {
		t.Fatalf("应回带 1 个活跃计划，实际 %v", body["plans"])
	}
	one, _ := plans[0].(map[string]any)
	if one["title"] != "补全项目文档" || one["state"] != "awaiting_confirm" || one["currentTitle"] != "梳理缺口" {
		t.Fatalf("计划视图不符: %v", one)
	}
	tasks, _ := body["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("应回带 1 条定时任务，实际 %v", body["tasks"])
	}
	counts, _ := body["counts"].(map[string]any)
	if counts["plans"] != float64(1) || counts["tasks"] != float64(1) {
		t.Fatalf("计数不符: %v", counts)
	}
	for _, key := range []string{"acp", "goals", "approvals", "failures"} {
		if _, ok := body[key].([]any); !ok {
			t.Fatalf("%s 应始终是数组，实际 %T", key, body[key])
		}
	}
}

func TestRunsDashboardWorksWithoutOptionalManagers(t *testing.T) {
	s, handler, token, _, _ := manageTestServer(t)
	s.acp = nil
	s.plans = nil
	s.planRunner = nil
	s.goalRunner = nil
	s.approver = nil
	code, body := acpCall(t, handler, token, "GET", "/api/runs", nil)
	if code != http.StatusOK {
		t.Fatalf("缺可选管理器时应 200，实际 %d: %v", code, body)
	}
	if _, ok := body["generatedAt"].(string); !ok {
		t.Fatalf("应回带 generatedAt: %v", body)
	}
}
