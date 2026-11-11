package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newEditFilesTool(t *testing.T, dir string) *EditFilesTool {
	t.Helper()
	return &EditFilesTool{FileTools: FileTools{AllowDirs: []string{dir}}}
}

func TestEditFilesAppliesAll(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "alpha\nkeep\n")
	writeFile(t, filepath.Join(dir, "b.go"), "beta\n")
	tool := newEditFilesTool(t, dir)

	out, err := tool.Call(context.Background(), map[string]any{
		"edits": []any{
			map[string]any{"path": "a.go", "old": "alpha", "new": "ALPHA"},
			map[string]any{"path": "b.go", "old": "beta", "new": "BETA"},
		},
	})
	if err != nil {
		t.Fatalf("批量编辑失败: %v", err)
	}
	if !strings.Contains(out, "已修改 2 个文件") {
		t.Fatalf("输出应说明改了 2 个文件:\n%s", out)
	}
	for name, want := range map[string]string{"a.go": "ALPHA", "b.go": "BETA"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s 未被修改: %s", name, data)
		}
		if _, err := os.Stat(filepath.Join(dir, name+".bak")); err != nil {
			t.Fatalf("%s 应有备份 .bak", name)
		}
	}
}

func TestEditFilesRejectsAmbiguousOldAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "dup\ndup\n")
	writeFile(t, filepath.Join(dir, "b.go"), "beta\n")
	tool := newEditFilesTool(t, dir)

	_, err := tool.Call(context.Background(), map[string]any{
		"edits": []any{
			map[string]any{"path": "b.go", "old": "beta", "new": "BETA"},
			map[string]any{"path": "a.go", "old": "dup", "new": "DUP"},
		},
	})
	if err == nil {
		t.Fatal("old 多处匹配应整批拒绝")
	}
	if !strings.Contains(err.Error(), "唯一匹配") {
		t.Fatalf("报错应说清唯一匹配要求:\n%v", err)
	}
	// 关键：前面的编辑也不能落盘——整批要么全改要么不改。
	data, err := os.ReadFile(filepath.Join(dir, "b.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "BETA") {
		t.Fatalf("整批被拒时不应改任何文件: %s", data)
	}
}

func TestEditFilesRejectsDuplicatePath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "one\n")
	tool := newEditFilesTool(t, dir)
	_, err := tool.Call(context.Background(), map[string]any{
		"edits": []any{
			map[string]any{"path": "a.go", "old": "one", "new": "1"},
			map[string]any{"path": "a.go", "old": "one", "new": "2"},
		},
	})
	if err == nil {
		t.Fatal("同一文件重复出现应报错")
	}
	if !strings.Contains(err.Error(), "多次") {
		t.Fatalf("报错应说清同一文件重复:\n%v", err)
	}
}

func TestEditFilesMissingOldChangesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "one\n")
	tool := newEditFilesTool(t, dir)
	_, err := tool.Call(context.Background(), map[string]any{
		"edits": []any{
			map[string]any{"path": "a.go", "old": "not-there", "new": "x"},
		},
	})
	if err == nil {
		t.Fatal("old 不存在应报错")
	}
	if !strings.Contains(err.Error(), "没找到") {
		t.Fatalf("报错应说清没找到:\n%v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	if string(data) != "one\n" {
		t.Fatalf("失败时不应改动文件: %s", data)
	}
}

func TestEditFilesRejectsEmptyEdits(t *testing.T) {
	dir := t.TempDir()
	tool := newEditFilesTool(t, dir)
	if _, err := tool.Call(context.Background(), map[string]any{"edits": []any{}}); err == nil {
		t.Fatal("空 edits 应报错")
	}
}
