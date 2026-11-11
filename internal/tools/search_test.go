package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/workdir"
)

func newSearchTools(t *testing.T, dir string) (*SearchCodeTool, *FindFilesTool) {
	t.Helper()
	base := FileTools{AllowDirs: []string{dir}}
	return &SearchCodeTool{FileTools: base}, &FindFilesTool{FileTools: base}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSearchCodeFindsMatchWithLineNumber(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package main\n\nfunc Hello() {}\n")
	search, _ := newSearchTools(t, dir)
	out, err := search.Call(context.Background(), map[string]any{"pattern": "func Hello"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if !strings.Contains(out, "a.go:3:") {
		t.Fatalf("输出应带文件名与行号:\n%s", out)
	}
}

func TestSearchCodeFixedIsLiteral(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "x := a.b\ny := axb\n")
	search, _ := newSearchTools(t, dir)
	// 不加 fixed 时 a.b 是正则（. 匹配任意字符），两行都该命中
	out, err := search.Call(context.Background(), map[string]any{"pattern": "a.b"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if !strings.Contains(out, "a.go:1:") || !strings.Contains(out, "a.go:2:") {
		t.Fatalf("正则模式应命中两行:\n%s", out)
	}
	// fixed=true 时只命中字面量 a.b
	out2, err := search.Call(context.Background(), map[string]any{"pattern": "a.b", "fixed": true})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if !strings.Contains(out2, "a.go:1:") || strings.Contains(out2, "a.go:2:") {
		t.Fatalf("fixed 模式结果不对:\n%s", out2)
	}
}

func TestSearchCodeGlobFilter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "target\n")
	writeFile(t, filepath.Join(dir, "b.md"), "target\n")
	search, _ := newSearchTools(t, dir)
	out, err := search.Call(context.Background(), map[string]any{"pattern": "target", "glob": "*.go"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if !strings.Contains(out, "a.go:1") || strings.Contains(out, "b.md") {
		t.Fatalf("glob 过滤没生效:\n%s", out)
	}
}

func TestSearchCodeNoMatchSaysSo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "hello\n")
	search, _ := newSearchTools(t, dir)
	out, err := search.Call(context.Background(), map[string]any{"pattern": "nothing-here"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if !strings.Contains(out, "没有匹配") {
		t.Fatalf("无命中时应明说:\n%s", out)
	}
}

func TestSearchCodeSkipsBinary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.bin"), "target\x00\x01binary")
	search, _ := newSearchTools(t, dir)
	out, err := search.Call(context.Background(), map[string]any{"pattern": "target"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if strings.Contains(out, "a.bin:") {
		t.Fatalf("二进制文件不该进结果:\n%s", out)
	}
}

func TestSearchCodeInvalidRegexExplainsFixed(t *testing.T) {
	dir := t.TempDir()
	search, _ := newSearchTools(t, dir)
	_, err := search.Call(context.Background(), map[string]any{"pattern": "([a-z"})
	if err == nil {
		t.Fatal("非法正则应报错")
	}
	if !strings.Contains(err.Error(), "fixed") {
		t.Fatalf("报错应提示 fixed=true:\n%v", err)
	}
}

func TestSearchCodeUsesSessionWorkdir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "in-session-dir\n")
	// AllowDirs 不含该目录，但会话目录在 ctx 上——会话目录就是允许根。
	base := FileTools{AllowDirs: []string{t.TempDir()}}
	search := &SearchCodeTool{FileTools: base}
	ctx := workdir.With(context.Background(), dir)
	out, err := search.Call(ctx, map[string]any{"pattern": "in-session-dir"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if !strings.Contains(out, "a.go:1") {
		t.Fatalf("应命中会话目录下的文件:\n%s", out)
	}
}

func TestFindFilesByGlob(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "x")
	writeFile(t, filepath.Join(dir, "b_test.go"), "x")
	writeFile(t, filepath.Join(dir, "sub", "c.go"), "x")
	_, find := newSearchTools(t, dir)

	out, err := find.Call(context.Background(), map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatalf("查找失败: %v", err)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "sub/c.go") {
		t.Fatalf("应同时命中根目录与子目录:\n%s", out)
	}
	if !strings.Contains(out, "b_test.go") {
		t.Fatalf("*.go 也该匹配 b_test.go:\n%s", out)
	}
	writeFile(t, filepath.Join(dir, "note.md"), "x")
	out3, err := find.Call(context.Background(), map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatalf("查找失败: %v", err)
	}
	if strings.Contains(out3, "note.md") {
		t.Fatalf("*.go 不该匹配 note.md:\n%s", out3)
	}

	out2, err := find.Call(context.Background(), map[string]any{"pattern": "**/*_test.go"})
	if err != nil {
		t.Fatalf("查找失败: %v", err)
	}
	if !strings.Contains(out2, "b_test.go") {
		t.Fatalf("** 应跨目录匹配:\n%s", out2)
	}
}

func TestFindFilesNoMatch(t *testing.T) {
	dir := t.TempDir()
	_, find := newSearchTools(t, dir)
	out, err := find.Call(context.Background(), map[string]any{"pattern": "*.rs"})
	if err != nil {
		t.Fatalf("查找失败: %v", err)
	}
	if !strings.Contains(out, "没有匹配") {
		t.Fatalf("无结果时应明说:\n%s", out)
	}
}
