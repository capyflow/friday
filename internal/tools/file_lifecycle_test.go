package tools

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// newMoveTool 建一个只允许访问 dir 的 move_file 工具。
func newMoveTool(t *testing.T) (*MoveFileTool, string) {
	t.Helper()
	dir := t.TempDir()
	return &MoveFileTool{FileTools: FileTools{AllowDirs: []string{dir}}}, dir
}

// newDeleteTool 建一个只允许访问 dir 的 delete_file 工具，回收站指向临时目录。
func newDeleteTool(t *testing.T) (*DeleteFileTool, string, string) {
	t.Helper()
	dir := t.TempDir()
	trash := filepath.Join(t.TempDir(), "friday-trash")
	return &DeleteFileTool{FileTools: FileTools{AllowDirs: []string{dir}}, TrashBase: trash}, dir, trash
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMoveFileRename(t *testing.T) {
	mv, dir := newMoveTool(t)
	src := filepath.Join(dir, "a.go")
	dst := filepath.Join(dir, "b.go")
	mustWrite(t, src, "package main\n")

	out, err := mv.Call(context.Background(), map[string]any{"from": src, "to": dst})
	if err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if !strings.Contains(out, "已移动") {
		t.Fatalf("输出缺少成功提示:\n%s", out)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("源文件应已消失，stat err=%v", err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读取目标失败: %v", err)
	}
	if string(raw) != "package main\n" {
		t.Fatalf("目标内容不符: %q", raw)
	}
}

func TestMoveFileMissingParentDir(t *testing.T) {
	mv, dir := newMoveTool(t)
	src := filepath.Join(dir, "a.go")
	mustWrite(t, src, "x")

	if _, err := mv.Call(context.Background(), map[string]any{
		"from": src,
		"to":   filepath.Join(dir, "no-such-dir", "b.go"),
	}); err == nil {
		t.Fatal("目标父目录不存在时应报错")
	}
	// 报错不得影响源文件
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("源文件应仍在原处: %v", err)
	}
}

func TestMoveFileTargetExistsNoOverwrite(t *testing.T) {
	mv, dir := newMoveTool(t)
	src := filepath.Join(dir, "a.go")
	dst := filepath.Join(dir, "b.go")
	mustWrite(t, src, "src-content")
	mustWrite(t, dst, "dst-content")

	if _, err := mv.Call(context.Background(), map[string]any{"from": src, "to": dst}); err == nil {
		t.Fatal("目标已存在且未指定 overwrite 时应报错")
	}
	// 关键：不能先动源再报错
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("源文件应仍在原处: %v", err)
	}
	if string(raw) != "src-content" {
		t.Fatalf("源文件内容被改动了: %q", raw)
	}
	raw, err = os.ReadFile(dst)
	if err != nil {
		t.Fatalf("目标文件应仍在原处: %v", err)
	}
	if string(raw) != "dst-content" {
		t.Fatalf("目标文件被改动了: %q", raw)
	}
	if _, err := os.Stat(dst + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("未覆盖时不应产生 .bak，stat err=%v", err)
	}
}

func TestMoveFileOverwrite(t *testing.T) {
	mv, dir := newMoveTool(t)
	src := filepath.Join(dir, "a.go")
	dst := filepath.Join(dir, "b.go")
	mustWrite(t, src, "src-content")
	mustWrite(t, dst, "dst-content")

	out, err := mv.Call(context.Background(), map[string]any{
		"from": src, "to": dst, "overwrite": true,
	})
	if err != nil {
		t.Fatalf("覆盖移动失败: %v", err)
	}
	if !strings.Contains(out, ".bak") {
		t.Fatalf("输出应说明旧目标备份位置:\n%s", out)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读取目标失败: %v", err)
	}
	if string(raw) != "src-content" {
		t.Fatalf("目标内容应变成源内容，实际 %q", raw)
	}
	raw, err = os.ReadFile(dst + ".bak")
	if err != nil {
		t.Fatalf("读取 .bak 失败: %v", err)
	}
	if string(raw) != "dst-content" {
		t.Fatalf(".bak 应是旧目标内容，实际 %q", raw)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("源文件应已消失，stat err=%v", err)
	}
}

func TestMoveFileMissingSource(t *testing.T) {
	mv, dir := newMoveTool(t)
	if _, err := mv.Call(context.Background(), map[string]any{
		"from": filepath.Join(dir, "nope.go"),
		"to":   filepath.Join(dir, "b.go"),
	}); err == nil {
		t.Fatal("源文件不存在时应报错")
	}
}

func TestMoveFileRejectsOutsideAllowDirs(t *testing.T) {
	mv, _ := newMoveTool(t)
	outside := t.TempDir()
	src := filepath.Join(outside, "a.go")
	mustWrite(t, src, "x")

	if _, err := mv.Call(context.Background(), map[string]any{
		"from": src,
		"to":   filepath.Join(outside, "b.go"),
	}); err == nil {
		t.Fatal("超出允许目录的路径应被拒绝")
	}
}

