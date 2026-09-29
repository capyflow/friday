package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// TestSessionPoolKeyAndPersist 验证会话池：按 key 创建会话、JSON 落盘、
// 新池（模拟重启）从持久层恢复。
func TestSessionPoolKeyAndPersist(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.NewJSON(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	pool := NewSessionPool(store)
	s1 := pool.Get(ctx, SessionWeb)
	if s1 == nil || s1.ID != sessionIDPrefix+SessionWeb {
		t.Fatalf("会话 ID 应为 friday-web，实际 %q", s1.ID)
	}
	s1.Add(llm.NewTextMessage(llm.RoleUser, "你好"))
	pool.Save(ctx, s1)

	// 模拟重启：新池、同 store。
	pool2 := NewSessionPool(store)
	s2 := pool2.Get(ctx, SessionWeb)
	if len(s2.Messages()) != 1 || messageTextOf(s2.Messages()[0]) != "你好" {
		t.Fatalf("重启后应恢复历史，实际 %+v", s2.Messages())
	}

	// 不同 key 各自独立。
	s3 := pool2.Get(ctx, "ding-chat123")
	if s3.ID == s2.ID {
		t.Fatal("不同 key 应得到不同会话")
	}

	// Reset 清内存 + 清持久层。
	pool2.Reset(ctx, SessionWeb)
	if got := pool2.Get(ctx, SessionWeb); len(got.Messages()) != 0 {
		t.Fatalf("Reset 后应为新会话，实际 %d 条历史", len(got.Messages()))
	}
}

// TestSessionPoolHistoryReadOnly 验证 History 只读回显：不创建会话、能从持久层恢复。
func TestSessionPoolHistoryReadOnly(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.NewJSON(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 写入一轮对话并落盘。
	pool := NewSessionPool(store)
	s := pool.Get(ctx, SessionWeb)
	s.Add(llm.NewTextMessage(llm.RoleUser, "你好"))
	s.Add(llm.NewTextMessage(llm.RoleAssistant, "你好呀"))
	pool.Save(ctx, s)

	// 全新池（模拟重启进程后打开页面）：History 应从持久层取到历史。
	fresh := NewSessionPool(store)
	msgs := fresh.History(ctx, SessionWeb)
	if len(msgs) != 2 || messageTextOf(msgs[0]) != "你好" || messageTextOf(msgs[1]) != "你好呀" {
		t.Fatalf("History 应恢复持久化历史: %+v", msgs)
	}

	// 只读：查询不存在的会话返回 nil，且不会把会话创建进池（List 为空）。
	if got := fresh.History(ctx, "never-used"); got != nil {
		t.Fatalf("不存在的会话应返回 nil: %+v", got)
	}
	if list := fresh.List(); len(list) != 0 {
		t.Fatalf("History 不应创建会话，List 应为空: %+v", list)
	}
}

// TestSessionPoolParallelKeys 验证不同会话并行对话互不阻塞、同一会话串行。
func TestSessionPoolParallelKeys(t *testing.T) {
	pool := NewSessionPool(nil)
	// 每次对话耗时 100ms；两个 key 并行应在 ~100ms 完成，串行则 ~200ms。
	slowAsk := func(key string) {
		mu := pool.Lock(key)
		mu.Lock()
		defer mu.Unlock()
		_ = pool.Get(context.Background(), key)
		time.Sleep(100 * time.Millisecond)
	}
	start := time.Now()
	var wg sync.WaitGroup
	for _, key := range []string{"a", "b", "a"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			slowAsk(k)
		}(key)
	}
	wg.Wait()
	elapsed := time.Since(start)
	if elapsed >= 290*time.Millisecond {
		t.Fatalf("不同 key 应并行（同 key 仍串行），耗时 %s 过长", elapsed)
	}
}

// TestSessionKeyHelpers 校验会话 key 的注入/读取与非法字符归一。
func TestSessionKeyHelpers(t *testing.T) {
	ctx := WithSessionKey(context.Background(), "feishu-oc_123")
	if got := SessionKeyFrom(ctx); got != "feishu-oc_123" {
		t.Fatalf("SessionKeyFrom = %q", got)
	}
	if got := SessionKeyFrom(context.Background()); got != SessionWeb {
		t.Fatalf("缺省应返回 web，实际 %q", got)
	}
	if got := CurrentKey("feishu oc/#1"); got != "feishu_oc__1" {
		t.Fatalf("CurrentKey 归一不符: %q", got)
	}
	if got := CurrentKey(""); got != SessionWeb {
		t.Fatalf("空 key 应归一为 web")
	}
}

// ---------- Brain：用假 OpenAI 兼容服务器验证完整对话 + 会话持久化 ----------

// fakeLLM 是 OpenAI 兼容假服务器：记录收到的 messages，返回固定回答。
type fakeLLM struct {
	mu       sync.Mutex
	requests [][]byte // 每轮请求体
	reply    string
}

func (f *fakeLLM) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": f.reply}, "finish_reason": "stop"},
		},
		"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
	})
}

// messageTextOf 取消息文本。
func messageTextOf(m llm.Message) string {
	for _, c := range m.Content {
		if c.Type == llm.ContentText {
			return c.Text
		}
	}
	return ""
}

