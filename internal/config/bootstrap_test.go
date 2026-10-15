package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapCreatesLoadableConfigurationAndKeepsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := EnsureInitial(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Host != "127.0.0.1" || cfg.Provider.Model == "" || cfg.Provider.APIKey != "" {
		t.Fatal("初始配置应为本机无密钥")
	}
	// provider 的两个值必须是明显示例占位：Validate() 要求非空所以不能留空，
	// 但也不能写死某家真实服务商（会被当成"已经配好了"）。
	if cfg.Provider.BaseURL != "https://api.example.com/v1" || cfg.Provider.Model != "请填模型名" {
		t.Fatalf("初始配置应为占位值，实际 %q / %q", cfg.Provider.BaseURL, cfg.Provider.Model)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0600 {
		t.Fatal("初始配置必须仅当前用户可读写")
	}
	old := []byte("{broken")
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureInitial(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(old) {
		t.Fatal("不得覆盖已存在配置")
	}
}

func TestBootstrapPreservesDanglingLinkAndReportsMissingParent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
		t.Skip(err)
	}
	if err := EnsureInitial(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("无效已有配置应阻止加载，不默默初始化")
	}
	if err := EnsureInitial(filepath.Join(dir, "no-dir", "config.yaml")); err == nil {
		t.Fatal("必须向调用方报告初始化写入失败")
	}
}
