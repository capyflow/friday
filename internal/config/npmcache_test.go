package config

// npmcache_test.go 覆盖包管理器拉起命令判定、npm 缓存目录解析与 MergeEnv 合并
// 语义（同名 key 覆盖生效、结果无重复——直接 append 会让 libc 子进程读到旧值）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envKeys(env []string) map[string][]string {
	out := map[string][]string{}
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			out[kv] = append(out[kv], "")
			continue
		}
		out[kv[:i]] = append(out[kv[:i]], kv[i+1:])
	}
	return out
}

func TestIsPackageLauncher(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"npx", true},
		{"/opt/homebrew/bin/npx", true}, // config.yaml 里的实际写法
		{"uvx", true},
		{"bunx", true},
		{"npm", true},
		{"NPM ", true}, // 空白与大小写都归一
		{"node", false},
		{"/usr/bin/python3", false},
		{"codebuddy-code", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsPackageLauncher(c.cmd); got != c.want {
			t.Errorf("IsPackageLauncher(%q) = %v，期望 %v", c.cmd, got, c.want)
		}
	}
}

func TestNpmCacheDir(t *testing.T) {
	want := filepath.Join(t.TempDir(), "npm-cache")
	orig := npmCacheDirFn
	npmCacheDirFn = func() (string, error) { return want, nil }
	defer func() { npmCacheDirFn = orig }()

	got, err := NpmCacheDir()
	if err != nil {
		t.Fatalf("NpmCacheDir: %v", err)
	}
	if got != want {
		t.Fatalf("NpmCacheDir = %q，期望注入原样返回 %q", got, want)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("目录应已创建: %v", err)
	}
}

func TestMergeEnvOverride(t *testing.T) {
	inherit := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/user",
		"LANG=zh_CN.UTF-8",
		"EXISTING=old",
	}
	got := MergeEnv(inherit, map[string]string{
		"HOME":     "/tmp/agent-home",
		"EXISTING": "new",
		"FRESH":    "added",
	})

	keys := envKeys(got)
	for name, want := range map[string]string{
		"PATH":     "/usr/bin:/bin", // 未覆盖的 key 原样保留
		"HOME":     "/tmp/agent-home",
		"EXISTING": "new",
		"FRESH":    "added",
		"LANG":     "zh_CN.UTF-8",
	} {
		if len(keys[name]) != 1 {
			t.Fatalf("%s 应恰好出现一次，实际 %d 次: %v", name, len(keys[name]), keys[name])
		}
		if keys[name][0] != want {
			t.Fatalf("%s = %q，期望 %q", name, keys[name][0], want)
		}
	}
}

func TestMergeEnvDuplicateInheritKeepsFirst(t *testing.T) {
	// 继承环境自身有重复时保留首条（与 libc getenv 同语义），配置覆盖仍生效。
	inherit := []string{"FOO=first", "FOO=second", "BAR=x"}
	got := MergeEnv(inherit, map[string]string{"BAR": "y"})
	keys := envKeys(got)
	if len(keys["FOO"]) != 1 || keys["FOO"][0] != "first" {
		t.Fatalf("继承重复应保留首条，实际 %v", keys["FOO"])
	}
	if len(keys["BAR"]) != 1 || keys["BAR"][0] != "y" {
		t.Fatalf("配置覆盖应生效，实际 %v", keys["BAR"])
	}
}

func TestMergeEnvCacheInjectionOrder(t *testing.T) {
	// 注入层的优先级契约（proc.go / client.go 共用）：全局环境里的旧
	// npm_config_cache 被注入覆盖，而 agent/MCP 配置里显式给的值又覆盖注入。
	inherit := []string{"npm_config_cache=/home/user/.npm", "PATH=/bin"}
	dir := "/data/Friday/npm-cache"

	extra := map[string]string{NpmCacheEnvKey: dir} // 只有注入
	keys := envKeys(MergeEnv(inherit, extra))
	if len(keys[NpmCacheEnvKey]) != 1 || keys[NpmCacheEnvKey][0] != dir {
		t.Fatalf("注入应覆盖全局旧值且无重复，实际 %v", keys[NpmCacheEnvKey])
	}

	// 配置显式给出 → 覆盖注入（逃生通道：用户可指回任意目录）。
	extra[NpmCacheEnvKey] = "/custom/cache"
	extra["OTHER"] = "x"
	keys = envKeys(MergeEnv(inherit, extra))
	if keys[NpmCacheEnvKey][0] != "/custom/cache" {
		t.Fatalf("配置显式值应覆盖注入，实际 %v", keys[NpmCacheEnvKey])
	}
}
