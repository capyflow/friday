package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/capyflow/friday/internal/config"
)

func TestCatalogEntriesValid(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range catalog {
		if e.ID == "" || e.Check == "" || e.InstallCmd == "" {
			t.Fatalf("目录条目字段不全: %+v", e)
		}
		if seen[e.ID] {
			t.Fatalf("目录 ID 重复: %s", e.ID)
		}
		seen[e.ID] = true
		if e.Template.Name == "" || e.Template.Command == "" {
			t.Fatalf("条目 %s 的启动模板不全", e.ID)
		}
	}
	if len(catalog) < 3 {
		t.Fatalf("目录至少应有 3 个条目，实际 %d", len(catalog))
	}
}

// TestCatalogLatestVersionMatchesUpdateAvailable 守护「升级徽标的两个版本号同源」。
//
// 场景：官方注册表说 2.180.0（已装 2.179.3），npm 缓存里还是旧的 2.179.2。
// 后端算出「可升级到 2.180.0」，但若 LatestVersion 仍报 npm 的旧值 2.179.2，
// 前端就会显示「可升级（v2.179.3 → v2.179.2）」——高版本向低版本升级
// （2026-11-06 实测）。所以 LatestVersion 必须取与 UpdateAvailable 同一个 avail。
func TestCatalogLatestVersionMatchesUpdateAvailable(t *testing.T) {
	// 官方注册表：codebuddy-code 最新 2.180.0
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": "1",
			"agents": []map[string]any{{
				"id":      "codebuddy-code",
				"name":    "Codebuddy Code",
				"version": "2.180.0",
				"distribution": map[string]any{
					"npx": map[string]any{
						"package": "@tencent-ai/codebuddy-code@2.180.0",
						"args":    []string{"--acp"},
					},
				},
			}},
		})
	}))
	defer regSrv.Close()
	oldURL, oldHTTP := registryURLFn, registryHTTP
	registryURLFn = func() string { return regSrv.URL }
	registryHTTP = regSrv.Client()
	defer func() { registryURLFn, registryHTTP = oldURL, oldHTTP }()

	// npm 真值：故意给旧的 2.179.2（模拟 npm 缓存滞后于注册表）
	var fail atomic.Bool
	npmSrv, _ := newNpmTestSrv(t, map[string]string{"@tencent-ai/codebuddy-code": "2.179.2"}, &fail)
	withNpmSrv(t, npmSrv)

	cfg := testConfig(t)
	cfg.Agents = []config.AcpAgentConfig{{
		Name:    "codebuddy-code",
		Command: "npx",
		Args:    []string{"-y", "@tencent-ai/codebuddy-code@2.179.3", "--acp"},
		Version: "2.179.3",
	}}
	m := newTestManager(t, &cfg, nil, nil)
	m.registryBase = t.TempDir()

	var found *CatalogView
	for i := range m.Catalog(context.Background(), true) {
		v := m.Catalog(context.Background(), true)[i]
		if v.ID == "codebuddy-code" {
			found = &v
			break
		}
	}
	if found == nil {
		t.Fatal("目录里应有 codebuddy-code 条目")
	}
	if !found.UpdateAvailable {
		t.Fatalf("2.180.0 相对已装 2.179.3 应判为可升级，got LatestVersion=%q", found.LatestVersion)
	}
	// 关键断言：LatestVersion 必须是最终 avail（2.180.0），不能是 npm 的旧值。
	if found.LatestVersion != "2.180.0" {
		t.Fatalf("LatestVersion = %q，want 2.180.0（npm 旧值不得成为升级目标）", found.LatestVersion)
	}
}
func TestCatalogViewAndInstallUnknown(t *testing.T) {
	m := NewManager(func() config.AcpConfig { return config.AcpConfig{} }, nil, t.TempDir()+"/t.json")
	defer m.Close()
	m.registryOff = true // 离线：只验证内置目录
	views := m.Catalog(context.Background(), false)
	if len(views) != len(catalog) {
		t.Fatal("目录视图数量不符")
	}
	if _, err := m.Install(context.Background(), "no-such-agent"); err == nil {
		t.Fatal("未知条目应报错")
	} else if !strings.Contains(err.Error(), "目录中没有") {
		t.Fatalf("错误信息不符: %v", err)
	}
}

func TestZcodeEntryIsDirectAdapter(t *testing.T) {
	var e *CatalogEntry
	for i := range catalog {
		if catalog[i].ID == "zcode" {
			e = &catalog[i]
		}
	}
	if e == nil {
		t.Fatal("目录里没有 zcode 条目")
	}
	// zcode 无原生 ACP，接入面就是适配器本身：模板命令必须与 Check 一致，
	// 这样 templateWithPath 才会回填绝对路径（不能是 npx 启动器形态）。
	if e.Template.Command != e.Check {
		t.Fatalf("zcode 模板命令 %q 应与 Check %q 一致", e.Template.Command, e.Check)
	}
	if got := templateWithPath(e.Template, e.Check, "/usr/local/bin/zcode-acp-server"); got.Command != "/usr/local/bin/zcode-acp-server" {
		t.Fatalf("zcode 模板应回填绝对路径，实际 %q", got.Command)
	}
}

func TestTemplateWithPath(t *testing.T) {
	orig := lookPathFn
	t.Cleanup(func() { lookPathFn = orig })

	// CLI 形态：模板命令就是被探测的二进制，回填探测路径。
	cli := config.AcpAgentConfig{Name: "opencode", Command: "opencode", Args: []string{"acp"}}
	if got := templateWithPath(cli, "opencode", "/usr/local/bin/opencode"); got.Command != "/usr/local/bin/opencode" {
		t.Fatalf("CLI 模板应回填绝对路径，实际 %q", got.Command)
	}

	adapter := config.AcpAgentConfig{Name: "codex", Command: "npx", Args: []string{"-y", "@agentclientprotocol/codex-acp"}}
	// 启动器形态：绝不替换成被探测 CLI 的路径（参数会被当成 CLI 子命令），
	// 但要把启动器本身锚定成绝对路径（daemon 运行时 PATH 可能与安装时不同）。
	lookPathFn = func(string) (string, error) { return "/opt/node/bin/npx", nil }
	if got := templateWithPath(adapter, "codex", "/usr/local/bin/codex"); got.Command != "/opt/node/bin/npx" {
		t.Fatalf("启动器应锚定自身绝对路径，实际 %q", got.Command)
	}
	// 解析失败保留原名（spawn 失败会提示写绝对路径）。
	lookPathFn = func(string) (string, error) { return "", exec.ErrNotFound }
	if got := templateWithPath(adapter, "codex", "/usr/local/bin/codex"); got.Command != "npx" {
		t.Fatalf("解析失败应保留启动器原名，实际 %q", got.Command)
	}
	if got := templateWithPath(adapter, "codex", ""); got.Command != "npx" {
		t.Fatalf("未探测到路径且解析失败时应保留模板命令，实际 %q", got.Command)
	}
}
