package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestDefaultAccount 文件不存在时初始化默认账号 admin/admin。
func TestDefaultAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-account.json")
	s := NewAccountStore(path)

	acc, ok := s.Login(DefaultUsername, DefaultPassword)
	if !ok {
		t.Fatal("默认账号 admin/admin 应可登录")
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

// TestUpdateAccount 预置账号（启动配置/环境变量）后：默认账号失效、新账号生效、落盘。
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
	if !ok {
		t.Fatalf("新账号应可登录，got %+v ok=%v", acc, ok)
	}
	if cur := s.Current(); cur.Username != "aaron" {
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
}

// TestUpdateValidation 预置账号的输入校验。
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

// TestLegacyAccountFileWithMustChange 旧格式账号文件（含 mustChange 字段）可正常加载。
func TestLegacyAccountFileWithMustChange(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("oldpass"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("生成哈希: %v", err)
	}
	path := filepath.Join(t.TempDir(), "admin-account.json")
	legacy := `{"username":"legacy","passwordHash":"` + string(hash) + `","mustChange":false,"updatedAt":"2026-09-01T00:00:00+08:00"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	s := NewAccountStore(path)
	if _, ok := s.Login("legacy", "oldpass"); !ok {
		t.Fatal("旧格式账号文件应可正常加载并登录")
	}
}

// TestCorruptAccountFile 账号文件损坏时回退默认账号（可重新进入系统）。
func TestCorruptAccountFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-account.json")
	if err := os.WriteFile(path, []byte("{{{不是 JSON"), 0o600); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	s := NewAccountStore(path)
	if _, ok := s.Login(DefaultUsername, DefaultPassword); !ok {
		t.Fatal("损坏时应回退默认账号 admin/admin")
	}
}
