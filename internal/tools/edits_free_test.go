package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/mcp"
)

// TestInternalApprovalGating 编辑类内部审批的开关语义：confirm 模式默认需要审批；
// 开启编辑免审（acceptEdits/fullAccess 权限模式）后放行；auto/off 模式沿用历史直行。
func TestInternalApprovalGating(t *testing.T) {
	q := approval.New(string(approval.ModeConfirm), nil, time.Second)
	if !q.InternalApprovalRequired() {
		t.Fatal("confirm 模式默认应需要编辑审批")
	}
	q.SetEditsFree(true)
	if q.InternalApprovalRequired() {
		t.Fatal("开启编辑免审后不应需要审批")
	}
	q.SetEditsFree(false)
	if !q.InternalApprovalRequired() {
		t.Fatal("关闭后应恢复审批")
	}
	for _, mode := range []approval.Mode{approval.ModeAuto, approval.ModeOff} {
		q2 := approval.New(string(mode), nil, 0)
		if q2.InternalApprovalRequired() {
			t.Fatalf("%s 模式不应需要编辑审批", mode)
		}
	}
}

// TestWriteFileEditsFree acceptEdits 权限模式下 write_file 直行（无审批卡片）。
func TestWriteFileEditsFree(t *testing.T) {
	dir := t.TempDir()
	q := approval.New(string(approval.ModeConfirm), nil, 500*time.Millisecond)
	q.SetEditsFree(true)
	ft := FileTools{AllowDirs: []string{dir}}
	tool := &WriteFileTool{FileTools: ft, Approver: q}
	out, err := tool.Call(context.Background(), map[string]any{
		"path": filepath.Join(dir, "a.txt"), "content": "hi",
	})
	if err != nil || !strings.Contains(out, "已写入") {
		t.Fatalf("编辑免审下 write_file 应直行: %q %v", out, err)
	}
	if len(q.Pending()) != 0 {
		t.Fatalf("不应产生审批卡片: %+v", q.Pending())
	}
	// 关闭免审：同样调用进入审批且超时拒绝。
	q.SetEditsFree(false)
	if _, err := tool.Call(context.Background(), map[string]any{
		"path": filepath.Join(dir, "b.txt"), "content": "hi",
	}); err == nil {
		t.Fatal("confirm 模式未批准的写入应失败")
	}
}

// TestMcpEditEditsFree acceptEdits 权限模式下 mcp_edit 变更直行。
func TestMcpEditEditsFree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("provider:\n  name: openai\n  baseURL: https://api.example.com/v1\n  model: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	q := approval.New(string(approval.ModeConfirm), nil, 500*time.Millisecond)
	q.SetEditsFree(true)
	mgr := mcp.NewManager(nil)
	defer mgr.Close()
	tool := &McpEditTool{Manager: mgr, Config: cfg, ConfigPath: path, Approver: q}

	out, err := tool.Call(context.Background(), map[string]any{
		"action": "add", "name": "blinko", "url": "https://b.example.com/mcp",
	})
	if err != nil || !strings.Contains(out, "已接入") {
		t.Fatalf("编辑免审下 mcp_edit add 应直行: %q %v", out, err)
	}
	if len(q.Pending()) != 0 {
		t.Fatalf("不应产生审批卡片: %+v", q.Pending())
	}
}
