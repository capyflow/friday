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

// 只有未跟踪文件的工作区是常态（Friday 自己就会留下未跟踪的 .friday/.gitignore）：
// 这时 git stash create 本来就该返回空，快照不能因此退化成目录副本。
// 一旦退化，既不看 .gitignore，又会把构建产物、嵌套 worktree 报进「本轮改动」。
func TestSnapshotKeepsGitBaselineWithOnlyUntrackedDirt(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".friday"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".friday", ".gitignore"), []byte("*\n!/.gitignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("gen/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".gitignore")
	gitTest(t, dir, "commit", "-qm", "ignore gen")

	s, err := beginTurnSnapshot(dir, t.TempDir(), "th-un", "t1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if s.meta.Kind != "git" {
		t.Fatalf("只有未跟踪文件时仍应用 git 基线，kind=%s note=%s", s.meta.Kind, s.meta.Note)
	}
	for rel := range s.meta.Start {
		if strings.HasPrefix(rel, ".friday/") {
			t.Fatalf("Friday 自己的目录不该进快照: %s", rel)
		}
	}
	// 轮内：改一个跟踪文件，同时被忽略的 gen/ 里冒出一堆构建产物
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nTWO\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "gen"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gen", "out.bin"), []byte("build output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ch := s.Finish()
	if len(ch.Files) != 1 || ch.Files[0].Path != "a.txt" || ch.Files[0].Status != "modified" {
		t.Fatalf("只该报 a.txt，实际: %+v", ch.Files)
	}
}

// 轮内切分支不是 agent 改文件：整条分支差异不能算进本轮，还原目标也要跟着换成
// 当前 HEAD，否则「还原本轮」会把新分支上的文件写回旧分支的内容。
func TestSnapshotBranchSwitchNotCountedAsChanges(t *testing.T) {
	dir := initTestRepo(t)
	base := strings.TrimSpace(gitTest(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
	gitTest(t, dir, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("feature one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-qm", "feature work")
	gitTest(t, dir, "checkout", "-q", base)

	s, err := beginTurnSnapshot(dir, t.TempDir(), "th-sw", "t1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	gitTest(t, dir, "checkout", "-q", "feature")
	// 切完之后只改这一个文件，同一个路径在两条分支上的内容也不同
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nSWITCHED\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ch := s.Finish()
	if len(ch.Files) != 1 || ch.Files[0].Path != "a.txt" {
		t.Fatalf("切分支只该报切换后真实改动的 a.txt，实际: %+v", ch.Files)
	}
	if !strings.Contains(ch.Note, "切到了") {
		t.Fatalf("note 应说明切了分支，实际: %q", ch.Note)
	}
	if !strings.Contains(ch.Files[0].Diff, "+SWITCHED") {
		t.Fatalf("diff 应相对切换后的分支算，实际:\n%s", ch.Files[0].Diff)
	}
}

// 轮内自己 commit 算 agent 干的活：要出现在本轮改动里，同时 note 说清还原不撤提交。
func TestSnapshotCountsCommitsMadeDuringTurn(t *testing.T) {
	dir := initTestRepo(t)
	headBefore := strings.TrimSpace(gitTest(t, dir, "rev-parse", "HEAD"))
	s, err := beginTurnSnapshot(dir, t.TempDir(), "th-cm", "t1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nCOMMITTED\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "a.txt")
	gitTest(t, dir, "commit", "-qm", "agent commit")
	ch := s.Finish()
	if len(ch.Files) != 1 || ch.Files[0].Path != "a.txt" {
		t.Fatalf("提交的改动应算本轮，实际: %+v", ch.Files)
	}
	if !strings.Contains(ch.Note, "commit") {
		t.Fatalf("note 应提示本轮有提交，实际: %q", ch.Note)
	}
	if _, err := s.Revert(); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\ntwo\nthree\n" {
		t.Fatalf("a.txt 应还原成轮前内容: %q", b)
	}
	if head := strings.TrimSpace(gitTest(t, dir, "rev-parse", "HEAD")); head == headBefore {
		t.Fatal("还原不该撤销提交，HEAD 却回到了轮前")
	}
}

// 没有提交的 git 仓库只能走目录副本，但文件清单仍应按 git 口径取：
// 被 .gitignore 掉的构建产物不该进快照，更不该被报成 agent 改动。
func TestCopySnapshotInGitRepoSkipsIgnoredJunk(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("gen/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "a.txt", ".gitignore")
	if err := os.MkdirAll(filepath.Join(dir, "gen"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gen", "old.bin"), []byte("old build\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".friday"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".friday", "t1.md"), []byte("摘要\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := beginTurnSnapshot(dir, t.TempDir(), "th-cg", "t1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if s.meta.Kind != "copy" {
		t.Fatalf("没有提交的仓库应走目录副本，kind=%s", s.meta.Kind)
	}
	for rel := range s.meta.Start {
		if strings.HasPrefix(rel, "gen/") {
			t.Fatalf("被忽略的构建产物不该进快照: %s", rel)
		}
		if strings.HasPrefix(rel, ".friday/") {
			t.Fatalf("Friday 自己的目录不该进快照: %s", rel)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "gen", "new.bin"), []byte("new build\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ch := s.Finish()
	if len(ch.Files) != 1 || ch.Files[0].Path != "a.txt" {
		t.Fatalf("只该报 a.txt，实际: %+v", ch.Files)
	}
}

// Friday 自己的 .friday 目录（会话记录等）不算 agent 的改动：非 git 项目没有
// .gitignore 可依赖，快照必须整个跳过它，否则会被备份、还会报进「本轮改动」。
func TestSnapshotSkipsFridayOwnDir(t *testing.T) {
	dir := t.TempDir()
	recordDir := filepath.Join(dir, sessionDirRel)
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recordDir, "t1.md"), []byte("旧摘要\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := beginTurnSnapshot(dir, filepath.Join(t.TempDir(), "snaps"), "th1", "t1")
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	// agent 真的改了工作区文件，同时改了 .friday 里的东西
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recordDir, "t1.md"), []byte("新摘要\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ch := snap.Finish()
	var got []string
	for _, f := range ch.Files {
		got = append(got, f.Path)
	}
	joined := strings.Join(got, ",")
	if strings.Contains(joined, ".friday") {
		t.Errorf("Friday 自己的目录被当成 agent 改动: %s", joined)
	}
	if !strings.Contains(joined, "a.txt") {
		t.Errorf("真实改动不该被漏掉: %s", joined)
	}
}
