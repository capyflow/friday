// custom.go 实现用户自定义指令：config custom.commands 里声明"触发词 → 提示词"，
// 聊天输入命中触发词（或"触发词 补充内容"）时在进入 agent 前展开为对应提示词，
// 相当于用户自定义的聊天快捷功能。更灵活的自定义能力（接外部工具/服务）走 MCP。
package agent

import (
	"strings"

	"github.com/capyflow/friday/internal/config"
)

// SetCustom 设置自定义指令表（启动时从 config 注入一次；顺序保留，先声明先匹配）。
func (b *Brain) SetCustom(cmds []config.CustomCommand) {
	b.customMu.Lock()
	defer b.customMu.Unlock()
	b.custom = make([]config.CustomCommand, len(cmds))
	copy(b.custom, cmds)
}

// expandCustom 把命中的自定义指令展开为提示词；未命中原样返回。
// 命中规则：输入去掉首尾空白后与触发词完全相等；或以"触发词+空白"开头，
// 余下部分作为"用户补充"附在提示词后（如"早安简报 只看天气"）。
func (b *Brain) expandCustom(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	b.customMu.RLock()
	cmds := b.custom
	b.customMu.RUnlock()
	for _, c := range cmds {
		name := strings.TrimSpace(c.Name)
		if name == "" || strings.TrimSpace(c.Prompt) == "" {
			continue
		}
		if trimmed == name {
			return strings.TrimSpace(c.Prompt)
		}
		if rest, ok := strings.CutPrefix(trimmed, name+" "); ok {
			rest = strings.TrimSpace(rest)
			if rest == "" {
				return strings.TrimSpace(c.Prompt)
			}
			return strings.TrimSpace(c.Prompt) + "\n\n用户补充：" + rest
		}
	}
	return text
}
