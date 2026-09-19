package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/capyflow/friday/internal/memory"
)

// RememberTool 让 agent 记住关于用户的重要信息。
type RememberTool struct {
	Memory *memory.Store
}

func (t *RememberTool) Name() string { return "remember" }

func (t *RememberTool) Description() string {
	return "记住关于用户的重要信息（偏好、习惯、家人、纪念日、身体状况等）。用户说“记住我喜欢…”" +
		"或分享了值得长期记住的事实时使用。content 用一句话概括，不要包含与内容无关的客套话。" +
		"当新内容修正了某条已有记忆（用户改变了偏好、之前记错了），传 replaceId=旧记忆ID 覆盖它，避免新旧矛盾并存。"
}

func (t *RememberTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content":   map[string]any{"type": "string", "description": "要记住的内容，一句话，如：用户喜欢在晚饭后散步"},
			"replaceId": map[string]any{"type": "string", "description": "可选：被本条内容修正/替代的旧记忆 ID（来自 recall）"},
		},
		"required": []string{"content"},
	}
}

func (t *RememberTool) Call(_ context.Context, args map[string]any) (string, error) {
	content, _ := args["content"].(string)
	if content == "" {
		return "", fmt.Errorf("缺少参数 content")
	}
	replaceID, _ := args["replaceId"].(string)
	m, err := t.Memory.Add(content, "user")
	if err != nil {
		return "", err
	}
	replaced := ""
	if replaceID != "" && t.Memory.Delete(replaceID) {
		replaced = fmt.Sprintf("（覆盖旧记忆 #%s）", replaceID)
	}
	return fmt.Sprintf("已记住%s（#%s）：%s", replaced, m.ID, m.Content), nil
}

// RecallTool 让 agent 回忆关于用户的长期记忆。
type RecallTool struct {
	Memory *memory.Store
}

func (t *RecallTool) Name() string { return "recall" }

func (t *RecallTool) Description() string {
	return "回忆关于用户的长期记忆。回答需要用户背景信息（习惯、偏好、家人等），或用户问" +
		"“还记得我…”“我是不是说过…”时使用。q 填关键词（如“喜欢”“生日”），留空返回最近的记忆。"
}

func (t *RecallTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{"type": "string", "description": "关键词，留空返回最近的记忆"},
		},
	}
}

func (t *RecallTool) Call(_ context.Context, args map[string]any) (string, error) {
	q, _ := args["q"].(string)
	ms := t.Memory.Search(q)
	if len(ms) == 0 {
		return "还没有找到相关记忆。", nil
	}
	if len(ms) > 20 {
		ms = ms[:20]
	}
	out := "相关记忆：\n"
	for _, m := range ms {
		out += fmt.Sprintf("- #%s（%s 记下）%s：%s\n",
			m.ID, m.CreatedAt.Format("2006-01-02"), sourceLabel(m.Source), m.Content)
	}
	return out, nil
}

// ForgetTool 让 agent 忘记一条长期记忆。
type ForgetTool struct {
	Memory *memory.Store
}

func (t *ForgetTool) Name() string { return "forget" }

func (t *ForgetTool) Description() string {
	return "忘记一条长期记忆。用户要求“忘掉…”时，先用 recall 找到对应记忆的 ID，再调用本工具。"
}

func (t *ForgetTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "要忘记的记忆 ID（来自 recall）"},
		},
		"required": []string{"id"},
	}
}

func (t *ForgetTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return "", fmt.Errorf("缺少参数 id")
	}
	if t.Memory.Delete(id) {
		return fmt.Sprintf("已忘记 #%s", id), nil
	}
	return fmt.Sprintf("未找到记忆 #%s", id), nil
}

func sourceLabel(source string) string {
	switch strings.TrimSpace(source) {
	case "agent":
		return "主动"
	case "admin":
		return "后台"
	default:
		return "用户"
	}
}
