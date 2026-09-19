package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/logx"
)

func newExecTool(mode string, allowlist []string) *ExecTool {
	return &ExecTool{Approver: approval.New(mode, allowlist, 3*time.Second)}
}

func TestExecWhitelistPasses(t *testing.T) {
	tool := newExecTool("auto", []string{"echo"})
	out, err := tool.Call(context.Background(), map[string]any{"command": "echo hello-world"})
	if err != nil {
		t.Fatalf("白名单命令应直接执行: %v", err)
	}
	if !strings.Contains(out, "hello-world") {
		t.Fatalf("应返回命令输出: %q", out)
	}
}

func TestExecAutoModeDeniesNonWhitelist(t *testing.T) {
	tool := newExecTool("auto", []string{"echo"})
	_, err := tool.Call(context.Background(), map[string]any{"command": "whoami"})
	if err == nil || !strings.Contains(err.Error(), "白名单") {
		t.Fatalf("auto 模式非白名单应拒绝: %v", err)
	}
}

func TestExecOffModeDisabled(t *testing.T) {
	tool := newExecTool("off", []string{"echo"})
	_, err := tool.Call(context.Background(), map[string]any{"command": "echo hi"})
	if err != approval.ErrDisabled {
		t.Fatalf("off 模式应禁用: %v", err)
	}
}

func TestExecBlacklistEvenIfAllowlisted(t *testing.T) {
	tool := newExecTool("auto", []string{"rm"})
	_, err := tool.Call(context.Background(), map[string]any{"command": "rm -rf /tmp/x"})
	if err == nil || !strings.Contains(err.Error(), "黑名单") {
		t.Fatalf("黑名单应拦截（即使命中白名单）: %v", err)
	}
}

func TestExecConfirmApprove(t *testing.T) {
	tool := newExecTool("confirm", []string{"echo"})
	done := make(chan error, 1)
	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := tool.Call(context.Background(), map[string]any{"command": "whoami"})
		ch <- result{out, err}
	}()
	q := tool.Approver
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	item := q.Pending()[0]
	q.Decide(item.ID, true)
	r := <-ch
	if r.err != nil {
		t.Fatalf("批准后应执行: %v", r.err)
	}
	if r.out == "" {
		t.Fatal("批准后应有输出")
	}
	_ = done
}

func TestExecConfirmReject(t *testing.T) {
	tool := newExecTool("confirm", nil)
	ch := make(chan error, 1)
	go func() {
		_, err := tool.Call(context.Background(), map[string]any{"command": "whoami"})
		ch <- err
	}()
	q := tool.Approver
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.Decide(q.Pending()[0].ID, false)
	err := <-ch
	if err != approval.ErrDenied {
		t.Fatalf("拒绝后应返回用户未批准: %v", err)
	}
}

func TestExecConfirmTimeoutDenies(t *testing.T) {
	tool := &ExecTool{Approver: approval.New("confirm", nil, 80*time.Millisecond)}
	_, err := tool.Call(context.Background(), map[string]any{"command": "whoami"})
	if err != approval.ErrDenied {
		t.Fatalf("审批超时应返回用户未批准: %v", err)
	}
}

func TestExecTimeout(t *testing.T) {
	tool := newExecTool("auto", []string{"sleep"})
	_, err := tool.Call(context.Background(), map[string]any{"command": "sleep 5", "timeout": float64(1)})
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("命令超时应报错: %v", err)
	}
}

func TestExecTimeoutCap(t *testing.T) {
	// timeout 参数超过 120s 应被钳制——这里只验证不报参数错误，正常返回即可。
	tool := newExecTool("auto", []string{"echo"})
	out, err := tool.Call(context.Background(), map[string]any{"command": "echo ok", "timeout": float64(999)})
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("超时钳制不应影响正常执行: %v %q", err, out)
	}
}

// ---------- write_file ----------

func testFileTools(t *testing.T) (FileTools, string) {
	t.Helper()
	dir := t.TempDir()
	return FileTools{AllowDirs: []string{dir}}, dir
}

