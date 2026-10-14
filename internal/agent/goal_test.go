package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/usage"
)

// goalAsk 记录目标推进的每一步提问，并可按需向账本记 token（模拟真实成本累计）。
type goalAsk struct {
	mu       sync.Mutex
	prompts  []string
	sessions []string
	runs     []string
	noAuto   int
	skills   []string
	replies  []string
	err      error
	gate     chan struct{} // 非 nil：第一次提问卡在这里，用于测并发/停止
	onCall   func(n int)   // 每次提问后被回调（用于在第 N 步时请求停止）
	charge   int64         // 每次成功提问记进账本的 token 数
}

func (g *goalAsk) AskWith(ctx context.Context, text string, _ []llm.Content, _ func(llm.Delta), opts ...CallOpt) (string, error) {
	cfg := callCfg{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	g.mu.Lock()
	g.prompts = append(g.prompts, text)
	g.sessions = append(g.sessions, SessionKeyFrom(ctx))
	g.runs = append(g.runs, RunFrom(ctx))
	if cfg.noAuto {
		g.noAuto++
	}
	if cfg.skillName != "" {
		g.skills = append(g.skills, cfg.skillName)
	}
	n := len(g.prompts)
	gate := g.gate
	charge := g.charge
	g.mu.Unlock()

	if g.onCall != nil {
		g.onCall(len(g.prompts))
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if g.err != nil {
		return "", g.err
	}
	reply := "[继续] 又推进了一点"
	if len(g.replies) > 0 {
		reply = g.replies[0]
		g.mu.Lock()
		if len(g.replies) > 1 {
			g.replies = g.replies[1:]
		}
		g.mu.Unlock()
	}
	if charge > 0 {
		usage.Record(usage.Call{Model: "m", Origin: OriginTask, Run: RunFrom(ctx),
			In: charge / 2, Out: charge / 2})
	}
	_ = n
	return reply, nil
}

func (g *goalAsk) snapshot() (int, []string, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.prompts), append([]string(nil), g.sessions...), append([]string(nil), g.runs...)
}

func (g *goalAsk) isolation() (noAuto int, skills []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.noAuto, append([]string(nil), g.skills...)
}

// allowGate 是不拦主动任务的闸门。
type allowGate struct {
	allowed bool
	reason  string
}

func (a allowGate) ProactiveAllowed(string) (bool, string) { return a.allowed, a.reason }

func goalTask() task.Task {
	return task.Task{ID: "g1", Text: "把年度报销办完", Kind: task.KindAgent, Goal: true,
		When: time.Now().Add(time.Hour)}
}

func newTestGoalRunner(ask AskerLike) *GoalRunner {
	r := NewGoalRunner(ask, nil)
	r.ConfigureSlots(1)
	return r
}

// AskerLike 是 GoalRunner 需要的对话能力（dialogue 的别名，测试里更好读）。
type AskerLike = dialogue

func TestGoalRunnerAchievesAndPushes(t *testing.T) {
	usage.Init("")
	ask := &goalAsk{replies: []string{"[达成] 已提交 12 张单据，全部通过"}}
	r := newTestGoalRunner(ask)

	text, push, err := r.Prepare(context.Background(), goalTask())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !push {
		t.Error("达成应推送给用户")
	}
	if !strings.Contains(text, "目标已完成") || !strings.Contains(text, "已提交 12 张单据") {
		t.Errorf("结论不符: %q", text)
	}
	calls, sessions, runs := ask.snapshot()
	if calls != 1 {
		t.Errorf("一步就达成，不该再问，实际 %d 次", calls)
	}
	if sessions[0] != SessionGoal+"g1" {
		t.Errorf("目标要跑在隔离会话上，实际 %q", sessions[0])
	}
	if runs[0] != "goal:g1" {
		t.Errorf("成本要归到 goal:g1，实际 %q", runs[0])
	}
	if noAuto, _ := ask.isolation(); noAuto != 1 {
		t.Error("目标每一步必须关闭技能自动匹配（机器生成的推进提示词）")
	}
	if !strings.Contains(ask.prompts[0], "把年度报销办完") || !strings.Contains(ask.prompts[0], "[达成]") {
		t.Error("单步提示词要带上目标原文与输出协议")
	}
}

func TestGoalRunnerStaysSilentWhenBlocked(t *testing.T) {
	usage.Init("")
	ask := &goalAsk{replies: []string{"[受阻] 需要你确认发票抬头"}}
	r := newTestGoalRunner(ask)

	text, push, err := r.Prepare(context.Background(), goalTask())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if push {
		t.Error("受阻不该主动打扰用户（结论仍可在通知页看到）")
	}
	if !strings.Contains(text, "需要你决定") || !strings.Contains(text, "发票抬头") {
		t.Errorf("受阻结论要带原因: %q", text)
	}
}

func TestGoalRunnerUsesStepBudget(t *testing.T) {
	usage.Init("")
	ask := &goalAsk{} // 一律 [继续]：只能靠步数上限收尾
	r := newTestGoalRunner(ask)
	r.DefaultSteps = 3

	text, push, err := r.Prepare(context.Background(), goalTask())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	calls, _, _ := ask.snapshot()
	if calls != 3 {
		t.Errorf("应按 maxSteps 推进 3 步，实际 %d", calls)
	}
	if push {
		t.Error("用完步数没宣布达成，不该推送")
	}
	if !strings.Contains(text, "暂告一段落") {
		t.Errorf("收尾文案要说明没办完: %q", text)
	}
	// 后一步要能看到前一步的进展（独立会话之外的显式摘要）
	if !strings.Contains(ask.prompts[2], "已经做过的事") {
		t.Error("后续步骤要带已推进的过程")
	}
}

func TestGoalRunnerHonorsPerTaskStepLimit(t *testing.T) {
	usage.Init("")
	ask := &goalAsk{}
	r := newTestGoalRunner(ask)
	r.DefaultSteps = 9
	tn := goalTask()
	tn.MaxSteps = 2

	if _, _, err := r.Prepare(context.Background(), tn); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if calls, _, _ := ask.snapshot(); calls != 2 {
		t.Errorf("任务自带的步数上限要优先于默认值，实际 %d 次", calls)
	}
}

func TestGoalRunnerTokenBudgetStopsAtBoundary(t *testing.T) {
	usage.Init("")
	ask := &goalAsk{charge: 400} // 每步 400 token
	r := newTestGoalRunner(ask)
	r.DefaultSteps = 10
	tn := goalTask()
	tn.MaxTokens = 300 // 第一步跑完就已超

	text, push, err := r.Prepare(context.Background(), tn)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	calls, _, _ := ask.snapshot()
	if calls != 1 {
		t.Errorf("预算触顶要停在步边界，实际推进 %d 步", calls)
	}
	if push {
		t.Error("超预算不是达成，不该推送")
	}
	if !strings.Contains(text, "暂告一段落") {
		t.Errorf("应给出可读的收尾说明: %q", text)
	}
}

func TestGoalRunnerRespectsBudgetGate(t *testing.T) {
	usage.Init("")
	ask := &goalAsk{}
	r := newTestGoalRunner(ask)
	r.Budget = allowGate{allowed: false, reason: "额度用量 105%（dailyTokens）"}

	text, push, err := r.Prepare(context.Background(), goalTask())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if calls, _, _ := ask.snapshot(); calls != 0 {
		t.Errorf("闸门拦截时不该调用模型，实际 %d 次", calls)
	}
	if push || !strings.Contains(text, "额度用量") {
		t.Errorf("拦截时应静默并说明原因: push=%v text=%q", push, text)
	}
}

func TestGoalRunnerStepFailureKeepsGoing(t *testing.T) {
	usage.Init("")
	ask := &goalAsk{
		replies: []string{"[继续] 第一步成了", "[继续] 第二步成了"},
	}
	r := newTestGoalRunner(ask)
	r.DefaultSteps = 3

	if _, _, err := r.Prepare(context.Background(), goalTask()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	calls, _, _ := ask.snapshot()
	if calls != 3 {
		t.Errorf("单步失败不该终止目标，应推进到步数上限，实际 %d 次", calls)
	}
}

func TestGoalRunnerRefusesConcurrentSecondRun(t *testing.T) {
	usage.Init("")
	gate := make(chan struct{})
	ask := &goalAsk{gate: gate}
	r := newTestGoalRunner(ask)
	r.ConfigureSlots(1)

	done := make(chan string, 1)
	go func() {
		text, _, err := r.Prepare(context.Background(), goalTask())
		if err != nil {
			text = "err:" + err.Error()
		}
		done <- text
	}()
	waitForTrue(t, func() bool { return r.Running("g1") }, "第一个目标应在跑")

	// 第二次触发（例如同一个循环目标又来了一次）应在等一小会儿后跳过，而不是堆队
	second := goalTask()
	second.ID = "g2"
	_, push, err := r.Prepare(context.Background(), second)
	if err != nil {
		t.Fatalf("第二次 Prepare: %v", err)
	}
	if push {
		t.Error("槽位满时不该推送")
	}

	close(gate)
	if got := <-done; !strings.Contains(got, "目标") {
		t.Errorf("第一个目标的结论异常: %q", got)
	}
	waitForTrue(t, func() bool { return !r.Running("g1") }, "结束后应解除运行标记")
}

func TestGoalRunnerStopBreaksAtBoundary(t *testing.T) {
	usage.Init("")
	ask := &goalAsk{}
	r := newTestGoalRunner(ask)
	r.DefaultSteps = 5
	// 第一步返回后立刻请求停止：循环必须在第二步的边界上收住。
	ask.onCall = func(n int) {
		if n == 1 {
			r.Stop("g1")
		}
	}

	text, _, err := r.Prepare(context.Background(), goalTask())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	calls, _, _ := ask.snapshot()
	if calls > 2 {
		t.Errorf("请求停止后要在步边界停下，实际 %d 次", calls)
	}
	if !strings.Contains(text, "停止") && !strings.Contains(text, "暂告一段落") {
		t.Errorf("停止要有明确交代: %q", text)
	}
}

func TestLaunchGoalSchedulesImmediateGoal(t *testing.T) {
	eng := task.NewEngine(nil)
	defer eng.Close()

	tn, err := LaunchGoal(eng, "  把阳台的花养好  ")
	if err != nil {
		t.Fatalf("LaunchGoal: %v", err)
	}
	if !tn.Goal || tn.Kind != task.KindAgent {
		t.Errorf("聊天里的目标要落成 agent 目标任务: %+v", tn)
	}
	if tn.Text != "把阳台的花养好" {
		t.Errorf("目标内容应去空白: %q", tn.Text)
	}
	if d := time.Until(tn.When); d <= 0 || d > time.Minute {
		t.Errorf("即时目标要在几秒后开跑，实际 %s", d)
	}
	if _, err := LaunchGoal(eng, "   "); err == nil {
		t.Error("空目标应被拒绝")
	}
}

func TestGoalRunnerErrorsPropagate(t *testing.T) {
	usage.Init("")
	ask := &goalAsk{err: errors.New("模型不可用")}
	r := newTestGoalRunner(ask)
	r.DefaultSteps = 2

	if _, _, err := r.Prepare(context.Background(), goalTask()); err != nil {
		t.Fatalf("单步失败不该整体报错: %v", err)
	}
	if r.Running("g1") {
		t.Error("Prepare 结束后应解除运行标记")
	}
	if calls, _, _ := ask.snapshot(); calls != 2 {
		t.Errorf("失败也应把步数用完（每步都带失败原因重试），实际 %d", calls)
	}
}

func TestGoalSessionKeyNormalization(t *testing.T) {
	if got := GoalSessionKey("g1"); got != SessionGoal+"g1" {
		t.Errorf("会话 key 应为 goal-<id>，实际 %q", got)
	}
	// 空 ID 必须返回空：Brain.Reset("") 会落到默认网页会话上。
	if got := GoalSessionKey("   "); got != "" {
		t.Errorf("空 ID 不该得出会话 key，实际 %q", got)
	}
	if got := GoalSessionKey("a/b"); got != SessionGoal+"a_b" {
		t.Errorf("ID 要与 SessionPool 的归一化口径一致，实际 %q", got)
	}
	b := &Brain{}
	if err := b.ReleaseGoalSession(""); err == nil {
		t.Error("空 ID 释放会话应报错，而不是清掉网页会话")
	}
}

func waitForTrue(t *testing.T, cond func() bool, why string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", why)
}
