package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFileTools(t *testing.T) (*ReadFileTool, *ListFilesTool, string) {
	t.Helper()
	dir := t.TempDir()
	base := FileTools{AllowDirs: []string{dir}}
	return &ReadFileTool{FileTools: base}, &ListFilesTool{FileTools: base}, dir
}

func TestReadFileBasic(t *testing.T) {
	read, _, dir := newFileTools(t)
	content := "第一行\n第二行\n第三行\n"
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := read.Call(context.Background(), map[string]any{"path": "a.txt"})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	for _, want := range []string{"共 3 行", "     1| 第一行", "     3| 第三行"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	read, _, dir := newFileTools(t)
	var b strings.Builder
	for i := 1; i <= 1000; i++ {
		b.WriteString("L")
		b.WriteString(strings.Repeat("x", 10))
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := read.Call(context.Background(), map[string]any{"path": "big.txt", "offset": 401})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(out, "显示第 401–") || !strings.Contains(out, "[未完] 用 offset=801 续读") {
		t.Fatalf("分段读取不符合预期:\n%s", out[:300])
	}
}

func TestReadFileRejectsEscape(t *testing.T) {
	read, _, dir := newFileTools(t)
	// 白名单外建一个文件，尝试穿越读取
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../../" + filepath.Base(outside) + "/secret.txt", "/etc/passwd"} {
		if _, err := read.Call(context.Background(), map[string]any{"path": p}); err == nil {
			t.Fatalf("路径 %s 应被拒绝", p)
		}
	}
	// 符号链接逃逸
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err == nil {
		if _, err := read.Call(context.Background(), map[string]any{"path": "link.txt"}); err == nil {
			t.Fatal("符号链接逃逸应被拒绝")
		}
	}
	_ = dir
}

func TestReadFileRejectsBinary(t *testing.T) {
	read, _, dir := newFileTools(t)
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), append([]byte("OK\x00\x00"), make([]byte, 32)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Call(context.Background(), map[string]any{"path": "bin.dat"}); err == nil {
		t.Fatal("二进制文件应被拒绝")
	}
}

func TestListFiles(t *testing.T) {
	_, list, dir := newFileTools(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := list.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	if !strings.Contains(out, "[目录] sub/") || !strings.Contains(out, "note.txt") {
		t.Fatalf("列目录输出不符:\n%s", out)
	}
	// 子目录参数
	if _, err := list.Call(context.Background(), map[string]any{"dir": "sub"}); err != nil {
		t.Fatalf("子目录列举失败: %v", err)
	}
}
