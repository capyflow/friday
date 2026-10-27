package acp

// registry_test.go 用 httptest 假装官方注册表与分发服务器，覆盖：wire schema
// 解析（npx/binary/双分发）、条目→目录卡片转换（含平台不支持）、缓存回退
// （断网用过期缓存，无缓存降级为仅内置）、内置覆盖官方、binary 安装的
// 下载+sha256+解包全链路、npx 安装的模板装配。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/config"
)

// registryJSON 是官方 wire 形态的固定样例（字段名与线上 registry.json 一致）。
const registryJSON = `{
  "version": "1.0.0",
  "agents": [
    {
      "id": "opencode", "name": "OpenCode（官方）", "version": "9.9.9",
      "description": "官方目录里的 opencode 条目（应被内置精选覆盖）",
      "distribution": {"npx": {"package": "opencode-ai@9.9.9", "args": ["acp"]}}
    },
    {
      "id": "some-new-agent", "name": "Some New Agent", "version": "0.1.0",
      "description": "上游新上架的 agent",
      "repository": "https://github.com/x/y",
      "distribution": {"npx": {"package": "some-new-agent@0.1.0", "args": ["--acp"],
                               "env": {"SOME_KEY": "v"}}}
    },
    {
      "id": "bin-agent", "name": "Binary Agent", "version": "2.0.0",
      "description": "binary 分发条目",
      "distribution": {"binary": {
        "darwin-aarch64": {"archive": "PLACEHOLDER", "cmd": "./sub/agent-bin", "sha256": "PLACEHOLDER"},
        "linux-x86_64": {"archive": "PLACEHOLDER", "cmd": "./agent-bin"},
        "windows-x86_64": {"archive": "PLACEHOLDER", "cmd": "./sub/agent-bin"}
      }}
    },
    {
      "id": "bin-other-platform", "name": "Other Platform Only", "version": "1.0.0",
      "description": "只有别的平台有分发",
      "distribution": {"binary": {"sunos-sparc": {"archive": "https://x/y.tgz", "cmd": "./a"}}}
    }
  ]
}`

// registryTestEnv 起一个假注册表服务并把全局注入指过去；返回恢复函数。
func registryTestEnv(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	oldURL, oldHTTP, oldBase := registryURLFn, registryHTTP, acpBaseDirFn
	base := t.TempDir()
	registryURLFn = func() string { return srv.URL + "/registry.json" }
	registryHTTP = srv.Client()
	acpBaseDirFn = func() (string, error) { return base, nil }
	t.Cleanup(func() {
		registryURLFn, registryHTTP, acpBaseDirFn = oldURL, oldHTTP, oldBase
		srv.Close()
	})
	return srv
}

func newIsolatedManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(func() config.AcpConfig { return config.AcpConfig{} }, nil, filepath.Join(t.TempDir(), "t.json"))
	t.Cleanup(m.Close)
	return m
}

func TestParseAndConvertRegistry(t *testing.T) {
	registryTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(registryJSON))
	}))
	m := newIsolatedManager(t)

	entries := m.registryIndex(context.Background(), false)
	if len(entries) != 4 {
		t.Fatalf("应解析出 4 个条目，实际 %d", len(entries))
	}
	var npx, bin, unsupported *registryEntry
	for i := range entries {
		switch entries[i].ID {
		case "some-new-agent":
			npx = &entries[i]
		case "bin-agent":
			bin = &entries[i]
		case "bin-other-platform":
			unsupported = &entries[i]
		}
	}

	// npx 条目 → 模板：启动器 npx + [-y 包名+版本, 官方参数...]，env 带入。
	ce, ok := npx.toCatalogEntry(m.registryBase)
	if !ok || ce.Template.Command != "npx" {
		t.Fatalf("npx 条目应转换为 npx 启动器: %+v", ce)
	}
	wantArgs := []string{"-y", "some-new-agent@0.1.0", "--acp"}
	if strings.Join(ce.Template.Args, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("npx 参数不符: %v", ce.Template.Args)
	}
	if ce.Template.Env["SOME_KEY"] != "v" || ce.Template.Version != "0.1.0" {
		t.Fatalf("env/版本未带入: %+v", ce.Template)
	}
	if ce.InstallCmd != "npm install -g some-new-agent@0.1.0" {
		t.Fatalf("安装命令不符: %q", ce.InstallCmd)
	}

	// binary 条目 → 预期安装路径（<base>/agents/<id>/<version>/<cmd>）。
	ce, ok = bin.toCatalogEntry(m.registryBase)
	if !ok {
		t.Fatal("binary 条目在 darwin/linux 上应有分发")
	}
	wantBin := filepath.Join(m.registryBase, "agents", "bin-agent", "2.0.0",
		filepath.FromSlash(strings.TrimPrefix(binCmdForTest(), "./")))
	if ce.Template.Command != wantBin {
		t.Fatalf("binary 预期路径不符:\n got %q\nwant %q", ce.Template.Command, wantBin)
	}

	// 只有其他平台分发的条目 → supported=false（卡片展示但禁安装）。
	ce, ok = unsupported.toCatalogEntry(m.registryBase)
	if ok {
		t.Fatal("无当前平台分发的条目应返回 supported=false")
	}
}

