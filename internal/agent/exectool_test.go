package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/workdir"
)

func TestExecRunsInSessionWorkdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("hello-from-project"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := NewExecCommandTool(30, scratchDir())
	ctx := workdir.With(context.Background(), dir)

	out, err := tool.Call(ctx, map[string]any{"command": "cat marker.txt"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "hello-from-project") {
		t.Fatalf("命令应在会话工作目录里执行，输出:\n%s", out)
	}
}

func TestExecPwdIsSessionWorkdir(t *testing.T) {
	dir := t.TempDir()
	tool := NewExecCommandTool(30, scratchDir())
	ctx := workdir.With(context.Background(), dir)
	out, err := tool.Call(ctx, map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	// macOS 的临时目录是 /var 软链到 /private/var，比较时取其一即可。
	if !strings.Contains(out, dir) && !strings.Contains(out, strings.TrimPrefix(dir, "/private")) {
		t.Fatalf("工作目录应为 %s，输出:\n%s", dir, out)
	}
}

func TestExecExplicitCwdWins(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "marker.txt"), []byte("elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := NewExecCommandTool(30, scratchDir())
	ctx := workdir.With(context.Background(), dir)

	out, err := tool.Call(ctx, map[string]any{"command": "cat marker.txt", "cwd": other})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "elsewhere") {
		t.Fatalf("显式 cwd 应生效，输出:\n%s", out)
	}
}

func TestExecBadCwdIsRejected(t *testing.T) {
	tool := NewExecCommandTool(30, scratchDir())
	_, err := tool.Call(context.Background(), map[string]any{"command": "ls", "cwd": "/definitely/not/here"})
	if err == nil {
		t.Fatal("不存在的 cwd 应报错")
	}
	if !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("报错应说清是 cwd 的问题:\n%v", err)
	}
}

func TestExecWithoutSessionDirFallsBackToScratch(t *testing.T) {
	tool := NewExecCommandTool(30, scratchDir())
	out, err := tool.Call(context.Background(), map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, filepath.Base(scratchDir())) {
		t.Fatalf("无会话目录时应落在临时目录，输出:\n%s", out)
	}
}

// TestExecKeepsTailOfLongOutput 长输出截断时必须保住末尾：测试失败信息、编译错误
// 都在末尾，只留开头等于把最该看的丢了。
func TestExecKeepsTailOfLongOutput(t *testing.T) {
	tool := NewExecCommandTool(30, scratchDir())
	cmd := "i=0; while [ $i -lt 900 ]; do printf '0123456789'; i=$((i+1)); done; echo TAIL-MARKER"
	out, err := tool.Call(context.Background(), map[string]any{"command": cmd})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "TAIL-MARKER") {
		t.Fatalf("截断后应保留末尾内容:\n%s", out)
	}
	if !strings.Contains(out, "中间省略") {
		t.Fatalf("应标注中间被省略:\n%s", out)
	}
}

func TestExecEmptyCommandRejected(t *testing.T) {
	tool := NewExecCommandTool(30, scratchDir())
	if _, err := tool.Call(context.Background(), map[string]any{"command": "  "}); err == nil {
		t.Fatal("空命令应报错")
	}
}
