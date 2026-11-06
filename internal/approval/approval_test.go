package approval

import (
	"strings"
	"testing"
	"time"
)

func TestModes(t *testing.T) {
	if m := New("auto", nil, 0).Mode(); m != ModeAuto {
		t.Fatalf("auto 模式不符: %s", m)
	}
	if m := New("confirm", nil, 0).Mode(); m != ModeConfirm {
		t.Fatalf("confirm 模式不符: %s", m)
	}
	if m := New("off", nil, 0).Mode(); m != ModeOff {
		t.Fatalf("off 模式不符: %s", m)
	}
	if m := New("bogus", nil, 0).Mode(); m != ModeConfirm {
		t.Fatalf("非法模式应回退 confirm: %s", m)
	}
	if d := New("confirm", nil, 0).Timeout(); d != DefaultTimeout {
		t.Fatalf("默认超时应为 5 分钟: %s", d)
	}
	if d := New("confirm", nil, time.Second).Timeout(); d != time.Second {
		t.Fatalf("自定义超时不符: %s", d)
	}
}

func TestAutoModeRequestPassesThrough(t *testing.T) {
	q := New("auto", nil, 0)
	if err := q.Request("write_file", "写文件", "detail"); err != nil {
		t.Fatalf("auto 模式下非 exec 操作应直接放行: %v", err)
	}
}

func TestOffModeRequestDisabled(t *testing.T) {
	q := New("off", nil, 0)
	if err := q.Request("exec", "执行", "detail"); err != ErrDisabled {
		t.Fatalf("off 模式应返回 ErrDisabled，实际 %v", err)
	}
}

func TestConfirmApproveFlow(t *testing.T) {
	q := New("confirm", nil, 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- q.Request("exec", "执行命令", "uname -a") }()
	// 等请求入队。
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	items := q.Pending()
	if len(items) != 1 || items[0].Tool != "exec" {
		t.Fatalf("应有 1 条待审批，实际 %+v", items)
	}
	if !q.Decide(items[0].ID, true) {
		t.Fatal("Decide 应命中待审批项")
	}
	if err := <-done; err != nil {
		t.Fatalf("批准后应放行: %v", err)
	}
	if len(q.Pending()) != 0 {
		t.Fatal("处理后待审批列表应为空")
	}
}