// binCmdForTest 返回当前平台在样例里的 cmd（测试机是 darwin/arm64、linux/amd64
// 或 windows/amd64；windows 样例与 darwin 同用 sub/agent-bin 布局）。
func binCmdForTest() string {
	if platformKey() == "linux-x86_64" {
		return "./agent-bin"
	}
	return "./sub/agent-bin"
}

func TestRegistryCacheFallbackAndBuiltinOverride(t *testing.T) {
	var serveBody []byte
	serve := true
	registryTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serve {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(serveBody)
	}))
	m := newIsolatedManager(t)

	// 第一次拉取写缓存。
	m.registryPath = filepath.Join(t.TempDir(), "not-created", "registry-cache.json")
	serveBody = []byte(registryJSON)
	if got := m.registryIndex(context.Background(), false); len(got) != 4 {
		t.Fatalf("首拉应得 4 条，实际 %d", len(got))
	}
	if _, err := os.Stat(m.registryPath); err != nil {
		t.Fatalf("缓存未落盘: %v", err)
	}

	// 服务挂掉：TTL 内仍走缓存（不打网络也成立，此处直接验证回退路径）。
	serve = false
	m.fetchMu.Lock() // 模拟缓存过期后拉取失败的场景：直接读回退逻辑
	cached := m.readRegistryCache()
	m.fetchMu.Unlock()
	if cached == nil || len(cached.Index.Agents) != 4 {
		t.Fatal("缓存应可读回")
	}

	// 目录合并：内置精选覆盖同 id 官方条目，官方新条目追加；离线时仅内置。
	serveBody = []byte(registryJSON)
	views := m.Catalog(context.Background(), false)
	var byID = map[string]CatalogView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if v, ok := byID["opencode"]; !ok || v.Source != "builtin" || v.InstallCmd != "curl -fsSL https://opencode.ai/install | bash" {
		t.Fatalf("内置条目应覆盖官方同 id 条目: %+v", byID["opencode"])
	}
	if v, ok := byID["some-new-agent"]; !ok || v.Source != "registry" || !v.Supported {
		t.Fatalf("官方新条目应出现在目录里: %+v", byID["some-new-agent"])
	}
	if v := byID["bin-other-platform"]; v.Supported {
		t.Fatal("无当前平台分发的条目应标记不支持")
	}

	// 离线：只返回内置目录，且不报错。
	m2 := newIsolatedManager(t)
	m2.registryOff = true
	if views := m2.Catalog(context.Background(), false); len(views) != len(catalog) {
		t.Fatalf("离线应只有内置 %d 条，实际 %d", len(catalog), len(views))
	}
}

func TestInstallRegistryNpx(t *testing.T) {
	registryTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(registryJSON))
	}))
	m := newIsolatedManager(t)

	var gotCmd string
	oldRun := runInstallCommand
	runInstallCommand = func(_ context.Context, cmd string) ([]byte, error) {
		gotCmd = cmd
		return []byte("added 1 package"), nil
	}
	oldLook := lookPathFn
	lookPathFn = func(string) (string, error) { return "/opt/node/bin/npx", nil }
	t.Cleanup(func() { runInstallCommand = oldRun; lookPathFn = oldLook })

	res, err := m.Install(context.Background(), "some-new-agent")
	if err != nil {
		t.Fatalf("npx 安装失败: %v", err)
	}
	if gotCmd != "npm install -g some-new-agent@0.1.0" {
		t.Fatalf("安装命令不符: %q", gotCmd)
	}
	if res.Template.Command != "/opt/node/bin/npx" {
		t.Fatalf("启动器应锚定绝对路径: %q", res.Template.Command)
	}
	if res.Version != "0.1.0" || res.Path != "/opt/node/bin/npx" {
		t.Fatalf("版本/路径未回填: %+v", res)
	}
}

