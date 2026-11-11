package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/workdir"
)

// initRepo 建一个只有一次提交的小仓库，用 -c 传身份，避免依赖本机 git 配置。
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v 失败: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	writeFile(t, dir+"/a.go", "package main\n")
	run("add", ".")
	run("commit", "-qm", "init")
	return dir
}

func TestGitStatusShowsBranchAndChange(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, dir+"/a.go", "package main\n// changed\n")
	base := FileTools{AllowDirs: []string{dir}}
	status := &GitStatusTool{FileTools: base}
	out, err := status.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("git_status 失败: %v", err)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "##") {
		t.Fatalf("状态应含分支与改动文件:\n%s", out)
	}
}

func TestGitDiffShowsContent(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, dir+"/a.go", "package main\n// changed\n")
	base := FileTools{AllowDirs: []string{dir}}
	diff := &GitDiffTool{FileTools: base}
	out, err := diff.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("git_diff 失败: %v", err)
	}
	if !strings.Contains(out, "+// changed") {
		t.Fatalf("diff 应含新增行:\n%s", out)
	}
}

func TestGitLogListsCommit(t *testing.T) {
	dir := initRepo(t)
	base := FileTools{AllowDirs: []string{dir}}
	log := &GitLogTool{FileTools: base}
	out, err := log.Call(context.Background(), map[string]any{"limit": 5})
	if err != nil {
		t.Fatalf("git_log 失败: %v", err)
	}
	if !strings.Contains(out, "init") {
		t.Fatalf("日志应含提交信息:\n%s", out)
	}
}

func TestGitToolsSayWhenNotARepo(t *testing.T) {
	dir := t.TempDir()
	base := FileTools{AllowDirs: []string{dir}}
	status := &GitStatusTool{FileTools: base}
	_, err := status.Call(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("非仓库目录应报错")
	}
	if !strings.Contains(err.Error(), "git 仓库") {
		t.Fatalf("报错应说清不是仓库:\n%v", err)
	}
}

func TestGitToolsUseSessionWorkdir(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, dir+"/a.go", "package main\n// changed\n")
	// AllowDirs 不含仓库，靠会话目录定位。
	base := FileTools{AllowDirs: []string{t.TempDir()}}
	status := &GitStatusTool{FileTools: base}
	ctx := workdir.With(context.Background(), dir)
	out, err := status.Call(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("git_status 失败: %v", err)
	}
	if !strings.Contains(out, "a.go") {
		t.Fatalf("应看到会话目录仓库的改动:\n%s", out)
	}
}
