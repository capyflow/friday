// http.go 实现 MCP 的 Streamable HTTP transport（远程服务器）：
// JSON-RPC 经 HTTP POST 发往服务器 URL，响应可能是单个 application/json
// 或 text/event-stream（SSE 流）；initialize 时记录服务器下发的
// MCP-Session-Id 并在后续请求回带。适用于"只给一个 URL"的托管型 MCP 服务
// （鉴权经自定义请求头，如 Authorization: Bearer xxx，或由 TokenProvider
// 提供 OAuth 令牌并在 401 时刷新重试）。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// declareVersion 是本客户端声明并默认使用的 MCP 协议版本
// （Streamable HTTP 自 2025-03-26 引入；服务器握手时可协商覆盖）。
const declareVersion = "2025-03-26"

// HTTPConn 是一台远程 MCP 服务器（Streamable HTTP）的连接。
type HTTPConn struct {
	name    string
	url     string
	headers map[string]string
	tokens  TokenProvider // 非 nil 时每请求注入 Authorization，并在 401 时刷新重试

	mu      sync.Mutex
	session string // 服务器在 initialize 响应头下发的 MCP-Session-Id
	version string // 握手协商出的协议版本
	nextID  int64
	tools   []Tool
	closed  bool
	client  *http.Client
}

// StartHTTP 连接远程 MCP 服务器并完成握手（静态请求头鉴权）。
func StartHTTP(name, rawURL string, headers map[string]string) (*HTTPConn, error) {
	return StartHTTPAuth(name, rawURL, headers, nil)
}

// StartHTTPAuth 在 StartHTTP 的基础上接入 OAuth 令牌提供者：握手与后续每次
// 请求都按需带 Bearer，被拒 401 时先刷新再重试一次，仍不行就报 ErrMCPAuthRequired。
func StartHTTPAuth(name, rawURL string, headers map[string]string, tokens TokenProvider) (*HTTPConn, error) {
	c := &HTTPConn{
		name:    name,
		url:     strings.TrimRight(rawURL, "/"),
		headers: headers,
		tokens:  tokens,
		client:  &http.Client{Timeout: 2 * time.Minute},
	}
	raw, hdr, err := c.post(context.Background(), "initialize", map[string]any{
		"protocolVersion": declareVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "friday", "version": "1.0"},
	}, handshakeTimeout)
	if err != nil {
		return nil, fmt.Errorf("MCP initialize 失败: %w", err)
	}
	// 记录服务器下发的会话 ID，后续请求必须回带。
	if sid := hdr.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.session = sid
		c.mu.Unlock()
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &init)
	if init.ProtocolVersion != "" {
		c.mu.Lock()
		c.version = init.ProtocolVersion
		c.mu.Unlock()
	}

	// notifications/initialized：无 id 的通知，服务器通常回 202，失败不影响使用。
	_ = c.notify(context.Background(), "notifications/initialized")

	if err := c.RefreshTools(); err != nil {
		return nil, err
	}
	return c, nil
}

// post 发送一次 JSON-RPC 请求并返回匹配的响应结果与响应头。
func (c *HTTPConn) post(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, http.Header, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, errors.New("MCP 连接已关闭")
	}
	c.nextID++
	id := c.nextID
	session, version := c.session, c.version
	c.mu.Unlock()

	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		body["params"] = params
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}

	if timeout <= 0 {
		timeout = handshakeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := c.do(ctx, http.MethodPost, b, session, version)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		return nil, resp.Header, nil // 通知类请求的确认
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, resp.Header, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		res, err := sseResponse(resp.Body, id)
		// SSE 形态的响应同样要回传响应头：initialize 下发的 Mcp-Session-Id
		// 只在响应头里，丢弃它后续请求会被服务器以"须先 initialize"拒绝。
		return res, resp.Header, err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Err    *rpcError       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, resp.Header, fmt.Errorf("解析响应失败: %w", err)
	}
	if envelope.Err != nil {
		return nil, resp.Header, envelope.Err
	}
	return envelope.Result, resp.Header, nil
}

