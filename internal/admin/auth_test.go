package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/capyflow/friday/internal/web"
)

// loginToken 走真实登录接口拿 JWT（顺带断言响应形状：token、无 Cookie）。
func loginToken(t *testing.T, handler http.Handler, username, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	r := setupTestRequest(handler, "POST", "/api/login", body, "127.0.0.1:1234", "localhost", "http://localhost", "")
	if r.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", r.Code, r.Body)
	}
	if len(r.Result().Cookies()) != 0 {
		t.Fatal("登录不应再写 Cookie")
	}
	var out struct {
		Body struct {
			Token    string `json:"token"`
			Username string `json:"username"`
		} `json:"body"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || out.Body.Token == "" {
		t.Fatalf("登录响应缺少 token: %s", r.Body)
	}
	if out.Body.Username != username {
		t.Fatalf("用户名不符: %s", out.Body.Username)
	}
	return out.Body.Token
}

func TestJWTLoginAndProtectedRoutes(t *testing.T) {
	s, handler, _ := setupTestServer(t)
	if err := s.accounts.Initialize("owner", "secret-password"); err != nil {
		t.Fatal(err)
	}

	bad := setupTestRequest(handler, "POST", "/api/login", []byte(`{"username":"owner","password":"wrong"}`), "127.0.0.1:1234", "localhost", "http://localhost", "")
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("密码错误应 401（不是 token 失效）: %d", bad.Code)
	}

	token := loginToken(t, handler, "owner", "secret-password")

	// 无 token / 错 token：403 + auth 标记，前端据此回登录页。
	for _, tok := range []string{"", "not-a-token"} {
		r := setupTestRequest(handler, "GET", "/api/traces", nil, "127.0.0.1:1234", "localhost", "", tok)
		if r.Code != http.StatusForbidden || !bytes.Contains(r.Body.Bytes(), []byte(`"auth":true`)) {
			t.Fatalf("非法 token 应 403 且带 auth 标记: %d %s", r.Code, r.Body)
		}
	}

	ok := setupTestRequest(handler, "GET", "/api/traces", nil, "127.0.0.1:1234", "localhost", "", token)
	if ok.Code != http.StatusOK {
		t.Fatalf("合法 token 应放行: %d %s", ok.Code, ok.Body)
	}

	// /api/session 公开但会读 Authorization 头汇报登录态。
	sess := setupTestRequest(handler, "GET", "/api/session", nil, "127.0.0.1:1234", "localhost", "", token)
	if sess.Code != http.StatusOK || !bytes.Contains(sess.Body.Bytes(), []byte(`"authenticated":true`)) {
		t.Fatalf("会话状态应识别 token: %d %s", sess.Code, sess.Body)
	}
	anon := setupTestRequest(handler, "GET", "/api/session", nil, "127.0.0.1:1234", "localhost", "", "")
	if !bytes.Contains(anon.Body.Bytes(), []byte(`"authenticated":false`)) {
		t.Fatalf("无 token 应为未登录: %s", anon.Body)
	}
}

func TestJWTInvalidAfterPasswordChange(t *testing.T) {
	s, handler, _ := setupTestServer(t)
	if err := s.accounts.Initialize("owner", "secret-password"); err != nil {
		t.Fatal(err)
	}
	token := loginToken(t, handler, "owner", "secret-password")
	if err := s.accounts.Update("owner", "another-password"); err != nil {
		t.Fatal(err)
	}
	r := setupTestRequest(handler, "GET", "/api/traces", nil, "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusForbidden {
		t.Fatalf("改密后旧 token 应失效: %d %s", r.Code, r.Body)
	}
}

func TestLogoutRevokesToken(t *testing.T) {
	s, handler, _ := setupTestServer(t)
	if err := s.accounts.Initialize("owner", "secret-password"); err != nil {
		t.Fatal(err)
	}
	token := loginToken(t, handler, "owner", "secret-password")
	if r := setupTestRequest(handler, "POST", "/api/logout", []byte(`{}`), "127.0.0.1:1234", "localhost", "", token); r.Code != http.StatusOK {
		t.Fatalf("退出失败: %d %s", r.Code, r.Body)
	}
	r := setupTestRequest(handler, "GET", "/api/traces", nil, "127.0.0.1:1234", "localhost", "", token)
	if r.Code != http.StatusForbidden {
		t.Fatalf("退出后 token 应失效: %d %s", r.Code, r.Body)
	}
}

func TestJWTExpiredRejected(t *testing.T) {
	s, _, _ := setupTestServer(t)
	if err := s.accounts.Initialize("owner", "secret-password"); err != nil {
		t.Fatal(err)
	}
	claims := sessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "owner",
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
		PV: s.accounts.CredentialVersion(),
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.authKey)
	if err != nil {
		t.Fatal(err)
	}
	if s.authenticated(raw) {
		t.Fatal("过期 token 不应通过鉴权")
	}
	// 篡改签名同样拒绝。
	if s.authenticated(raw[:len(raw)-2] + "xy") {
		t.Fatal("篡改 token 不应通过鉴权")
	}
}

func TestUploadReadAcceptsQueryToken(t *testing.T) {
	s, _, _ := setupTestServer(t)
	if err := s.accounts.Initialize("owner", "secret-password"); err != nil {
		t.Fatal(err)
	}
	uploads := t.TempDir()
	s.chat = web.New(nil, uploads, filepath.Join(uploads, "paused.json"))
	name := "0123456789abcdef0123456789abcdef.png"
	if err := os.WriteFile(filepath.Join(uploads, name), []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	token := testAuthToken(t, s)

	// <img> 无法带头：?token= 可读；没 token 仍 403。
	ok := setupTestRequest(handler, "GET", "/api/upload/"+name+"?token="+token, nil, "127.0.0.1:1234", "localhost", "", "")
	if ok.Code != http.StatusOK || ok.Body.String() != "png-bytes" {
		t.Fatalf("query token 应能读取附件: %d %s", ok.Code, ok.Body)
	}
	denied := setupTestRequest(handler, "GET", "/api/upload/"+name, nil, "127.0.0.1:1234", "localhost", "", "")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("无 token 读附件应 403: %d", denied.Code)
	}
}

func TestAuthKeyPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth-key")
	first := loadOrCreateAuthKey(path)
	second := loadOrCreateAuthKey(path)
	if !bytes.Equal(first, second) || len(first) < authKeyBytes {
		t.Fatal("签名密钥应落盘并在重启后复用")
	}
	if runtime.GOOS != "windows" { // Windows 的 Stat 不反映 Unix 权限位，0600 由创建调用声明
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("密钥文件权限应为 0600: %v", err)
		}
	}
}
