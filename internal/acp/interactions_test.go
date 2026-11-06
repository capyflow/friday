package acp

import (
	"encoding/json"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestParseCodeBuddyQuestions(t *testing.T) {
	params := json.RawMessage(`{
		"sessionId":"s1","toolCallId":"t1","inputType":"question",
		"schema":{"questions":[
			{"id":"q_0","header":"方案","question":"选哪个方案？","multiSelect":false,
			 "options":[{"label":"方案 A","description":"改动小"},{"label":"方案 B"}]},
			{"question":"第二步呢？","options":[{"label":"继续"}]}
		]}
	}`)
	prompts, ids, err := parseCodeBuddyQuestions(params)
	if err != nil {
		t.Fatalf("parseCodeBuddyQuestions: %v", err)
	}
	if len(prompts) != 2 || len(ids) != 2 {
		t.Fatalf("应解析出 2 题，实际 prompts=%+v ids=%v", prompts, ids)
	}
	if ids[0] != "q_0" || ids[1] != "q_1" {
		t.Errorf("题目 id 缺失时应用 q_<序号> 兜底，实际 %v", ids)
	}
	if prompts[0].Header != "方案" || prompts[0].Question != "选哪个方案？" {
		t.Errorf("第一题解析错误: %+v", prompts[0])
	}
	if !prompts[0].AllowText {
		t.Error("CodeBuddy 的 AskUserQuestion 支持自由输入，应允许文本框")
	}
	if len(prompts[0].Choices) != 2 || prompts[0].Choices[0].Value != "方案 A" ||
		prompts[0].Choices[0].Description != "改动小" {
		t.Errorf("选项应保留文案与说明、answer 值用文案: %+v", prompts[0].Choices)
	}
	if prompts[1].Question != "第二步呢？" || len(prompts[1].Choices) != 1 {
		t.Errorf("第二题解析错误: %+v", prompts[1])
	}
}

func TestParseCodeBuddyQuestionsRejectsNoOptions(t *testing.T) {
	_, _, err := parseCodeBuddyQuestions(json.RawMessage(
		`{"schema":{"questions":[{"question":"自由文本"}]}}`))
	if err == nil {
		t.Fatal("没有可选项的题应报错，避免生成无按钮的卡")
	}
}

func TestElicitationPromptsSingleAndMulti(t *testing.T) {
	form := acp.UnstableCreateElicitationForm{
		Message: "请选择",
		RequestedSchema: acp.UnstableElicitationSchema{
			Properties: map[string]any{
				"q1": map[string]any{
					"type": "string", "title": "方案", "description": "选一个",
					"oneOf": []any{
						map[string]any{"const": "a", "title": "方案 A", "description": "改动小"},
						map[string]any{"const": "b", "title": "方案 B"},
					},
				},
				"q2": map[string]any{
					"type": "array", "title": "要改哪些模块？",
					"items": map[string]any{"anyOf": []any{
						map[string]any{"const": "web", "title": "前端"},
						map[string]any{"const": "api", "title": "接口"},
					}},
				},
				"q1__other": map[string]any{"type": "string", "title": "方案 — Other"},
			},
			Required: []string{"q1"},
		},
	}
	prompts, err := elicitationPrompts(form)
	if err != nil {
		t.Fatalf("elicitationPrompts: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("应解析出 2 题（Other 输入框跳过），实际 %+v", prompts)
	}
	if prompts[0].ID != "q1" || prompts[0].Question != "方案" || prompts[0].MultiSelect {
		t.Errorf("单选题解析错误: %+v", prompts[0])
	}
	if len(prompts[0].Choices) != 2 || prompts[0].Choices[0].Value != "a" ||
		prompts[0].Choices[0].Label != "方案 A" {
		t.Errorf("选项应区分回传值 const 与按钮文案: %+v", prompts[0].Choices)
	}
	if prompts[1].ID != "q2" || !prompts[1].MultiSelect || len(prompts[1].Choices) != 2 {
		t.Errorf("多选题解析错误: %+v", prompts[1])
	}
}

func TestElicitationPromptsKeepsRequiredFreeText(t *testing.T) {
	form := acp.UnstableCreateElicitationForm{
		RequestedSchema: acp.UnstableElicitationSchema{
			Properties: map[string]any{
				"q1": map[string]any{"type": "string", "title": "请描述"},
			},
			Required: []string{"q1"},
		},
	}
	prompts, err := elicitationPrompts(form)
	if err != nil {
		t.Fatalf("必答自由文本应转成文本框，而不是拒绝整张表单: %v", err)
	}
	if len(prompts) != 1 || !prompts[0].AllowText || len(prompts[0].Choices) != 0 {
		t.Fatalf("自由文本题应带 AllowText 且没有选项: %+v", prompts)
	}
}

func TestElicitationPromptsOtherField(t *testing.T) {
	form := acp.UnstableCreateElicitationForm{
		RequestedSchema: acp.UnstableElicitationSchema{
			Properties: map[string]any{
				"q1": map[string]any{
					"type": "string", "title": "方案",
					"oneOf": []any{map[string]any{"const": "a", "title": "方案 A"}},
				},
				"q1__other": map[string]any{"type": "string", "title": "方案 — Other"},
			},
			Required: []string{"q1"},
		},
	}
	prompts, err := elicitationPrompts(form)
	if err != nil {
		t.Fatalf("elicitationPrompts: %v", err)
	}
	if len(prompts) != 1 || !prompts[0].AllowText || prompts[0].OtherKey != "q1__other" {
		t.Fatalf("Other 字段应开启文本框并记录回填键: %+v", prompts)
	}
}

func TestQuestionnaireFallbackBuildsGroup(t *testing.T) {
	r := permReq([]acp.PermissionOption{
		{OptionId: "questionnaire:r1:a", Kind: acp.PermissionOptionKindAllowOnce, Name: "方案 A"},
		{OptionId: "questionnaire:r1:b", Kind: acp.PermissionOptionKindAllowOnce, Name: "方案 B"},
		{OptionId: "questionnaire:r1:cancel", Kind: acp.PermissionOptionKindRejectOnce, Name: "Cancel"},
	}, nil, "选哪个方案？")
	groups := questionGroups(r)
	if len(groups) != 1 || groups[0].Question != "选哪个方案？" {
		t.Fatalf("MiniMax 的退化权限请求应还原成一道题，实际 %+v", groups)
	}
	if len(groups[0].Options) != 3 {
		t.Errorf("选项应保留（含 Cancel），实际 %+v", groups[0].Options)
	}
	if !isQuestionRequest(r) {
		t.Error("带 questionnaire: 前缀的请求应判成问答卡")
	}
	if strings.Contains(questionDetail("minimax", groups), "0 个问题") {
		t.Error("正文不该再显示 0 个问题")
	}
}
