package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAugmentPathAppendsMissingUserDirs(t *testing.T) {
	home := t.TempDir()
	var wants []string
	if runtime.GOOS == "windows" {
		// Windows 补 %APPDATA%\npm（APPDATA 缺失时按 ~\AppData\Roaming 推）、
		// scoop shims 与 pnpm；不含 Unix 专属目录。
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			appdata = filepath.Join(home, "AppData", "Roaming")
		}
		wants = []string{
			filepath.Join(appdata, "npm"),
			filepath.Join(home, "scoop", "shims"),
			filepath.Join(home, "AppData", "Local", "pnpm"),
			filepath.Join(home, ".local", "bin"), // 跨平台家目录条目照补
		}
	} else {
		nvmBin := filepath.Join(home, ".nvm", "versions", "node", "v20.1.0", "bin")
		if err := os.MkdirAll(nvmBin, 0o755); err != nil {
			t.Fatal(err)
		}
		wants = []string{
			"/opt/homebrew/bin", filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".opencode", "bin"), nvmBin,
		}
	}
	sep := string(os.PathListSeparator)
	got := augmentPath(strings.Join([]string{"/usr/bin", "/bin"}, sep), home)
	dirs := strings.Split(got, sep)
	if dirs[0] != "/usr/bin" || dirs[1] != "/bin" {
		t.Fatalf("现有 PATH 顺序应保留: %v", dirs)
	}
	for _, want := range wants {
		found := false
		for _, dir := range dirs {
			if dir == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("补全后的 PATH 缺少 %q: %v", want, dirs)
		}
	}
	if runtime.GOOS == "windows" {
		for _, dir := range dirs {
			if strings.HasPrefix(dir, "/opt/homebrew") {
				t.Fatalf("Windows 不应补 Unix 专属目录: %v", dirs)
			}
		}
	}
	// 幂等：已有目录不重复追加
	if again := augmentPath(got, home); again != got {
		t.Fatalf("重复补全应保持稳定:\n%s\n%s", got, again)
	}
}

func TestAugmentPathHandlesEmptyInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 无固定系统目录可补，家目录为空时不追加任何条目。
		if got := augmentPath("", ""); got != "" {
			t.Fatalf("空 PATH 且无 HOME 应原样返回: %q", got)
		}
		home := t.TempDir()
		got := augmentPath("", home)
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			appdata = filepath.Join(home, "AppData", "Roaming")
		}
		if !strings.Contains(got, filepath.Join(appdata, "npm")) ||
			!strings.Contains(got, filepath.Join(home, "scoop", "shims")) {
			t.Fatalf("空 PATH 应补上 npm/scoop 目录: %q", got)
		}
		return
	}
	got := augmentPath("", "")
	for _, want := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(got, want) {
			t.Fatalf("空 PATH 应补上 %q: %q", want, got)
		}
	}
}
