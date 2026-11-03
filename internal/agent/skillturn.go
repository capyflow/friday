// skillturn.go 决定"命中技能时交给框架的是哪一份内容"。
//
// 框架会把 Skill.Content 整段拼进 <skill-instruction>，正文多长就花多少 token。
// 所以这里按预算分两路：预算内整段给（多数技能一次到位）；超了只给概览（description +
// 技能目录 + 开头一段），并把 read_skill 补进 allowed-tools，让模型真要按这份技能做事时
// 自己去取全文与 references/。思路与 Anthropic Agent Skills 的渐进披露一致：常驻只留
// 索引，细节按需加载。
package agent

import (
	"unicode/utf8"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/skill"
	tools_pkg "github.com/capyflow/friday/internal/tools"
)

// skillForTurn 返回这一轮交给框架的技能视图（不改仓库里的原文）。
func skillForTurn(e *skill.Entry) *vxagent.Skill {
	if e == nil || e.Skill == nil {
		return nil
	}
	sk := *e.Skill
	if utf8.RuneCountInString(sk.Content) <= skill.InlineBudget {
		return &sk
	}
	sk.Content = e.Overview()
	sk.Metadata.AllowedTools = withTool(sk.Metadata.AllowedTools, tools_pkg.ReadSkillName)
	return &sk
}

// withTool 返回追加了 name 的**新**切片：技能来自共享仓库，别动它原来的底层数组。
// 技能写了 allowed-tools 时是白名单，概览模式下必须把 read_skill 放进去，否则模型
// 取不到被概览替代的正文。
func withTool(list []string, name string) []string {
	out := append([]string(nil), list...)
	for _, t := range out {
		if t == name {
			return out
		}
	}
	return append(out, name)
}
