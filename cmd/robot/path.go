package main

import (
	"os"
	"path/filepath"
	"strings"
)

// augmentPath 补齐 GUI 启动时缺失的用户 PATH：Finder/launchd 拉起应用只有系统
// 目录，Homebrew、nvm、uv、opencode 等常装在用户目录——一条 `npm install -g ...`
// 就会以 exit status 127（command not found）失败。保留现有顺序，只追加缺失目录。
func augmentPath(current, home string) string {
	parts := make([]string, 0, 20)
	seen := map[string]bool{}
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		parts = append(parts, dir)
	}
	if current != "" {
		for _, dir := range strings.Split(current, string(os.PathListSeparator)) {
			add(dir)
		}
	}
	for _, dir := range []string{
		"/opt/homebrew/bin", "/opt/homebrew/sbin",
		"/usr/local/bin", "/usr/local/sbin",
	} {
		add(dir)
	}
	if home != "" {
		for _, rel := range []string{
			".local/bin",    // uv / pipx
			".bun/bin",      // bun
			".cargo/bin",    // cargo / uv 的部分安装
			".opencode/bin", // opencode 官方安装脚本
			"Library/pnpm",  // pnpm
			".volta/bin",    // volta
		} {
			add(filepath.Join(home, rel))
		}
		// nvm 的多版本 node bin（存在才加；glob 顺序即可）
		if matches, err := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin")); err == nil {
			for _, dir := range matches {
				add(dir)
			}
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// ensureUserPath 在桌面模式下把补齐后的 PATH 写回当前进程，供安装命令、
// lookPath 探测与后续 agent 子进程继承。
func ensureUserPath() {
	home, _ := os.UserHomeDir()
	os.Setenv("PATH", augmentPath(os.Getenv("PATH"), home))
}
