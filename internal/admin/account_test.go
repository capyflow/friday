package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultAccountMustChange 文件不存在时初始化默认账号 admin/admin，且强制首次改号。
func TestDefaultAccountMustChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-account.json")
	s := NewAccountStore(path)

	acc, ok := s.Login(DefaultUsername, DefaultPassword)
	if !ok {
		t.Fatal("默认账号 admin/admin 应可登录")
	}
	if !acc.MustChange {
		t.Fatal("默认账号应标记 MustChange=true（首次登录强制改号）")
	}
	if acc.Username != DefaultUsername {
		t.Fatalf("用户名应为 admin，got %q", acc.Username)
	}
	if _, ok := s.Login(DefaultUsername, "wrong"); ok {
		t.Fatal("错误密码不应登录成功")
	}
	if _, ok := s.Login("root", DefaultPassword); ok {
		t.Fatal("错误用户名不应登录成功")
	}
}

// TestUpdateAccount 重新设置账号后：默认账号失效、新账号生效、MustChange 清除、落盘。
func TestUpdateAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-account.json")
	s := NewAccountStore(path)

	if err := s.Update("aaron", "secret123"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, ok := s.Login(DefaultUsername, DefaultPassword); ok {
		t.Fatal("改号后默认账号应失效")
	}
	acc, ok := s.Login("aaron", "secret123")
	if !ok || acc.MustChange {
		t.Fatalf("新账号应可登录且 MustChange=false，got %+v ok=%v", acc, ok)
	}
	if cur := s.Current(); cur.Username != "aaron" || cur.MustChange {
		t.Fatalf("Current 不符，got %+v", cur)
	}

	// 重启（重新加载文件）后仍以新账号为准。
	s2 := NewAccountStore(path)
	if _, ok := s2.Login("aaron", "secret123"); !ok {
		t.Fatal("重启后新账号应可登录")
	}
	if _, ok := s2.Login(DefaultUsername, DefaultPassword); ok {
		t.Fatal("重启后默认账号不应复活")
	}
	if acc := s2.Current(); acc.MustChange {
		t.Fatal("重启后不应再要求改号")
	}
}

// TestUpdateValidation 改号的输入校验。
func TestUpdateValidation(t *testing.T) {
	s := NewAccountStore("")

	cases := []struct {
		name, user, pass, wantErr string
	}{
		{"用户名为空", "   ", "secret123", "用户名不能为空"},
		{"用户名超长", strings.Repeat("长", MaxUsernameLen+1), "secret123", "用户名最长"},
		{"密码过短", "aaron", "12345", "密码至少"},
	}
	for _, c := range cases {
		if err := s.Update(c.user, c.pass); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s：应报错 %q，got %v", c.name, c.wantErr, err)
		}
	}
	// 校验失败不应影响原账号。
	if _, ok := s.Login(DefaultUsername, DefaultPassword); !ok {
		t.Fatal("校验失败不应破坏原账号")
	}
}

// TestCorruptAccountFile 账号文件损坏时回退默认账号（可重新进入系统）。
func TestCorruptAccountFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-account.json")
	if err := os.WriteFile(path, []byte("{{{不是 JSON"), 0o600); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	s := NewAccountStore(path)
	acc, ok := s.Login(DefaultUsername, DefaultPassword)
	if !ok || !acc.MustChange {
		t.Fatalf("损坏时应回退默认账号 admin/admin，ok=%v acc=%+v", ok, acc)
	}
}
