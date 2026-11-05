package acp

// question_test.go 守两件事：哪些权限请求该判成问答卡（判错方向都要守），
// 以及问题正文从 rawInput 里能不能读成人话（agent 字段名各家不同，宽松兼容）。

import (
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func ptrStr(s string) *string              { return &s }
func ptrKind(k acp.ToolKind) *acp.ToolKind { return &k }

func permReq(opts []acp.PermissionOption, rawInput any, title string) acp.RequestPermissionRequest {
	return acp.RequestPermissionRequest{
		Options: opts,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "t1", Title: ptrStr(title), RawInput: rawInput,
		},
	}
}

var canonical = []acp.PermissionOption{
	{OptionId: "allow", Kind: acp.PermissionOptionKindAllowOnce, Name: "允许"},
	{OptionId: "reject", Kind: acp.PermissionOptionKindRejectOnce, Name: "拒绝"},
}

func TestIsQuestionRequest(t *testing.T) {
	cases := []struct {
		name string
		req  acp.RequestPermissionRequest
		want bool
	}{
		{"规范两选项的授权", permReq(canonical, map[string]any{"command": "rm -rf ./tmp"}, "执行 rm"), false},
		{"三个选项就是答案", permReq([]acp.PermissionOption{
			{OptionId: "a", Kind: acp.PermissionOptionKindAllowOnce, Name: "方案 A"},
			{OptionId: "b", Kind: acp.PermissionOptionKindAllowOnce, Name: "方案 B"},
			{OptionId: "reject", Kind: acp.PermissionOptionKindRejectOnce, Name: "拒绝"},
		}, nil, "选哪个？"), true},
		{"自定义 optionId", permReq([]acp.PermissionOption{
			{OptionId: "q1_opt_keep", Kind: acp.PermissionOptionKindAllowOnce, Name: "保留接口"},
			{OptionId: "reject", Kind: acp.PermissionOptionKindRejectOnce, Name: "拒绝"},
		}, nil, "怎么处理？"), true},
		{"rawInput 带 questions", permReq(canonical, map[string]any{
			"questions": []any{map[string]any{"question": "范围怎么定？"}},
		}, "范围怎么定？"), true},
		{"工具名命中 AskUserQuestion", permReq(canonical, nil, "AskUserQuestion"), true},
		{"ExitPlanMode 仍算授权", permReq(canonical, nil, "Exit plan mode"), false}, // 它问的是"可否退出计划模式"
		{"空选项不炸", permReq(nil, nil, "随便"), false},
		{"选项文案像授权但带 questions，仍按结构判成问答", permReq([]acp.PermissionOption{
			{OptionId: "allow", Kind: acp.PermissionOptionKindAllowOnce, Name: "允许匿名访问"},
			{OptionId: "reject", Kind: acp.PermissionOptionKindRejectOnce, Name: "禁止匿名访问"},
		}, map[string]any{"questions": []any{map[string]any{"question": "匿名访问怎么定？"}}}, "匿名访问怎么定？"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isQuestionRequest(c.req); got != c.want {
				t.Errorf("isQuestionRequest = %v，期望 %v", got, c.want)
			}
		})
	}
}

// agent 字段名各家不同：label/title/name 都得认，单题形态（没有 questions 数组）也要认。
func TestQuestionGroupsTolerantParsing(t *testing.T) {
	got := questionGroups(permReq(canonical, map[string]any{
		"questions": []any{
			map[string]any{
				"header": "STS 处置", "question": "直传接口怎么处理？", "multiSelect": true,
				"options": []any{
					map[string]any{"label": "保留接口", "description": "为开时拒绝签发"},
					map[string]any{"title": "移除接口", "hint": "彻底下线"},
					"只给文案的选项",
				},
			},
			map[string]any{"text": "第二题", "options": []any{map[string]any{"name": "甲"}}},
		},
	}, "x"))
	if len(got) != 2 {
		t.Fatalf("应解析出 2 个问题，实际 %+v", got)
	}
	if got[0].Header != "STS 处置" || !got[0].MultiSelect {
		t.Errorf("第一题头部/多选标志没解析出来: %+v", got[0])
	}
	if len(got[0].Options) != 3 || got[0].Options[1].Label != "移除接口" ||
		got[0].Options[1].Description != "彻底下线" || got[0].Options[2].Label != "只给文案的选项" {
		t.Errorf("选项字段兼容没做到: %+v", got[0].Options)
	}
	if got[1].Question != "第二题" || got[1].Options[0].Label != "甲" {
		t.Errorf("第二题没解析出来: %+v", got[1])
	}

	single := questionGroups(permReq(canonical, map[string]any{
		"question": "只有一题", "options": []any{map[string]any{"label": "甲"}},
	}, "只有一题"))
	if len(single) != 1 || single[0].Question != "只有一题" || len(single[0].Options) != 1 {
		t.Errorf("单题形态应识别: %+v", single)
	}

	if got := questionGroups(permReq(canonical, nil, "x")); got != nil {
		t.Errorf("没有 rawInput 不该编出问题: %+v", got)
	}
}