func TestDeleteFileMovesToTrash(t *testing.T) {
	del, dir, trash := newDeleteTool(t)
	path := filepath.Join(dir, "a.go")
	mustWrite(t, path, "package main\n")

	out, err := del.Call(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("原路径应已消失，stat err=%v", err)
	}
	// 回收站里应有带时间戳前缀的同名文件
	entries, err := os.ReadDir(trash)
	if err != nil {
		t.Fatalf("读取回收站失败: %v", err)
	}
	found := ""
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-a.go") {
			found = e.Name()
		}
	}
	if found == "" {
		t.Fatalf("回收站里没找到带时间戳的 a.go: %v", entries)
	}
	raw, err := os.ReadFile(filepath.Join(trash, found))
	if err != nil {
		t.Fatalf("读取回收站文件失败: %v", err)
	}
	if string(raw) != "package main\n" {
		t.Fatalf("回收站文件内容不符: %q", raw)
	}
	if !strings.Contains(out, filepath.Join(trash, found)) {
		t.Fatalf("返回值应含回收站完整路径:\n%s", out)
	}
}

func TestDeleteFileRejectsDir(t *testing.T) {
	del, dir, _ := newDeleteTool(t)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := del.Call(context.Background(), map[string]any{"path": sub}); err == nil {
		t.Fatal("目录应被拒绝")
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatalf("目录不应被删除: %v", err)
	}
}

func TestDeleteFileMissing(t *testing.T) {
	del, dir, _ := newDeleteTool(t)
	if _, err := del.Call(context.Background(), map[string]any{
		"path": filepath.Join(dir, "nope.go"),
	}); err == nil {
		t.Fatal("不存在的文件应报错")
	}
}

// copyEntry 是跨设备移动的兜底路径（正常 rename 成功时不会走到），单独测一下：
// 普通文件要保留权限位，软链要复制链接本身而不是目标内容。
func TestCopyEntryPreservesModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.sh")
	mustWrite(t, src, "echo hi")
	if err := os.Chmod(src, 0o750); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "b.sh")
	if err := copyEntry(src, dst); err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o750 {
		t.Fatalf("权限位未保留: %v", st.Mode().Perm())
	}

	target := filepath.Join(dir, "real.txt")
	mustWrite(t, target, "real")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	linkCopy := filepath.Join(dir, "link-copy.txt")
	if err := copyEntry(link, linkCopy); err != nil {
		t.Fatalf("复制软链失败: %v", err)
	}
	lst, err := os.Lstat(linkCopy)
	if err != nil {
		t.Fatal(err)
	}
	if lst.Mode()&os.ModeSymlink == 0 {
		t.Fatal("复制结果应是软链本身")
	}
	got, err := os.Readlink(linkCopy)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("软链指向变了: %s", got)
	}
}

// TestMoveFileMovesSymlinkItself 移动软链要移动链接本身：调用方说"改个软链的名字"，
// 搬走的应该是链接，而不是它指向的文件。
func TestMoveFileMovesSymlinkItself(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	mustWrite(t, target, "real")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(dir, "link-new.txt")
	tool := &MoveFileTool{FileTools: FileTools{AllowDirs: []string{dir}}}
	if _, err := tool.Call(context.Background(), map[string]any{
		"from": link, "to": moved,
	}); err != nil {
		t.Fatalf("移动软链失败: %v", err)
	}
	if _, err := os.Lstat(moved); err != nil {
		t.Fatalf("移动后应存在链接: %v", err)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("软链指向的文件不该被搬走: %v", err)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Fatal("原链接应已不在原处")
	}
}

// TestDeleteFileSameNameTwiceKeepsBoth 同名文件连删两次不能互相覆盖：回收站里
// 撞名会直接盖掉前一个，用户删错文件就再也找不回来。
func TestDeleteFileSameNameTwiceKeepsBoth(t *testing.T) {
	dir := t.TempDir()
	trash := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "第一份")
	mustWrite(t, filepath.Join(dir, "b.txt"), "第二份")
	tool := &DeleteFileTool{FileTools: FileTools{AllowDirs: []string{dir}}, TrashBase: trash}

	// 连续删两个同名文件（同一秒内）：回收站里必须是两份，不能互相覆盖。
	mustWrite(t, filepath.Join(dir, "c.txt"), "第三份")
	for _, name := range []string{"a.txt", "c.txt"} {
		out, err := tool.Call(context.Background(), map[string]any{"path": filepath.Join(dir, name)})
		if err != nil {
			t.Fatalf("删除 %s 失败: %v", name, err)
		}
		if !strings.Contains(out, trash) {
			t.Fatalf("返回值应给出回收站路径:\n%s", out)
		}
	}
	entries, err := os.ReadDir(trash)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("回收站里应有两份文件，实际 %d", len(entries))
	}
	// 两份内容都还在：删错文件要能找回来。
	var contents []string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(trash, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, string(data))
	}
	sort.Strings(contents)
	if len(contents) != 2 || contents[0] == contents[1] {
		t.Fatalf("回收站里两份文件内容应不同: %v", contents)
	}
}
