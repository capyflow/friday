package tools

import (
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/task"
)

// TestParseTaskArgsGoal 目标参数要落到任务字段，并强制 agent 类型
// （只有 agent 任务有"多轮自主推进"的执行器接管）。
func TestParseTaskArgsGoal(t *testing.T) {
	tk, err := ParseTaskArgs(map[string]any{
		"text": "把年度报销办完", "in": "10分钟", "goal": true,
		"maxSteps": float64(6), "maxTokens": float64(120000), "skill": "单据整理",
	})
	if err != nil {
		t.Fatalf("ParseTaskArgs: %v", err)
	}
	if !tk.Goal || tk.Kind != task.KindAgent {
		t.Errorf("goal=true 必须是 agent 任务: %+v", tk)
	}
	if tk.MaxSteps != 6 || tk.MaxTokens != 120000 || tk.Skill != "单据整理" {
		t.Errorf("目标预算未解析: %+v", tk)
	}

	// 只给 goal、没给 agent：仍按目标处理（用户说"把这事办成"时不该退回普通提醒）
	onlyGoal, err := ParseTaskArgs(map[string]any{"text": "把花养好", "in": "1小时", "goal": true})
	if err != nil {
		t.Fatalf("仅 goal: %v", err)
	}
	if onlyGoal.Kind != task.KindAgent || !onlyGoal.Goal {
		t.Errorf("goal 应隐含 agent 类型: %+v", onlyGoal)
	}
}

// TestParseTaskArgsRejectsBadGoalBudget 非法预算要报错，不能悄悄变成 0（0 步等于不推进）。
func TestParseTaskArgsRejectsBadGoalBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"负步数", map[string]any{"text": "x", "in": "1分钟", "goal": true, "maxSteps": float64(-1)}},
		{"非数字步数", map[string]any{"text": "x", "in": "1分钟", "goal": true, "maxSteps": "很多"}},
		{"负 token 预算", map[string]any{"text": "x", "in": "1分钟", "goal": true, "maxTokens": float64(-100)}},
	} {
		if _, err := ParseTaskArgs(tc.args); err == nil {
			t.Errorf("%s: 应被拒绝", tc.name)
		}
	}
	// 0 表示"不设预算，用配置默认"，应放行
	if _, err := ParseTaskArgs(map[string]any{"text": "x", "in": "1分钟", "goal": true, "maxSteps": float64(0)}); err != nil {
		t.Errorf("maxSteps=0 应按默认处理: %v", err)
	}
}

// TestScheduleTaskSchemaMentionsGoal 模型只有从 schema 才知道有目标模式可用。
func TestScheduleTaskSchemaMentionsGoal(t *testing.T) {
	schema := (&ScheduleTaskTool{}).Schema()
	props, _ := schema["properties"].(map[string]any)
	for _, key := range []string{"goal", "maxSteps", "maxTokens", "skill", "agent"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema 缺少参数 %s", key)
		}
	}
	if !strings.Contains((&ScheduleTaskTool{}).Description(), "目标") {
		t.Error("工具说明要提到目标模式，否则模型不会主动用")
	}
}