// 正文给的是"人可读兜底"：通知中心、日志和旧前端只看这一行，不能是 JSON。
func TestQuestionDetailReadable(t *testing.T) {
	groups := questionGroups(permReq(canonical, map[string]any{
		"questions": []any{map[string]any{
			"header": "范围", "question": "本轮修复的范围怎么定？",
			"options": []any{map[string]any{"label": "最小影响面", "description": "只拦新上传"}},
		}},
	}, "本轮修复的范围怎么定？"))
	text := questionDetail("zcode", groups)
	for _, want := range []string{"zcode", "本轮修复的范围怎么定？", "最小影响面", "只拦新上传", "范围"} {
		if !strings.Contains(text, want) {
			t.Errorf("正文缺少 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "{\"questions\"") || strings.HasPrefix(text, "{") {
		t.Errorf("正文不该是 JSON 原文:\n%s", text)
	}
}

// 选项映射：文案缺失时回落到 id，保证按钮永远有字可点。
func TestToApprovalOptions(t *testing.T) {
	opts := toApprovalOptions([]acp.PermissionOption{
		{OptionId: "a", Name: "  甲  ", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: "b"},
	})
	if len(opts) != 2 || opts[0].Name != "甲" || opts[0].ID != "a" || opts[0].Kind != "allow_once" {
		t.Fatalf("映射不符: %+v", opts)
	}
	if opts[1].Name != "b" {
		t.Errorf("空文案应回落到 optionId，实际 %q", opts[1].Name)
	}
}

// 下面这些形状是从本机装着的 zcode-acp-server@0.75.0 里抠出来的真值
// （dist/interaction/adapter.js 的选项构造 + locale 文案），不是编的：
// 沙箱授权一次给满 4 条、ExitPlanMode 用 approve/reject、AskUserQuestion 单选
// 带 __skip__、多选把答案编码成「<label>:yes / <label>:no」。
// 曾经按「选项多于两个」判问答，结果每一条命令权限都被画成一堆答案按钮。
func TestRealAgentPermissionShapes(t *testing.T) {
	sandbox := func(once, always, rejectOnce, rejectAlways string) []acp.PermissionOption {
		return []acp.PermissionOption{
			{OptionId: "sandbox_allow_once", Kind: acp.PermissionOptionKindAllowOnce, Name: once},
			{OptionId: "sandbox_allow_always", Kind: acp.PermissionOptionKindAllowAlways, Name: always},
			{OptionId: "sandbox_reject_once", Kind: acp.PermissionOptionKindRejectOnce, Name: rejectOnce},
			{OptionId: "sandbox_reject_always", Kind: acp.PermissionOptionKindRejectAlways, Name: rejectAlways},
		}
	}
	cases := []struct {
		name string
		req  acp.RequestPermissionRequest
		want bool
	}{
		{"zcode 沙箱授权（中文 4 选项）", permReq(
			sandbox("仅此一次", "始终允许", "拒绝一次", "始终拒绝"),
			map[string]any{"command": "rm -rf ./tmp", "cwd": "/Users/a/proj"}, "执行 rm -rf ./tmp"), false},
		{"zcode 沙箱授权（英文 4 选项）", permReq(
			sandbox("Allow once", "Always allow", "Reject once", "Always reject"),
			map[string]any{"command": "npm run build"}, "npm run build"), false},
		// 本机跑真实 zcode 会话抓下来的原帧（ZCODE_ACP_SANDBOX=1 让它往工作区外写），
		// 选项顺序是 always 在前、once 在后，与按源码重建的那条不同，单独留一条。
		{"zcode 沙箱授权（实抓帧：always 在前）", permReq([]acp.PermissionOption{
			{OptionId: "sandbox_allow_always", Kind: acp.PermissionOptionKindAllowAlways, Name: "始终允许"},
			{OptionId: "sandbox_allow_once", Kind: acp.PermissionOptionKindAllowOnce, Name: "仅此一次"},
			{OptionId: "sandbox_reject_once", Kind: acp.PermissionOptionKindRejectOnce, Name: "拒绝一次"},
			{OptionId: "sandbox_reject_always", Kind: acp.PermissionOptionKindRejectAlways, Name: "始终拒绝"},
		}, map[string]any{"path": "/Users/a/zcode-sandbox-probe/note.txt"},
			"沙箱写入放行:/Users/aaron/zcode-sandbox-probe"), false},
		// 同一次会话抓下来的真问答帧：选项 id 就是答案文案，__skip__ 是 reject_once。
		{"zcode AskUserQuestion（实抓帧）", permReq([]acp.PermissionOption{
			{OptionId: "继续补测试", Kind: acp.PermissionOptionKindAllowOnce, Name: "继续补测试"},
			{OptionId: "就此收工", Kind: acp.PermissionOptionKindAllowOnce, Name: "就此收工"},
			{OptionId: "__skip__", Kind: acp.PermissionOptionKindRejectOnce, Name: "Skip"},
		}, map[string]any{"questions": []any{map[string]any{
			"question": "接下来该做什么？", "header": "下一步", "multiSelect": false,
			"options": []any{
				map[string]any{"label": "继续补测试", "description": "继续为当前代码补充测试用例"},
				map[string]any{"label": "就此收工", "description": "到此为止，结束本次任务"},
			},
		}}}, "接下来该做什么？"), true},
		{"zcode ExitPlanMode（approve/reject）", permReq([]acp.PermissionOption{
			{OptionId: "approve", Kind: acp.PermissionOptionKindAllowOnce, Name: "同意——退出计划模式"},
			{OptionId: "reject", Kind: acp.PermissionOptionKindRejectOnce, Name: "拒绝——继续规划"},
		}, map[string]any{"plan": "1. 登录 2. 注册"}, "退出计划模式？"), false},
		{"zcode AskUserQuestion 单选（带 questions）", permReq([]acp.PermissionOption{
			{OptionId: "保留接口", Kind: acp.PermissionOptionKindAllowOnce, Name: "保留接口"},
			{OptionId: "移除接口", Kind: acp.PermissionOptionKindAllowOnce, Name: "移除接口"},
			{OptionId: "__skip__", Kind: acp.PermissionOptionKindRejectOnce, Name: "跳过"},
		}, map[string]any{"questions": []any{map[string]any{
			"question": "直传接口怎么处理？",
			"options":  []any{map[string]any{"label": "保留接口"}, map[string]any{"label": "移除接口"}},
		}}}, "直传接口怎么处理？"), true},
		{"zcode AskUserQuestion 单选（questions 丢了也要认）", permReq([]acp.PermissionOption{
			{OptionId: "保留接口", Kind: acp.PermissionOptionKindAllowOnce, Name: "保留接口"},
			{OptionId: "__skip__", Kind: acp.PermissionOptionKindRejectOnce, Name: "跳过"},
		}, nil, "直传接口怎么处理？"), true},
		{"zcode AskUserQuestion 多选（带 questions）", permReq([]acp.PermissionOption{
			{OptionId: "接口A:yes", Kind: acp.PermissionOptionKindAllowOnce, Name: "包含：接口A"},
			{OptionId: "接口A:no", Kind: acp.PermissionOptionKindRejectOnce, Name: "跳过：接口A"},
			{OptionId: "接口B:yes", Kind: acp.PermissionOptionKindAllowOnce, Name: "包含：接口B"},
			{OptionId: "接口B:no", Kind: acp.PermissionOptionKindRejectOnce, Name: "跳过：接口B"},
		}, map[string]any{"questions": []any{map[string]any{
			"question": "哪些接口保留？", "multiSelect": true,
			"options": []any{map[string]any{"label": "接口A"}, map[string]any{"label": "接口B"}},
		}}}, "哪些接口保留？"), true},
		{"zcode AskUserQuestion 多选（questions 丢了靠 id 配对认）", permReq([]acp.PermissionOption{
			{OptionId: "接口A:yes", Kind: acp.PermissionOptionKindAllowOnce, Name: "包含：接口A"},
			{OptionId: "接口A:no", Kind: acp.PermissionOptionKindRejectOnce, Name: "跳过：接口A"},
		}, nil, "哪些接口保留？"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isQuestionRequest(c.req); got != c.want {
				t.Errorf("isQuestionRequest = %v，期望 %v", got, c.want)
			}
		})
	}
}

// 问答卡的正文必须能从 zcode 的真实 rawInput 里还原出来：它把 questions 同时放在
// 顶层和 input.questions 下，且选项只有 label 一个字段。
func TestQuestionGroupsFromRealZcodePayload(t *testing.T) {
	groups := questionGroups(permReq([]acp.PermissionOption{
		{OptionId: "保留接口", Kind: acp.PermissionOptionKindAllowOnce, Name: "保留接口"},
		{OptionId: "__skip__", Kind: acp.PermissionOptionKindRejectOnce, Name: "跳过"},
	}, map[string]any{
		"questions": []any{map[string]any{
			"question": "直传接口怎么处理？", "header": "STS 处置",
			"options": []any{map[string]any{"label": "保留接口", "description": "为开时拒绝签发"}},
		}},
	}, "直传接口怎么处理？"))
	if len(groups) != 1 {
		t.Fatalf("应还原出 1 个问题，实际 %+v", groups)
	}
	if groups[0].Question != "直传接口怎么处理？" || groups[0].Header != "STS 处置" ||
		len(groups[0].Options) != 1 || groups[0].Options[0].Label != "保留接口" {
		t.Errorf("真实问答正文还原不符: %+v", groups[0])
	}
	// 多选题的按钮是 agent 给的 yes/no 对，卡片按原样画，不能自己合并。
	opts := toApprovalOptions([]acp.PermissionOption{
		{OptionId: "接口A:yes", Kind: acp.PermissionOptionKindAllowOnce, Name: "包含：接口A"},
		{OptionId: "接口A:no", Kind: acp.PermissionOptionKindRejectOnce, Name: "跳过：接口A"},
	})
	if len(opts) != 2 || opts[0].ID != "接口A:yes" || opts[1].Name != "跳过：接口A" {
		t.Errorf("多选选项应原样搬进卡片: %+v", opts)
	}
}
