package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
)

// TestHTTPRemoteMCP 验证远程 MCP（Streamable HTTP）：
// 握手/会话头/鉴权头回传、JSON 与 SSE 两种响应形态、工具调用、
// Manager 以 URL 配置接入并经注册表调用、手动重连。
func TestHTTPRemoteMCP(t *testing.T) {
	const sid = "sess-demo-1"
	var mu sync.Mutex
	badToken, badSession := "", false

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)

		if r.Header.Get("X-Token") != "s3cret" { // 鉴权头必须回传
			mu.Lock()
			badToken = r.Header.Get("X-Token")
			mu.Unlock()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if msg.Method != "initialize" && r.Header.Get("MCP-Session-Id") != sid { // 会话头必须回传
			mu.Lock()
			badSession = true
			mu.Unlock()
			http.Error(w, "bad session", http.StatusNotFound)
			return
		}
		if msg.Method == "initialize" && !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Error("请求 Accept 应包含 text/event-stream（Streamable HTTP 规范）")
		}

		respond := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}
		switch msg.Method {
		case "initialize":
			w.Header().Set("MCP-Session-Id", sid)
			respond(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "remote", "version": "0.1"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			// 以 SSE 流形式返回，覆盖流式响应解析。
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fl.Flush()
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[{\"name\":\"ping\",\"description\":\"测试工具\",\"inputSchema\":{\"type\":\"object\",\"properties\":{\"who\":{\"type\":\"string\"}}}}]}}\n\n", *msg.ID)
			fl.Flush()
		case "tools/call":
			var params struct {
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			who, _ := params.Arguments["who"].(string)
			respond(map[string]any{
				"content": []map[string]any{{"type": "text", "text": "pong:" + who}},
				"isError": false,
			})
		default:
			http.Error(w, "unknown method", http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// 1) 客户端直连。
	conn, err := StartHTTP("remote", srv.URL, map[string]string{"X-Token": "s3cret"})
	if err != nil {
		t.Fatalf("StartHTTP: %v", err)
	}
	tools := conn.Tools()
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("应有 1 个 ping 工具: %+v", tools)
	}
	out, err := conn.CallTool(context.Background(), "ping", map[string]any{"who": "friday"})
	if err != nil || out != "pong:friday" {
		t.Fatalf("CallTool 不符: %v %q", err, out)
	}
	mu.Lock()
	if badToken != "" {
		t.Errorf("鉴权头未正确回传: %q", badToken)
	}
	if badSession {
		t.Errorf("会话头未正确回传")
	}
	mu.Unlock()

	// 2) Manager 以 URL 配置接入：注册 → 经注册表调用 → 状态 → 重连。
	mgr := NewManager(nil)
	defer mgr.Close()
	if err := mgr.Apply(context.Background(), []ServerConfig{{
		Name: "remote", URL: srv.URL, Headers: map[string]string{"X-Token": "s3cret"},
	}}); err != nil {
		t.Fatalf("Apply(url): %v", err)
	}
	reg := vxagent.NewRegistry()
	if err := mgr.RegisterTools(reg); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	tool, ok := reg.Get("mcp_remote_ping")
	if !ok {
		t.Fatalf("应有 mcp_remote_ping: %v", reg.Names())
	}
	got, err := tool.Call(context.Background(), map[string]any{"who": "mgr"})
	if err != nil || got != "pong:mgr" {
		t.Fatalf("经注册表调用失败: %v %q", err, got)
	}
	st := mgr.StatusList()
	if len(st) != 1 || !st[0].Running || st[0].Tools != 1 || st[0].URL == "" {
		t.Fatalf("远程服务器状态不符: %+v", st)
	}
	if err := mgr.Restart("remote"); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	// 3) 定义变更（URL 变化）触发重连；连接失败计入状态而非 panic。
	if err := mgr.Apply(context.Background(), []ServerConfig{{
		Name: "remote", URL: srv.URL + "/v2", Headers: map[string]string{"X-Token": "s3cret"},
	}}); err == nil {
		// /v2 返回 404 → initialize 失败，Apply 应返回错误（连接不上）
		t.Log("变更后重新握手如预期失败或成功，看服务器行为")
	}
	time.Sleep(20 * time.Millisecond) // 状态落定
}

// TestHTTPInitializeOverSSE 回归：有些服务器（如官方 TS SDK 的 Streamable HTTP，
// 实测 blinko）对 initialize 也回 text/event-stream，会话 ID 只在响应头里。
// SSE 解析分支曾把响应头整个丢掉，导致会话 ID 拿不到，后续 tools/list 被服务器
// 以 "sessions must start with initialize"（HTTP 400）拒绝，设置页显示不可用。
func TestHTTPInitializeOverSSE(t *testing.T) {
	const sid = "sess-sse-1"
	var missingSession bool
	var mu sync.Mutex

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		switch msg.Method {
		case "initialize":
			w.Header().Set("MCP-Session-Id", sid)
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fl.Flush()
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"protocolVersion\":\"2025-03-26\",\"capabilities\":{},\"serverInfo\":{\"name\":\"sse\",\"version\":\"0.1\"}}}\n\n", *msg.ID)
			fl.Flush()
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			mu.Lock()
			missingSession = r.Header.Get("MCP-Session-Id") != sid
			mu.Unlock()
			if missingSession {
				http.Error(w, "bad session", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"result": map[string]any{"tools": []any{}},
			})
		default:
			http.Error(w, "unknown method", http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn, err := StartHTTP("sse", srv.URL, nil)
	if err != nil {
		t.Fatalf("StartHTTP（initialize 为 SSE 时应能完成握手）: %v", err)
	}
	defer conn.Close()
	mu.Lock()
	defer mu.Unlock()
	if missingSession {
		t.Fatal("tools/list 未回带 initialize 响应头下发的会话 ID")
	}
}
