package acp

import (
	"context"
	"os/exec"
	"strings"
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