func TestWriteFileCreatesAndBacksUp(t *testing.T) {
	ft, dir := testFileTools(t)
	tool := &WriteFileTool{FileTools: ft, Approver: approval.New("auto", nil, 0)}
	target := filepath.Join(dir, "notes.txt")

	if _, err := tool.Call(context.Background(), map[string]any{"path": target, "content": "第一版"}); err != nil {
		t.Fatalf("新建文件失败: %v", err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "第一版" {
		t.Fatalf("内容不符: %q", data)
	}
	if _, err := tool.Call(context.Background(), map[string]any{"path": target, "content": "第二版"}); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	bak, err := os.ReadFile(target + ".bak")
	if err != nil || string(bak) != "第一版" {
		t.Fatalf("原文件应备份为 .bak: %v %q", err, bak)
	}
}

func TestWriteFileRejectsEscape(t *testing.T) {
	ft, dir := testFileTools(t)
	tool := &WriteFileTool{FileTools: ft, Approver: approval.New("auto", nil, 0)}
	// 相对路径逃逸。
	if _, err := tool.Call(context.Background(), map[string]any{"path": "../../etc/evil.txt", "content": "x"}); err == nil {
		t.Fatal("路径逃逸应被拒绝")
	}
	// 绝对路径出白名单。
	if _, err := tool.Call(context.Background(), map[string]any{"path": "/etc/evil.txt", "content": "x"}); err == nil {
		t.Fatal("白名单外路径应被拒绝")
	}
	// 符号链接指向白名单外。
	outside := filepath.Join(t.TempDir(), "outside.txt")
	_ = os.WriteFile(outside, []byte("out"), 0o644)
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := tool.Call(context.Background(), map[string]any{"path": link, "content": "x"}); err == nil {
			t.Fatal("写入指向白名单外的软链应被拒绝")
		}
	}
}

func TestWriteFileConfirmApproval(t *testing.T) {
	ft, dir := testFileTools(t)
	tool := &WriteFileTool{FileTools: ft, Approver: approval.New("confirm", nil, 3*time.Second)}
	target := filepath.Join(dir, "a.txt")
	ch := make(chan error, 1)
	go func() {
		_, err := tool.Call(context.Background(), map[string]any{"path": target, "content": "内容"})
		ch <- err
	}()
	q := tool.Approver
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	items := q.Pending()
	if len(items) != 1 || items[0].Tool != "write_file" {
		t.Fatalf("confirm 模式应产生审批卡片: %+v", items)
	}
	q.Decide(items[0].ID, true)
	if err := <-ch; err != nil {
		t.Fatalf("批准后应写入: %v", err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "内容" {
		t.Fatalf("内容不符: %q", data)
	}
}

// ---------- edit_file ----------

func TestEditFileUniqueReplace(t *testing.T) {
	ft, dir := testFileTools(t)
	tool := &EditFileTool{FileTools: ft, Approver: approval.New("auto", nil, 0)}
	target := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(target, []byte("晚上 9 点：钙片\n早上：维生素 D"), 0o644)

	out, err := tool.Call(context.Background(), map[string]any{
		"path": target, "old": "晚上 9 点：钙片", "new": "晚上 8 点：钙片",
	})
	if err != nil {
		t.Fatalf("编辑失败: %v", err)
	}
	if !strings.Contains(out, "已编辑") {
		t.Fatalf("输出不符: %q", out)
	}
	data, _ := os.ReadFile(target)
	if !strings.Contains(string(data), "晚上 8 点：钙片") {
		t.Fatalf("替换未生效: %q", data)
	}
	bak, err := os.ReadFile(target + ".bak")
	if err != nil || !strings.Contains(string(bak), "晚上 9 点") {
		t.Fatalf("编辑前应备份: %v", err)
	}
}

func TestEditFileMultiMatchRejected(t *testing.T) {
	ft, dir := testFileTools(t)
	tool := &EditFileTool{FileTools: ft, Approver: approval.New("auto", nil, 0)}
	target := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(target, []byte("重复行\n重复行"), 0o644)
	_, err := tool.Call(context.Background(), map[string]any{"path": target, "old": "重复行", "new": "x"})
	if err == nil || !strings.Contains(err.Error(), "2 次") {
		t.Fatalf("多处匹配应拒绝: %v", err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "重复行\n重复行" {
		t.Fatalf("拒绝后文件不应被修改: %q", data)
	}
}

func TestEditFileNoMatchRejected(t *testing.T) {
	ft, dir := testFileTools(t)
	tool := &EditFileTool{FileTools: ft, Approver: approval.New("auto", nil, 0)}
	target := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(target, []byte("hello"), 0o644)
	if _, err := tool.Call(context.Background(), map[string]any{"path": target, "old": "world", "new": "x"}); err == nil {
		t.Fatal("未找到 old 应拒绝")
	}
}

func TestEditFileEscapeRejected(t *testing.T) {
	ft, _ := testFileTools(t)
	tool := &EditFileTool{FileTools: ft, Approver: approval.New("auto", nil, 0)}
	if _, err := tool.Call(context.Background(), map[string]any{
		"path": "../../etc/passwd", "old": "root", "new": "x",
	}); err == nil {
		t.Fatal("路径逃逸应被拒绝")
	}
}

// 审批拒绝：edit_file 不执行修改。
func TestEditFileApprovalDenied(t *testing.T) {
	ft, dir := testFileTools(t)
	tool := &EditFileTool{FileTools: ft, Approver: approval.New("confirm", nil, 80*time.Millisecond)}
	target := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(target, []byte("原文"), 0o644)
	rctx := logx.WithID(context.Background(), "req-test")
	_, err := tool.Call(rctx, map[string]any{"path": target, "old": "原文", "new": "改"})
	if err != approval.ErrDenied {
		t.Fatalf("审批超时应拒绝: %v", err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "原文" {
		t.Fatalf("未批准时文件不应被修改: %q", data)
	}
}
