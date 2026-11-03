package agent

import (
	"strings"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/skill"
	tools_pkg "github.com/capyflow/friday/internal/tools"
)

func testEntry(body string, allowed []string) *skill.Entry {
	return &skill.Entry{
		ID:   "x",
		Path: "/tmp/x/SKILL.md",
		Skill: &vxagent.Skill{
			Metadata: vxagent.SkillMetadata{Name: "测试技能", Description: "一句话说明", AllowedTools: allowed},
			Content:  body,
		},
	}
}

// TestSkillForTurnShortBodyKeepsFullText 预算内的技能整段注入：多数技能都走这条，
// 一次到位、不多一次工具往返。
func TestSkillForTurnShortBodyKeepsFullText(t *testing.T) {
	e := testEntry("按这份说明干活。", []string{"list_todos"})
	sk := skillForTurn(e)
	if sk.Content != "按这份说明干活。" {
		t.Errorf("短正文应原样给: %q", sk.Content)
	}
	if len(sk.Metadata.AllowedTools) != 1 {
		t.Errorf("不该动 allowed-tools: %v", sk.Metadata.AllowedTools)
	}
	if e.Skill.Content != "按这份说明干活。" {
		t.Error("不能改动仓库里的原文")
	}
}

// TestSkillForTurnLongBodyFallsBackToOverview 超预算只给概览，并把 read_skill 补进
// allowed-tools——技能收窄工具时也得能取回被概览替代的正文。
func TestSkillForTurnLongBodyFallsBackToOverview(t *testing.T) {
	long := strings.Repeat("正文", skill.InlineBudget) // 明显超过预算
	e := testEntry(long, []string{"list_todos"})
	sk := skillForTurn(e)
	if sk.Content == long {
		t.Error("超预算的正文不该整段注入")
	}
	for _, want := range []string{"超过一次注入的预算", "read_skill", "测试技能"} {
		if !strings.Contains(sk.Content, want) {
			t.Errorf("概览应包含 %q:\n%s", want, sk.Content)
		}
	}
	if !containsTool(sk.Metadata.AllowedTools, tools_pkg.ReadSkillName) {
		t.Errorf("概览模式必须能调用 read_skill: %v", sk.Metadata.AllowedTools)
	}
	// 仓库里的条目不能跟着变
	if e.Skill.Content != long || containsTool(e.Skill.Metadata.AllowedTools, tools_pkg.ReadSkillName) {
		t.Error("不能改动仓库里的原文与 allowed-tools")
	}
	// 已经写了 read_skill 的不重复追加
	e2 := testEntry(long, []string{"list_todos", tools_pkg.ReadSkillName})
	if got := skillForTurn(e2).Metadata.AllowedTools; len(got) != 2 {
		t.Errorf("不该重复追加: %v", got)
	}
}

func containsTool(list []string, name string) bool {
	for _, t := range list {
		if t == name {
			return true
		}
	}
	return false
}
