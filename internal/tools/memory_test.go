package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/memory"
)

// TestRememberAndRecall 记住 → 回忆 的完整链路。
func TestRememberAndRecall(t *testing.T) {
	mem := memory.NewStore("")
	rt := &RememberTool{Memory: mem}
	qt := &RecallTool{Memory: mem}

	if _, err := rt.Call(context.Background(), map[string]any{"content": "用户喜欢在晚饭后散步"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := rt.Call(context.Background(), map[string]any{"content": "用户对花粉过敏"}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	// 关键词回忆。
	res, err := qt.Call(context.Background(), map[string]any{"q": "花粉"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !strings.Contains(res, "花粉过敏") || !strings.Contains(res, "#m") {
		t.Fatalf("recall 应命中过敏记忆并带 ID，got %q", res)
	}

	// 空关键词 → 最近记忆。
	res, err = qt.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("recall(空): %v", err)
	}
	if !strings.Contains(res, "散步") {
		t.Fatalf("recall(空) 应返回最近记忆，got %q", res)
	}

	// 未命中。
	res, _ = qt.Call(context.Background(), map[string]any{"q": "不存在"})
	if !strings.Contains(res, "没有找到") {
		t.Fatalf("未命中应友好提示，got %q", res)
	}
}

// TestForget 忘记：先记再忘，ID 错误时友好提示。
func TestForget(t *testing.T) {
	mem := memory.NewStore("")
	rt := &RememberTool{Memory: mem}
	ft := &ForgetTool{Memory: mem}

	if _, err := rt.Call(context.Background(), map[string]any{"content": "用户怕打雷"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	id := mem.List()[0].ID

	out, err := ft.Call(context.Background(), map[string]any{"id": id})
	if err != nil || !strings.Contains(out, "已忘记") {
		t.Fatalf("forget 应成功，got %q %v", out, err)
	}
	out, err = ft.Call(context.Background(), map[string]any{"id": "m404"})
	if err != nil || !strings.Contains(out, "未找到") {
		t.Fatalf("forget 不存在的 ID 应友好提示，got %q %v", out, err)
	}
	if _, err := ft.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("缺少 id 应报错")
	}
	if mem.Count() != 0 {
		t.Fatalf("忘记后应为 0，实际 %d", mem.Count())
	}
}

// TestRememberEmpty 空内容应报错且不入库。
func TestRememberEmpty(t *testing.T) {
	mem := memory.NewStore("")
	rt := &RememberTool{Memory: mem}
	if _, err := rt.Call(context.Background(), map[string]any{"content": "  "}); err == nil {
		t.Fatal("空内容应报错")
	}
	if mem.Count() != 0 {
		t.Fatalf("不应入库，实际 %d", mem.Count())
	}
}
