// Package mcp 实现 MCP（Model Context Protocol）客户端：Friday 作为 MCP 客户端，
// 以 stdio transport（本机子进程，开发板友好）连接外部 MCP 服务器，把其工具
// 动态注册进 agent——解锁整个 MCP 工具生态。
//
// 协议：JSON-RPC 2.0，换行分隔帧。握手流程 initialize → notifications/initialized
// → tools/list；工具调用转发为 tools/call。进程随 Friday 启停，崩溃自动重启，
// 连接状态供管理后台设置页展示。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/procctl"
)

// protocolVersion 是本客户端声明的 MCP 协议版本。
const protocolVersion = "2024-11-05"

// handshakeTimeout 是 initialize/tools/list 的等待上限。
const handshakeTimeout = 15 * time.Second

// Tool 是 MCP 服务器声明的一个工具（tools/list 返回项）。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"inputSchema"`
}

// Client 是一台 MCP 服务器（stdio 子进程）的 JSON-RPC 客户端连接。
type Client struct {
	Name string

	cmd      *exec.Cmd
	cancel   context.CancelFunc // 取消即终止子进程（Close 调用）
	stdin    io.WriteCloser
	stderr   io.ReadCloser
	killTree func() error // 整树击杀（Windows Job Object / Unix 进程组），可为 nil
	closeJob func() error // 释放击杀载体句柄，可为 nil

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResp
	tools   []Tool
	closed  bool

	onExit func() // 进程退出回调（管理器据它自动重启）
}

// rpcResp 是一次请求的响应（或错误）。
type rpcResp struct {
	Result json.RawMessage
	Err    *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("MCP 错误 %d: %s", e.Code, e.Message) }

// Start 启动 MCP 服务器子进程并完成握手（initialize + tools/list）。
// stderr 并入进程自身 stderr 便于排查。
//
// 信任边界：command/args 来自 config.yaml 的 mcp.servers——管理员在管理后台
// 显式声明的可信配置，等价于管理员亲手执行的启动命令；不经 shell、参数逐项
// 传递，不接受对话或外部输入。这里仍先做显式校验（非空、无控制字符）并经
// exec.LookPath 解析可执行文件，尽早给出明确错误；随后以 exec.CommandContext
// 启动，子进程生命周期绑定到 Client 私有 context（Close 取消即终止）。
func Start(name, command string, args []string, env map[string]string, onExit func()) (*Client, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("MCP 服务器 %q 未配置启动命令", name)
	}
	parts := append([]string{command}, args...)
	for _, p := range parts {
		if strings.ContainsAny(p, "\x00\r\n") {
			return nil, fmt.Errorf("MCP 服务器 %q 的命令/参数包含非法控制字符", name)
		}
	}
	bin, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("MCP 服务器 %q 的启动命令不可用: %w", name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, args...)
	// 平台进程属性（Windows CREATE_NO_WINDOW 不闪黑窗；Unix 进程组整树击杀）。
	procctl.Prepare(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("创建 stdin 管道: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("创建 stdout 管道: %w", err)
	}
	cmd.Stderr = os.Stderr
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("启动 MCP 服务器 %q: %w", command, err)
	}
	// 挂接整树击杀载体：npx 系 MCP 服务器同样有 node→server 孙进程；失败
	// 退化为 Close 里的 Process.Kill（只杀直接子进程）。
	killTree, closeJob, _ := procctl.Attach(cmd)

	c := &Client{
		Name:     name,
		cmd:      cmd,
		cancel:   cancel,
		stdin:    stdin,
		stderr:   nil,
		killTree: killTree,
		closeJob: closeJob,
		pending:  make(map[int64]chan rpcResp),
		onExit:   onExit,
	}
	go c.readLoop(stdout)
	go c.waitExit()

	if _, err := c.call(context.Background(), "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "friday", "version": "1.0"},
	}, handshakeTimeout); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP initialize 失败: %w", err)
	}
	_ = c.notify("notifications/initialized")

	if err := c.RefreshTools(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// readLoop 逐行读取服务器响应/通知并分发到等待者。
func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024) // 单帧上限 8MB（工具结果可能很大）
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // 非 JSON 行忽略（容错服务器启动横幅）
		}
		if msg.ID == nil {
			continue // 通知（如 tools/list_changed）暂不需处理，忽略
		}
		c.mu.Lock()
		ch, ok := c.pending[*msg.ID]
		delete(c.pending, *msg.ID)
		c.mu.Unlock()
		if !ok {
			continue
		}
		ch <- rpcResp{Result: msg.Result, Err: msg.Error}
	}
}

// waitExit 等待子进程退出并通知管理器（自动重启）。
func (c *Client) waitExit() {
	_ = c.cmd.Wait()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	// 唤醒所有还在等待的请求，避免调用方悬挂到超时。
	c.mu.Lock()
	for id, ch := range c.pending {
		ch <- rpcResp{Err: &rpcError{Code: -1, Message: "MCP 服务器进程已退出"}}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !closed && c.onExit != nil {
		c.onExit()
	}
}

// call 发送一次 JSON-RPC 请求并等待响应。
func (c *Client) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("MCP 连接已关闭")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResp, 1)
	c.pending[id] = ch
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}
	c.mu.Unlock()

	if timeout <= 0 {
		timeout = handshakeTimeout
	}
	select {
	case resp := <-ch:
		if resp.Err != nil {
			return nil, resp.Err
		}
		return resp.Result, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("等待 %s 响应超时（%s）", method, timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// notify 发送一条通知（无 id、无响应）。
func (c *Client) notify(method string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("MCP 连接已关闭")
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	_, err := c.stdin.Write(append(b, '\n'))
	return err
}

// RefreshTools 重新拉取 tools/list（连接建立与 tools/list_changed 时调用）。
func (c *Client) RefreshTools() error {
	res, err := c.call(context.Background(), "tools/list", map[string]any{}, handshakeTimeout)
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
func (c *Client) Tools() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// CallTool 转发一次工具调用，拼接文本型 content 作为结果。
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	res, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, 120*time.Second)
	if err != nil {
		return "", err
	}
	return parseToolResult(res)
}

// parseToolResult 解析 tools/call 的 result：拼接文本型 content；
// isError 时把内容转为错误。stdio 与 HTTP 两种连接共用。
func parseToolResult(res json.RawMessage) (string, error) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", fmt.Errorf("解析 tools/call 响应失败: %w", err)
	}
	var b strings.Builder
	for _, item := range out.Content {
		if item.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(item.Text)
		}
	}
	if out.IsError {
		return "", fmt.Errorf("%s", b.String())
	}
	if b.Len() == 0 {
		return "（工具执行成功，无文本输出）", nil
	}
	return b.String(), nil
}

// Close 关闭连接并终止子进程（幂等）：取消生命周期 context 杀进程，
// 整树击杀优先（npx 系服务器的 node 孙进程一并带走），显式 Kill 兜底，
// 再关闭 stdin 与击杀载体句柄。
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	if c.killTree != nil {
		_ = c.killTree()
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.stdin.Close()
	if c.closeJob != nil {
		_ = c.closeJob()
	}
}
