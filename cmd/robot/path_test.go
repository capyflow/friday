package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAugmentPathAppendsMissingUserDirs(t *testing.T) {
	home := t.TempDir()
	nvmBin := filepath.Join(home, ".nvm", "versions", "node", "v20.1.0", "bin")
	if err := os.MkdirAll(nvmBin, 0o755); err != nil {
		t.Fatal(err)
	}
	got := augmentPath("/usr/bin:/bin", home)
	dirs := strings.Split(got, string(os.PathListSeparator))
	if dirs[0] != "/usr/bin" || dirs[1] != "/bin" {
		t.Fatalf("现有 PATH 顺序应保留: %v", dirs)
	}
	for _, want := range []string{
		"/opt/homebrew/bin", filepath.Join(home, ".local/bin"),
		filepath.Join(home, ".opencode/bin"), nvmBin,
	} {
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
	// 幂等：已有目录不重复追加
	if again := augmentPath(got, home); again != got {
		t.Fatalf("重复补全应保持稳定:\n%s\n%s", got, again)
	}
}

func TestAugmentPathHandlesEmptyInput(t *testing.T) {
	got := augmentPath("", "")
	for _, want := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(got, want) {
			t.Fatalf("空 PATH 应补上 %q: %q", want, got)
		}
	}
}
