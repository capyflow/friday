package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
)

// ---------- 假 MCP 服务器（TestHelperProcess 模式，同进程内被 exec 复用） ----------

const helperEnv = "GO_WANT_MCP_HELPER_PROCESS"

// TestHelperProcess 是一个极简 MCP stdio 服务器：读取 JSON-RPC 行，
// 响应 initialize / tools/list / tools/call；收到 {"exit":true}（作为
// tools/call 的 exit 工具参数）时自行退出，用于测试自动重启。
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	defer os.Exit(0)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for in.Scan() {
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(in.Bytes(), &msg); err != nil {
			continue
		}
		if msg.ID == nil {
			continue
		}
		resp := handleHelper(msg.Method, msg.Params)
		resp["jsonrpc"] = "2.0"
		resp["id"] = *msg.ID
		b, _ := json.Marshal(resp)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
}

func handleHelper(method string, params json.RawMessage) map[string]any {
	switch method {
	case "initialize":
		return map[string]any{"result": map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "helper", "version": "0.1"},
		}}
	case "tools/list":
		return map[string]any{"result": map[string]any{
			"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "回显输入文本",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]any{"type": "string"}},
					},
				},
				{
					"name":        "exit",
					"description": "退出服务器进程",
					"inputSchema": map[string]any{"type": "object"},
				},
			},
		}}
	case "tools/call":
		var req struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(params, &req)
		if req.Name == "exit" {
			// 先无法优雅通知（ID 已回），直接退出进程。
			fmt.Println(`{"jsonrpc":"2.0"}`)
			os.Exit(3)
		}
		text, _ := req.Arguments["text"].(string)
		return map[string]any{"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "echo:" + text}},
			"isError": false,
		}}
	}
	return map[string]any{"error": map[string]any{"code": -32601, "message": "method not found: " + method}}
}

// newHelperServerConfig 返回启动假 MCP 服务器的配置（复用测试二进制自身，
// 经环境变量触发 TestHelperProcess 的服务器分支）。
func newHelperServerConfig(name string) ServerConfig {
	return ServerConfig{
		Name:    name,
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess", "--"},
		Env:     map[string]string{helperEnv: "1"},
	}
}

// ---------- Client 测试 ----------

func TestClientHandshakeAndCall(t *testing.T) {
	cfg := newHelperServerConfig("t1")
	cli, err := Start("t1", cfg.Command, cfg.Args, cfg.Env, nil)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer cli.Close()

	tools := cli.Tools()
	if len(tools) != 2 {
		t.Fatalf("应有 2 个工具，实际 %d: %+v", len(tools), tools)
	}
	if tools[0].Name != "echo" {
		t.Fatalf("第一个工具应为 echo: %+v", tools[0])
	}

	out, err := cli.CallTool(context.Background(), "echo", map[string]any{"text": "你好"})
	if err != nil {
		t.Fatalf("CallTool 失败: %v", err)
	}
	if out != "echo:你好" {
		t.Fatalf("CallTool 输出不符: %q", out)
	}

	// 工具结果错误（未知工具名）→ 服务器 method? no: name 未知仍走 tools/call → result missing key → content 空
	// Close 后再调用应报错。
	cli.Close()
	if _, err := cli.CallTool(context.Background(), "echo", nil); err == nil {
		t.Fatal("关闭后调用应报错")
	}
}

// ---------- Manager 测试 ----------

// TestManagerLifecycle 验收 M5：
//  1. config 声明的服务器随 Apply 自动拉起，工具出现在注册表；
//  2. 进程被 kill 后自动重启并恢复工具；
//  3. 移除配置后工具消失。
func TestManagerLifecycle(t *testing.T) {
	var mu sync.Mutex
	changes := 0
	onChange := func() {
		mu.Lock()
		changes++
		mu.Unlock()
	}
	mgr := NewManager(onChange)
	defer mgr.Close()

	reg := vxagent.NewRegistry()

	// 1) 拉起 + 注册。
	if err := mgr.Apply(context.Background(), []ServerConfig{newHelperServerConfig("helper")}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := mgr.RegisterTools(reg); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	names := reg.Names()
	if len(names) != 2 || !strings.HasPrefix(names[0], "mcp_helper_") {
		t.Fatalf("MCP 工具应带前缀注册: %v", names)
	}
	tool, ok := reg.Get("mcp_helper_echo")
	if !ok {
		t.Fatalf("应有 mcp_helper_echo: %v", names)
	}
	out, err := tool.Call(context.Background(), map[string]any{"text": "hi"})
	if err != nil || out != "echo:hi" {
		t.Fatalf("经注册表调用失败: %v %q", err, out)
	}
	st := mgr.StatusList()
	if len(st) != 1 || !st[0].Running || st[0].Tools != 2 {
		t.Fatalf("状态不符: %+v", st)
	}

	// 2) kill 进程 → 自动重启恢复。
	mgr.mu.Lock()
	cli := mgr.clients["helper"]
	mgr.mu.Unlock()
	if cli == nil || cli.cmd.Process == nil {
		t.Fatal("应存在运行中的客户端进程")
	}
	_ = cli.cmd.Process.Kill()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mgr.mu.Lock()
		_, running := mgr.clients["helper"]
		mgr.mu.Unlock()
		st := mgr.StatusList()
		if running && len(st) == 1 && st[0].Running && st[0].Restarts >= 1 && st[0].Tools == 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	st = mgr.StatusList()
	if len(st) != 1 || !st[0].Running || st[0].Restarts < 1 || st[0].Tools != 2 {
		t.Fatalf("kill 后应在数秒内自动重启并恢复工具: %+v", st)
	}

	// 3) 移除配置 → 连接停止，重建后注册表不再有 MCP 工具。
	if err := mgr.Apply(context.Background(), nil); err != nil {
		t.Fatalf("Apply(empty): %v", err)
	}
	reg2 := vxagent.NewRegistry()
	if err := mgr.RegisterTools(reg2); err != nil {
		t.Fatalf("RegisterTools after removal: %v", err)
	}
	if len(reg2.Names()) != 0 {
		t.Fatalf("移除配置后工具应消失: %v", reg2.Names())
	}
}

// TestManagerConfigChangeRestarts 验证服务器定义变更（命令参数变化）触发重启。
func TestManagerConfigChangeRestarts(t *testing.T) {
	mgr := NewManager(nil)
	defer mgr.Close()

	cfg := newHelperServerConfig("chg")
	if err := mgr.Apply(context.Background(), []ServerConfig{cfg}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 变更 args → Apply 应重启连接。
	cfg.Args = append(cfg.Args, "-test.v")
	if err := mgr.Apply(context.Background(), []ServerConfig{cfg}); err != nil {
		t.Fatalf("Apply(changed): %v", err)
	}
	st := mgr.StatusList()
	if len(st) != 1 || !st[0].Running {
		t.Fatalf("变更后应保持在线: %+v", st)
	}

	// Restart 手动重连。
	if err := mgr.Restart("chg"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if st := mgr.StatusList(); !st[0].Running {
		t.Fatalf("手动重启后应在线: %+v", st)
	}
	if err := mgr.Restart("不存在"); err == nil {
		t.Fatal("Restart 未知服务器应报错")
	}
}
