package acp

import (
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/agent"
)

// 一次工具调用被 agent 拆成多条 update（codebuddy 的进度更新常常不带
// status）：时间线必须按 toolCallId 归并成一行，run 只投一次，最终状态
// 回填同一行——否则每来一条进度就多一个永远转圈的"工具调用"。

func toolCall(id, title string, status acp.ToolCallStatus) acp.SessionNotification {
	return acp.SessionNotification{Update: acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{ToolCallId: acp.ToolCallId(id), Title: title, Status: status},
	}}
}

func toolUpdate(id string, status string) acp.SessionNotification {
	u := &acp.SessionToolCallUpdate{ToolCallId: acp.ToolCallId(id)}
	if status != "" {
		s := acp.ToolCallStatus(status)
		u.Status = &s
	}
	return acp.SessionNotification{Update: acp.SessionUpdate{ToolCallUpdate: u}}
}

func messageChunk(text string) acp.SessionNotification {
	return acp.SessionNotification{Update: acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(text)},
	}}
}

// collect 跑一串 update，返回投出的步骤事件。
func collect(updates ...acp.SessionNotification) []agent.Step {
	var steps []agent.Step
	sink := newTurnSink(TurnHooks{OnStep: func(s agent.Step) { steps = append(steps, s) }})
	for _, u := range updates {
		sink.handle(u)
	}
	return steps
}

func TestToolCallUpdatesMergeByID(t *testing.T) {
	steps := collect(
		toolCall("t1", "Bash", acp.ToolCallStatusInProgress),
		toolUpdate("t1", ""), // 进度内容：不带 status，不改时间线
		toolUpdate("t1", "in_progress"),
		toolUpdate("t1", "completed"), // 完成时不带标题，沿用首条标题
	)
	if len(steps) != 2 {
		t.Fatalf("一次调用应只投 run + ok 两条，实际 %d: %+v", len(steps), steps)
	}
	if s := steps[0]; s.Status != "run" || s.ID != "t1" || s.Name != "Bash" {
		t.Fatalf("首条状态不符: %+v", s)
	}
	if s := steps[1]; s.Status != "ok" || s.ID != "t1" || s.Name != "Bash" {
		t.Fatalf("收尾状态不符: %+v", s)
	}
}

func TestToolCallFailAndCancelled(t *testing.T) {
	for _, raw := range []string{"failed", "cancelled"} {
		steps := collect(
			toolCall("t1", "Bash", acp.ToolCallStatusInProgress),
			toolUpdate("t1", raw),
			toolUpdate("t1", raw), // 重复收尾不再投
		)
		if len(steps) != 2 || steps[1].Status != "fail" {
			t.Fatalf("%s 应映射为 fail 且只投一次，实际 %+v", raw, steps)
		}
	}
}

func TestToolCallsKeepSeparateRows(t *testing.T) {
	steps := collect(
		toolCall("t1", "Bash", acp.ToolCallStatusInProgress),
		toolCall("t2", "Bash", acp.ToolCallStatusInProgress), // 同名但在跑，仍是两次调用
		toolUpdate("t1", "completed"),
		toolUpdate("t2", "completed"),
	)
	if len(steps) != 4 {
		t.Fatalf("两次调用各应 run+ok，实际 %+v", steps)
	}
	got := []string{steps[0].ID, steps[1].ID, steps[2].ID, steps[3].ID}
	want := []string{"t1", "t2", "t1", "t2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("事件顺序不符: %v", got)
		}
	}
}

func TestToolStepFlushesPendingTextFirst(t *testing.T) {
	var events []string
	sink := newTurnSink(TurnHooks{
		OnText: func(s string) { events = append(events, "text:"+s) },
		OnStep: func(s agent.Step) { events = append(events, "step:"+s.Status) },
	})
	sink.handle(messageChunk("先看看文件"))
	sink.handle(toolCall("t1", "Bash", acp.ToolCallStatusInProgress))
	sink.handle(toolUpdate("t1", "completed"))
	sink.handle(messageChunk("再继续回答"))
	sink.finish()
	want := []string{"text:先看看文件", "step:run", "step:ok", "text:再继续回答"}
	if len(events) != len(want) {
		t.Fatalf("事件序列不符: %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("事件序列不符: %v", events)
		}
	}
}

func TestToolDetailsCarryInputAndOutput(t *testing.T) {
	var steps []agent.Step
	sink := newTurnSink(TurnHooks{OnStep: func(s agent.Step) { steps = append(steps, s) }})
	sink.handle(acp.SessionNotification{Update: acp.SessionUpdate{
		ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: "t1", Title: "Bash", Kind: acp.ToolKindExecute,
			Status:   acp.ToolCallStatusInProgress,
			RawInput: map[string]any{"command": "uname -a"},
		},
	}})
	text := "hello"
	sink.handle(acp.SessionNotification{Update: acp.SessionUpdate{
		ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: "t1", Status: ptrStatus("completed"),
			Content: []acp.ToolCallContent{{Content: &acp.ToolCallContentContent{
				Content: acp.TextBlock(text),
			}}},
		},
	}})
	if len(steps) != 2 {
		t.Fatalf("应投 run + ok，实际 %+v", steps)
	}
	if !strings.Contains(steps[0].Detail, "uname -a") {
		t.Fatalf("运行中应带参数: %+v", steps[0])
	}
	if steps[1].Detail != text {
		t.Fatalf("完成时展开区应显示输出: %+v", steps[1])
	}
	// 同状态再次带来新详情：允许更新展示。
	sink.handle(acp.SessionNotification{Update: acp.SessionUpdate{
		ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: "t1", Status: ptrStatus("completed"),
			RawOutput: map[string]any{"exit": 0},
		},
	}})
	if len(steps) != 3 || !strings.Contains(steps[2].Detail, "exit") {
		t.Fatalf("后到的输出应更新同一行: %+v", steps)
	}
}

func ptrStatus(s string) *acp.ToolCallStatus {
	st := acp.ToolCallStatus(s)
	return &st
}

func TestToolProgressCarriesArgsAndOutput(t *testing.T) {
	var steps []agent.Step
	sink := newTurnSink(TurnHooks{OnStep: func(s agent.Step) { steps = append(steps, s) }})
	sink.handle(toolCall("t1", "Bash", acp.ToolCallStatusInProgress)) // 首条没有参数
	sink.handle(acp.SessionNotification{Update: acp.SessionUpdate{
		ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: "t1", RawInput: map[string]any{"command": "ls -la"},
			Content: []acp.ToolCallContent{{Content: &acp.ToolCallContentContent{
				Content: acp.TextBlock("目录列表输出"),
			}}},
		},
	}})
	if len(steps) != 2 || !strings.Contains(steps[1].Detail, "ls -la") || steps[1].Status != "run" {
		t.Fatalf("进度 update 的参数应原地补到运行行: %+v", steps)
	}
	// 完成事件不带内容：沿用进度里见过的输出。
	sink.handle(toolUpdate("t1", "completed"))
	last := steps[len(steps)-1]
	if last.Status != "ok" || last.Detail != "目录列表输出" {
		t.Fatalf("完成时应沿用进度里的输出: %+v", last)
	}
}
