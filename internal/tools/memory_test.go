package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/memory"
)

// TestRememberAndRecall 保存 → 搜索 → 读整页 的完整链路。
func TestRememberAndRecall(t *testing.T) {
	mem := memory.NewWikiStore("", "")
	rt := &RememberTool{Memory: mem}
	qt := &RecallTool{Memory: mem}

	if _, err := rt.Call(context.Background(), map[string]any{
		"title": "用户偏好", "body": "- 喜欢在晚饭后散步",
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := rt.Call(context.Background(), map[string]any{
		"title": "健康状况", "body": "- 对花粉过敏",
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	// 关键词只命中一页：wiki 里这通常就是要找的那页，直接给正文（省一次往返）。
	res, err := qt.Call(context.Background(), map[string]any{"q": "花粉"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !strings.Contains(res, "花粉过敏") || !strings.Contains(res, "#m") || !strings.Contains(res, "健康状况") {
		t.Fatalf("recall 应命中页面并带 ID，got %q", res)
	}
	if strings.Contains(res, "相关记忆页：") {
		t.Fatalf("唯一命中应直接给正文，实际还是列表: %q", res)
	}

	// 按 id 读整页正文。
	id := mem.Pages()[0].ID
	res, err = qt.Call(context.Background(), map[string]any{"id": id})
	if err != nil {
		t.Fatalf("recall(id): %v", err)
	}
	if !strings.Contains(res, "记忆页") || !strings.Contains(res, mem.Pages()[0].Title) {
		t.Fatalf("recall(id) 应返回整页，got %q", res)
	}

	// 空参数 → 全部页面列表（仅标题，不含正文片段）。
	res, err = qt.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("recall(空): %v", err)
	}
	if !strings.Contains(res, "《用户偏好》") || !strings.Contains(res, "《健康状况》") {
		t.Fatalf("recall(空) 应列出全部页面，got %q", res)
	}

	// 未命中：不是"没有就算了"，而是把目录摊开让模型自己挑页面。
	res, _ = qt.Call(context.Background(), map[string]any{"q": "不存在"})
	if !strings.Contains(res, "没有命中") || !strings.Contains(res, "目录") {
		t.Fatalf("未命中应说明并回给目录，got %q", res)
	}
	if !strings.Contains(res, "《用户偏好》") || !strings.Contains(res, "《健康状况》") {
		t.Fatalf("未命中时应列出全部页面标题，got %q", res)
	}

	// 空仓库：明确说"还没有页面"，不编造。
	empty := &RecallTool{Memory: memory.NewWikiStore("", "")}
	res, _ = empty.Call(context.Background(), map[string]any{"q": "称呼"})
	if !strings.Contains(res, "还没有任何页面") {
		t.Fatalf("空仓库应明说还没有记忆，got %q", res)
	}
	// 不存在的 id。
	res, _ = qt.Call(context.Background(), map[string]any{"id": "m404"})
	if !strings.Contains(res, "未找到") {
		t.Fatalf("读不存在的页面应友好提示，got %q", res)
	}
}

// TestRememberMergeFlow 整编链路：同标题更新 = 合并写入，不产生重复页面。
func TestRememberMergeFlow(t *testing.T) {
	mem := memory.NewWikiStore("", "")
	rt := &RememberTool{Memory: mem}

	if _, err := rt.Call(context.Background(), map[string]any{
		"title": "用户偏好", "body": "喜欢散步",
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	id := mem.Pages()[0].ID

	// 同标题再记一条：应更新同一页（agent 负责把新旧内容合并进 body）。
	out, err := rt.Call(context.Background(), map[string]any{
		"title": "用户偏好", "body": "喜欢散步\n- 对花粉过敏",
	})
	if err != nil {
		t.Fatalf("remember 更新: %v", err)
	}
	if !strings.Contains(out, "已更新") || strings.Contains(out, "已新建") {
		t.Fatalf("同标题应提示更新，got %q", out)
	}
	if mem.Count() != 1 {
		t.Fatalf("同标题保存不应新建页面，实际 %d 页", mem.Count())
	}

	// 传 id 更新并改标题。
	out, err = rt.Call(context.Background(), map[string]any{
		"id": id, "title": "偏好与习惯", "body": "新正文",
	})
	if err != nil || !strings.Contains(out, "已更新") {
		t.Fatalf("按 id 更新应成功，got %q %v", out, err)
	}
	if p, _ := mem.Read(id); p.Title != "偏好与习惯" {
		t.Fatalf("按 id 更新应改标题，got %+v", p)
	}
}

// TestForget 删除页面：先记再忘，ID 错误时友好提示。
func TestForget(t *testing.T) {
	mem := memory.NewWikiStore("", "")
	rt := &RememberTool{Memory: mem}
	ft := &ForgetTool{Memory: mem}

	if _, err := rt.Call(context.Background(), map[string]any{
		"title": "恐惧", "body": "用户怕打雷",
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	id := mem.Pages()[0].ID

	out, err := ft.Call(context.Background(), map[string]any{"id": id})
	if err != nil || !strings.Contains(out, "已删除") {
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

// TestRememberEmpty 缺参数应报错且不入库。
func TestRememberEmpty(t *testing.T) {
	mem := memory.NewWikiStore("", "")
	rt := &RememberTool{Memory: mem}
	if _, err := rt.Call(context.Background(), map[string]any{"title": "  ", "body": "正文"}); err == nil {
		t.Fatal("空标题应报错")
	}
	if _, err := rt.Call(context.Background(), map[string]any{"title": "标题", "body": "  "}); err == nil {
		t.Fatal("空正文应报错")
	}
	if _, err := rt.Call(context.Background(), map[string]any{"body": "只有正文"}); err == nil {
		t.Fatal("缺少 title 应报错")
	}
	if mem.Count() != 0 {
		t.Fatalf("不应入库，实际 %d", mem.Count())
	}
}
