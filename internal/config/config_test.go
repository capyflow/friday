package config

import (
	"os"
	"testing"
)

func TestResolveAdminCredentials(t *testing.T) {
	setEnv := func(k, v string) {
		if v == "" {
			os.Unsetenv(k)
			return
		}
		os.Setenv(k, v)
	}
	defer setEnv(AdminUsernameEnv, "")
	defer setEnv(AdminPasswordEnv, "")

	cfg := &Config{}
	cfg.Admin.Username = "cfguser"
	cfg.Admin.Password = "cfgpass"

	// 1. 环境变量优先于配置文件
	setEnv(AdminUsernameEnv, "envuser")
	setEnv(AdminPasswordEnv, "envpass")
	user, pass, err := cfg.ResolveAdminCredentials()
	if err != nil || user != "envuser" || pass != "envpass" {
		t.Fatalf("环境变量应优先: user=%q pass=%q err=%v", user, pass, err)
	}

	// 2. 环境变量未设置时用配置文件
	setEnv(AdminUsernameEnv, "")
	setEnv(AdminPasswordEnv, "")
	user, pass, err = cfg.ResolveAdminCredentials()
	if err != nil || user != "cfguser" || pass != "cfgpass" {
		t.Fatalf("应回退配置文件: user=%q pass=%q err=%v", user, pass, err)
	}

	// 3. 都未提供：返回空，走默认账号流程
	cfg2 := &Config{}
	user, pass, err = cfg2.ResolveAdminCredentials()
	if err != nil || user != "" || pass != "" {
		t.Fatalf("未配置应返回空: user=%q pass=%q err=%v", user, pass, err)
	}

	// 4. 只配其一：报错（用户名密码跨配置文件/环境变量合并补齐是合法的，不在此列）
	cfg3 := &Config{}
	cfg3.Admin.Username = "onlyuser"
	if _, _, err := cfg3.ResolveAdminCredentials(); err == nil {
		t.Fatal("只配用户名应报错")
	}
	cfg4 := &Config{}
	setEnv(AdminUsernameEnv, "")
	setEnv(AdminPasswordEnv, "onlypass")
	if _, _, err := cfg4.ResolveAdminCredentials(); err == nil {
		t.Fatal("环境变量只配密码也应报错")
	}

	// 5. 跨来源合并补齐（环境变量密码 + 配置文件用户名）：合法
	cfg5 := &Config{}
	cfg5.Admin.Username = "cfguser"
	user, pass, err = cfg5.ResolveAdminCredentials()
	if err != nil || user != "cfguser" || pass != "onlypass" {
		t.Fatalf("跨来源合并应合法: user=%q pass=%q err=%v", user, pass, err)
	}
}
