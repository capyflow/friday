// npm_test.go 覆盖 npm 真值查询与版本比较：compareVersions 的分段数字语义、
// npmLatest 的缓存/降级纪律、upgradeNpx 的原位 token 替换（含多 bin 包的
// -p … mcode 形态）与"不降级"拒绝。
package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/config"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.7", "0.6.12", -1},
		{"0.6.12", "0.2.7", 1},
		{"0.6.12", "0.6.12", 0},
		{"1.0", "1.0.0", 0},             // 段数不齐右侧补 0
		{"3000.11.228", "3000.10.1", 1}, // 大数字段按数值比
		{"2", "10", -1},                 // 数字比较不是字典序
		{"0.6.12-beta", "0.6.12", 1},    // 非数字段退化为字典序
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// newNpmTestSrv 起一个假 npm registry（GET /<pkg> → {"version": v}），
// 返回 server 与请求计数；fail 置真模拟 npm 不可用。
func newNpmTestSrv(t *testing.T, versions map[string]string, fail *atomic.Bool) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		pkg := strings.TrimPrefix(r.URL.Path, "/")
		v, ok := versions[pkg]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": v})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// withNpmSrv 把 npm 真值源指向假 server，测试结束还原。
func withNpmSrv(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldURL, oldHTTP := npmURLFn, npmHTTP
	npmURLFn = func(pkg string) string { return srv.URL + "/" + pkg }
	npmHTTP = srv.Client()
	t.Cleanup(func() { npmURLFn, npmHTTP = oldURL, oldHTTP })
}

func TestNpmLatestCacheAndFallback(t *testing.T) {
	var fail atomic.Bool
	srv, hits := newNpmTestSrv(t, map[string]string{"@minimax-ai/code": "0.6.12"}, &fail)
	withNpmSrv(t, srv)
	m := newTestManager(t, nil, nil, nil)
	m.registryBase = t.TempDir()
	ctx := context.Background()

	// 首查：走网络，写缓存。
	got := m.npmLatest(ctx, []string{"@minimax-ai/code"}, false)
	if got["@minimax-ai/code"] != "0.6.12" {
		t.Fatalf("首查结果 = %v, want 0.6.12", got)
	}
	// 二查：缓存命中，不再发请求。
	before := atomic.LoadInt64(hits)
	got = m.npmLatest(ctx, []string{"@minimax-ai/code"}, false)
	if got["@minimax-ai/code"] != "0.6.12" || atomic.LoadInt64(hits) != before {
		t.Fatalf("二查应命中缓存：got=%v hits %d→%d", got, before, atomic.LoadInt64(hits))
	}
	// npm 挂掉 + 无缓存的新包：静默缺席，不报错。
	fail.Store(true)
	got = m.npmLatest(ctx, []string{"@other/pkg"}, false)
	if v, ok := got["@other/pkg"]; ok {
		t.Fatalf("npm 挂掉时应缺席，got %q", v)
	}
}

