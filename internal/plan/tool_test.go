package plan

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/workdir"
)

func newPlanStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestCreatePlanToolCreatesRunnablePlan agent 立出来的计划要能直接跑起来：
// 步骤齐、状态待确认、目录记着。
func TestCreatePlanToolCreatesRunnablePlan(t *testing.T) {
	s := newPlanStore(t)
	tool := NewCreatePlanTool()
	tool.SetStore(s)
	tool.SetStarter(func(id string) error { return nil })

	out, err := tool.Call(context.Background(), map[string]any{
		"goal": "把导出功能补上",
		"steps": []any{
			map[string]any{"title": "找到导出入口", "detail": "搜 export 相关实现"},
			map[string]any{"title": "实现导出", "doneWhen": "能导出 CSV"},
			map[string]any{"title": "补测试", "doneWhen": "测试通过"},
		},
	})
	if err != nil {
		t.Fatalf("创建计划失败: %v", err)
	}
	if !strings.Contains(out, "3 步") {
		t.Fatalf("输出应说明步骤数:\n%s", out)
	}

	plans := s.List("all")
	if len(plans) != 1 {
		t.Fatalf("应有 1 份计划，实际 %d", len(plans))
	}
	p := plans[0]
	if p.Goal != "把导出功能补上" {
		t.Fatalf("目标不对: %q", p.Goal)
	}
	if len(p.Steps) != 3 {
		t.Fatalf("步骤数不对: %d", len(p.Steps))
	}
	if p.Steps[0].Title != "找到导出入口" || p.Steps[2].DoneWhen != "测试通过" {
		t.Fatalf("步骤内容不对: %+v", p.Steps)
	}
	if p.Steps[0].N != 1 || p.Steps[2].N != 3 {
		t.Fatalf("步骤序号不对: %+v", p.Steps)
	}
	if p.Session == "" {
		t.Fatal("计划应有自己的会话 key")
	}
	if p.State != StateAwaiting {
		t.Fatalf("状态应为待确认，实际 %s", p.State)
	}
}

// TestCreatePlanToolRecordsWorkdir 计划要记住发起时的工作目录：
// 否则步骤会在错地方干活（文件读写与命令都落到别处）。
func TestCreatePlanToolRecordsWorkdir(t *testing.T) {
	s := newPlanStore(t)
	tool := NewCreatePlanTool()
	tool.SetStore(s)
	tool.SetStarter(func(id string) error { return nil })

	dir := t.TempDir()
	ctx := workdir.With(context.Background(), dir)
	if _, err := tool.Call(ctx, map[string]any{
		"goal":       "改点东西",
		"steps":      []any{map[string]any{"title": "第一步"}},
		"auto_start": false,
	}); err != nil {
		t.Fatalf("创建计划失败: %v", err)
	}
	plans := s.List("all")
	if len(plans) != 1 {
		t.Fatalf("应有 1 份计划，实际 %d", len(plans))
	}
	if plans[0].Dir != dir {
		t.Fatalf("计划应记住工作目录 %s，实际 %q", dir, plans[0].Dir)
	}
}

// TestCreatePlanToolAutoStart  auto_start 默认开：立完就按步执行。
func TestCreatePlanToolAutoStart(t *testing.T) {
	s := newPlanStore(t)
	tool := NewCreatePlanTool()
	tool.SetStore(s)
	started := ""
	tool.SetStarter(func(id string) error {
		started = id
		return nil
	})
	if _, err := tool.Call(context.Background(), map[string]any{
		"goal":  "干活",
		"steps": []any{map[string]any{"title": "第一步"}},
	}); err != nil {
		t.Fatalf("创建计划失败: %v", err)
	}
	if started == "" {
		t.Fatal("默认应自动开始执行")
	}
	plans := s.List("all")
	if plans[0].ID != started {
		t.Fatalf("启动的应是刚建的计划: %s vs %s", started, plans[0].ID)
	}
}

// TestCreatePlanToolNoAutoStart auto_start=false 时只立计划，不启动。
func TestCreatePlanToolNoAutoStart(t *testing.T) {
	s := newPlanStore(t)
	tool := NewCreatePlanTool()
	tool.SetStore(s)
	started := false
	tool.SetStarter(func(id string) error {
		started = true
		return nil
	})
	out, err := tool.Call(context.Background(), map[string]any{
		"goal":       "干活",
		"steps":      []any{map[string]any{"title": "第一步"}},
		"auto_start": false,
	})
	if err != nil {
		t.Fatalf("创建计划失败: %v", err)
	}
	if started {
		t.Fatal("auto_start=false 时不该启动")
	}
	if !strings.Contains(out, "等你确认") {
		t.Fatalf("输出应说明要等确认:\n%s", out)
	}
}

func TestCreatePlanToolValidatesInput(t *testing.T) {
	s := newPlanStore(t)
	tool := NewCreatePlanTool()
	tool.SetStore(s)
	tool.SetStarter(func(id string) error { return nil })

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"缺目标", map[string]any{"steps": []any{map[string]any{"title": "a"}}}, "goal"},
		{"缺步骤", map[string]any{"goal": "g"}, "steps"},
		{"步骤没标题", map[string]any{"goal": "g", "steps": []any{map[string]any{"detail": "d"}}}, "title"},
	}
	for _, tc := range cases {
		_, err := tool.Call(context.Background(), tc.args)
		if err == nil {
			t.Fatalf("%s: 应报错", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: 报错应提到 %q，实际 %v", tc.name, tc.want, err)
		}
	}
}

// TestCreatePlanToolNeedsStore 没接上仓库时要如实报错，不能装作成功。
func TestCreatePlanToolNeedsStore(t *testing.T) {
	tool := NewCreatePlanTool()
	_, err := tool.Call(context.Background(), map[string]any{
		"goal":  "g",
		"steps": []any{map[string]any{"title": "a"}},
	})
	if err == nil {
		t.Fatal("没接上仓库应报错")
	}
	if !strings.Contains(err.Error(), "未启用") {
		t.Fatalf("报错应说清未启用: %v", err)
	}
}

// TestRunnerRunsStepsInPlanDir 计划执行时每一步都要落在计划自己的目录里。
func TestRunnerRunsStepsInPlanDir(t *testing.T) {
	s := newPlanStore(t)
	created, err := s.Create("目标", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := s.Update(created.ID, func(q *Plan) error {
		q.Steps = []Step{{N: 1, Title: "第一步", State: StepPending}}
		q.State = StateAwaiting
		q.Dir = dir
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	asker := &fakeAsker{replies: []string{"做完了"}}
	r := NewRunner(context.Background(), asker, s)
	if err := r.Start(created.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool {
		p, err := s.Get(created.ID)
		return err == nil && p.State == StateDone
	}, "步骤执行完成")
	if got := asker.dirAt(0); got != dir {
		t.Fatalf("步骤应在计划目录 %s 里执行，实际 %q", dir, got)
	}
}
