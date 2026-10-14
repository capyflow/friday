package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNormalizePermissionMode 权限模式的规范化与别名容错。
func TestNormalizePermissionMode(t *testing.T) {
	cases := map[string]string{
		"":                   PermModeConfirm,
		"confirm":            PermModeConfirm,
		"default":            PermModeConfirm,
		"acceptEdits":        PermModeAcceptEdits,
		"accept-edits":       PermModeAcceptEdits,
		"fullAccess":         PermModeFullAccess,
		"full_access":        PermModeFullAccess,
		"bypass-permissions": PermModeFullAccess,
		"  confirm  ":        PermModeConfirm,
	}
	for in, want := range cases {
		got, err := NormalizePermissionMode(in)
		if err != nil || got != want {
			t.Errorf("NormalizePermissionMode(%q) = %q, %v；应为 %q", in, got, err, want)
		}
	}
	if _, err := NormalizePermissionMode("yolo"); err == nil {
		t.Error("未知模式应报错")
	}
}

// TestPermissionsModeHelpers 派生判断：编辑免审与全放行。
func TestPermissionsModeHelpers(t *testing.T) {
	if (&PermissionsConfig{}).EditsAutoApproved() {
		t.Error("未配置（confirm）不应免审批")
	}
	if !(&PermissionsConfig{Mode: PermModeAcceptEdits}).EditsAutoApproved() {
		t.Error("acceptEdits 应免编辑审批")
	}
	if (&PermissionsConfig{Mode: PermModeAcceptEdits}).BypassesAll() {
		t.Error("acceptEdits 不应全放行")
	}
	if !(&PermissionsConfig{Mode: PermModeFullAccess}).BypassesAll() ||
		!(&PermissionsConfig{Mode: PermModeFullAccess}).EditsAutoApproved() {
		t.Error("fullAccess 应全放行")
	}
}

// TestUpdatePermissionMode 热写回：保留注释与其他键，permissions 段缺失时自动追加。
func TestUpdatePermissionMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := `# 顶部注释
provider:
  name: openai
  baseURL: https://api.example.com/v1
  model: demo
permissions:
  mcp: allow # 行内注释
  allow: ["mcp_muse_*"]
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdatePermissionMode(path, PermModeAcceptEdits); err != nil {
		t.Fatalf("UpdatePermissionMode: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# 顶部注释", "# 行内注释", "mcp: allow", "mcp_muse_*", "mode: acceptEdits"} {
		if !strings.Contains(text, want) {
			t.Fatalf("写回后应保留/包含 %q，实际:\n%s", want, text)
		}
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Permissions.EffectiveMode() != PermModeAcceptEdits || cfg.Permissions.MCP != "allow" ||
		len(cfg.Permissions.Allow) != 1 {
		t.Fatalf("回读配置不符: %+v", cfg.Permissions)
	}

	// permissions 段不存在的配置：自动追加。
	path2 := filepath.Join(dir, "no-perms.yaml")
	if err := os.WriteFile(path2, []byte("provider:\n  name: openai\n  baseURL: https://a.com/v1\n  model: m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdatePermissionMode(path2, PermModeFullAccess); err != nil {
		t.Fatalf("UpdatePermissionMode(无段): %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if cfg2.Permissions.EffectiveMode() != PermModeFullAccess {
		t.Fatalf("应自动追加 permissions 段并写回: %+v", cfg2.Permissions)
	}
}

// TestValidateRejectsUnknownPermissionMode 配置加载时拒绝非法权限模式。
func TestValidateRejectsUnknownPermissionMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "provider:\n  name: openai\n  baseURL: https://a.com/v1\n  model: m\npermissions:\n  mode: yolo\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "permissions.mode") {
		t.Fatalf("非法权限模式应导致加载失败: %v", err)
	}
}
