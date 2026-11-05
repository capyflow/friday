// session_dir_test.go 覆盖会话工作目录对文件工具的影响：工作模式里建会话时点选的
// 目录走 ctx 传进来，它既是相对路径的基准，也算一个额外的允许根；范围之外的路径
// 照旧拒绝，没带目录时行为与以往一模一样。
package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/workdir"
)

func toolsIn(configDir, sessionDir string) (*ReadFileTool, *ListFilesTool, *WriteFileTool) {
	base := FileTools{AllowDirs: []string{configDir}}
	return &ReadFileTool{FileTools: base}, &ListFilesTool{FileTools: base}, &WriteFileTool{FileTools: base}
}

func TestSessionDirIsAnAllowedRoot(t *testing.T) {
	configDir, sessionDir := t.TempDir(), t.TempDir()
	read, list, write := toolsIn(configDir, sessionDir)
	if err := os.WriteFile(filepath.Join(sessionDir, "notes.md"), []byte("# 项目笔记\n第一行\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "a.txt"), []byte("配置目录的文件\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := workdir.With(context.Background(), sessionDir)

	// 相对路径按会话目录解析，而不是配置的白名单首项。
	out, err := read.Call(ctx, map[string]any{"path": "notes.md"})
	if err != nil {
		t.Fatalf("会话目录内的相对路径应读得到: %v", err)
	}
	if !strings.Contains(out, "项目笔记") {
		t.Fatalf("读到的内容不对: %s", out)
	}

	// 绝对路径同样放行（模型常直接给绝对路径）。
	if _, err := read.Call(ctx, map[string]any{"path": filepath.Join(sessionDir, "notes.md")}); err != nil {
		t.Fatalf("会话目录内的绝对路径应读得到: %v", err)
	}

	// 写也认这个根。
	if _, err := write.Call(ctx, map[string]any{"path": "plan.md", "content": "第一步"}); err != nil {
		t.Fatalf("写入会话目录失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "plan.md")); err != nil {
		t.Fatalf("文件没落在会话目录里: %v", err)
	}

	// list_files 留空列的是这条会话的目录。
	out, err = list.Call(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("列会话目录失败: %v", err)
	}
	if !strings.Contains(out, "notes.md") || strings.Contains(out, "a.txt") {
		t.Fatalf("列出的应是会话目录而不是配置目录:\n%s", out)
	}

	// 两个根之外照旧拦住。
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Call(ctx, map[string]any{"path": filepath.Join(outside, "secret.txt")}); err == nil {
		t.Fatal("范围外的路径不该读得到")
	}
	// 软链把会话目录外的文件挂进来也不行。
	link := filepath.Join(sessionDir, "link.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Call(ctx, map[string]any{"path": "link.txt"}); err == nil {
		t.Fatal("指向范围外的软链不该放行")
	}
}

func TestNoSessionDirKeepsOldBehavior(t *testing.T) {
	configDir, sessionDir := t.TempDir(), t.TempDir()
	read, list, _ := toolsIn(configDir, sessionDir)
	if err := os.WriteFile(filepath.Join(configDir, "a.txt"), []byte("配置目录的文件\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "b.txt"), []byte("会话目录的文件\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := read.Call(ctx, map[string]any{"path": "a.txt"}); err != nil {
		t.Fatalf("没带会话目录时相对路径仍按配置白名单解析: %v", err)
	}
	if _, err := read.Call(ctx, map[string]any{"path": filepath.Join(sessionDir, "b.txt")}); err == nil {
		t.Fatal("没带会话目录时那个目录不该算根")
	}
	out, err := list.Call(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.txt") {
		t.Fatalf("留空应列配置白名单首项:\n%s", out)
	}
}
