package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeDelegateRunner 是派发的假实现：只回固定答案，不真的跑子会话。
type fakeDelegateRunner struct {
	task   string
	answer string
	err    error
}

func (f *fakeDelegateRunner) Delegate(ctx context.Context, task string) (string, error) {
	f.task = task
	return f.answer, f.err
}

func TestDelegateToolNeedsTask(t *testing.T) {
	tool := NewDelegateTool()
	tool.SetRunner(&fakeDelegateRunner{answer: "x"})
	if _, err := tool.Call(context.Background(), map[string]any{"task": "  "}); err == nil {
		t.Fatal("空 task 应报错")
	}
}

func TestDelegateToolNeedsRunner(t *testing.T) {
	tool := NewDelegateTool()
	_, err := tool.Call(context.Background(), map[string]any{"task": "查一下"})
	if err == nil {
		t.Fatal("没接上大脑时应报错")
	}
	if !strings.Contains(err.Error(), "未接入") {
		t.Fatalf("报错应说清是装配缺失: %v", err)
	}
}

// TestDelegateToolRejectsNested 子任务里再派子任务要挡住：递归派发能把预算烧光。
func TestDelegateToolRejectsNested(t *testing.T) {
	tool := NewDelegateTool()
	runner := &fakeDelegateRunner{answer: "x"}
	tool.SetRunner(runner)
	ctx := WithDelegateDepth(context.Background(), maxDelegateDepth)
	_, err := tool.Call(ctx, map[string]any{"task": "再派一个"})
	if err == nil {
		t.Fatal("嵌套派发应被拒绝")
	}
	if !strings.Contains(err.Error(), "嵌套上限") {
		t.Fatalf("报错应说清是嵌套上限: %v", err)
	}
	if runner.task != "" {
		t.Fatal("被拒时不该真的派出去")
	}
}

func TestDelegateToolReturnsAnswer(t *testing.T) {
	tool := NewDelegateTool()
	runner := &fakeDelegateRunner{answer: "结论在这"}
	tool.SetRunner(runner)
	out, err := tool.Call(context.Background(), map[string]any{"task": "查一下 X"})
	if err != nil {
		t.Fatalf("派发失败: %v", err)
	}
	if out != "结论在这" {
		t.Fatalf("应原样带回子任务结论，实际 %q", out)
	}
	if runner.task != "查一下 X" {
		t.Fatalf("任务应原样传给子任务，实际 %q", runner.task)
	}
}

func TestDelegateToolEmptyAnswerSaysSo(t *testing.T) {
	tool := NewDelegateTool()
	tool.SetRunner(&fakeDelegateRunner{answer: "  "})
	out, err := tool.Call(context.Background(), map[string]any{"task": "查一下 X"})
	if err != nil {
		t.Fatalf("派发失败: %v", err)
	}
	if !strings.Contains(out, "没给出结论") {
		t.Fatalf("空结论应如实说明: %q", out)
	}
}

// TestBrainDelegateIsolatesSession 子会话要独立于主会话，跑完还要清掉（一次性）。
func TestBrainDelegateIsolatesSession(t *testing.T) {
	p := &fakeProvider{responses: []string{"子任务结论"}}
	b := newTestBrain(t, p)
	ctx := WithSessionKey(context.Background(), SessionWeb)

	before := delegateSeq.Load()
	answer, err := b.Delegate(ctx, "查一下 X 在哪")
	if err != nil {
		t.Fatalf("派发失败: %v", err)
	}
	if answer != "子任务结论" {
		t.Fatalf("应带回子任务回答，实际 %q", answer)
	}

	key := fmt.Sprintf("sub-%s-%d", SessionWeb, before+1)
	if msgs := b.Pool().History(ctx, key); len(msgs) != 0 {
		t.Fatalf("子会话跑完应被清理，实际还剩 %d 条", len(msgs))
	}
	if msgs := b.Pool().History(ctx, SessionWeb); len(msgs) != 0 {
		t.Fatalf("子任务不该污染主会话，实际主会话有 %d 条", len(msgs))
	}

	// 子会话收到的指令要带子任务标记与任务正文。
	msgs := p.messages()
	joined := ""
	for _, m := range msgs {
		joined += messageTextOf(m)
	}
	if !strings.Contains(joined, "【子任务】") || !strings.Contains(joined, "查一下 X 在哪") {
		t.Fatalf("子会话应收到带任务正文的子任务指令，实际:\n%s", joined)
	}
}
