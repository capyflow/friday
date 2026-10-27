//go:build windows

package procctl

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

// ShellCommandContext 经平台 shell 执行一条命令行（固定可信字符串，见包注释）：
// 优先 Git Bash——官方目录的安装命令多为 curl|bash 类 POSIX 语法，PowerShell
// 跑不了；找不到 Git Bash 退 PowerShell，npm install -g 这类仍可用。
// shell 判定参考 Zed crates/util/src/shell.rs（Windows 未知程序按 PowerShell）。
func ShellCommandContext(ctx context.Context, line string) *exec.Cmd {
	if bash := findGitBash(); bash != "" {
		return exec.CommandContext(ctx, bash, "-c", line)
	}
	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", line)
}

// findGitBash 定位 Git for Windows 自带的 bash.exe：GIT_INSTALL_ROOT 优先
// （Zed 同款约定），否则顺 PATH 里的 git.exe 找安装根下的 bin\bash.exe。
// 常见布局：…\Git\cmd\git.exe 与 …\Git\mingw64\bin\git.exe，bash 都在
// 安装根的 bin\ 下。
func findGitBash() string {
	if root := os.Getenv("GIT_INSTALL_ROOT"); root != "" {
		if p := filepath.Join(root, "bin", "bash.exe"); fileExists(p) {
			return p
		}
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	dir := filepath.Dir(git)
	for _, rel := range []string{
		filepath.Join(dir, "..", "bin"),       // cmd/git.exe
		filepath.Join(dir, "..", "..", "bin"), // mingw64/bin/git.exe
		dir,                                   // bin/git.exe（罕见）
	} {
		if p := filepath.Join(rel, "bash.exe"); fileExists(p) {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
