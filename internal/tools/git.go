package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 只读 git 工具（git_status / git_diff / git_log）。
//
// 为什么只做只读：改代码的 agent 必须先看见现状（我改了什么、别人改了什么、
// 当前在哪个分支），否则就是在盲改。写操作（commit/checkout/reset）留给用户
// 在界面上点——那是有后果的动作，不该由模型自己按一下就生效。
//
// 这三个工具不进审批队列：它们不改动任何东西，理由和 read_file 一样。

const (
	gitTimeout     = 20 * time.Second
	gitMaxOutBytes = 24 << 10
	gitDefaultLog  = 10
	gitMaxLog      = 50
)

// gitRepoDir 找到本次会话所在目录对应的 git 仓库根。找不到就明说，不要静默返回空。
func (t *FileTools) gitRepoDir(ctx context.Context) (string, error) {
	roots := t.roots(ctx)
	if len(roots) == 0 {
		return "", fmt.Errorf("未配置可访问目录（file.allowDirs，或给这条会话选一个工作目录）")
	}
	dir, err := filepath.Abs(roots[0])
	if err != nil {
		return "", fmt.Errorf("工作目录无法解析: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("%s 及其上级目录都不是 git 仓库（没找到 .git）", roots[0])
}

// runGit 在仓库目录里跑一条只读 git 命令。
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s 失败: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// truncateBytes 按字节截断并标注原文长度（不切断多字节字符）。
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[…输出已截断，完整输出共 %d 字节]", len(s))
}

// utf8Start 判断字节是否是多字节字符的起始位。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// GitStatusTool 看当前仓库状态：分支 + 未提交改动。
type GitStatusTool struct {
	FileTools
}

func (t *GitStatusTool) Name() string { return "git_status" }

func (t *GitStatusTool) Description() string {
	return "查看本次会话工作目录所在 git 仓库的状态：当前分支与未提交改动（含未跟踪文件）。" +
		"改代码前后都应该先看一次，确认不会覆盖别人的改动。"
}

func (t *GitStatusTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *GitStatusTool) Call(ctx context.Context, args map[string]any) (string, error) {
	repo, err := t.gitRepoDir(ctx)
	if err != nil {
		return "", err
	}
	out, err := runGit(ctx, repo, "status", "--short", "--branch")
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return fmt.Sprintf("%s：工作区干净（无未提交改动）", repo), nil
	}
	return fmt.Sprintf("%s（分支与改动）\n%s", repo, truncateBytes(out, gitMaxOutBytes)), nil
}

// GitDiffTool 看未提交改动的差异。
type GitDiffTool struct {
	FileTools
}

func (t *GitDiffTool) Name() string { return "git_diff" }

func (t *GitDiffTool) Description() string {
	return "查看 git 仓库的未提交差异（diff）。staged=true 看已暂存部分，path 可限定某个文件或目录。" +
		"想知道" +
		"自己（或别人）改了什么、以及改动是否正确时用它。"
}

func (t *GitDiffTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"staged": map[string]any{"type": "boolean", "description": "看已暂存(staged)的差异，默认 false"},
			"path":   map[string]any{"type": "string", "description": "只看某个文件/目录的差异"},
		},
	}
}

func (t *GitDiffTool) Call(ctx context.Context, args map[string]any) (string, error) {
	repo, err := t.gitRepoDir(ctx)
	if err != nil {
		return "", err
	}
	gitArgs := []string{"diff"}
	if boolArg(args, "staged") {
		gitArgs = append(gitArgs, "--cached")
	}
	if p := strArg(args, "path"); p != "" {
		gitArgs = append(gitArgs, "--", p)
	}
	out, err := runGit(ctx, repo, gitArgs...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		what := "未暂存的改动"
		if boolArg(args, "staged") {
			what = "已暂存的改动"
		}
		return fmt.Sprintf("%s：没有%s", repo, what), nil
	}
	return fmt.Sprintf("%s（%s）\n%s", repo, strings.Join(gitArgs, " "), truncateBytes(out, gitMaxOutBytes)), nil
}

// GitLogTool 看最近的提交。
type GitLogTool struct {
	FileTools
}

func (t *GitLogTool) Name() string { return "git_log" }

func (t *GitLogTool) Description() string {
	return "查看 git 仓库最近的提交（默认 10 条，最多 50 条），一行一条。" +
		"想知道这个项目最近改了什么、谁在动哪块时用它。"
}

func (t *GitLogTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{"type": "integer", "description": "看多少条，默认 10，最多 50"},
		},
	}
}

func (t *GitLogTool) Call(ctx context.Context, args map[string]any) (string, error) {
	repo, err := t.gitRepoDir(ctx)
	if err != nil {
		return "", err
	}
	limit := toInt(args["limit"], gitDefaultLog)
	if limit <= 0 {
		limit = gitDefaultLog
	}
	if limit > gitMaxLog {
		limit = gitMaxLog
	}
	out, err := runGit(ctx, repo, "log", "--oneline", "-n", fmt.Sprint(limit))
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return fmt.Sprintf("%s：还没有任何提交", repo), nil
	}
	return fmt.Sprintf("%s（最近 %d 条提交）\n%s", repo, limit, truncateBytes(out, gitMaxOutBytes)), nil
}
