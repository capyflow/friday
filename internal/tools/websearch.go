// websearch.go 是 agent 的联网搜索工具（设置 → 扩展 → 工具 可配供应商）：
// 默认抓取必应（cn.bing.com，国内直连可用；DuckDuckGo 实测被墙）的 HTML 结果
// 页，解析出标题/链接/摘要——固定站点结构（li.b_algo 块多年稳定）+ 容错正则，
// 不引入 HTML 解析依赖，结构变化时表现为"无结果"而非误报。用户填了 key 可切
// Tavily（免费 1000 次/月）/ Brave（免费 2000 次/月）/ 博查（按次付费，中文优）。
package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// WebSearchTool 联网搜索。零配置可用（必应免密钥）；用户在设置 → 扩展 → 工具
// 里选供应商并填 key 即切换（Tavily / Brave / 博查）。
type WebSearchTool struct {
	// SearchFn 每次调用读取当前供应商与密钥（保存即生效，无需重建 agent）；
	// nil 按 bing 免密钥处理。
	SearchFn func() (provider, apiKey string)
	// 各供应商基地址（测试可替换为 httptest 服务）。
	BingBase   string
	TavilyBase string
	BraveBase  string
	BochaBase  string
	// HTTP 请求客户端（nil 时用 12 秒超时默认）。
	HTTP *http.Client
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	return "联网搜索最新信息（新闻、时效性事实、文档、价格等训练语料之外的内容）。" +
		"query 用一句话写清要找什么；想限定中文结果可加「site:」或中文关键词。" +
		"返回标题+链接+摘要列表；需要页面全文时用返回的链接调 web_fetch。"
}

func (t *WebSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "搜索关键词，如：Go 1.25 新特性 / 2026 新能源车销量"},
			"count": map[string]any{"type": "integer", "description": "返回条数，1-10，默认 6"},
		},
		"required": []string{"query"},
	}
}

func (t *WebSearchTool) Call(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("query 不能为空")
	}
	count := 6
	if n, ok := args["count"].(float64); ok && n >= 1 && n <= 10 {
		count = int(n)
	}

	provider, key := t.currentProvider()
	switch provider {
	case "tavily":
		return t.searchTavily(ctx, key, query, count)
	case "brave":
		return t.searchBrave(ctx, key, query, count)
	case "bocha":
		return t.searchBocha(ctx, key, query, count)
	default:
		return t.searchBing(ctx, query, count)
	}
}

// currentProvider 解析当前供应商与密钥：SearchFn 未接线或 key 为空的付费
// 供应商回落 bing（免密钥保底，不能因为配置缺失让工具瘫掉）。
func (t *WebSearchTool) currentProvider() (provider, key string) {
	if t.SearchFn == nil {
		return "bing", ""
	}
	provider, key = t.SearchFn()
	provider = strings.TrimSpace(strings.ToLower(provider))
	if key == "" && provider != "bing" {
		return "bing", ""
	}
	if provider == "" {
		provider = "bing"
	}
	return provider, key
}

// ---- 必应（免密钥抓取） ----

// bingResult 是一条搜索结果。
type bingResult struct {
	Title   string
	URL     string
	Snippet string
}

