package task

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGoalFieldsRoundTripThroughFile 目标的 goal/maxSteps/maxTokens/skill 必须穿过
// tasks.json 与 Goal.Meta 两条路径（列表视图来自 Goal，落盘来自文件store）。
func TestGoalFieldsRoundTripThroughFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := NewReliableEngineFile(nil, path)
	defer e.Close()

	when := time.Now().Add(time.Hour)
	in := Task{ID: "g1", Text: "把年度报销办完", Kind: KindAgent, When: when,
		Goal: true, MaxSteps: 6, MaxTokens: 80000, Skill: "单据整理"}
	out, err := e.Schedule(in)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if !out.Goal || out.MaxSteps != 6 || out.MaxTokens != 80000 || out.Skill != "单据整理" {
		t.Fatalf("Schedule 返回值丢了目标字段: %+v", out)
	}

	// 列表视图（经 autonomous Goal.Meta）
	list := e.List()
	if len(list) != 1 {
		t.Fatalf("列表应有 1 条: %+v", list)
	}
	got := list[0]
	if !got.Goal || got.MaxSteps != 6 || got.MaxTokens != 80000 || got.Skill != "单据整理" {
		t.Errorf("List 视图未还原目标字段: %+v", got)
	}
	if got.Kind != KindAgent {
		t.Errorf("目标必须是 agent 类型，实际 %s", got.Kind)
	}

	// 重启恢复（文件store）
	reopened := NewReliableEngineFile(nil, path)
	defer reopened.Close()
	rl := reopened.List()
	if len(rl) != 1 || !rl[0].Goal || rl[0].MaxSteps != 6 || rl[0].MaxTokens != 80000 || rl[0].Skill != "单据整理" {
		t.Fatalf("重启后目标字段丢失: %+v", rl)
	}
}

// TestGoalWithoutBudgetUsesDefaults 不填预算时字段留空（由执行器回落到配置默认值）。
func TestGoalWithoutBudgetUsesDefaults(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	if _, err := e.Schedule(Task{ID: "g2", Text: "目标", Kind: KindAgent,
		When: time.Now().Add(time.Hour), Goal: true}); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	got := e.List()[0]
	if !got.Goal || got.MaxSteps != 0 || got.MaxTokens != 0 || got.Skill != "" {
		t.Errorf("未配预算应保持为空: %+v", got)
	}
}

// TestStoredTaskRejectsBadGoal 落盘数据里的非法目标字段要整体报错，
// 而不是悄悄按 0 处理（0 步等于一步就停）。
func TestStoredTaskRejectsBadGoal(t *testing.T) {
	const when = "2026-10-14T12:00:00Z"
	st := storedTask{ID: "x", Kind: KindAgent, When: when, CreatedAt: when, Goal: true, MaxSteps: -1}
	if _, ok := st.toTask(); ok {
		t.Error("负步数预算应被拒绝")
	}
	st2 := storedTask{ID: "y", Kind: KindCron, Cron: "0 8 * * *", CreatedAt: when, Goal: true}
	if _, ok := st2.toTask(); ok {
		t.Error("goal=true 但不是 agent 任务应被拒绝（没有多轮执行器接管）")
	}
	st3 := storedTask{ID: "z", Kind: KindAgent, When: when, CreatedAt: when, Goal: true, MaxSteps: 5, MaxTokens: 1000, Skill: "s"}
	got, ok := st3.toTask()
	if !ok || !got.Goal || got.MaxSteps != 5 || got.MaxTokens != 1000 || got.Skill != "s" {
		t.Errorf("合法目标字段应还原: ok=%v %+v", ok, got)
	}
}

// TestLegacyTasksStillLoadWithoutGoalFields 旧 tasks.json 没有目标字段也要照常加载。
func TestLegacyTasksStillLoadWithoutGoalFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	legacy := `[{"id":"old","text":"喝水","kind":"once","when":"2026-10-14T10:00:00Z","createdAt":"2026-10-14T09:00:00Z"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewReliableEngineFile(nil, path)
	defer e.Close()
	list := e.List()
	if len(list) != 1 || list[0].Goal {
		t.Fatalf("旧任务应正常加载且非目标: %+v", list)
	}
}
