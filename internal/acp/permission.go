// permission.go 负责把权限请求翻成人话。CodeBuddy 在 ACP 模式下不填
// toolCall.title/kind，只把工具名放进 _meta["codebuddy.ai/toolName"]，
// 以前一律显示"未知操作"；这里按「title → meta 描述 → rawInput 关键字段 →
// 工具名 → kind」的顺序兜底，任何一条能读出信息就不该出现"未知"。
package acp

import (
	"encoding/json"
	"fmt"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// permissionSummary 给授权卡一个可读摘要（也是日志里那一行）。
func permissionSummary(r acp.RequestPermissionRequest) string {
	if r.ToolCall.Title != nil {
		if title := strings.TrimSpace(*r.ToolCall.Title); title != "" {
			return title
		}
	}
	meta := r.ToolCall.Meta
	if desc := metaString(meta, "codebuddy.ai/description", "description"); desc != "" {
		return clipOneLine(desc, 120)
	}
	if detail := rawInputSummary(r.ToolCall.RawInput); detail != "" {
		return detail
	}
	if tool := metaToolName(meta); tool != "" {
		return toolLabel(tool)
	}
	if kind := kindString(r.ToolCall.Kind); kind != "" {
		return kindLabel(kind)
	}
	return "请求授权"
}

// permissionDetail 是卡片正文：保留工具名、类型与 rawInput 原文（截断后的），
// 供用户判断这次到底要做什么。
func permissionDetail(agentName string, r acp.RequestPermissionRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "智能体 %s 请求授权", agentName)
	if tool := metaToolName(r.ToolCall.Meta); tool != "" {
		fmt.Fprintf(&b, "\n工具：%s", tool)
	}
	if kind := kindString(r.ToolCall.Kind); kind != "" {
		if label := kindLabel(kind); label != "" {
			fmt.Fprintf(&b, "\n类型：%s", label)
		}
	}
	if r.ToolCall.RawInput != nil {
		if raw, err := json.Marshal(r.ToolCall.RawInput); err == nil && len(raw) > 0 {
			fmt.Fprintf(&b, "\n%s", sanitizeLog(string(raw)))
		}
	}
	return b.String()
}

// metaToolName 取 agent 放在 _meta 里的工具名（CodeBuddy 用它替代 title）。
func metaToolName(meta map[string]any) string {
	return metaString(meta, "codebuddy.ai/toolName", "toolName", "tool")
}

func metaString(meta map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := meta[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func kindString(kind *acp.ToolKind) string {
	if kind == nil {
		return ""
	}
	return strings.TrimSpace(string(*kind))
}

// rawInputSummary 从工具参数里挑一个最能说明动作的字段。顺序有讲究：
// command/文件路径比泛泛的 description 更具体，但 description 是 agent 自己
// 写的人话，优先展示。
func rawInputSummary(raw any) string {
	m := rawInputMap(raw)
	if m == nil {
		return ""
	}
	if s := mapString(m, "description", "reason"); s != "" {
		return clipOneLine(s, 120)
	}
	if s := mapString(m, "command", "cmd"); s != "" {
		return "执行命令：" + clipOneLine(s, 100)
	}
	if s := mapString(m, "file_path", "filePath", "path", "target"); s != "" {
		return "操作文件：" + clipOneLine(s, 100)
	}
	if s := mapString(m, "url"); s != "" {
		return "访问：" + clipOneLine(s, 100)
	}
	if s := mapString(m, "query", "pattern"); s != "" {
		return "搜索：" + clipOneLine(s, 100)
	}
	if _, ok := m["plan"]; ok {
		return "退出计划模式"
	}
	// 写文件类：正文可能很大，只说动作，不把内容塞进摘要。
	if _, ok := m["content"]; ok {
		return "写入文件内容"
	}
	if _, ok := m["old_string"]; ok {
		return "修改文件内容"
	}
	return ""
}

func rawInputMap(raw any) map[string]any {
	switch v := raw.(type) {
	case map[string]any:
		return v
	case json.RawMessage:
		var m map[string]any
		if json.Unmarshal(v, &m) == nil {
			return m
		}
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil {
			return m
		}
	}
	return nil
}

func mapString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// toolLabel 把常见工具名翻成动作。命中不了的保留原名——显示 "Bash" 也比
// "未知操作" 有用。
func toolLabel(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case n == "":
		return ""
	case strings.Contains(n, "bash"), strings.Contains(n, "shell"),
		strings.Contains(n, "exec"), strings.Contains(n, "command"):
		return "执行命令"
	case strings.Contains(n, "todo"), strings.Contains(n, "task"):
		return "更新任务"
	case strings.Contains(n, "write"), strings.Contains(n, "edit"), strings.Contains(n, "notebook"):
		return "修改文件"
	case strings.Contains(n, "read"):
		return "读取文件"
	case strings.Contains(n, "delete"), strings.Contains(n, "remove"):
		return "删除文件"
	case strings.Contains(n, "move"), strings.Contains(n, "rename"):
		return "移动文件"
	case strings.Contains(n, "webfetch"), strings.Contains(n, "websearch"),
		strings.Contains(n, "fetch"), strings.Contains(n, "browser"):
		return "访问网络"
	case strings.Contains(n, "glob"), strings.Contains(n, "grep"), strings.Contains(n, "search"):
		return "搜索"
	case n == "exitplanmode":
		return "退出计划模式"
	case n == "enterplanmode":
		return "进入计划模式"
	case n == "askuserquestion":
		return "提问"
	default:
		return name
	}
}

func kindLabel(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "read":
		return "读取文件"
	case "edit":
		return "修改文件"
	case "delete":
		return "删除文件"
	case "move":
		return "移动文件"
	case "search":
		return "搜索"
	case "execute":
		return "执行命令"
	case "think":
		return "思考"
	case "fetch":
		return "访问网络"
	case "other":
		return ""
	default:
		return kind
	}
}

// clipOneLine 压成一行并截断，避免命令里的换行把卡片标题撑开。
func clipOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len([]rune(s)) > max {
		r := []rune(s)
		return string(r[:max]) + "…"
	}
	return s
}
