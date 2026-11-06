package acp

import (
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/config"
)

func permissionReqForSummary(title string, meta map[string]any, raw any, kind *acp.ToolKind) acp.RequestPermissionRequest {
	r := acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{ToolCallId: "t1", Meta: meta, RawInput: raw},
	}
	if title != "" {
		r.ToolCall.Title = &title
	}
	if kind != nil {
		r.ToolCall.Kind = kind
	}
	return r
}

func TestPermissionSummaryFallsBackToMetaAndRawInput(t *testing.T) {
	execute := acp.ToolKindExecute
	cases := []struct {
		name string
		req  acp.RequestPermissionRequest
		want string
	}{
		{
			name: "title 优先",
			req:  permissionReqForSummary("执行 rm", nil, map[string]any{"command": "rm"}, nil),
			want: "执行 rm",
		},
		{
			name: "CodeBuddy 的 toolName + command",
			req: permissionReqForSummary("", map[string]any{
				"codebuddy.ai/toolName": "Bash",
			}, map[string]any{"command": "rm -rf ./tmp"}, nil),
			want: "执行命令：rm -rf ./tmp",
		},
		{
			name: "CodeBuddy 的 description",
			req: permissionReqForSummary("", map[string]any{
				"codebuddy.ai/toolName":    "Bash",
				"codebuddy.ai/description": "需要写入工作区外的目录",
			}, map[string]any{"command": "mkdir /tmp/x"}, nil),
			want: "需要写入工作区外的目录",
		},
		{
			name: "Write 的 file_path",
			req: permissionReqForSummary("", map[string]any{
				"codebuddy.ai/toolName": "Write",
			}, map[string]any{"file_path": "internal/acp/bridge.go"}, nil),
			want: "操作文件：internal/acp/bridge.go",
		},
		{
			name: "ExitPlanMode 没有 rawInput",
			req:  permissionReqForSummary("", map[string]any{"codebuddy.ai/toolName": "ExitPlanMode"}, nil, nil),
			want: "退出计划模式",
		},
		{
			name: "只有 kind",
			req:  permissionReqForSummary("", nil, nil, &execute),
			want: "执行命令",
		},
		{
			name: "什么都没有",
			req:  permissionReqForSummary("", nil, nil, nil),
			want: "请求授权",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := permissionSummary(c.req); got != c.want {
				t.Errorf("permissionSummary = %q，期望 %q", got, c.want)
			}
		})
	}
}

func TestPermissionDetailKeepsToolAndRawInput(t *testing.T) {
	r := permissionReqForSummary("", map[string]any{
		"codebuddy.ai/toolName": "Bash",
	}, map[string]any{"command": "rm -rf ./tmp"}, nil)
	got := permissionDetail("codebuddy-code", r)
	for _, want := range []string{"codebuddy-code", "Bash", "rm -rf"} {
		if !strings.Contains(got, want) {
			t.Errorf("详情缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "（）") {
		t.Errorf("kind 为空时不该留下空括号: %q", got)
	}
}

func TestClientCapabilitiesPerAgent(t *testing.T) {
	codebuddy := clientCapabilitiesFor(config.AcpAgentConfig{
		Name: "codebuddy-code", Command: "npx",
		Args: []string{"-y", "@tencent-ai/codebuddy-code@2.179.3", "--acp"},
	})
	if got := codebuddy.Meta["codebuddy.ai"]; got == nil {
		t.Fatalf("CodeBuddy 应声明 codebuddy.ai 能力，实际 %+v", codebuddy)
	} else if m, ok := got.(map[string]any); !ok || m["question"] != true {
		t.Errorf("CodeBuddy 的 question 能力应为 true，实际 %#v", got)
	}
	if codebuddy.Elicitation != nil {
		t.Error("CodeBuddy 不该被声明 elicitation（提问走私有扩展方法）")
	}

	minimax := clientCapabilitiesFor(config.AcpAgentConfig{
		Name: "minimax-code", Command: "npx",
		Args: []string{"-y", "@minimax-ai/code@0.7.4", "mcode", "acp"},
	})
	if minimax.Elicitation == nil || minimax.Elicitation.Form == nil {
		t.Errorf("MiniMax 应声明 elicitation.form，实际 %+v", minimax)
	}
	if minimax.Meta != nil {
		t.Error("MiniMax 不该被声明 codebuddy.ai 能力")
	}

	other := clientCapabilitiesFor(config.AcpAgentConfig{Name: "codex", Command: "npx"})
	if other.Meta != nil || other.Elicitation != nil {
		t.Errorf("其他 agent 不该被开提问能力，实际 %+v", other)
	}
}
