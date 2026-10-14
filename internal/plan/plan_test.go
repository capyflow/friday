package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/usage"
)

// fakeAsker 记录每次提问的文本与归属，按脚本返回回答或报错。
type fakeAsker struct {
	mu       sync.Mutex
	prompts  []string
	sessions []string
	runs     []string
	replies  []string
	err      error
	errFor   func(n int) error // 按第几次调用决定报错（模拟单步失败）
}

func (f *fakeAsker) AskWith(ctx context.Context, text string, _ []llm.Content, _ func(llm.Delta), _ ...agent.CallOpt) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, text)
	f.sessions = append(f.sessions, agent.SessionKeyFrom(ctx))
	f.runs = append(f.runs, agent.RunFrom(ctx))
	n := len(f.prompts)
	if f.errFor != nil {
		if err := f.errFor(n); err != nil {
			return "", err
		}
	}
	if f.err != nil {
		return "", f.err
	}
	if len(f.replies) == 0 {
		return "完成", nil
	}
	reply := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return reply, nil
}

func (f *fakeAsker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeAsker) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

func (f *fakeAsker) sessionAt(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.sessions) {
		return ""
	}
	return f.sessions[n]
}

func (f *fakeAsker) runAt(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.runs) {
		return ""
	}
	return f.runs[n]
}

// waitFor 轮询等待条件成立（执行器在协程里跑，测试不能假设同步完成）。
func waitFor(t *testing.T, cond func() bool, why string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", why)
}

func planJSON(steps int) string {
	raw := rawPlan{Title: "周末采购", Summary: "按清单买齐东西"}
	for i := 0; i < steps; i++ {
		raw.Steps = append(raw.Steps, rawStep{
			Title: fmt.Sprintf("步骤%d", i+1), Detail: "做事细节", DoneWhen: "买到为止",
		})
	}
	b, _ := json.Marshal(raw)
	return string(b)
}

