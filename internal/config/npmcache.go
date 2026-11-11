// npmcache.go 是包管理器拉起命令（npx 等）的公共判定与 npm 缓存收编。
// acp（agent 子进程）与 mcp（MCP 服务器子进程）两处 spawn 共用。
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// NpmCacheEnvKey 注入给 npm 系子进程的环境变量名。npm 与 npx 的包缓存
// （tarball 的 _cacache 与物化目录 _npx）都落在该变量指定的根下。
const NpmCacheEnvKey = "npm_config_cache"

// packageLauncherNames 是"现拉包"型启动命令的 basename 表。同一张表两个用途：
// ① 握手按冷启动预算给足（AcpColdStartHandshakeSeconds；261030 codex 走 npx
// 冷启动握手超时）；② spawn 时注入 NpmCacheEnvKey 把包缓存收进数据目录
// npm-cache/——261110 用户清 ~/.npm/_npx 打瘫全部 npx 型 agent 并引发 MCP
// 重启风暴的事故，缓存隔离进数据目录后清 ~/.npm 不再影响 Friday。
var packageLauncherNames = map[string]bool{
	"npx": true, "pnpm": true, "pnpm-dlx": true, "dlx": true, "bunx": true,
	"yarn": true, "deno": true, "uvx": true, "pipx": true, "npm": true,
}

// IsPackageLauncher 判断启动命令是不是包管理器拉起器（只看 basename，绝对路径也算）。
// npm/pnpm/yarn/deno 只在 run/dlx 现拉包时慢，但多给一次预算/多注入一个变量无害。
func IsPackageLauncher(command string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	if base == "pnpm" || base == "npm" || base == "yarn" || base == "deno" {
		return true
	}
	return packageLauncherNames[base]
}

// npmCacheDirFn 可测试替换。内核启动已 chdir 到数据目录（cmd/robot/main.go），
// 这里只写相对目录名；返回值必须绝对路径——npm_config_cache 里的相对路径是
// 按子进程 cwd 解析的，ACP 子进程 cwd 是工作区，相对值会把缓存撒进用户项目。
var npmCacheDirFn = func() (string, error) { return filepath.Abs("npm-cache") }

// NpmCacheDir 返回数据目录下 npm-cache/ 的绝对路径，目录不存在则创建。
// 失败时调用方跳过注入即可（退回系统默认 ~/.npm），不要让缓存目录问题挡住 spawn。
func NpmCacheDir() (string, error) {
	dir, err := npmCacheDirFn()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// MergeEnv 在继承环境上叠加覆盖：同名 key 原地替换，结果无重复 key。
// 直接 append 会出现重复——libc 的 getenv 返回第一个匹配，覆盖对 node 等
// 非 Go 子进程静默失效，故必须合并。
func MergeEnv(inherit []string, extra map[string]string) []string {
	env := make([]string, 0, len(inherit)+len(extra))
	at := make(map[string]int, len(inherit))
	for _, kv := range inherit {
		if i := strings.IndexByte(kv, '='); i > 0 {
			if _, dup := at[kv[:i]]; dup {
				continue // 继承环境自身有重复时保留首条，语义与 libc getenv 一致
			}
			at[kv[:i]] = len(env)
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		kv := k + "=" + v
		if i, dup := at[k]; dup {
			env[i] = kv
		} else {
			env = append(env, kv)
			at[k] = len(env) - 1
		}
	}
	return env
}