// do 发送一次 JSON-RPC 请求：配了令牌提供者时按需注入 Authorization，被 401
// 拒绝则把 WWW-Authenticate 交回提供者（记录挑战 + 尝试刷新），拿到新令牌就重试
// 一次。只重试一次——刷新仍不被接受时说明确实要用户重新授权，不能循环打转。
func (c *HTTPConn) do(ctx context.Context, method string, payload []byte, session, version string) (*http.Response, error) {
	authValue := ""
	if c.tokens != nil {
		if v, ok := c.tokens.Authorization(ctx); ok {
			authValue = v
		}
	}
	req, err := c.newRequest(ctx, method, payload, session, version, authValue)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized || c.tokens == nil {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	value, ok := c.tokens.Unauthorized(ctx, challenge)
	if !ok {
		return nil, ErrMCPAuthRequired
	}
	req, err = c.newRequest(ctx, method, payload, session, version, value)
	if err != nil {
		return nil, err
	}
	return c.client.Do(req)
}

// newRequest 组装一次远程 MCP 请求（协议版本、会话 ID、静态头与令牌头）。
func (c *HTTPConn) newRequest(ctx context.Context, method string, payload []byte, session, version, authValue string) (*http.Request, error) {
	var body io.Reader
	if len(payload) > 0 {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", declareVersion)
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if session != "" {
		req.Header.Set("MCP-Session-Id", session)
	}
	if authValue != "" {
		req.Header.Set("Authorization", authValue)
	}
	for k, v := range c.headers {
		if k == "Authorization" && authValue != "" {
			continue // OAuth 令牌优先于配置里的静态头，避免两套凭据混发
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

// notify 发送一条无 id 的 JSON-RPC 通知（服务器回 202 即视为成功）。
func (c *HTTPConn) notify(ctx context.Context, method string) error {
	c.mu.Lock()
	closed := c.closed
	session, version := c.session, c.version
	c.mu.Unlock()
	if closed {
		return errors.New("MCP 连接已关闭")
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	resp, err := c.do(ctx, http.MethodPost, b, session, version)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return nil
}

// sseResponse 从 SSE 流中取回 id 匹配的那条响应（不含响应头；
// 调用方直接从 http.Response 取）。
func sseResponse(r io.Reader, wantID int64) (json.RawMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Err    *rpcError       `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			continue // 非 JSON 行（注释/心跳）忽略
		}
		if msg.ID == nil || *msg.ID != wantID {
			continue
		}
		if msg.Err != nil {
			return nil, msg.Err
		}
		return msg.Result, nil
	}
	return nil, errors.New("SSE 流中未收到匹配的响应")
}

// RefreshTools 重新拉取 tools/list。
func (c *HTTPConn) RefreshTools() error {
	res, _, err := c.post(context.Background(), "tools/list", map[string]any{}, handshakeTimeout)
	if err != nil {
		return fmt.Errorf("MCP tools/list 失败: %w", err)
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return fmt.Errorf("解析 tools/list 响应失败: %w", err)
	}
	c.mu.Lock()
	c.tools = out.Tools
	c.mu.Unlock()
	return nil
}

// Tools 返回最近一次 tools/list 的工具清单。
func (c *HTTPConn) Tools() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// CallTool 转发一次工具调用（响应可能 2 分钟才到，覆盖长任务）。
func (c *HTTPConn) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	res, _, err := c.post(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, 120*time.Second)
	if err != nil {
		return "", err
	}
	return parseToolResult(res)
}

// Close 关闭连接：标记失效并向服务器发送会话终止（DELETE，尽力而为）。
func (c *HTTPConn) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	session := c.session
	c.mu.Unlock()

	if session != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		authValue := ""
		if c.tokens != nil {
			if v, ok := c.tokens.Authorization(ctx); ok {
				authValue = v
			}
		}
		// 会话终止也要带凭据：托管型服务器普遍对未鉴权的 DELETE 直接拒绝。
		req, err := c.newRequest(ctx, http.MethodDelete, nil, session, "", authValue)
		if err != nil {
			return
		}
		if resp, err := c.client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
}