// ---------- 解析 ----------

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"纯对象", `{"title":"a"}`, `{"title":"a"}`, true},
		{"代码围栏", "```json\n{\"title\":\"a\"}\n```", `{"title":"a"}`, true},
		{"前后有解释文字", "好的，这是计划：\n{\"title\":\"a\"}\n还需要我做什么吗？", `{"title":"a"}`, true},
		{"嵌套对象", `{"a":{"b":1}}`, `{"a":{"b":1}}`, true},
		{"字符串里含花括号", `{"title":"做完 {A} 就行"}`, `{"title":"做完 {A} 就行"}`, true},
		{"没有对象", "随便说了几句", "", false},
		{"花括号不配对", `{"title":"坏`, "", false},
	}
	for _, c := range cases {
		got, ok := ExtractJSON(c.in)
		if ok != c.ok || (c.ok && got != c.want) {
			t.Errorf("%s: ExtractJSON(%q) = (%q,%v)，期望 (%q,%v)", c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParse(t *testing.T) {
	p, err := Parse("买齐周末的东西", planJSON(3))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Title != "周末采购" || len(p.Steps) != 3 {
		t.Fatalf("解析结果不符: %+v", p)
	}
	if p.Steps[0].N != 1 || p.Steps[2].N != 3 || p.Steps[1].State != StepPending {
		t.Errorf("步骤序号/状态不符: %+v", p.Steps)
	}
	if p.Steps[0].DoneWhen != "买到为止" {
		t.Errorf("完成标准应带入: %+v", p.Steps[0])
	}

	for _, tc := range []struct {
		name, answer string
	}{
		{"空 steps", `{"title":"t","steps":[]}`},
		{"steps 超限", planJSON(maxSteps + 1)},
		{"缺 title", `{"steps":[{"title":"  "}]}`},
		{"不是 JSON", "我直接给个答案吧"},
		{"JSON 不完整", `{"title":"t","steps":[{"title":"a"}`},
	} {
		if _, err := Parse("goal", tc.answer); err == nil {
			t.Errorf("%s: 应判为无效计划", tc.name)
		}
	}
}

// ---------- 仓库 ----------

func TestStoreCreateUpdateReload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plans.json")
	s, err := NewStore(file)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	created, err := s.Create("买齐周末的东西", "web", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.State != StateDraft || created.Session != "plan-"+created.ID {
		t.Fatalf("新建计划不符: %+v", created)
	}

	if _, err := s.Update(created.ID, func(q *Plan) error {
		MergeInto(q, mustParse(t, planJSON(2)))
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reopened, err := NewStore(file)
	if err != nil {
		t.Fatalf("重新加载: %v", err)
	}
	got, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateAwaiting || len(got.Steps) != 2 || got.Goal != "买齐周末的东西" {
		t.Fatalf("重启后计划不符: %+v", got)
	}

	// 待确认的可查、active 过滤正确
	if len(reopened.List("awaiting")) != 1 || len(reopened.List("active")) != 1 {
		t.Errorf("状态过滤不符")
	}
}

func TestStoreUpdateRollsBackOnInvalidChange(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	p, _ := s.Create("目标", "web", "")
	if _, err := s.Update(p.ID, func(q *Plan) error {
		MergeInto(q, mustParse(t, planJSON(2)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	before, _ := s.Get(p.ID)
	if _, err := s.Update(p.ID, func(q *Plan) error {
		q.State = "nonsense"
		return nil
	}); err == nil {
		t.Fatal("非法状态应被拒绝")
	}
	after, _ := s.Get(p.ID)
	if after.State != before.State || len(after.Steps) != len(before.Steps) {
		t.Errorf("失败更新应完全回退: before=%+v after=%+v", before, after)
	}
}

func TestStoreLoadDowngradesRunning(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plans.json")
	// 手工造一份"正在执行"的历史数据（模拟执行中被 kill）
	raw := `{"plans":[{"id":"p1","goal":"g","origin":"web","session":"plan-p1","state":"running",
	  "steps":[{"n":1,"title":"第一步","state":"running"},{"n":2,"title":"第二步","state":"pending"}],
	  "createdAt":"2026-10-14T10:00:00Z","updatedAt":"2026-10-14T10:00:00Z"}]}`
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(file)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	p, _ := s.Get("p1")
	if p.State != StatePaused {
		t.Errorf("重启后 running 应降为 paused，实际 %s", p.State)
	}
	if !strings.Contains(p.Error, "重启") {
		t.Errorf("应说明中断原因: %q", p.Error)
	}
	if p.Steps[0].State != StepPending {
		t.Errorf("在跑步应退回待执行: %+v", p.Steps[0])
	}
}

func TestStoreLoadsDraftAsFailed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plans.json")
	raw := `{"plans":[{"id":"p1","goal":"g","state":"draft","createdAt":"2026-10-14T10:00:00Z"}]}`
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Get("p1")
	if p.State != StateFailed {
		t.Errorf("空草稿应判失败，实际 %s", p.State)
	}
}

func TestStoreCorruptFilePreserved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plans.json")
	original := "{ broken"
	if err := os.WriteFile(file, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(file)
	if err == nil {
		t.Fatal("损坏文件应报错")
	}
	if _, err := s.Create("目标", "web", ""); err == nil {
		t.Error("损坏状态下写回应失败（不用空数据覆盖）")
	}
	data, _ := os.ReadFile(file)
	if string(data) != original {
		t.Errorf("原文件应被保留，实际 %q", data)
	}
}

func TestStoreTrimsTerminalPlans(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	ids := []string{}
	for i := 0; i < maxPlans+10; i++ {
		p, err := s.Create(fmt.Sprintf("目标%d", i), "web", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
		if i%2 == 0 { // 一半设为终态
			if _, err := s.Update(p.ID, func(q *Plan) error { q.State = StateDone; return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	kept := len(s.List(""))
	if kept > maxPlans {
		t.Errorf("应淘汰终态记录到上限，实际保留 %d", kept)
	}
	// 未终态的必须都还在
	for i, id := range ids {
		if i%2 == 0 {
			continue
		}
		if _, err := s.Get(id); err != nil {
			t.Errorf("进行中的计划 %s 不该被淘汰（%v）", id, err)
		}
	}
}

// ---------- 规划 ----------

func TestPlannerDraftProducesAwaitingPlan(t *testing.T) {
	usage.Init("")
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	asker := &fakeAsker{replies: []string{planJSON(3)}}
	p := &Planner{Ask: asker, Store: s, MaxIterations: 8}

	plan0, fallback, err := p.Draft(context.Background(), "帮我准备周末采购", "web", "")
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if fallback != "" {
		t.Errorf("成功时不该有降级回答: %q", fallback)
	}
	if plan0.State != StateAwaiting || len(plan0.Steps) != 3 {
		t.Fatalf("计划不符: %+v", plan0)
	}
	if asker.count() != 1 {
		t.Errorf("规划应只调用一次模型，实际 %d", asker.count())
	}
	// 规划轮跑在自己的会话上、归到该计划的成本桶
	if asker.sessionAt(0) != plan0.Session {
		t.Errorf("会话应为 %s，实际 %s", plan0.Session, asker.sessionAt(0))
	}
	if asker.runAt(0) != "plan:"+plan0.ID {
		t.Errorf("成本归属应为 plan:%s，实际 %q", plan0.ID, asker.runAt(0))
	}
	// 步骤未确认前不得执行任何动作
	stored, _ := s.Get(plan0.ID)
	for _, st := range stored.Steps {
		if st.State != StepPending {
			t.Errorf("未确认计划不该有已执行步骤: %+v", st)
		}
	}
}

func TestPlannerRetriesOnceThenDowngrades(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	asker := &fakeAsker{replies: []string{"这不是 JSON", "还不是 JSON"}}
	p := &Planner{Ask: asker, Store: s}

	stored, answer, err := p.Draft(context.Background(), "目标", "web", "")
	if err != nil {
		t.Fatalf("降级不该报错: %v", err)
	}
	if stored != nil {
		t.Error("解析失败时不该返回可确认计划")
	}
	if answer != "还不是 JSON" {
		t.Errorf("应把模型原话当回答返回，实际 %q", answer)
	}
	if asker.count() != 2 {
		t.Errorf("应重试一次纠正提示，实际调用 %d 次", asker.count())
	}
	if !strings.Contains(asker.lastPrompt(), "无法解析为计划 JSON") {
		t.Errorf("第二次提问应带纠正说明，实际 %q", asker.lastPrompt())
	}
	list := s.List("")
	if len(list) != 1 || list[0].State != StateFailed {
		t.Fatalf("应留下一条失败计划说明原因: %+v", list)
	}
}

func TestPlannerPropagatesAskError(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	p := &Planner{Ask: &fakeAsker{err: errors.New("模型不可用")}, Store: s}
	if _, _, err := p.Draft(context.Background(), "目标", "web", ""); err == nil {
		t.Fatal("模型报错应上抛")
	}
	list := s.List("")
	if len(list) != 1 || !strings.Contains(list[0].Error, "模型不可用") {
		t.Errorf("应留下失败记录: %+v", list)
	}
}

// ---------- 执行 ----------

func TestRunnerExecutesStepsInOrder(t *testing.T) {
	usage.Init("")
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	created, _ := s.Create("准备周末采购", "web", "")
	if _, err := s.Update(created.ID, func(q *Plan) error {
		MergeInto(q, mustParse(t, planJSON(3)))
		q.State = StateAwaiting
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	asker := &fakeAsker{replies: []string{"已查清单", "已下单", "已通知家人"}}
	r := NewRunner(context.Background(), asker, s)

	if err := r.Start(created.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool {
		p, err := s.Get(created.ID)
		return err == nil && p.State == StateDone
	}, "三步执行完成")

	p, _ := s.Get(created.ID)
	if len(p.Steps) != 3 {
		t.Fatalf("步骤数不符: %+v", p)
	}
	for i, st := range p.Steps {
		if st.State != StepDone {
			t.Errorf("第 %d 步应为 done: %+v", i+1, st)
		}
		if st.Answer == "" {
			t.Errorf("第 %d 步应记录结果", i+1)
		}
	}
	if asker.count() != 3 {
		t.Errorf("应一步一问共 3 次，实际 %d", asker.count())
	}
	// 每步都跑在该计划的独立会话与成本桶上
	for i := 0; i < 3; i++ {
		if asker.sessionAt(i) != created.Session || asker.runAt(i) != "plan:"+created.ID {
			t.Errorf("第 %d 步归属不符: session=%s run=%s",
				i+1, asker.sessionAt(i), asker.runAt(i))
		}
	}
	// 后一步能看到前一步的结论（计划会话连续，但不含用户聊天记录）
	if !strings.Contains(asker.lastPrompt(), "已完成的步骤") {
		t.Errorf("最后一步应带已完成摘要，实际:\n%s", asker.lastPrompt())
	}
}

func TestRunnerStepFailureDoesNotBlockRest(t *testing.T) {
	usage.Init("")
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	created, _ := s.Create("目标", "web", "")
	if _, err := s.Update(created.ID, func(q *Plan) error {
		MergeInto(q, mustParse(t, planJSON(2)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	asker := &fakeAsker{errFor: func(n int) error {
		if n == 1 {
			return errors.New("第 1 步工具报错")
		}
		return nil
	}}
	r := NewRunner(context.Background(), asker, s)
	if err := r.Start(created.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool {
		p, err := s.Get(created.ID)
		return err == nil && p.State == StateDone
	}, "第一步失败也应跑完第二步")

	p, _ := s.Get(created.ID)
	if p.Steps[0].State != StepFailed || !strings.Contains(p.Steps[0].Error, "工具报错") {
		t.Errorf("第 1 步应记失败: %+v", p.Steps[0])
	}
	if p.Steps[1].State != StepDone {
		t.Errorf("第 2 步应照常执行: %+v", p.Steps[1])
	}

	// 失败步骤可重试，重试后再回到完成
	if err := r.RetryStep(created.ID, 1); err != nil {
		t.Fatalf("RetryStep: %v", err)
	}
	waitFor(t, func() bool {
		p, err := s.Get(created.ID)
		return err == nil && p.Steps[0].State == StepDone && p.State == StateDone
	}, "重试后第 1 步完成")
}

func TestRunnerStopsAtBudgetBoundary(t *testing.T) {
	usage.Init("")
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	created, _ := s.Create("目标", "web", "")
	if _, err := s.Update(created.ID, func(q *Plan) error {
		MergeInto(q, mustParse(t, planJSON(3)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	run := "plan:" + created.ID
	asker := &fakeAsker{}
	// 每步"花掉" 400 token：预算 500 → 第一步后就必须停在边界
	asker2 := &billingAsker{inner: asker, run: run, tokens: 400}
	r := NewRunner(context.Background(), asker2, s)
	r.PerPlanTokens = 300 // 每步记 400 token：第一步后就不该再开第二步

	if err := r.Start(created.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool {
		p, err := s.Get(created.ID)
		return err == nil && p.State == StatePaused
	}, "超预算应在步边界暂停")

	p, _ := s.Get(created.ID)
	if !strings.Contains(p.Error, "预算") {
		t.Errorf("暂停原因要写清楚: %+v", p)
	}
	if p.Steps[0].State != StepDone || p.Steps[1].State != StepPending {
		t.Errorf("已完成的第一步不该被回退，未执行的第二步也不该被跑: %+v", p.Steps)
	}
	if asker.count() != 1 {
		t.Errorf("超预算后不应继续调用模型，实际 %d 次", asker.count())
	}
}

func TestRunnerRejectsDoubleStartAndTerminalPlans(t *testing.T) {
	usage.Init("")
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	created, _ := s.Create("目标", "web", "")
	if _, err := s.Update(created.ID, func(q *Plan) error {
		MergeInto(q, mustParse(t, planJSON(2)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	asker := &gateAsker{inner: &fakeAsker{replies: []string{"答"}}, gate: block}
	r := NewRunner(context.Background(), asker, s)

	if err := r.Start(created.ID); err != nil {
		t.Fatalf("首次 Start: %v", err)
	}
	waitFor(t, func() bool { return r.Running(created.ID) }, "执行协程应已起")
	if err := r.Start(created.ID); err == nil {
		t.Error("同一计划不该双跑")
	}
	close(block)
	waitFor(t, func() bool {
		p, err := s.Get(created.ID)
		return err == nil && p.State == StateDone
	}, "计划完成")
	if err := r.Start(created.ID); err == nil {
		t.Error("已结束的计划不能再启动")
	}
}

func TestRunnerCancelIsTerminal(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "plans.json"))
	created, _ := s.Create("目标", "web", "")
	if _, err := s.Update(created.ID, func(q *Plan) error {
		MergeInto(q, mustParse(t, planJSON(2)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	r := NewRunner(context.Background(), &gateAsker{inner: &fakeAsker{}, gate: block}, s)
	if err := r.Start(created.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r.Running(created.ID) }, "执行中")
	if err := r.Cancel(created.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(block)
	waitFor(t, func() bool {
		p, err := s.Get(created.ID)
		return err == nil && p.State == StateCanceled
	}, "取消应为终态")
	p, _ := s.Get(created.ID)
	for _, st := range p.Steps {
		if st.State == StepPending || st.State == StepRunning {
			t.Errorf("取消后不该有待执行/在跑步: %+v", p.Steps)
		}
	}
}

// ---------- 测试替身 ----------

// billingAsker 在每次提问后往账本记一笔该计划的 token（模拟真实成本累计）。
type billingAsker struct {
	inner  *fakeAsker
	run    string
	tokens int64
	mu     sync.Mutex
	calls  int
}

func (b *billingAsker) AskWith(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta), opts ...agent.CallOpt) (string, error) {
	answer, err := b.inner.AskWith(ctx, text, imgs, onDelta, opts...)
	if err == nil {
		usage.Record(usage.Call{Model: "m", Session: "plan-x", Origin: agent.OriginPlan,
			Run: b.run, In: b.tokens / 2, Out: b.tokens / 2})
	}
	return answer, err
}

// gateAsker 卡住第一次提问，直到测试关闭 gate（验证"不双跑"与"取消"）。
type gateAsker struct {
	inner *fakeAsker
	gate  chan struct{}
	once  sync.Once
}

func (g *gateAsker) AskWith(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta), opts ...agent.CallOpt) (string, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return g.inner.AskWith(ctx, text, imgs, onDelta, opts...)
}

func mustParse(t *testing.T, answer string) *Plan {
	t.Helper()
	p, err := Parse("goal", answer)
	if err != nil {
		t.Fatalf("测试数据解析失败: %v", err)
	}
	return p
}
