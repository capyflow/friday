package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/mcp"
)

// newMcpEditTool 搭建最小可用的工具环境：临时 config.yaml + 空跑的 MCP 管理器。
func newMcpEditTool(t *testing.T) (*McpEditTool, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := `provider:
  name: openai
  baseURL: https://api.example.com/v1
  model: demo
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	mgr := mcp.NewManager(nil)
	t.Cleanup(mgr.Close)
	return &McpEditTool{Manager: mgr, Config: cfg, ConfigPath: path}, cfg, path
}

// reloadConfig 重新从磁盘读配置（验证写回结果）。
func reloadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("回读配置失败: %v", err)
	}
	return cfg
}

// TestMcpEditRemoteServerRoundTrip 走一遍"接入远程服务器（Blinko 场景）→ 查 → 改 → 删"，
// 验证配置写回与热生效（连接失败不回滚，状态保留错误）。
func TestMcpEditRemoteServerRoundTrip(t *testing.T) {
	tool, cfg, path := newMcpEditTool(t)
	ctx := context.Background()

	// 1) add：远程服务器 + 鉴权头。
	out, err := tool.Call(ctx, map[string]any{
		"action": "add", "name": "blinko",
		"url":     "https://blinko.example.com/mcp",
		"headers": map[string]any{"Authorization": "Bearer tok-1"},
	})
	if err != nil {
		t.Fatalf("add 失败: %v", err)
	}
	if !strings.Contains(out, "blinko") || !strings.Contains(out, "已接入") {
		t.Fatalf("add 返回不符: %s", out)
	}
	got := reloadConfig(t, path)
	if len(got.Mcp.Servers) != 1 || got.Mcp.Servers[0].URL != "https://blinko.example.com/mcp" ||
		got.Mcp.Servers[0].Headers["Authorization"] != "Bearer tok-1" {
		t.Fatalf("配置未写回: %+v", got.Mcp.Servers)
	}
	if len(cfg.Snapshot().Mcp.Servers) != 1 {
		t.Fatal("运行配置应同步更新")
	}

	// 2) list / get：能看到服务器与状态（远程地址连不上，状态保留 lastError）。
	out, err = tool.Call(ctx, map[string]any{"action": "list"})
	if err != nil || !strings.Contains(out, "blinko") {
		t.Fatalf("list 不符: %s %v", out, err)
	}
	out, err = tool.Call(ctx, map[string]any{"action": "get", "name": "blinko"})
	if err != nil || !strings.Contains(out, "Bearer tok-1") {
		t.Fatalf("get 应返回完整配置: %s %v", out, err)
	}

	// 3) update：只传要改的 headers，url 保留。
	out, err = tool.Call(ctx, map[string]any{
		"action": "update", "name": "blinko",
		"headers": map[string]any{"Authorization": "Bearer tok-2"},
	})
	if err != nil || !strings.Contains(out, "已更新") {
		t.Fatalf("update 不符: %s %v", out, err)
	}
	got = reloadConfig(t, path)
	if got.Mcp.Servers[0].Headers["Authorization"] != "Bearer tok-2" ||
		got.Mcp.Servers[0].URL != "https://blinko.example.com/mcp" {
		t.Fatalf("update 写回不符: %+v", got.Mcp.Servers[0])
	}

	// 3b) update 无变化时直接返回，不触发写回。
	before, _ := os.Stat(path)
	out, err = tool.Call(ctx, map[string]any{
		"action": "update", "name": "blinko",
		"headers": map[string]any{"Authorization": "Bearer tok-2"},
	})
	if err != nil || !strings.Contains(out, "未变化") {
		t.Fatalf("重复 update 应提示未变化: %s %v", out, err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("无变化的 update 不应写回配置文件")
	}

	// 4) remove：配置与运行时同时移除。
	out, err = tool.Call(ctx, map[string]any{"action": "remove", "name": "blinko"})
	if err != nil || !strings.Contains(out, "已移除") {
		t.Fatalf("remove 不符: %s %v", out, err)
	}
	if got := reloadConfig(t, path); len(got.Mcp.Servers) != 0 {
		t.Fatalf("remove 后配置应为空: %+v", got.Mcp.Servers)
	}
	if _, err := tool.Call(ctx, map[string]any{"action": "get", "name": "blinko"}); err == nil {
		t.Fatal("移除后 get 应报未找到")
	}
}

// TestMcpEditLocalServerCRUD 本地命令服务器的增改查，含 env/args 的部分更新语义。
func TestMcpEditLocalServerCRUD(t *testing.T) {
	tool, _, path := newMcpEditTool(t)
	ctx := context.Background()

	out, err := tool.Call(ctx, map[string]any{
		"action": "add", "name": "fs", "command": "definitely-missing-binary-xyz",
		"args": []any{"-y", "pkg"},
		"env":  map[string]any{"A": "1"},
	})
	if err != nil {
		t.Fatalf("add 失败: %v", err)
	}
	// 命令不存在：配置已保存热生效，但连接失败要如实提示。
	if !strings.Contains(out, "已接入") || !strings.Contains(out, "连接失败") {
		t.Fatalf("add 返回不符: %s", out)
	}
	got := reloadConfig(t, path)
	if got.Mcp.Servers[0].Command != "definitely-missing-binary-xyz" || got.Mcp.Servers[0].Env["A"] != "1" {
		t.Fatalf("配置未写回: %+v", got.Mcp.Servers[0])
	}

	// update：只改 args，env 保留。
	if _, err := tool.Call(ctx, map[string]any{
		"action": "update", "name": "fs", "args": []any{"-y", "pkg2"},
	}); err != nil {
		t.Fatalf("update 失败: %v", err)
	}
	got = reloadConfig(t, path)
	sv := got.Mcp.Servers[0]
	if len(sv.Args) != 2 || sv.Args[1] != "pkg2" || sv.Env["A"] != "1" {
		t.Fatalf("update 应只覆盖 args: %+v", sv)
	}

	// update：切换为远程模式（command 被清空）。
	if _, err := tool.Call(ctx, map[string]any{
		"action": "update", "name": "fs", "url": "https://mcp.example.com/sse",
	}); err != nil {
		t.Fatalf("update 切换模式失败: %v", err)
	}
	got = reloadConfig(t, path)
	sv = got.Mcp.Servers[0]
	if sv.URL != "https://mcp.example.com/sse" || sv.Command != "" || len(sv.Args) != 0 {
		t.Fatalf("切换远程模式后应清空本地字段: %+v", sv)
	}
}

// TestMcpEditValidation 参数校验：缺 name、command/url 二选一、url 协议、
// 重复 add、update/remove 不存在的服务器。
func TestMcpEditValidation(t *testing.T) {
	tool, _, _ := newMcpEditTool(t)
	ctx := context.Background()

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"缺 name", map[string]any{"action": "add", "command": "x"}, "name"},
		{"缺 command 与 url", map[string]any{"action": "add", "name": "a"}, "二选一"},
		{"两者都给", map[string]any{"action": "add", "name": "a", "command": "x", "url": "https://e.com"}, "只能提供其一"},
		{"url 协议非法", map[string]any{"action": "add", "name": "a", "url": "ftp://e.com"}, "http://"},
		{"headers 值非字符串", map[string]any{"action": "add", "name": "a", "url": "https://e.com", "headers": map[string]any{"K": 1}}, "须为字符串"},
	}
	for _, tc := range cases {
		if _, err := tool.Call(ctx, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s：应报错含 %q，实际 %v", tc.name, tc.want, err)
		}
	}

	// 重复 add。
	if _, err := tool.Call(ctx, map[string]any{"action": "add", "name": "x", "command": "x"}); err != nil {
		t.Fatalf("首次 add 不应失败: %v", err)
	}
	if _, err := tool.Call(ctx, map[string]any{"action": "add", "name": "x", "command": "y"}); err == nil ||
		!strings.Contains(err.Error(), "已存在") {
		t.Fatalf("重复 add 应报已存在: %v", err)
	}
	// update/remove/restart 不存在的服务器。
	for _, args := range []map[string]any{
		{"action": "update", "name": "nope", "command": "x"},
		{"action": "remove", "name": "nope"},
		{"action": "restart", "name": "nope"},
	} {
		if _, err := tool.Call(ctx, args); err == nil || !strings.Contains(err.Error(), "未找到") {
			t.Errorf("%v 应报未找到: %v", args, err)
		}
	}
	// 未知 action。
	if _, err := tool.Call(ctx, map[string]any{"action": "deploy"}); err == nil {
		t.Fatal("未知 action 应报错")
	}
}

// TestMcpEditApproval confirm 模式下变更需网页批准：批准后执行、拒绝后不执行。
func TestMcpEditApproval(t *testing.T) {
	tool, _, path := newMcpEditTool(t)
	q := approval.New(string(approval.ModeConfirm), nil, 3*time.Second)
	tool.Approver = q
	ctx := context.Background()

	// 查询不需要审批，直接返回。
	if _, err := tool.Call(ctx, map[string]any{"action": "list"}); err != nil {
		t.Fatalf("list 不应审批: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := tool.Call(ctx, map[string]any{"action": "add", "name": "approved", "command": "x"})
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	items := q.Pending()
	if len(items) != 1 {
		t.Fatalf("应有 1 条待审批，实际 %d", len(items))
	}
	if items[0].Tool != "mcp_edit" || !strings.Contains(items[0].Summary, "approved") {
		t.Fatalf("审批卡片不符: %+v", items[0])
	}
	q.DecideWith(items[0].ID, true, false)
	if err := <-done; err != nil {
		t.Fatalf("批准后 add 应成功: %v", err)
	}
	if got := reloadConfig(t, path); len(got.Mcp.Servers) != 1 {
		t.Fatal("批准后配置应写回")
	}

	// 拒绝：配置不变。
	done2 := make(chan error, 1)
	go func() {
		_, err := tool.Call(ctx, map[string]any{"action": "remove", "name": "approved"})
		done2 <- err
	}()
	deadline = time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	card := q.Pending()[0]
	q.DecideWith(card.ID, false, false)
	if err := <-done2; err == nil {
		t.Fatal("拒绝后 remove 应失败")
	}
	if got := reloadConfig(t, path); len(got.Mcp.Servers) != 1 {
		t.Fatal("被拒绝的 remove 不应改配置")
	}
}

// TestMcpEditAppliesToManager 验证 add 后管理器确已接管该服务器（状态可见、
// onChange 被触发），即"新增即可用"的热生效链路。
func TestMcpEditAppliesToManager(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("provider:\n  name: openai\n  baseURL: https://api.example.com/v1\n  model: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	changes := 0
	mgr := mcp.NewManager(func() { mu.Lock(); changes++; mu.Unlock() })
	defer mgr.Close()
	tool := &McpEditTool{Manager: mgr, Config: cfg, ConfigPath: path}

	if _, err := tool.Call(context.Background(), map[string]any{
		"action": "add", "name": "gone", "command": "definitely-missing-binary-xyz",
	}); err != nil {
		t.Fatalf("add 失败: %v", err)
	}
	mu.Lock()
	n := changes
	mu.Unlock()
	if n == 0 {
		t.Fatal("add 后应触发工具清单变化回调（重建注册表）")
	}
	st := mgr.StatusList()
	if len(st) != 1 || st[0].Name != "gone" || st[0].Running {
		t.Fatalf("管理器应接管服务器（连接失败状态保留）: %+v", st)
	}
	if st[0].LastError == "" {
		t.Fatal("命令不存在时状态应保留 lastError")
	}
}
