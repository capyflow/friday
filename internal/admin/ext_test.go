package admin

import (
	"strings"
	"testing"

	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
)

// TestHistoryItemsGroupsTurnBlocks 校验历史回显把一轮问答里的思考、工具调用与
// 正文聚合成同一条 bot 消息，刷新页面后时间线不丢内容。
func TestHistoryItemsGroupsTurnBlocks(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "系统提示词"),
		llm.NewTextMessage(llm.RoleUser, "现在几点"),
		{Role: llm.RoleAssistant, Content: []llm.Content{
			{Type: llm.ContentThinking, Thinking: "先查时区"},
		}, ToolCalls: []llm.ToolCall{
			{ID: "t1", Name: "get_time", Arguments: map[string]any{"zone": "Asia/Shanghai"}},
		}},
		llm.NewToolResultMessage("t1", "2026-10-14T22:00:00+08:00"),
		llm.NewTextMessage(llm.RoleAssistant, "现在是晚上 10 点。"),
		llm.NewTextMessage(llm.RoleUser, "谢谢"),
		llm.NewTextMessage(llm.RoleAssistant, "不客气～"),
	}

	items := historyItems(msgs)
	if len(items) != 4 {
		t.Fatalf("消息条数 = %d，期望 4（两轮问答）: %+v", len(items), items)
	}

	bot := items[1]
	if bot.Role != "assistant" || bot.Text != "现在是晚上 10 点。" {
		t.Fatalf("bot 正文 = %q / %q，期望聚合最终回答", bot.Role, bot.Text)
	}
	if len(bot.Blocks) != 3 {
		t.Fatalf("blocks = %+v，期望 think/tool/text 三块", bot.Blocks)
	}
	if b := bot.Blocks[0]; b.Kind != "think" || b.Text != "先查时区" {
		t.Errorf("第一块 = %+v，期望思考全文", b)
	}
	if b := bot.Blocks[1]; b.Kind != "tool" || b.Name != "get_time" ||
		b.Status != "ok" || !strings.Contains(b.Text, "Asia/Shanghai") ||
		!strings.Contains(b.Out, "22:00") {
		t.Errorf("第二块 = %+v，期望工具调用（参数摘要 + 结果回填）", b)
	}
	if b := bot.Blocks[2]; b.Kind != "text" || b.Text != "现在是晚上 10 点。" {
		t.Errorf("第三块 = %+v，期望正文", b)
	}

	if items[3].Text != "不客气～" || len(items[3].Blocks) != 1 {
		t.Errorf("第二条 bot = %+v，期望独立成条", items[3])
	}
}

// TestHistoryItemsToolFailureAndThinkingOnly 校验工具失败标记与纯思考轮次保留。
func TestHistoryItemsToolFailureAndThinkingOnly(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "删掉那个文件"),
		{Role: llm.RoleAssistant, Content: []llm.Content{
			{Type: llm.ContentThinking, Thinking: "用户要删文件"},
		}, ToolCalls: []llm.ToolCall{{ID: "t9", Name: "exec_command"}}},
		llm.NewToolResultMessage("t9", "工具执行出错: 权限被拒绝"),
		llm.NewTextMessage(llm.RoleAssistant, "这个操作被拦下了。"),
	}

	items := historyItems(msgs)
	if len(items) != 2 {
		t.Fatalf("消息条数 = %d，期望 2", len(items))
	}
	bot := items[1]
	if len(bot.Blocks) != 3 || bot.Blocks[0].Kind != "think" {
		t.Fatalf("blocks = %+v，期望思考块保留", bot.Blocks)
	}
	tool := bot.Blocks[1]
	if tool.Status != "fail" || !strings.Contains(tool.Out, "权限被拒绝") {
		t.Errorf("工具块 = %+v，期望失败状态与错误摘要", tool)
	}
	if tool.Text != "" {
		t.Errorf("工具块参数摘要 = %q，期望无参数时为空", tool.Text)
	}
}

// TestHistoryItemsSkipsEmptyTurns 校验空（无块）轮次不出现在回显里，
// 且用户消息带图片时降级为占位标记。
func TestHistoryItemsSkipsEmptyTurns(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.Content{
			{Type: llm.ContentText, Text: "看看这张图"},
			{Type: llm.ContentImage, MIME: "image/png"},
		}},
		{Role: llm.RoleAssistant, Content: []llm.Content{{Type: llm.ContentThinking, Thinking: "   "}}},
	}

	items := historyItems(msgs)
	if len(items) != 1 {
		t.Fatalf("消息条数 = %d，期望 1（空 assistant 轮不出现）: %+v", len(items), items)
	}
	if items[0].Text != "看看这张图[图片]" {
		t.Errorf("用户消息 = %q，期望图片降级为占位标记", items[0].Text)
	}
}

// TestCompactMessage 压缩结果的三种说法各按实际改动给出：真归档了要报块数与前后
// 占用；闸门放行但活跃块没超过保留数时框架静默返回，此时不能说"已压缩"。
func TestCompactMessage(t *testing.T) {
	before := agent.ContextSnapshot{Enabled: true, Blocks: 20, Messages: 44, Used: 100000, KeepRecent: 12}
	after := agent.ContextSnapshot{Enabled: true, Blocks: 8, Messages: 16, Used: 27000, KeepRecent: 12}
	if got := compactMessage(before, after); got != "已归档 12 块，占用 100000 → 27000 tokens" {
		t.Errorf("归档了 %q", got)
	}

	noop := agent.ContextSnapshot{Enabled: true, Blocks: 3, Messages: 6, Used: 900, KeepRecent: 12}
	got := compactMessage(noop, noop)
	if !strings.Contains(got, "没有可归档") {
		t.Errorf("无事可做应如实说明，实际 %q", got)
	}
	if strings.Contains(got, "已压缩") {
		t.Errorf("无事可做时不该报成功，实际 %q", got)
	}
}
