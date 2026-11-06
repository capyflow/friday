// npm.go 是 npx 型 agent 的版本真值源（M-4 补强）：官方 ACP registry 的条目
// 常滞后于 npm（实测 minimax-code registry=0.2.7 时 npm 已 0.6.12），且版本
// 比对原是纯字符串不等，手动升过级的用户会被提示"升级"到旧版。这里直接问
// npm registry 要 latest，配合分段数字比较给出有方向的升级判断；缓存与降级
// 纪律与 registry.go 一致（失败回退过期缓存，无缓存返回空——离线时目录页
// 照常可用，只是没有 npm 真值）。
package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/config"
)

// npmLatestURL 返回一个 npm 包的 latest dist-tag 查询地址（scope 包的 /
// 要转义成 %2F）。
func npmLatestURL(pkg string) string {
	return "https://registry.npmjs.org/" + strings.ReplaceAll(pkg, "/", "%2F") + "/latest"
}

// fetchNpmLatest 拉一个包的 latest 版本号。测试经 npmURLFn / npmHTTP 替换。
var npmURLFn = npmLatestURL

var npmHTTP = &http.Client{Timeout: registryFetchTimeout}

type npmLatestResp struct {
	Version string `json:"version"`
}

// fetchNpmLatest 单次拉取（无缓存）。pkg 允许带版本尾巴（registry 的
// Npx.Package 形态）——带版本查 latest 会被 npm 以 HTTP 400 拒掉，这里统一剥掉。
func fetchNpmLatest(ctx context.Context, pkg string) (string, error) {
	if at := strings.LastIndex(pkg, "@"); at > 0 {
		pkg = pkg[:at]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, npmURLFn(pkg), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := npmHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out npmLatestResp
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析 npm 响应失败: %w", err)
	}
	if out.Version == "" {
		return "", fmt.Errorf("npm 响应里没有 version 字段")
	}
	return out.Version, nil
}

// npmCacheFile 是 npm latest 查询的本地缓存（与 registryCacheFile 同形态）。
type npmCacheFile struct {
	FetchedAt time.Time         `json:"fetchedAt"`
	Versions  map[string]string `json:"versions"` // pkg → latest 版本
}

