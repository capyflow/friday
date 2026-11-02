package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	gitTest(t, dir, "config", "user.name", "test")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gone.txt"), []byte("bye\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-qm", "init")
	return dir
}

func TestTurnSnapshotGitDetectsAndReverts(t *testing.T) {
	dir := initTestRepo(t)
	base := t.TempDir()
	s, err := beginTurnSnapshot(dir, base, "th1", "t1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if s.meta.Kind != "git" {
		t.Fatalf("kind=%s, want git", s.meta.Kind)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nTWO\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	ch := s.Finish()
	if !ch.Snapshot {
		t.Fatalf("snapshot=false: %s", ch.Note)
	}
	byPath := map[string]FileChange{}
	for _, f := range ch.Files {
		byPath[f.Path] = f
	}
	if byPath["a.txt"].Status != "modified" || byPath["new.txt"].Status != "added" || byPath["gone.txt"].Status != "deleted" {
		t.Fatalf("unexpected changes: %+v", ch.Files)
	}
	if !strings.Contains(byPath["a.txt"].Diff, "-TWO") && !strings.Contains(byPath["a.txt"].Diff, "-two") {
		t.Fatalf("diff missing removed line:\n%s", byPath["a.txt"].Diff)
	}
	if !strings.Contains(byPath["a.txt"].Diff, "+TWO") {
		t.Fatalf("diff missing added line:\n%s", byPath["a.txt"].Diff)
	}
	if byPath["new.txt"].Additions != 1 || byPath["gone.txt"].Deletions != 1 {
		t.Fatalf("stats wrong: %+v", ch.Files)
	}
	reverted, err := s.Revert()
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted.RevertedAt == nil {
		t.Fatal("revertedAt not set")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\ntwo\nthree\n" {
		t.Fatalf("a.txt not restored: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt should be removed, err=%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "gone.txt")); string(b) != "bye\n" {
		t.Fatalf("gone.txt not restored: %q", b)
	}
	if _, err := s.Revert(); err == nil || !strings.Contains(err.Error(), "已经还原") {
		t.Fatalf("second revert err=%v", err)
	}
}

func TestTurnSnapshotCopyDetectsAndReverts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gone.txt"), []byte("bye\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "x", "skip.js"), []byte("skip"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := beginTurnSnapshot(dir, t.TempDir(), "th2", "t1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if s.meta.Kind != "copy" {
		t.Fatalf("kind=%s, want copy", s.meta.Kind)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nTWO\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "x", "skip.js"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	ch := s.Finish()
	byPath := map[string]FileChange{}
	for _, f := range ch.Files {
		byPath[f.Path] = f
	}
	if byPath["a.txt"].Status != "modified" || byPath["new.txt"].Status != "added" || byPath["gone.txt"].Status != "deleted" {
		t.Fatalf("unexpected changes: %+v", ch.Files)
	}
	if _, ok := byPath["node_modules/x/skip.js"]; ok {
		t.Fatalf("node_modules should be ignored: %+v", ch.Files)
	}
	if !strings.Contains(byPath["a.txt"].Diff, "+TWO") {
		t.Fatalf("diff missing added line:\n%s", byPath["a.txt"].Diff)
	}
	if _, err := s.Revert(); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\ntwo\n" {
		t.Fatalf("a.txt not restored: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt should be removed, err=%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "gone.txt")); string(b) != "bye\n" {
		t.Fatalf("gone.txt not restored: %q", b)
	}
}

func TestTurnSnapshotRevertRefusesLaterEdit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := beginTurnSnapshot(dir, t.TempDir(), "th3", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = s.Finish()
	// 用户在本轮结束后又改了一次：还原必须拒绝，不能覆盖。
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revert(); err == nil || !strings.Contains(err.Error(), "又被改过") {
		t.Fatalf("revert should refuse, err=%v", err)
	}
}

func TestWorkspaceFilesRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, full, _, err := listWorkspaceFiles(root, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if full != root || len(entries) != 1 || entries[0].Path != "a.txt" {
		t.Fatalf("unexpected listing: %+v %q", entries, full)
	}
	if _, _, _, err := listWorkspaceFiles(root, "../"); err == nil {
		t.Fatal("escape should be rejected")
	}
	if _, err := safeTurnID("../x"); err == nil {
		t.Fatal("turn id escape should be rejected")
	}
}

func TestLineDiffRendersHunks(t *testing.T) {
	old := []byte("a\nb\nc\nd\ne\nf\ng\n")
	new := []byte("a\nb\nC\nd\ne\nf\ng\nh\n")
	diff, add, del, binary, truncated := lineDiff(old, new, "x.txt")
	if binary || truncated {
		t.Fatalf("unexpected flags binary=%v truncated=%v", binary, truncated)
	}
	if add != 2 || del != 1 {
		t.Fatalf("add=%d del=%d\n%s", add, del, diff)
	}
	for _, want := range []string{"--- a/x.txt", "+++ b/x.txt", "@@", "-c", "+C", "+h"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}

func TestTurnSnapshotGitWorkspaceSubdir(t *testing.T) {
	root := initTestRepo(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-qm", "sub")
	s, err := beginTurnSnapshot(sub, t.TempDir(), "th-sub", "t1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 工作区外的文件改动不应出现在这次变更清单里
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("changed outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ch := s.Finish()
	if len(ch.Files) != 1 || ch.Files[0].Path != "inner.txt" {
		t.Fatalf("子目录工作区只应看到 inner.txt: %+v", ch.Files)
	}
	if _, err := s.Revert(); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(sub, "inner.txt")); string(b) != "before\n" {
		t.Fatalf("inner.txt not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "outside.txt")); string(b) != "changed outside\n" {
		t.Fatalf("workspace 外的文件不该被还原: %q", b)
	}
}

// 超过单文件上限的副本不能还原——这时必须当场说清（V-5 验收点），而不是等用户点
// 「还原本轮」才给一个看不懂的错。
func TestTurnSnapshotCopyFlagsOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", int(snapshotMaxCopyFile)+1)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := beginTurnSnapshot(dir, t.TempDir(), "th1", "t1")
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	if snap.meta.Kind != "copy" {
		t.Fatalf("非 git 目录应走副本快照: %+v", snap.meta)
	}
	if !strings.Contains(snap.meta.Note, "1 个文件") || !strings.Contains(snap.meta.Note, "不能还原") {
		t.Fatalf("超限要在快照阶段就写明不能还原: %q", snap.meta.Note)
	}
	// 改了超限文件之后再还原：整轮预检要拒绝并点名是哪个文件
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ch := snap.Finish(); ch == nil || len(ch.Files) == 0 {
		t.Fatalf("收尾应给出改动清单: %+v", ch)
	}
	if _, err := snap.Revert(); err == nil || !strings.Contains(err.Error(), "big.bin") {
		t.Fatalf("超限文件应拒绝还原并点名: %v", err)
	}
}
