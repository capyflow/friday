// webtools_test.go 覆盖 web_search（必应解析/跳转还原/无结果）与 web_fetch
// （正文提取/脚本剥离/非文本提示/非法 URL）。
package tools

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearchBingParse(t *testing.T) {
	// 一条直接外链 + 一条 bing.com/ck/a 包裹的跳转（u=a1<base64url>）。
	real := "https://example.com/post/1"
	wrapped := "https://www.bing.com/ck/a?!&&p=x&u=a1" +
		base64.RawURLEncoding.EncodeToString([]byte(real))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`<html><body><ol>` +
			`<li class="b_algo"><h2><a href="https://example.com/a">Go 1.25 <b>新特性</b></a></h2>` +
			`<div><p>特性一：泛型别名。</p></div></li>` +
			`<li class="b_algo"><h2><a href="` + wrapped + `">第二篇文章</a></h2>` +
			`<div><p>这是摘要内容。</p></div></li>` +
			`<li class="b_algo"><h2><a href="">无链接块</a></h2></li>` +
			`</ol></body></html>`))
	}))
	t.Cleanup(srv.Close)

	tr := &WebSearchTool{BingBase: srv.URL}
	out, err := tr.Call(context.Background(), map[string]any{"query": "Go 1.25 新特性", "count": float64(2)})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "1. Go 1.25 新特性") || !strings.Contains(out, "https://example.com/a") ||
		!strings.Contains(out, "特性一：泛型别名") {
		t.Fatalf("首条结果缺失: %q", out)
	}
	if !strings.Contains(out, real) {
		t.Fatalf("ck/a 跳转未还原真实地址: %q", out)
	}
	if strings.Contains(out, "bing.com/ck/") {
		t.Fatalf("输出里不应残留必应跳转链接: %q", out)
	}
}

func TestWebSearchBingEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><div class="b_no">没有找到</div></body></html>`))
	}))
	t.Cleanup(srv.Close)

	tr := &WebSearchTool{BingBase: srv.URL}
	out, err := tr.Call(context.Background(), map[string]any{"query": "生僻词"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "没有返回结果") {
		t.Fatalf("无结果应友好提示: %q", out)
	}
	if _, err := tr.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("空 query 应报错")
	}
}

func TestWebSearchTavily(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"T1","url":"https://a.com","content":"内容A"},{"title":"T2","url":"https://b.com","content":""}]}`))
	}))
	t.Cleanup(srv.Close)

	tr := &WebSearchTool{TavilyBase: srv.URL,
		SearchFn: func() (string, string) { return "tavily", "tvly-test" }}
	out, err := tr.Call(context.Background(), map[string]any{"query": "test"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "T1") || !strings.Contains(out, "内容A") || !strings.Contains(out, "T2") {
		t.Fatalf("Tavily 结果缺失: %q", out)
	}
}

func TestWebFetchExtract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>测试页面 &amp; 标题</title>` +
			`<style>body{color:red}</style><script>var x=1;</script></head><body>` +
			`<h1>标题行</h1><p>第一段内容，含 <b>加粗</b> 与 &quot;实体&quot;。</p>` +
			`<div>第二段，很长的一行   带多余   空格。</div><script>evil()</script></body></html>`))
	}))
	t.Cleanup(srv.Close)

	tr := &WebFetchTool{}
	out, err := tr.Call(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "测试页面 & 标题") {
		t.Fatalf("标题未解实体: %q", out)
	}
	if !strings.Contains(out, "第一段内容，含 加粗 与 \"实体\"。") {
		t.Fatalf("正文提取失败: %q", out)
	}
	if strings.Contains(out, "script") || strings.Contains(out, "evil()") || strings.Contains(out, "color:red") {
		t.Fatalf("脚本/样式未剥离: %q", out)
	}
	if strings.Contains(out, "   ") {
		t.Fatalf("空白未压缩: %q", out)
	}
}

func TestWebFetchGuards(t *testing.T) {
	// 非文本内容类型给友好提示。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4"))
	}))
	t.Cleanup(srv.Close)

	tr := &WebFetchTool{}
	out, err := tr.Call(context.Background(), map[string]any{"url": srv.URL})
	if err != nil || !strings.Contains(out, "不是文本页面") {
		t.Fatalf("PDF 应友好提示: out=%q err=%v", out, err)
	}

	// 非法协议拒绝。
	if _, err := tr.Call(context.Background(), map[string]any{"url": "ftp://x/y"}); err == nil {
		t.Fatal("ftp 应拒绝")
	}
	if _, err := tr.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("空 url 应报错")
	}

	// 404 报错。
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv404.Close)
	if _, err := tr.Call(context.Background(), map[string]any{"url": srv404.URL}); err == nil {
		t.Fatal("404 应报错")
	}
}
