package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/capyflow/friday/internal/memory"
)

// RememberTool 让 agent 保存/更新一页长期记忆（wiki 主题页）。
type RememberTool struct {
	Memory *memory.WikiStore
}

func (t *RememberTool) Name() string { return "remember" }

func (t *RememberTool) Description() string {
	return "保存/更新一页长期记忆（按主题分页的 wiki）。用户说“记住…”或分享了值得长期记住的事实时使用。" +
		"body 必须是该主题的完整正文——先 recall 查相关主题页，把新信息与已知内容合并整理后整体写入" +
		"（保存会替换整页，不要只写增量）。有对应页面就传其 id（或相同 title）更新，没有就新建" +
		"（title 起主题名，如“用户偏好”“家庭与重要的人”）。用户改变偏好/纠正旧信息时，在正文中改写后再保存。"
}

func (t *RememberTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "description": "页面标题（主题名），如：用户偏好"},
			"body":  map[string]any{"type": "string", "description": "页面完整 markdown 正文（该主题全部已知信息，保存后整体替换）"},
			"id":    map[string]any{"type": "string", "description": "可选：要更新的页面 ID（来自 recall）；缺省按标题匹配更新，无匹配则新建"},
		},
		"required": []string{"title", "body"},
	}
}

func (t *RememberTool) Call(_ context.Context, args map[string]any) (string, error) {
	title, _ := args["title"].(string)
	body, _ := args["body"].(string)
	id, _ := args["id"].(string)
	if title == "" {
		return "", fmt.Errorf("缺少参数 title")
	}
	if body == "" {
		return "", fmt.Errorf("缺少参数 body")
	}
	p, created, err := t.Memory.Save(id, title, body, "agent")
	if err != nil {
		return "", err
	}
	if created {
		return fmt.Sprintf("已新建记忆页《%s》（#%s）", p.Title, p.ID), nil
	}
	return fmt.Sprintf("已更新记忆页《%s》（#%s）", p.Title, p.ID), nil
}

// RecallTool 让 agent 回忆关于用户的长期记忆（wiki 主题页）。
type RecallTool struct {
	Memory *memory.WikiStore
}

func (t *RecallTool) Name() string { return "recall" }

func (t *RecallTool) Description() string {
	return "回忆关于用户的长期记忆（按主题分页）。回答需要用户背景信息（习惯、偏好、家人等），" +
		"或用户问“还记得我…”“我是不是说过…”时使用。这是按主题分页的 wiki，用法是" +
		"「先看目录、再读整页」：不确定主题名时先不带参数列目录；有把握就 q 填关键词（空格分隔、任意命中即算），" +
		"命中唯一一页会直接给出正文，多页时返回列表与片段、再用 id 读取整页。搜不到时会自动把目录给你。"
}

func (t *RecallTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q":  map[string]any{"type": "string", "description": "关键词（匹配页面标题与正文），留空返回全部页面"},
			"id": map[string]any{"type": "string", "description": "页面 ID（来自搜索结果），读取整页正文"},
		},
	}
}

func (t *RecallTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	if strings.TrimSpace(id) != "" {
		p, ok := t.Memory.Read(id)
		if !ok {
			return fmt.Sprintf("未找到记忆页 #%s", id), nil
		}
		return fmt.Sprintf("记忆页 #%s《%s》（%s 更新 · %s）：\n%s",
			p.ID, p.Title, p.UpdatedAt.Format("2006-01-02"), memorySourceLabel(p.Source), p.Body), nil
	}
	q, _ := args["q"].(string)
	ms := t.Memory.Search(q)
	if len(ms) == 0 {
		// wiki 的兜底不是"没有就算了"，而是把目录摊开：让模型按标题自己挑页面。
		if all := t.Memory.Pages(); len(all) == 0 {
			return "长期记忆里还没有任何页面（用户还没交代过值得长期记住的事，或已被清空）。", nil
		} else {
			return fmt.Sprintf("没有命中「%s」的页面（关键词按空格拆开、任意命中即算）。下面是全部记忆页目录，"+
				"按标题挑一页用 id 读全文，或换个更短的词再搜：\n%s", strings.TrimSpace(q), pageIndex(all, 20)), nil
		}
	}
	// 只命中一页：wiki 里这通常就是要找的那页，直接把正文给出，省一次往返。
	if len(ms) == 1 {
		if page, ok := t.Memory.Read(ms[0].ID); ok && len([]rune(page.Body)) <= 4000 {
			return fmt.Sprintf("命中记忆页 #%s《%s》（%s 更新 · %s）：\n%s",
				page.ID, page.Title, page.UpdatedAt.Format("2006-01-02"), memorySourceLabel(page.Source), page.Body), nil
		}
	}
	if len(ms) > 20 {
		ms = ms[:20]
	}
	out := "相关记忆页：\n"
	for _, m := range ms {
		line := fmt.Sprintf("- #%s《%s》（%s 更新 · %s）",
			m.ID, m.Title, m.UpdatedAt.Format("2006-01-02"), memorySourceLabel(m.Source))
		if m.Snippet != "" {
			line += "：" + m.Snippet
		}
		out += line + "\n"
	}
	out += "需要完整内容时用 id 参数读取对应页面。"
	return out, nil
}

// pageIndex 列页面目录（标题 + 更新时间），最多 max 条；搜不到与无参数列目录共用。
func pageIndex(pages []memory.PageMeta, max int) string {
	if len(pages) > max {
		pages = pages[:max]
	}
	var b strings.Builder
	for _, m := range pages {
		fmt.Fprintf(&b, "- #%s《%s》（%s 更新 · %s）\n",
			m.ID, m.Title, m.UpdatedAt.Format("2006-01-02"), memorySourceLabel(m.Source))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ForgetTool 让 agent 删除一页长期记忆（整个主题页）。
type ForgetTool struct {
	Memory *memory.WikiStore
}

func (t *ForgetTool) Name() string { return "forget" }

func (t *ForgetTool) Description() string {
	return "删除一页长期记忆（整页删除，不可恢复）。用户要求“忘掉…”且该主题页整体过时时使用：" +
		"先用 recall 找到对应页面的 ID，再调用本工具；只是部分内容过时就改用 remember 改写页面。"
}

func (t *ForgetTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "要删除的记忆页 ID（来自 recall）"},
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
		return fmt.Sprintf("已删除记忆页 #%s", id), nil
	}
	return fmt.Sprintf("未找到记忆页 #%s", id), nil
}

// memorySourceLabel 记忆来源的中文标签。
func memorySourceLabel(source string) string {
	switch strings.TrimSpace(source) {
	case "agent":
		return "主动"
	case "admin":
		return "后台"
	case "migrate":
		return "迁移"
	default:
		return "用户"
	}
}