// npmLatest 批量取 npm latest（带缓存，TTL 与官方目录一致）。force=true 强制
// 重查全部包（忽略 TTL 与已有缓存）；force=false 时只有缓存新鲜才复用，过期即
// 重查——原判定 `(!force || fresh)` 在 force=false 时恒真，TTL 被架空：过期
// 缓存照旧返回，且 missing 为空直接早退、一个请求都不发，于是 npm 上已发布的
// 2.180.0 被 11-03 的 2.179.2 一直盖住（2026-11-06 实测）。
// 单包失败静默缺席（下次再查）；全败时不刷新缓存时间戳，否则旧版本号会被
// 「保鲜」一整个 TTL。cachePath 为空（无基目录）直接不查。
func (m *Manager) npmLatest(ctx context.Context, pkgs []string, force bool) map[string]string {
	out := map[string]string{}
	if m.registryBase == "" || len(pkgs) == 0 {
		return out
	}
	m.fetchMu.Lock()
	defer m.fetchMu.Unlock()
	cachePath := filepath.Join(m.registryBase, "npm-cache.json")
	cached := readNpmCache(cachePath)
	fresh := cached != nil && time.Since(cached.FetchedAt) < registryCacheTTL

	var missing []string
	for _, p := range pkgs {
		if cached != nil {
			if v, ok := cached.Versions[p]; ok && !force && fresh {
				out[p] = v // 缓存新鲜且未要求强制刷新：直接用，不再发请求
				continue
			}
		}
		missing = append(missing, p)
	}
	if len(missing) == 0 {
		return out
	}

	// 并发拉取：npm registry 每包一次请求，串行几十个包挤在一个 10s 预算里
	// 必然整体超时（2026-11-03 实测 20 个包全数失败、缓存都没写上）。每包
	// 独立超时，worker 池限流；失败的包静默缺席（下次再查），有成功才汇总记日志。
	updated := map[string]string{}
	if cached != nil {
		for k, v := range cached.Versions {
			updated[k] = v
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	failed := 0
	okCount := 0
	var firstErr error
	for _, p := range missing {
		wg.Add(1)
		go func(pkg string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ictx, cancel := context.WithTimeout(ctx, registryFetchTimeout)
			defer cancel()
			v, err := fetchNpmLatest(ictx, pkg)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			updated[pkg] = v
			out[pkg] = v
			okCount++
		}(p)
	}
	wg.Wait()
	if failed > 0 {
		log.Printf("[智能体] npm latest 查询 %d/%d 个包失败（示例: %v；缺席的包下次再查）", failed, len(missing), firstErr)
	}
	// 只有真查成了才刷新缓存时间戳：全败时若照旧写入，FetchedAt 被推到当前，
	// 旧版本号就又「新鲜」了一整个 TTL（11-03 那次 HTTP 400 全败正是这样把
	// 2.179.2 冻住的）。
	if okCount > 0 && len(updated) > 0 {
		writeNpmCache(cachePath, updated)
	}
	return out
}

func readNpmCache(path string) *npmCacheFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f npmCacheFile
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	return &f
}

func writeNpmCache(path string, versions map[string]string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(npmCacheFile{FetchedAt: time.Now(), Versions: versions})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// compareVersions 分段比较两个版本号：按 "." 切分逐段数字比较，段数不齐右侧
// 补 0（1.0 == 1.0.0）；非数字段退化为字典序。返回 -1/0/1。不引第三方 semver
// 库——agent 版本戳都是简单的点分形态，还出现过 3000.11.228 这种大数。
func compareVersions(a, b string) int {
	as, bs := strings.Split(strings.TrimSpace(a), "."), strings.Split(strings.TrimSpace(b), ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := "0", "0"
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		an, aerr := strconv.Atoi(av)
		bn, berr := strconv.Atoi(bv)
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		default: // 任意一侧非数字段：字典序兜底
			if av != bv {
				if av < bv {
					return -1
				}
				return 1
			}
		}
	}
	return 0
}

// npxPackageToken 在启动参数里找 <包名>@<版本> 形态的 token（npx 型 agent 的
// 版本就锚在这里）：从右往左找第一个含 "@" 且 "@" 不在首位的参数——scoped
// 包（@scope/name@1.2.3）的最后一个 @ 之前是包名。返回包名与下标；没找到
// 返回 ("", -1)（如 codex 模板的无版本参数、-y/-p/子命令都不含 @）。
func npxPackageToken(args []string) (pkg string, idx int) {
	for i := len(args) - 1; i >= 0; i-- {
		at := strings.LastIndex(args[i], "@")
		if at > 0 {
			return args[i][:at], i
		}
	}
	return "", -1
}

// upgradeNpx 对已注册的 npx 型 agent 做原位版本升级：Args 里的 <包名>@<版本>
// token 换成 npm latest，其余参数原样保留（-y/-p/子命令/适配器 bin 名等——
// 多 bin 包必需的自定义形态不能被目录模板覆盖）。不跑 npm 安装：npx 按需拉包，
// 下次启动自动用新版。返回改好的 agent 配置与结果，调用方负责写盘并 Reload。
func (m *Manager) upgradeNpx(ctx context.Context, ag config.AcpAgentConfig) (InstallResult, error) {
	res := InstallResult{AlreadyRegistered: true, Template: ag}
	pkg, idx := npxPackageToken(ag.Args)
	if idx < 0 {
		return res, fmt.Errorf("agent %q 的启动参数里没有 <包名>@<版本> 形态，无法原位升级", ag.Name)
	}
	old := strings.TrimPrefix(ag.Args[idx], pkg+"@")
	latest, err := fetchNpmLatest(ctx, pkg)
	if err != nil {
		return res, fmt.Errorf("查询 npm latest 失败（%s）: %w", pkg, err)
	}
	if compareVersions(latest, old) <= 0 {
		return res, fmt.Errorf("已是最新：npm latest=%s，当前登记=%s", latest, old)
	}
	args := append([]string(nil), ag.Args...)
	args[idx] = pkg + "@" + latest
	res.Template.Args = args
	res.Template.Version = latest
	res.Version = latest
	res.Output = fmt.Sprintf("npx 原位升级：%s@%s → %s（下次启动生效，npx 将按需拉取新包）", pkg, old, latest)
	return res, nil
}