// bingAlgoRe 按 li.b_algo 块切结果；h2 里的 a 带 href 与标题，块内第一个 <p> 是摘要。
var (
	bingAlgoRe    = regexp.MustCompile(`(?s)<li class="b_algo".*?</li>`)
	bingTitleRe   = regexp.MustCompile(`(?s)<h2[^>]*>\s*<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	bingSnippetRe = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
	bingTagRe     = regexp.MustCompile(`<[^>]+>`)
)

func (t *WebSearchTool) searchBing(ctx context.Context, query string, count int) (string, error) {
	base := t.BingBase
	if base == "" {
		base = "https://cn.bing.com"
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", fmt.Sprint(maxInt(count+2, 8))) // 多抓几条，剔除后仍够数
	q.Set("mkt", "zh-CN")
	q.Set("setlang", "zh-hans")
	u := base + "/search?" + q.Encode()

	page, err := t.get(ctx, u)
	if err != nil {
		return "", fmt.Errorf("搜索失败: %w", err)
	}
	results := parseBingResults(page, count)
	if len(results) == 0 {
		return "搜索「" + query + "」没有返回结果（可能是关键词太生僻或必应页面结构变化）。", nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("「%s」的搜索结果：\n", query))
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return b.String(), nil
}

// parseBingResults 从结果页 HTML 里提取结果块；href 经 bingDirectURL 还原真实地址。
func parseBingResults(page string, count int) []bingResult {
	var out []bingResult
	for _, block := range bingAlgoRe.FindAllString(page, -1) {
		m := bingTitleRe.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		rawURL, rawTitle := m[1], m[2]
		href, err := url.QueryUnescape(rawURL)
		if err != nil {
			href = rawURL
		}
		res := bingResult{
			Title: cleanText(rawTitle),
			URL:   bingDirectURL(href),
		}
		if p := bingSnippetRe.FindStringSubmatch(block); p != nil {
			res.Snippet = cleanText(p[1])
		}
		if res.Title == "" || res.URL == "" {
			continue
		}
		out = append(out, res)
		if len(out) >= count {
			break
		}
	}
	return out
}

// bingDirectURL 必应有时把链接包成 /ck/a?...&u=a1<base64url> 跳转（计数器），
// 还原其中的真实地址；直接外链原样返回。
func bingDirectURL(href string) string {
	if !strings.Contains(href, "bing.com/ck/") {
		return href
	}
	if u, err := url.Parse(href); err == nil {
		raw := u.Query().Get("u")
		raw = strings.TrimPrefix(raw, "a1")
		if raw != "" {
			if dec, err := base64.RawURLEncoding.DecodeString(raw); err == nil && len(dec) > 0 {
				return string(dec)
			}
		}
	}
	return href
}

// ---- Tavily（免费 key 1000 次/月，https://tavily.com 注册） ----

func (t *WebSearchTool) searchTavily(ctx context.Context, key, query string, count int) (string, error) {
	base := t.TavilyBase
	if base == "" {
		base = "https://api.tavily.com"
	}
	body := fmt.Sprintf(`{"api_key":%q,"query":%q,"max_results":%d,"search_depth":"basic"}`,
		key, query, count)
	page, err := t.post(ctx, base+"/search", "application/json", body)
	if err != nil {
		return "", fmt.Errorf("搜索失败: %w", err)
	}
	// Tavily 响应：{"results":[{"title","url","content"}...]}
	results := struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}{}
	if err := json.Unmarshal([]byte(page), &results); err != nil {
		return "", fmt.Errorf("解析搜索结果失败: %w", err)
	}
	if len(results.Results) == 0 {
		return "搜索「" + query + "」没有返回结果。", nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("「%s」的搜索结果：\n", query))
	for i, r := range results.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Content != "" {
			fmt.Fprintf(&b, "   %s\n", r.Content)
		}
	}
	return b.String(), nil
}

// ---- Brave（免费 key 2000 次/月，https://brave.com/search/api 注册） ----

func (t *WebSearchTool) searchBrave(ctx context.Context, key, query string, count int) (string, error) {
	base := t.BraveBase
	if base == "" {
		base = "https://api.search.brave.com"
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", fmt.Sprint(maxInt(count, 1)))
	u := base + "/res/v1/web/search?" + q.Encode()

	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Subscription-Token", key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("搜索失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("读取搜索结果失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("搜索失败: HTTP %d: %s", resp.StatusCode, truncateRunes(string(data), 200))
	}
	// Brave 响应：{"web":{"results":[{"title","url","description"}...]}}
	results := struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}{}
	if err := json.Unmarshal(data, &results); err != nil {
		return "", fmt.Errorf("解析搜索结果失败: %w", err)
	}
	if len(results.Web.Results) == 0 {
		return "搜索「" + query + "」没有返回结果。", nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("「%s」的搜索结果：\n", query))
	for i, r := range results.Web.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Description != "" {
			fmt.Fprintf(&b, "   %s\n", r.Description)
		}
	}
	return b.String(), nil
}

// ---- 博查（按次付费，中文质量好，https://bochaai.com 注册） ----

func (t *WebSearchTool) searchBocha(ctx context.Context, key, query string, count int) (string, error) {
	base := t.BochaBase
	if base == "" {
		base = "https://api.bochaai.com"
	}
	body := fmt.Sprintf(`{"query":%q,"summary":true,"count":%d}`, query, maxInt(count, 1))
	page, err := t.postAuth(ctx, base+"/v1/web-search", "Bearer "+key, "application/json", body)
	if err != nil {
		return "", fmt.Errorf("搜索失败: %w", err)
	}
	// 博查响应：{"data":{"webPages":{"value":[{"name","url","summary"...}]}}}
	results := struct {
		Data struct {
			WebPages struct {
				Value []struct {
					Name    string `json:"name"`
					URL     string `json:"url"`
					Summary string `json:"summary"`
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
	}{}
	if err := json.Unmarshal([]byte(page), &results); err != nil {
		return "", fmt.Errorf("解析搜索结果失败: %w", err)
	}
	items := results.Data.WebPages.Value
	if len(items) == 0 {
		return "搜索「" + query + "」没有返回结果。", nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("「%s」的搜索结果：\n", query))
	for i, r := range items {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Name, r.URL)
		if r.Summary != "" {
			fmt.Fprintf(&b, "   %s\n", r.Summary)
		}
	}
	return b.String(), nil
}

// ---- 公共 HTTP 与文本清理 ----

func (t *WebSearchTool) get(ctx context.Context, u string) (string, error) {
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (t *WebSearchTool) post(ctx context.Context, u, contentType, body string) (string, error) {
	return t.postAuth(ctx, u, "", contentType, body)
}

func (t *WebSearchTool) postAuth(ctx context.Context, u, auth, contentType, body string) (string, error) {
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateRunes(string(data), 200))
	}
	return string(data), nil
}

// cleanText 去 HTML 标签、解实体、压空白。
func cleanText(s string) string {
	s = bingTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// truncateRunes 截到 n 个 rune（错误响应展示用）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