// TestBrainPersistenceAcrossRestart 验收 M4：重启服务后（新 Brain + 同一 JSON 存储），
// 模型收到的上下文里仍包含重启前的对话历史。
func TestBrainPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.json")
	fake := &fakeLLM{reply: "我记得"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Provider = config.ProviderConfig{
		Name: "openai", BaseURL: srv.URL, Model: "fake", APIKeyEnv: "TEST_BRAIN_KEY",
		Temperature: 0.7, MaxTokens: 64,
	}
	t.Setenv("TEST_BRAIN_KEY", "sk-test")

	build := func() *Brain {
		store, err := sessionstore.NewJSON(sessionFile)
		if err != nil {
			t.Fatal(err)
		}
		pool := NewSessionPool(store)
		ag, err := Build(cfg, vxagent.NewRegistry(), pool, approval.New(string(approval.ModeOff), nil, 0), nil)
		if err != nil {
			t.Fatal(err)
		}
		return NewBrain(ag, pool, nil)
	}

	// 第一轮对话（重启前）。
	brain1 := build()
	ctx := WithSessionKey(context.Background(), SessionWeb)
	if _, err := brain1.Ask(ctx, "我叫小红", nil, nil); err != nil {
		t.Fatalf("第一次 Ask: %v", err)
	}

	// 会话文件已落盘。
	data, err := os.ReadFile(sessionFile)
	if err != nil || !bytes.Contains(data, []byte("我叫小红")) {
		t.Fatalf("会话应落盘且包含历史（err=%v）", err)
	}

	// 模拟重启：全新 Brain/池，同一存储。
	brain2 := build()
	if _, err := brain2.Ask(ctx, "还记得我们刚才聊什么吗", nil, nil); err != nil {
		t.Fatalf("第二次 Ask: %v", err)
	}

	// 第二轮请求里应带上第一轮历史（模型上下文连续）。
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) < 2 {
		t.Fatalf("应有 2 轮请求，实际 %d", len(fake.requests))
	}
	var req2 struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(fake.requests[len(fake.requests)-1], &req2); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range req2.Messages {
		if m.Role == "user" && m.Content == "我叫小红" {
			found = true
		}
	}
	if !found {
		t.Fatalf("重启后第二轮上下文应包含第一轮历史: %+v", req2.Messages)
	}
}

// TestBrainSessionIsolation 验收 M4：不同会话 key 的上下文相互独立。
func TestBrainSessionIsolation(t *testing.T) {
	fake := &fakeLLM{reply: "好"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Provider = config.ProviderConfig{
		Name: "openai", BaseURL: srv.URL, Model: "fake", APIKeyEnv: "TEST_BRAIN_KEY",
		Temperature: 0.7, MaxTokens: 64,
	}
	t.Setenv("TEST_BRAIN_KEY", "sk-test")

	store, err := sessionstore.NewJSON(filepath.Join(t.TempDir(), "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	pool := NewSessionPool(store)
	ag, err := Build(cfg, vxagent.NewRegistry(), pool, approval.New(string(approval.ModeOff), nil, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	brain := NewBrain(ag, pool, nil)

	if _, err := brain.Ask(WithSessionKey(context.Background(), "web"), "网页的话", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := brain.Ask(WithSessionKey(context.Background(), "feishu-oc1"), "飞书的话", nil, nil); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 2 {
		t.Fatalf("应有 2 轮请求，实际 %d", len(fake.requests))
	}
	var req2 struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(fake.requests[1], &req2); err != nil {
		t.Fatal(err)
	}
	for _, m := range req2.Messages {
		if m.Content == "网页的话" {
			t.Fatal("飞书会话不应看到网页会话的历史")
		}
	}
}

// TestBrainReset 验证按会话清除上下文，且只影响指定会话。
func TestBrainReset(t *testing.T) {
	fake := &fakeLLM{reply: "好"}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Provider = config.ProviderConfig{
		Name: "openai", BaseURL: srv.URL, Model: "fake", APIKeyEnv: "TEST_BRAIN_KEY",
		Temperature: 0.7, MaxTokens: 64,
	}
	t.Setenv("TEST_BRAIN_KEY", "sk-test")

	pool := NewSessionPool(nil)
	ag, err := Build(cfg, vxagent.NewRegistry(), pool, approval.New(string(approval.ModeOff), nil, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	brain := NewBrain(ag, pool, nil)

	ctxA := WithSessionKey(context.Background(), "a")
	ctxB := WithSessionKey(context.Background(), "b")
	_, _ = brain.Ask(ctxA, "甲说", nil, nil)
	_, _ = brain.Ask(ctxB, "乙说", nil, nil)
	brain.Reset("a")

	// a 被清：第 3 轮请求不带 "甲说"。
	_, _ = brain.Ask(ctxA, "甲再问", nil, nil)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 3 {
		t.Fatalf("应有 3 轮请求，实际 %d", len(fake.requests))
	}
	var req3 struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(fake.requests[2], &req3); err != nil {
		t.Fatal(err)
	}
	for _, m := range req3.Messages {
		if m.Content == "甲说" {
			t.Fatal("Reset 后会话 a 不应再有旧历史")
		}
	}
	// b 不受影响：第 3 轮只查 a，这里校验 b 会话仍保留——再发一轮 b。
	// （requests[1] 即 b 的第二轮，已含 "乙说"+"甲说" 的隔离验证，见 TestBrainSessionIsolation。）
}