func TestInstallRegistryBinary(t *testing.T) {
	// 组一个 tar.gz：sub/agent-bin（可执行脚本内容），按平台给出 archive 与 sha256。
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := []byte("#!/bin/sh\necho agent-ok\n")
	rel := "sub/agent-bin"
	if platformKey() == "linux-x86_64" {
		rel = "agent-bin"
	}
	if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	digest := hex.EncodeToString(sum[:])

	// registry JSON 里只留当前平台的 binary 条目，archive 指向本测试服务器。
	// tamper 置位时分发端点返回篡改后的字节（sha256 应当拒绝）。
	indexJSON := `{"version":"1","agents":[{"id":"bin-agent","name":"Binary Agent",
		"version":"2.0.0","description":"d",
		"distribution":{"binary":{"PLATFORM":{"archive":"%ARCHIVE%/bin.tar.gz","cmd":"CMD","sha256":"DIGEST"}}}}]}`
	indexJSON = strings.ReplaceAll(indexJSON, "PLATFORM", platformKey())
	indexJSON = strings.ReplaceAll(indexJSON, "CMD", rel)
	indexJSON = strings.ReplaceAll(indexJSON, "DIGEST", digest)

	tampered := false
	archive := buf.Bytes()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/registry.json") {
			_, _ = w.Write([]byte(strings.ReplaceAll(indexJSON, "%ARCHIVE%", srv.URL)))
			return
		}
		if tampered {
			bad := bytes.Clone(archive)
			bad[len(bad)-1] ^= 0xFF
			_, _ = w.Write(bad)
			return
		}
		_, _ = w.Write(archive)
	}))
	oldURL, oldHTTP, oldBase := registryURLFn, registryHTTP, acpBaseDirFn
	base := t.TempDir()
	registryURLFn = func() string { return srv.URL + "/registry.json" }
	registryHTTP = srv.Client()
	acpBaseDirFn = func() (string, error) { return base, nil }
	t.Cleanup(func() {
		registryURLFn, registryHTTP, acpBaseDirFn = oldURL, oldHTTP, oldBase
		srv.Close()
	})
	m := newIsolatedManager(t)

	res, err := m.Install(context.Background(), "bin-agent")
	if err != nil {
		t.Fatalf("binary 安装失败: %v", err)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil || !bytes.Contains(data, []byte("agent-ok")) {
		t.Fatalf("可执行文件未就位: %v %q", err, res.Path)
	}
	if !strings.Contains(res.Path, filepath.Join("agents", "bin-agent", "2.0.0")) {
		t.Fatalf("应装入版本目录: %q", res.Path)
	}
	if res.Template.Version != "2.0.0" || res.Template.Args != nil {
		t.Fatalf("模板回填不符: %+v", res.Template)
	}

	// 分发被篡改（index 不变）时 sha256 校验必须拒绝。
	tampered = true
	os.RemoveAll(filepath.Join(m.registryBase, "agents"))
	if _, err := m.Install(context.Background(), "bin-agent"); err == nil ||
		!strings.Contains(err.Error(), "sha256") {
		t.Fatalf("校验失败应报 sha256 错误: %v", err)
	}
	// 上游校验值过短也应返回错误，不能 panic。
	entry := registryEntry{ID: "bad-digest", Distribution: registryDistribution{
		Binary: map[string]registryBinaryTarget{platformKey(): {
			Archive: srv.URL + "/bin.tar.gz", Cmd: rel, SHA256: "bad",
		}},
	}}
	if _, _, err := installBinaryDistribution(context.Background(), base, entry); err == nil {
		t.Fatal("非法校验值应拒绝安装")
	}
}

// startRegistryServer 起一个「/registry.json + 分发文件」的假源，并注入全局指
// 向（含恢复）。
func startRegistryServer(t *testing.T, bin []byte, indexJSON string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/registry.json") {
			_, _ = w.Write([]byte(strings.ReplaceAll(indexJSON, "%ARCHIVE%", srv.URL)))
			return
		}
		_, _ = w.Write(bin)
	}))
	oldURL, oldHTTP, oldBase := registryURLFn, registryHTTP, acpBaseDirFn
	base := t.TempDir()
	registryURLFn = func() string { return srv.URL + "/registry.json" }
	registryHTTP = srv.Client()
	acpBaseDirFn = func() (string, error) { return base, nil }
	t.Cleanup(func() {
		registryURLFn, registryHTTP, acpBaseDirFn = oldURL, oldHTTP, oldBase
		srv.Close()
	})
	return srv
}

func TestExtractArchiveRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape.sh", Mode: 0o755, Size: 3})
	_, _ = tw.Write([]byte("x\n"))
	_ = tw.Close()
	_ = gz.Close()

	dir := t.TempDir()
	if _, err := extractArchive("https://x/a.tar.gz", buf.Bytes(), dir, "./escape.sh"); err == nil {
		t.Fatal("cmd 指向包外路径应报错")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escape.sh")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("越界条目不应落盘")
	}
}
