package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// augmentPath 补齐 GUI 启动时缺失的用户 PATH：macOS Finder/launchd 拉起应用
// 只有系统目录，Homebrew、nvm、uv、opencode 等常装在用户目录——一条
// `npm install -g ...` 就会以 exit status 127（command not found）失败。
// Windows 侧同源问题：npm 全局目录（%APPDATA%\npm）、scoop/volta/pnpm 的
// 用户级 shims 不一定在系统 PATH 里。保留现有顺序，只追加缺失目录。
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
	if runtime.GOOS != "windows" {
		for _, dir := range []string{
			"/opt/homebrew/bin", "/opt/homebrew/sbin",
			"/usr/local/bin", "/usr/local/sbin",
		} {
			add(dir)
		}
	}
	if home != "" {
		for _, rel := range []string{
			".local/bin",    // uv / pipx
			".bun/bin",      // bun（Windows 亦装到 ~\.bun\bin）
			".cargo/bin",    // cargo / uv 的部分安装
			".opencode/bin", // opencode 官方安装脚本
			"Library/pnpm",  // pnpm（macOS）
			".volta/bin",    // volta（Windows/Linux 布局相同）
		} {
			add(filepath.Join(home, rel))
		}
		if runtime.GOOS == "windows" {
			// npm 全局 bin 默认在 %APPDATA%\npm；环境变量缺失时按布局推。
			// scoop 默认单用户装在 ~\scoop\shims，pnpm Windows 独立路径一并补。
			appdata := os.Getenv("APPDATA")
			if appdata == "" {
				appdata = filepath.Join(home, "AppData", "Roaming")
			}
			add(filepath.Join(appdata, "npm"))
			add(filepath.Join(home, "scoop", "shims"))
			add(filepath.Join(home, "AppData", "Local", "pnpm"))
		} else if matches, err := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin")); err == nil {
			// nvm 的多版本 node bin（存在才加；glob 顺序即可）
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