func TestRequestRememberDecideWith(t *testing.T) {
	q := New("confirm", nil, 3*time.Second)
	type result struct {
		remember bool
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		remember, err := q.RequestRemember("exec", "执行命令", "whoami")
		ch <- result{remember, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !q.DecideWith(q.Pending()[0].ID, true, true) {
		t.Fatal("DecideWith 应命中待审批项")
	}
	r := <-ch
	if r.err != nil || !r.remember {
		t.Fatalf("批准并记住应放行且 remember=true: %+v", r)
	}

	// 不记住的批准：remember=false。
	ch2 := make(chan result, 1)
	go func() {
		remember, err := q.RequestRemember("exec", "执行命令", "date")
		ch2 <- result{remember, err}
	}()
	deadline = time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !q.DecideWith(q.Pending()[0].ID, true, false) {
		t.Fatal("DecideWith 应命中待审批项")
	}
	r = <-ch2
	if r.err != nil || r.remember {
		t.Fatalf("普通批准应 remember=false: %+v", r)
	}

	// 拒绝：ErrDenied。
	ch3 := make(chan result, 1)
	go func() {
		remember, err := q.RequestRemember("exec", "执行命令", "curl evil")
		ch3 <- result{remember, err}
	}()
	deadline = time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.DecideWith(q.Pending()[0].ID, false, true)
	r = <-ch3
	if r.err != ErrDenied {
		t.Fatalf("拒绝应返回 ErrDenied，实际 %v", r.err)
	}
}

// Request 是 RequestRemember 的薄包装：批准返回 nil，拒绝返回 ErrDenied。
func TestRequestWrapsRequestRemember(t *testing.T) {
	q := New("confirm", nil, 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- q.Request("exec", "执行命令", "uname -a") }()
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.Decide(q.Pending()[0].ID, true)
	if err := <-done; err != nil {
		t.Fatalf("批准后应放行: %v", err)
	}
}

// DenyPatterns 返回黑名单副本，供权限层翻译成 exec_command deny 规则。
func TestDenyPatterns(t *testing.T) {
	pats := DenyPatterns()
	if len(pats) == 0 {
		t.Fatal("黑名单不应为空")
	}
	pats[0] = "tampered"
	if DenyPatterns()[0] == "tampered" {
		t.Fatal("DenyPatterns 应返回副本，避免调用方污染黑名单")
	}
	found := false
	for _, p := range DenyPatterns() {
		if p == "rm -rf" {
			found = true
		}
	}
	if !found {
		t.Fatal("黑名单应包含 rm -rf")
	}
}

func TestConfirmRejectFlow(t *testing.T) {
	q := New("confirm", nil, 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- q.Request("exec", "执行命令", "curl evil") }()
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	item := q.Pending()[0]
	if !q.Decide(item.ID, false) {
		t.Fatal("Decide 应命中")
	}
	if err := <-done; err != ErrDenied {
		t.Fatalf("拒绝应返回 ErrDenied，实际 %v", err)
	}
}

func TestConfirmTimeoutDenies(t *testing.T) {
	q := New("confirm", nil, 80*time.Millisecond)
	start := time.Now()
	err := q.Request("exec", "执行命令", "sleep")
	if err != ErrDenied {
		t.Fatalf("超时应返回 ErrDenied，实际 %v", err)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatal("应等待超时窗口再拒绝")
	}
}

// RequestAlways 无视模式强制入队（智能体权限专用）：off 模式也弹卡，批准返回
// true；拒绝/超时返回 ErrDenied。超时用自定义值，不随队列默认。
func TestRequestAlwaysBypassesMode(t *testing.T) {
	q := New("off", nil, 3*time.Second)
	done := make(chan error, 1)
	go func() {
		approved, err := q.RequestAlways("智能体权限", "执行命令", "whoami", 2*time.Second)
		if err == nil && !approved {
			t.Error("批准应返回 approved=true")
		}
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(q.Pending()) != 1 {
		t.Fatal("off 模式下 RequestAlways 也应入队")
	}
	q.Decide(q.Pending()[0].ID, true)
	if err := <-done; err != nil {
		t.Fatalf("批准应放行: %v", err)
	}

	// 拒绝路径。
	done2 := make(chan error, 1)
	go func() {
		_, err := q.RequestAlways("智能体权限", "执行命令", "rm -rf /", 2*time.Second)
		done2 <- err
	}()
	deadline = time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.Decide(q.Pending()[0].ID, false)
	if err := <-done2; err != ErrDenied {
		t.Fatalf("拒绝应返回 ErrDenied，实际 %v", err)
	}
}

func TestRequestCancelableCancel(t *testing.T) {
	q := New("confirm", nil, 0)
	card := q.RequestCancelable("智能体权限", "执行命令", "uname -a", time.Minute)
	done := make(chan struct{})
	go func() {
		if _, err := card.Wait(); err != ErrDenied {
			t.Errorf("Cancel 后 Wait 应返回 ErrDenied，实际 %v", err)
		}
		close(done)
	}()
	// 等卡入队后由业务侧撤卡。
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(q.Pending()) != 1 {
		t.Fatal("Cancel 前应有 1 条待审批")
	}
	card.Cancel()
	<-done
	if len(q.Pending()) != 0 {
		t.Fatal("Cancel 后卡应从待审批列表移除")
	}
	// 迟到的用户决定落空（幂等、不 panic）。
	if q.Decide(card.req.item.ID, true) {
		t.Fatal("已撤下的卡不应再可决定")
	}
	// 幂等：重复 Cancel 无操作。
	card.Cancel()
}

func TestRequestCancelableApprove(t *testing.T) {
	q := New("confirm", nil, 0)
	card := q.RequestCancelable("智能体权限", "执行命令", "uname -a", time.Minute)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for len(q.Pending()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		q.Decide(q.Pending()[0].ID, true)
	}()
	allow, err := card.Wait()
	if err != nil || !allow {
		t.Fatalf("批准应返回 (true,nil)，实际 (%v,%v)", allow, err)
	}
	select {
	case <-card.Done():
	default:
		t.Fatal("Wait 返回后 Done 应已关闭")
	}
}

// TestRequestCardQuestionDecideOption 问答卡：入队带选项与问题正文，按 optionId
// 作答能取回该 id；乱填的 id、以及对授权卡作答都必须被拒（否则会把不存在的选项
// 回传给 agent）。
func TestRequestCardQuestionDecideOption(t *testing.T) {
	q := New("confirm", nil, time.Minute)
	opts := []Option{{ID: "a", Name: "方案 A", Kind: "allow_once"}, {ID: "b", Name: "方案 B", Kind: "allow_once"}}
	groups := []QuestionGroup{{Header: "范围", Question: "怎么定？",
		Options: []QuestionOption{{Label: "方案 A", Description: "只拦新上传"}}}}
	card := q.RequestCard(Card{Tool: "智能体提问", Summary: "怎么定？", Kind: KindQuestion,
		Options: opts, Questions: groups}, 0)

	list := q.Pending()
	if len(list) != 1 || list[0].Kind != KindQuestion || len(list[0].Options) != 2 {
		t.Fatalf("问答卡入队形态不符: %+v", list)
	}
	if list[0].Questions[0].Header != "范围" {
		t.Fatalf("问题正文没带上: %+v", list[0].Questions)
	}

	if q.DecideOption(card.ID(), "nope") {
		t.Error("不存在的选项不该被接受")
	}
	if !q.DecideOption(card.ID(), "b") {
		t.Fatal("按 optionId 作答应被接受")
	}
	out, err := card.WaitDecision()
	if err != nil || !out.Allow || out.OptionID != "b" {
		t.Fatalf("作答结果不符: %+v err=%v", out, err)
	}
	if len(q.Pending()) != 0 {
		t.Errorf("作答后卡片应出队: %+v", q.Pending())
	}
}

// TestDecideTextQuestionCard 自由文本答案：只有允许文本的问答卡接受，
// 文本会去首尾空白后原样带回 Outcome.Text。
func TestDecideTextQuestionCard(t *testing.T) {
	q := New("confirm", nil, time.Minute)
	card := q.RequestCard(Card{Tool: "智能体提问", Summary: "怎么定？", Kind: KindQuestion,
		AllowText: true, TextPlaceholder: "也可以自己写",
		Options: []Option{{ID: "a", Name: "方案 A", Kind: "allow_once"}}}, 0)

	list := q.Pending()
	if len(list) != 1 || !list[0].AllowText || list[0].TextPlaceholder != "也可以自己写" {
		t.Fatalf("自由文本标记没带上: %+v", list)
	}
	if q.DecideText(card.ID(), "   ") {
		t.Error("空文本不该被接受")
	}
	if q.DecideText(card.ID(), strings.Repeat("x", MaxAnswerText+1)) {
		t.Error("超长文本不该被接受")
	}
	if !q.DecideText(card.ID(), "  自定义方案  ") {
		t.Fatal("自由文本作答应被接受")
	}
	out, err := card.WaitDecision()
	if err != nil || !out.Allow || out.Text != "自定义方案" || out.OptionID != "" {
		t.Fatalf("自由文本结果不符: %+v err=%v", out, err)
	}
}

// TestDecideTextRejectsPlainQuestionCard 没有 AllowText 的问答卡不接文本，
// 更不能把文本塞给真正的授权卡。
func TestDecideTextRejectsPlainQuestionCard(t *testing.T) {
	q := New("confirm", nil, time.Minute)
	question := q.RequestCard(Card{Tool: "智能体提问", Kind: KindQuestion,
		Options: []Option{{ID: "a", Name: "A"}}}, 0)
	if q.DecideText(question.ID(), "自己写") {
		t.Error("未声明 AllowText 的卡不该接受文本")
	}
	approvalCard := q.RequestCancelable("exec", "执行 rm", "", 0)
	if q.DecideText(approvalCard.ID(), "自己写") {
		t.Error("授权卡不该接受文本")
	}
	q.Decide(question.ID(), false)
	q.Decide(approvalCard.ID(), false)
}

// TestDecideOptionsMultiSelectCard 多选问答卡：一次提交多条勾选，顺序保留、
// 重复与空白去重；没勾、含未知选项都拒绝；非多选卡与授权卡不吃多选作答。
func TestDecideOptionsMultiSelectCard(t *testing.T) {
	q := New("confirm", nil, time.Minute)
	card := q.RequestCard(Card{Tool: "智能体提问", Summary: "带哪些模块？", Kind: KindQuestion,
		MultiSelect: true,
		Options:     []Option{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "C"}}}, 0)
	list := q.Pending()
	if len(list) != 1 || !list[0].MultiSelect {
		t.Fatalf("multiSelect 标记没带上: %+v", list)
	}
	if q.DecideOptions(card.ID(), nil) || q.DecideOptions(card.ID(), []string{" ", ""}) {
		t.Error("没勾任何项不该被接受")
	}
	if q.DecideOptions(card.ID(), []string{"a", "nope"}) {
		t.Error("含未知选项的提交不该被接受")
	}
	if !q.DecideOptions(card.ID(), []string{"c", "a", "a", "  "}) {
		t.Fatal("多选作答应被接受")
	}
	out, err := card.WaitDecision()
	if err != nil || !out.Allow {
		t.Fatalf("多选结算异常: %+v err=%v", out, err)
	}
	if len(out.OptionIDs) != 2 || out.OptionIDs[0] != "c" || out.OptionIDs[1] != "a" || out.OptionID != "" {
		t.Fatalf("勾选顺序与去重不符: %+v", out)
	}

	plain := q.RequestCard(Card{Tool: "智能体提问", Kind: KindQuestion,
		Options: []Option{{ID: "a", Name: "A"}}}, 0)
	if q.DecideOptions(plain.ID(), []string{"a"}) {
		t.Error("未声明 MultiSelect 的卡不该接受多选作答")
	}
	q.Decide(plain.ID(), false)
	authz := q.RequestCancelable("exec", "执行 rm", "", 0)
	if q.DecideOptions(authz.ID(), []string{"allow"}) {
		t.Error("授权卡不该接受多选作答")
	}
	q.Decide(authz.ID(), false)
}

// TestDecideOptionRejectsApprovalCard 授权卡要的是批准/拒绝，不接受选项作答。
func TestDecideOptionRejectsApprovalCard(t *testing.T) {
	q := New("confirm", nil, time.Minute)
	card := q.RequestCancelable("exec", "执行 rm", "rm -rf /tmp/x", 0)
	if q.DecideOption(card.ID(), "allow") {
		t.Error("授权卡不该被 DecideOption 处理")
	}
	if !q.Decide(card.ID(), true) {
		t.Fatal("授权卡仍应按批准结算")
	}
	if ok, err := card.Wait(); !ok || err != nil {
		t.Fatalf("批准结果不符: %v %v", ok, err)
	}
}

// TestWaitDecisionTimeoutKeepsDeniedSemantics 超时/撤卡仍是 ErrDenied，且没有
// optionId——调用方据此回 cancelled，而不是替用户挑一条。
func TestWaitDecisionTimeoutKeepsDeniedSemantics(t *testing.T) {
	q := New("confirm", nil, 0)
	card := q.RequestCard(Card{Tool: "智能体提问", Summary: "问", Kind: KindQuestion,
		Options: []Option{{ID: "a", Name: "A"}}}, time.Nanosecond)
	out, err := card.WaitDecision()
	if err != ErrDenied {
		t.Fatalf("超时应为 ErrDenied: %v", err)
	}
	if out.OptionID != "" || out.Allow {
		t.Errorf("超时不该带选项: %+v", out)
	}
}
