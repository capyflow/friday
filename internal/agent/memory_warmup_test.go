package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/memory"
)

func TestMemoryWarmupInjectedOncePerSession(t *testing.T) {
	p := &fakeProvider{responses: []string{"好", "好", "好"}}
	b := newTestBrain(t, p)
	store := memory.NewWikiStore(t.TempDir(), "")
	if _, _, err := store.Save("", "用户偏好", "用户希望被称为「老A」，偏好清淡口味。", "user"); err != nil {
		t.Fatal(err)
	}
	b.SetMemory(store)

	findWarm := func() (int, string) {
		n, text := 0, ""
		for _, m := range p.messages() {
			if t := messageTextOf(m); strings.Contains(t, "长期记忆速览") {
				n++
				text = t
			}
		}
		return n, text
	}

	ctx := context.Background()
	if _, err := b.Ask(ctx, "你好", nil, nil); err != nil {
		t.Fatal(err)
	}
	n, text := findWarm()
	if n != 1 {
		t.Fatalf("会话首轮应注入一条记忆速览，实际 %d", n)
	}
	for _, want := range []string{"用户偏好", "老A", "recall"} {
		if !strings.Contains(text, want) {
			t.Fatalf("速览应包含 %q，实际 %q", want, text)
		}
	}

	if _, err := b.Ask(ctx, "再聊一句", nil, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := findWarm(); n != 1 {
		t.Fatalf("同一会话不该重复注入，实际 %d 条", n)
	}

	// 清掉上下文后第一条消息：重新垫一份，agent 又"想起来"了
	if err := b.Pool().Reset(ctx, SessionWeb); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Ask(ctx, "清空后的第一句", nil, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := findWarm(); n != 1 {
		t.Fatalf("清空上下文后的新会话应重新注入，实际 %d 条", n)
	}
}

func TestMemoryWarmupSkippedWithoutMemory(t *testing.T) {
	p := &fakeProvider{responses: []string{"好"}}
	b := newTestBrain(t, p)
	b.SetMemory(memory.NewWikiStore(t.TempDir(), "")) // 一页都没有
	if _, err := b.Ask(context.Background(), "你好", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, m := range p.messages() {
		if strings.Contains(messageTextOf(m), "长期记忆速览") {
			t.Fatal("没有记忆页面时不该注入速览")
		}
	}
}