// TestNpmLatestStaleCacheRefetched 守护 npm 缓存的 TTL 语义：缓存过期后必须
// 重新查 npm，而不是把过期值当命中继续用。
//
// 原判定 (!force || fresh) 在 force=false 时恒真，TTL 被架空：过期缓存照旧
// 返回，且 missing 为空直接早退、一个请求都不发。后果是 npm 上已发布的
// 2.180.0 被 11-03 缓存的 2.179.2 一直盖住（2026-11-06 实测）。
func TestNpmLatestStaleCacheRefetched(t *testing.T) {
	var fail atomic.Bool
	srv, hits := newNpmTestSrv(t, map[string]string{"@tencent-ai/codebuddy-code": "2.180.0"}, &fail)
	withNpmSrv(t, srv)
	m := newTestManager(t, nil, nil, nil)
	base := t.TempDir()
	m.registryBase = base
	ctx := context.Background()

	// 预置一份「过期且版本陈旧」的缓存（模拟 11-03 抓到的 2.179.2）。
	old := map[string]string{"@tencent-ai/codebuddy-code": "2.179.2"}
	writeNpmCache(filepath.Join(base, "npm-cache.json"), old)
	// 把时间戳改旧，强制越过 TTL。
	f, err := os.OpenFile(filepath.Join(base, "npm-cache.json"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("打开缓存失败: %v", err)
	}
	raw, _ := json.Marshal(npmCacheFile{
		FetchedAt: time.Now().Add(-48 * time.Hour),
		Versions:  old,
	})
	_ = f.Truncate(0)
	_, _ = f.WriteAt(raw, 0)
	_ = f.Close()

	// 过期缓存 + force=false：必须重查并拿到新版本。
	got := m.npmLatest(ctx, []string{"@tencent-ai/codebuddy-code"}, false)
	if got["@tencent-ai/codebuddy-code"] != "2.180.0" {
		t.Fatalf("过期缓存应重查，got %v（want 2.180.0）", got)
	}
	if atomic.LoadInt64(hits) == 0 {
		t.Fatal("过期缓存后应发出请求")
	}
}

// TestNpmLatestAllFailKeepsStaleTimestamp 守护「全败不刷新缓存时间戳」：
// 拉取全失败时若照旧写缓存，FetchedAt 被推到当前，旧版本号就又「新鲜」了
// 一整个 TTL，下次仍被当成命中——11-03 那次 HTTP 400 全败正是这样把 2.179.2
// 冻住的。
func TestNpmLatestAllFailKeepsStaleTimestamp(t *testing.T) {
	var fail atomic.Bool
	srv, _ := newNpmTestSrv(t, map[string]string{"@tencent-ai/codebuddy-code": "2.180.0"}, &fail)
	withNpmSrv(t, srv)
	m := newTestManager(t, nil, nil, nil)
	base := t.TempDir()
	m.registryBase = base
	ctx := context.Background()

	// 先成功查一次，把缓存写出来。
	if got := m.npmLatest(ctx, []string{"@tencent-ai/codebuddy-code"}, false); got["@tencent-ai/codebuddy-code"] != "2.180.0" {
		t.Fatalf("首查结果 = %v", got)
	}
	first := readNpmCache(filepath.Join(base, "npm-cache.json"))
	if first == nil {
		t.Fatal("首查后应有缓存")
	}

	// 让缓存过期，再让 npm 全败。
	f, err := os.OpenFile(filepath.Join(base, "npm-cache.json"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("打开缓存失败: %v", err)
	}
	raw, _ := json.Marshal(npmCacheFile{
		FetchedAt: time.Now().Add(-48 * time.Hour),
		Versions:  first.Versions,
	})
	_ = f.Truncate(0)
	_, _ = f.WriteAt(raw, 0)
	_ = f.Close()
	fail.Store(true)

	m.npmLatest(ctx, []string{"@tencent-ai/codebuddy-code"}, false)
	after := readNpmCache(filepath.Join(base, "npm-cache.json"))
	if after == nil {
		t.Fatal("全败后缓存不应消失")
	}
	if time.Since(after.FetchedAt) < 24*time.Hour {
		t.Fatalf("全败时缓存时间戳被刷新了（FetchedAt=%v），旧版本号会被保鲜一整个 TTL", after.FetchedAt)
	}
}
func TestUpgradeNpxTokenSwap(t *testing.T) {
	var fail atomic.Bool
	srv, _ := newNpmTestSrv(t, map[string]string{"@minimax-ai/code": "0.6.12"}, &fail)
	withNpmSrv(t, srv)
	m := newTestManager(t, nil, nil, nil)
	m.registryBase = t.TempDir()

	// 形态一：经典 -y pkg@ver acp
	ag := config.AcpAgentConfig{Name: "minimax-code", Command: "/opt/homebrew/bin/npx",
		Args: []string{"-y", "@minimax-ai/code@0.2.7", "acp"}, Version: "0.2.7"}
	res, err := m.upgradeNpx(context.Background(), ag)
	if err != nil {
		t.Fatalf("upgradeNpx: %v", err)
	}
	want := []string{"-y", "@minimax-ai/code@0.6.12", "acp"}
	if !stringSlicesEqual(res.Template.Args, want) || res.Template.Version != "0.6.12" {
		t.Fatalf("形态一升级结果 = %v v%s, want %v v0.6.12", res.Template.Args, res.Template.Version, want)
	}
	if !res.AlreadyRegistered || res.Version != "0.6.12" {
		t.Fatalf("res 标志不对：AlreadyRegistered=%v Version=%s", res.AlreadyRegistered, res.Version)
	}

	// 形态二：多 bin 包 -y -p pkg@ver mcode acp（minimax 0.6.x 必需形态）
	ag2 := config.AcpAgentConfig{Name: "minimax-code", Command: "/opt/homebrew/bin/npx",
		Args: []string{"-y", "-p", "@minimax-ai/code@0.2.7", "mcode", "acp"}, Version: "0.6.12"}
	res2, err := m.upgradeNpx(context.Background(), ag2)
	if err != nil {
		t.Fatalf("upgradeNpx 形态二: %v", err)
	}
	want2 := []string{"-y", "-p", "@minimax-ai/code@0.6.12", "mcode", "acp"}
	if !stringSlicesEqual(res2.Template.Args, want2) {
		t.Fatalf("形态二升级结果 = %v, want %v", res2.Template.Args, want2)
	}

	// 形态三：无版本 token（codex 模板）→ 报错而不是乱改
	ag3 := config.AcpAgentConfig{Name: "codex", Command: "npx",
		Args: []string{"-y", "@agentclientprotocol/codex-acp"}}
	if _, err := m.upgradeNpx(context.Background(), ag3); err == nil {
		t.Fatal("无版本 token 应报错")
	}

	// 形态四：npm latest 不比当前新 → 拒绝（不降级）
	ag4 := config.AcpAgentConfig{Name: "minimax-code", Command: "npx",
		Args: []string{"-y", "@minimax-ai/code@0.7.0", "acp"}, Version: "0.7.0"}
	if _, err := m.upgradeNpx(context.Background(), ag4); err == nil {
		t.Fatal("latest 0.6.12 < 登记 0.7.0 应拒绝降级")
	}
}

func TestNpxPackageToken(t *testing.T) {
	cases := []struct {
		args []string
		pkg  string
		idx  int
	}{
		{[]string{"-y", "@minimax-ai/code@0.2.7", "acp"}, "@minimax-ai/code", 1},
		{[]string{"-y", "-p", "@minimax-ai/code@0.2.7", "mcode", "acp"}, "@minimax-ai/code", 2},
		{[]string{"-y", "@agentclientprotocol/codex-acp"}, "", -1}, // 无版本
		{[]string{"acp"}, "", -1},
	}
	for _, c := range cases {
		pkg, idx := npxPackageToken(c.args)
		if pkg != c.pkg || idx != c.idx {
			t.Errorf("npxPackageToken(%v) = (%q, %d), want (%q, %d)", c.args, pkg, idx, c.pkg, c.idx)
		}
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFetchNpmLatestStripsVersion(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = strings.TrimPrefix(r.URL.Path, "/")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.1.0"})
	}))
	t.Cleanup(srv.Close)
	withNpmSrv(t, srv)
	v, err := fetchNpmLatest(context.Background(), "@minimax-ai/code@9.9.9")
	if err != nil || v != "0.1.0" {
		t.Fatalf("fetchNpmLatest = %q, %v; want 0.1.0", v, err)
	}
	if gotPath != "@minimax-ai/code" {
		t.Fatalf("请求路径 = %q, want 纯包名 @minimax-ai/code", gotPath)
	}
}
