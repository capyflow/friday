// webfetch.go 是 agent 的网页阅读工具：抓 URL 转纯文本。与 web_search 配套——
// 搜索只给摘要，agent 需要页面全文时用返回的链接调这里。只读出网、无审批
// （与天气工具同一信任级别）；限制文本类内容与 2MB 体积，剥脚本样式压空白，
// 截断到 8000 字（超长页面让 agent 自己用搜索摘要或分段抓）。
package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// WebFetchTool 抓取网页正文转纯文本。
type WebFetchTool struct {
	// HTTP 请求客户端（nil 时用 15 秒超时默认）。
	HTTP *http.Client
}

func (t *WebFetchTool) Name() string { return "web_fetch" }

func (t *WebFetchTool) Description() string {
	return "抓取一个网页并转为纯文本（自动去脚本/样式/标签）。" +
		"用于阅读 web_search 返回的链接、或打开用户给到的具体 URL。" +
		"返回正文前 8000 字；PDF/图片等非文本内容会提示不支持。"
}

func (t *WebFetchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string", "description": "完整 URL（含 http/https 前缀）"},
		},
		"required": []string{"url"},
	}
}

const fetchMaxBytes = 2 << 20 // 2MB：正常文章页足够，防大文件拖垮

func (t *WebFetchTool) Call(ctx context.Context, args map[string]any) (string, error) {
	raw, _ := args["url"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("url 须为合法的 http/https 地址")
	}

	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("请求构造失败: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("抓取失败: %w", err)
	}
	defer resp.Body.Close()

	// 非文本类（PDF/图片/二进制）直接说明，不硬转。
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err == nil &&
			!strings.HasPrefix(mt, "text/") && mt != "application/xhtml+xml" && mt != "application/json" {
			return fmt.Sprintf("该地址的内容类型是 %s，不是文本页面，无法转为文字阅读。", mt), nil
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d（页面可能已失效或需登录）", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}

	title := extractTitle(string(data))
	text := htmlToText(string(data))
	if text == "" {
		return "页面抓到了但提不出文本（可能是纯脚本渲染的页面）。", nil
	}
	var b strings.Builder
	if title != "" {
		b.WriteString("标题：" + title + "\n\n")
	}
	b.WriteString(truncateRunes(text, 8000))
	return b.String(), nil
}

var (
	fetchTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	// RE2 不支持反向引用 \1，每个要整段剥掉的标签各编译一条。
	fetchStripRes = func() []*regexp.Regexp {
		var out []*regexp.Regexp
		for _, tag := range []string{"script", "style", "noscript", "svg", "head"} {
			out = append(out, regexp.MustCompile(`(?is)<`+tag+`[^>]*>.*?</`+tag+`>`))
		}
		return out
	}()
	fetchCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	fetchTagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
)

// extractTitle 取 <title>（正文前单独给 agent 看页面主题）。
func extractTitle(page string) string {
	if m := fetchTitleRe.FindStringSubmatch(page); m != nil {
		return strings.TrimSpace(html.UnescapeString(m[1]))
	}
	return ""
}

// htmlToText 剥脚本/样式/注释/标签成纯文本：块级标签转行，行内标签删除，
// 解 HTML 实体，压空白。轻量实现，面向"读文章"够用。
func htmlToText(page string) string {
	for _, re := range fetchStripRes {
		page = re.ReplaceAllString(page, " ")
	}
	page = fetchCommentRe.ReplaceAllString(page, " ")
	// 块级边界转换行，保住可读的分段。
	for _, tag := range []string{"</p>", "</div>", "</li>", "<br", "<br/>", "<br />", "</h1>", "</h2>", "</h3>", "</h4>", "</tr>"} {
		page = strings.ReplaceAll(page, tag, "\n")
	}
	page = fetchTagRe.ReplaceAllString(page, "")
	page = html.UnescapeString(page)
	var lines []string
	for _, ln := range strings.Split(page, "\n") {
		ln = strings.Join(strings.Fields(ln), " ")
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	return strings.Join(lines, "\n")
}
