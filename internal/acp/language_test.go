package acp

import (
	"context"
	"strings"
	"testing"
)

// 外部 agent 没有 Friday 的系统提示词：语言规则得随 prompt 块发过去，
// 否则外部 agent 会按它自己的默认语言回答。
func TestExternalAgentGetsLanguageRule(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	m.SetLanguage(func() string { return "English" })
	th := mustThread(t, m)
	c := &collector{}
	if _, err := m.Send(context.Background(), th.ID, "打个招呼", nil, c.hooks()); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	f.mu.Lock()
	prompts := append([][]string{}, f.prompts...)
	f.mu.Unlock()
	if len(prompts) == 0 || len(prompts[0]) == 0 {
		t.Fatal("没收到 prompt 块")
	}
	head := prompts[0][0]
	if !strings.Contains(head, "【回复语言】") || !strings.Contains(head, "「English」") {
		t.Fatalf("外部 agent 的 prompt 应带回复语言规则: %q", head)
	}
}

// 没注入语言来源时也不能少这条规则：默认中文照样要发过去。
func TestExternalAgentLanguageDefaultsToChinese(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)
	c := &collector{}
	if _, err := m.Send(context.Background(), th.ID, "打个招呼", nil, c.hooks()); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	f.mu.Lock()
	prompts := append([][]string{}, f.prompts...)
	f.mu.Unlock()
	if len(prompts) == 0 || len(prompts[0]) == 0 {
		t.Fatal("没收到 prompt 块")
	}
	if !strings.Contains(prompts[0][0], "「中文」") {
		t.Fatalf("默认语言应是中文: %q", prompts[0][0])
	}
}

// 内置 agent 走同一个 Brain（规则已在系统提示词里）：不该再往提问正文里塞一遍，
// 否则用户那句话前面会多出一段本该是规则的东西。
func TestBuiltinAgentDoesNotGetLanguageBlock(t *testing.T) {
	r := &stubRunner{handler: func(ctx context.Context, in stubAsk) (string, error) {
		return "好的", nil
	}}
	m, ws := newBuiltinManager(t, r, "")
	m.SetLanguage(func() string { return "English" })
	th := mustBuiltinThread(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "打个招呼", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if q := r.lastAsk(t).Text; strings.Contains(q, "【回复语言】") {
		t.Fatalf("内置会话的提问正文不该带语言规则: %q", q)
	}
}
