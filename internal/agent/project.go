// project.go 把「Friday 正在哪个目录里干活」翻译成模型需要知道的两件事：
// 项目约定（AGENTS.md）与项目自述（README.md）。
//
// 为什么一个整段、一个摘要：AGENTS.md 本来就是写给 agent 看的指令，照办需要全文；
// README 是写给人的长文档，整段进系统提示词会让每轮都变贵，所以只给开头一段加路径，
// 细节让模型自己用 read_file 取。两个文件都不存在时什么都不注入——没有项目上下文
// 是常态（个人助理跑在自己的数据目录里），不该为此多一行提示词。
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// agentsMaxRunes 是 AGENTS.md 的注入上限。超了不静默吞掉：截断并在末尾写明
	// 还剩多少、原文在哪，让人知道该去精简或去读文件。
	agentsMaxRunes = 6000
	// readmeMaxRunes 是 README 摘要长度：够说清"这个项目是干什么的"即可。
	readmeMaxRunes = 900
)

// projectFileNames 是候选文件名（按顺序取第一个存在的）。macOS 大小写不敏感，
// 但 Linux 与容器里敏感，所以两种写法都认。
var projectFileNames = map[string][]string{
	"agents": {"AGENTS.md", "agents.md"},
	"readme": {"README.md", "readme.md"},
}

// firstExisting 返回目录下第一个存在的候选文件路径；都没有时返回空串。
func firstExisting(dir, kind string) string {
	for _, name := range projectFileNames[kind] {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// readBrief 读取一个文本文件并按 rune 截断（绝不切断多字节字符），同时返回原文
// 总字数——截断时要告诉模型"还剩多少、去哪看全文"。
func readBrief(path string, max int) (text string, total int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	text = strings.TrimSpace(string(data))
	total = utf8.RuneCountInString(text)
	if total <= max {
		return text, total, nil
	}
	return string([]rune(text)[:max]), total, nil
}

// codingRules 编码场景下的工作守则。
//
// 为什么单独一段而不是塞进全局人设提示词：这段只在"这条会话绑定了某个项目目录"时
// 注入（见 sessionProjectContext），生活模式的日常对话不会平白多出几百字；而且
// 内容是干代码的纪律——先定位再动手、改完要验证——这些正是内置 agent 从"管家"
// 变成"能自己把活干完"时最缺的行为约束。
const codingRules = `【在这个目录里干活的规矩】
1. 先定位再动手：不知道代码在哪，先用 search_code 搜内容、find_files 找文件名，别凭猜测改文件。
2. 改之前先读：用 read_file 看清楚要改的那一段（可带 offset 续读），别凭记忆改。
3. 改动要小而准：优先 edit_files（一次改多处、多个文件）或 edit_file（单处精确替换）；只有整份重写才用 write_file。
4. 动手前先看清现状：git_status 看当前分支与未提交改动，git_diff 看已有差异，别覆盖别人的改动。
5. 改完要验证：用 exec_command 在这个目录里跑相关测试或构建，把真实结果（通过/失败/报错）如实说出来，别只说"改好了"。
6. 活儿大可以拆开并行：互相独立的调查（摸清某块代码、同时查几处）用 delegate_task 派子任务去做，自己只收结论；要分步推进的活儿（改好几处、每处都得验证）用 create_plan 列步骤再按步执行。`

// sessionProjectContext 生成"带项目目录的会话"的项目上下文：项目约定（AGENTS.md
// / README.md）+ 编码守则。目录里没有这两个文件时也要给守则——那恰恰是最需要
// 行为约束的项目（没人写过 AGENTS.md 的仓库）。
func sessionProjectContext(dir string) string {
	var b strings.Builder
	if pc := projectContext(dir); pc != "" {
		b.WriteString(pc)
		b.WriteString("\n\n")
	}
	b.WriteString(codingRules)
	return b.String()
}

// projectContext 生成会话开始时注入的项目上下文系统消息；无事可说时返回空串。
func projectContext(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	agentsPath, readmePath := firstExisting(dir, "agents"), firstExisting(dir, "readme")
	if agentsPath == "" && readmePath == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("【项目上下文｜会话开始时自动注入，不是用户说的话】\n")
	b.WriteString("当前工作目录：" + dir + "\n")
	if agentsPath != "" {
		text, total, err := readBrief(agentsPath, agentsMaxRunes)
		if err != nil {
			fmt.Fprintf(&b, "\n（%s 读不动：%v——需要时用 read_file 自己读）\n", agentsPath, err)
		} else {
			b.WriteString("\n以下是该项目的约定（" + agentsPath)
			if total > agentsMaxRunes {
				fmt.Fprintf(&b, "，共 %d 字，此处只放了前 %d 字，余下用 read_file 读原文", total, agentsMaxRunes)
			}
			b.WriteString("）：\n")
			b.WriteString(text + "\n")
		}
	}
	if readmePath != "" {
		text, total, err := readBrief(readmePath, readmeMaxRunes)
		if err != nil {
			fmt.Fprintf(&b, "\n（%s 读不动：%v）\n", readmePath, err)
		} else {
			b.WriteString("\n项目自述（" + readmePath + " 开头摘要")
			if total > readmeMaxRunes {
				fmt.Fprintf(&b, "，全文 %d 字、已截断", total)
			}
			b.WriteString("）：\n")
			b.WriteString(text + "\n")
			b.WriteString("需要细节就用 read_file 读这个文件，不要凭摘要猜。\n")
		}
	}
	b.WriteString("\n按上面的项目约定干活。这是背景，不必在回复里复述，也不必声明「我读了 AGENTS.md」。")
	return b.String()
}
