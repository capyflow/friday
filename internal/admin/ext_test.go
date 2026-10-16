package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/trace"
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

// TestHistoryItemsOrdersThinkingBeforeText 校验思考块恒排在正文前：
// OpenAI 兼容层流结束时把 text 排在 thinking 之前（llm/openai.go 的 append 顺序），
// 照原样投影会让刷新后的思考跑到回答下面。
func TestHistoryItemsOrdersThinkingBeforeText(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "你是谁"),
		{Role: llm.RoleAssistant, Content: []llm.Content{
			{Type: llm.ContentText, Text: "我是小五。"},
			{Type: llm.ContentThinking, Thinking: "用户问身份，直接按人设回答"},
		}},
	}

	items := historyItems(msgs)
	bot := items[1]
	if len(bot.Blocks) != 2 {
		t.Fatalf("blocks = %+v，期望 think + text 两块", bot.Blocks)
	}
	if bot.Blocks[0].Kind != "think" || bot.Blocks[1].Kind != "text" {
		t.Errorf("块顺序 = [%s, %s]，期望先思考后正文", bot.Blocks[0].Kind, bot.Blocks[1].Kind)
	}
	if bot.Text != "我是小五。" {
		t.Errorf("正文汇总 = %q，期望只含 text 块", bot.Text)
	}
}

// TestAttachTurnUsage 校验每轮 token 汇总配回自己那一轮：同一句话问两次时，
// 两次问答各自拿到自己的轨迹（回显只截尾部，配错就等于数字串轮）；提问文本
// 被技能改写、只有回答对得上的那一轮走回答兜底；两边都对不上的留空。
func TestAttachTurnUsage(t *testing.T) {
	base := time.Now()
	req := func(input, answer string, in, out int64, secs int) trace.Request {
		r := trace.Request{Input: input, Answer: answer, In: in, Out: out, Started: base}
		if secs > 0 {
			r.Ended = base.Add(time.Duration(secs) * time.Second)
		}
		return r
	}
	items := []histItem{
		{Role: "user", Text: "现在几点"},
		{Role: "assistant", Text: "第一次的回答"},
		{Role: "user", Text: "现在几点"},
		{Role: "assistant", Text: "第二次的回答"},
		{Role: "user", Text: "这段英文"},
		{Role: "assistant", Text: "翻译结果"},
		{Role: "user", Text: "[图片]"},
		{Role: "assistant", Text: "没有轨迹的一轮"},
	}
	// 时间升序（sessionTurns 的口径）。第三条的 Input 是用户原话，而会话里存的是
	// 技能改写后的提问文本，两边对不上，只能靠 Answer 配上。
	reqs := []trace.Request{
		req("现在几点", "第一次的回答", 1000, 20, 2),
		req("现在几点", "第二次的回答", 2000, 40, 4),
		req("/翻译 这段英文", "翻译结果", 3000, 60, 6),
	}

	attachTurnUsage(items, reqs)

	cases := []struct {
		idx     int
		in, out int64
		ms      int64
	}{
		{1, 1000, 20, 2000},
		{3, 2000, 40, 4000},
		{5, 3000, 60, 6000},
	}
	for _, c := range cases {
		u := items[c.idx].Turn
		if u == nil {
			t.Errorf("第 %d 条 bot 没配上轨迹", c.idx)
			continue
		}
		if u.In != c.in || u.Out != c.out || u.Ms != c.ms {
			t.Errorf("第 %d 条 bot = %+v，期望 in=%d out=%d ms=%d", c.idx, u, c.in, c.out, c.ms)
		}
	}
	if items[7].Turn != nil {
		t.Errorf("没有轨迹的一轮 = %+v，期望留空", items[7].Turn)
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
