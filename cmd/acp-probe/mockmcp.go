package main

// mockMCP 是一个最小的 MCP streamable-HTTP 门面，只服务于一个目的：
// 验证真实 ACP agent 会不会去调用客户端在 session/new 里挂给它的 MCP 工具，
// 以及调用外部 MCP 工具时是否触发 request_permission。
// 它不是 Friday 门面的实现参考——正式门面应当复用 mark3labs/mcp-go 的 server 包。

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

var mockStart = time.Now()

type mockMCP struct {
	nonce string
	block time.Duration // friday_approval_gate 阻塞多久，模拟 Friday 侧人工审批

	mu        sync.Mutex
	listHits  int
	calls     []string
	other     []string
	gateState string // waiting / returned / released@turn-end
	mode      string // sync（挂住等审批）| async（立刻回 pending，靠轮询取结果）
	gateAt    time.Time

	shutdown    chan struct{}
	releaseOnce sync.Once

	url    string
	server *http.Server
}

func (m *mockMCP) release() {
	m.releaseOnce.Do(func() { close(m.shutdown) })
}

func startMockMCP() (*mockMCP, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m := &mockMCP{nonce: randHex(6), shutdown: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", m.handle)
	m.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = m.server.Serve(ln) }()
	m.url = fmt.Sprintf("http://127.0.0.1:%d/mcp", ln.Addr().(*net.TCPAddr).Port)
	return m, nil
}

func (m *mockMCP) stop() {
	m.release()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = m.server.Shutdown(ctx)
}

type mockReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (m *mockMCP) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// 不提供 SSE 反向通道：规范要求此时返回 405。
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusOK)
		return
	}
	var req mockReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"jsonrpc":"2.0","error":{"code":-32700,"message":"parse error"}}`, http.StatusBadRequest)
		return
	}

	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	if isNotification {
		m.note("notification " + req.Method)
		if req.Method == "notifications/cancelled" {
			// 对方明说它不等了：Friday 侧的审批卡应当据此释放。
			m.release()
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", "mock-"+m.nonce)

	result, rerr := func() (any, map[string]any) {
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			v := p.ProtocolVersion
			if v == "" {
				v = "2025-03-26"
			}
			m.note("initialize (client asked " + v + ")")
			return map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "acp-probe-mock-mcp", "version": "0.0.1"},
			}, nil
		case "tools/list":
			m.mu.Lock()
			m.listHits++
			m.mu.Unlock()
			m.note("tools/list")
			return map[string]any{"tools": []any{
				map[string]any{
					"name":        "friday_probe_tool",
					"description": "Return the magic token that proves the agent reached the client-side MCP facade. Takes no arguments.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
				},
				map[string]any{
					"name":        "friday_approval_gate",
					"description": "Ask Friday for human approval. Returns immediately with a pending handle in async mode; blocks until the human decides in sync mode.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
				},
				map[string]any{
					"name":        "friday_check_approval",
					"description": "Poll the result of a previously submitted Friday approval. Safe to call repeatedly.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
				},
			}}, nil
		case "tools/call":
			var cp struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &cp)
			m.mu.Lock()
			m.calls = append(m.calls, string(req.Params))
			if cp.Name == "friday_approval_gate" {
				m.gateState = "waiting"
			}
			m.mu.Unlock()
			m.note("tools/call " + cp.Name)

			if cp.Name == "friday_check_approval" {
				m.mu.Lock()
				waited := time.Since(m.gateAt)
				m.mu.Unlock()
				if !m.gateAt.IsZero() && waited >= m.block {
					m.note(fmt.Sprintf("轮询在 %s 后拿到放行", waited))
					return map[string]any{
						"content": []any{map[string]any{"type": "text", "text": "APPROVED_BY_HUMAN " + m.nonce}},
						"isError": false,
					}, nil
				}
				return map[string]any{
					"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("STILL_PENDING（已等 %s，请继续轮询或稍后再查）", waited.Round(time.Second))}},
					"isError": false,
				}, nil
			}

			if cp.Name == "friday_approval_gate" && m.block > 0 {
				if m.mode == "async" {
					// 异步两拍：立刻回 pending，靠 friday_check_approval 取结果。
					// 这是为了绕开对方 agent 的 MCP 调用超时（实测 opencode 60s 就放弃）。
					m.mu.Lock()
					m.gateAt = time.Now()
					m.gateState = "pending-async"
					m.mu.Unlock()
					return map[string]any{
						"content": []any{map[string]any{"type": "text", "text": "PENDING: 审批已提交，句柄 " + m.nonce + "；请调用 friday_check_approval 轮询结果。"}},
						"isError": false,
					}, nil
				}
				// 模拟人工审批：在 Friday 手里挂住这个请求，看对方 agent 等多久、
				// 被 session/cancel 时会不会通知下来释放。
				select {
				case <-time.After(m.block):
					m.mu.Lock()
					m.gateState = "returned"
					m.mu.Unlock()
					m.note(fmt.Sprintf("审批在 %s 后放行", m.block))
					return map[string]any{
						"content": []any{map[string]any{"type": "text", "text": "APPROVED_BY_HUMAN " + m.nonce}},
						"isError": false,
					}, nil
				case <-m.shutdown:
					m.mu.Lock()
					m.gateState = "released@turn-end"
					m.mu.Unlock()
					m.note("审批挂起中被释放（探测结束/收到 cancelled）")
					return nil, map[string]any{"code": -32001, "message": "approval request cancelled by client"}
				}
			}
			return map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "FRIDAY_MCP_OK " + m.nonce}},
				"isError": false,
			}, nil
		case "ping":
			return map[string]any{}, nil
		}
		m.note("unhandled " + req.Method)
		return nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method}
	}()

	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rerr != nil {
		out["error"] = rerr
	} else {
		out["result"] = result
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (m *mockMCP) note(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.other = append(m.other, fmt.Sprintf("[%dms] %s", time.Since(mockStart).Milliseconds(), s))
}

func (m *mockMCP) summary() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := fmt.Sprintf("tools/list %d 次，tools/call %d 次，审批门状态=%q", m.listHits, len(m.calls), m.gateState)
	for _, c := range m.calls {
		out += "\n   call params: " + firstN(c, 400)
	}
	for _, o := range m.other {
		out += "\n   " + o
	}
	return out
}
