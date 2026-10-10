package admin

import (
	"bytes"
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
	if err := s.Err(); err != nil {
		t.Fatalf("初始化账号: %v", err)
	}

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

// TestCorruptAccountFile 账号文件损坏时拒绝登录和更新，保留原文件供恢复。
func TestCorruptAccountFile(t *testing.T) {
	for _, data := range []string{
		"{{{不是 JSON", "", "null", `{}`,
		`{"username":"admin","passwordHash":"not-bcrypt"}`,
		`{"username":"admin","passwordHash":"$2a$04$!` + strings.Repeat("A", 52) + `"}`,
	} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "admin-account.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatalf("写文件: %v", err)
			}
			s := NewAccountStore(path)
			if err := s.Err(); err == nil {
				t.Fatal("损坏文件应返回初始化错误")
			}
			if _, ok := s.Login(DefaultUsername, DefaultPassword); ok {
				t.Fatal("损坏时不能启用默认账号 admin/admin")
			}
			if err := s.Update("replacement", "secret123"); err == nil {
				t.Fatal("损坏时不能用预置凭据覆盖账号文件")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatalf("应保留原文件，got %q err=%v", got, err)
			}
		})
	}
}

func TestUnreadableAccountPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-account.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	s := NewAccountStore(path)
	if s.Err() == nil {
		t.Fatal("无法读取的路径应返回错误")
	}
	if _, ok := s.Login(DefaultUsername, DefaultPassword); ok {
		t.Fatal("读取失败时不能启用默认凭据")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("原路径应保留，info=%v err=%v", info, err)
	}
}

func TestDanglingAccountSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin-account.json")
	missing := filepath.Join(dir, "missing.json")
	if err := os.Symlink(missing, path); err != nil {
		t.Fatal(err)
	}
	s := NewAccountStore(path)
	if s.Err() == nil {
		t.Fatal("悬空链接不能视为首次初始化")
	}
	if _, ok := s.Login(DefaultUsername, DefaultPassword); ok {
		t.Fatal("悬空链接不能启用默认凭据")
	}
	got, err := os.Readlink(path)
	if err != nil || got != missing {
		t.Fatalf("原符号链接应保留，got=%q err=%v", got, err)
	}
}

func TestAccountInitializationSaveFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "admin-account.json")
	s := NewAccountStore(path)
	if s.Err() == nil {
		t.Fatal("首次保存失败应返回错误")
	}
	if _, ok := s.Login(DefaultUsername, DefaultPassword); ok {
		t.Fatal("首次保存失败不能启用未持久化的默认账号")
	}
}

func TestAccountUpdateSaveFailureKeepsCurrentAccount(t *testing.T) {
	for _, failure := range []string{"create", "rename"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "admin-account.json")
			s := NewAccountStore(path)
			if err := s.Update("original", "original123"); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(dir, "account-backup.json")
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			if failure == "create" {
				// 临时文件的目录不可用；已保存的原账号仍位于 backup。
				s.path = filepath.Join(dir, "missing-dir", "admin-account.json")
			} else if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := s.Update("replacement", "replacement123"); err == nil {
				t.Fatal("持久化失败必须返回错误")
			}
			if _, ok := s.Login("original", "original123"); !ok {
				t.Fatal("更新失败必须保持原账号可登录")
			}
			if _, ok := s.Login("replacement", "replacement123"); ok {
				t.Fatal("更新失败不能启用新账号")
			}
			if cur := s.Current(); cur.Username != "original" {
				t.Fatalf("Current 应保留原账号，got %+v", cur)
			}
			got, err := os.ReadFile(backup)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("原账号文件发生变化，err=%v", err)
			}
			if _, ok := NewAccountStore(backup).Login("original", "original123"); !ok {
				t.Fatal("重新加载后应仍可用原账号登录")
			}
			tmpFiles, err := filepath.Glob(filepath.Join(dir, ".admin-account.json.tmp-*"))
			if err != nil || len(tmpFiles) != 0 {
				t.Fatalf("临时文件应清理，files=%v err=%v", tmpFiles, err)
			}
		})
	}
}
