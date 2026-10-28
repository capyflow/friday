package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/capyflow/friday/internal/config"
)

func setupTestServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account.json")
	accounts := NewAccountStore(path)
	if err := accounts.Err(); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Accounts: accounts, Cfg: &config.Config{Provider: config.ProviderConfig{BaseURL: "https://provider.example/v1", Model: "model"}}})
	return s, s.Handler(), path
}

func setupTestRequest(handler http.Handler, method, path string, body []byte, remote, host, origin, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(body))
	r.RemoteAddr, r.Host = remote, host
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestSetupCreatesAccountOnceAndKeepsLegacyLogin(t *testing.T) {
	s, handler, path := setupTestServer(t)
	status := setupTestRequest(handler, "GET", "/api/setup/status", nil, "127.0.0.1:1234", "localhost", "", "")
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"accountRequired":true`)) {
		t.Fatalf("首次设置状态错误: %d %s", status.Code, status.Body)
	}
	if _, ok := s.accounts.Login(DefaultUsername, DefaultPassword); ok {
		t.Fatal("首次设置前不能登录默认账号")
	}
	body := []byte(`{"username":"owner","password":"independent-password"}`)
	created := setupTestRequest(handler, "POST", "/api/setup", body, "127.0.0.1:1234", "localhost", "http://localhost", "")
	if created.Code != http.StatusOK {
		t.Fatalf("设置失败: %d %s", created.Code, created.Body)
	}
	if bytes.Contains(created.Body.Bytes(), []byte("independent-password")) {
		t.Fatal("首次设置响应泄露密码")
	}
	// 建号即登录：返回 JWT，且不能再走 Cookie（鉴权改为 Authorization 头）。
	var createdBody struct {
		Body struct {
			Token string `json:"token"`
		} `json:"body"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil || createdBody.Body.Token == "" {
		t.Fatalf("首次设置应返回登录 token: %s", created.Body)
	}
	if !s.authenticated(createdBody.Body.Token) {
		t.Fatal("首次设置签发的 token 应能通过鉴权")
	}
	if len(created.Result().Cookies()) != 0 {
		t.Fatal("登录凭据不应再写 Cookie")
	}
	if s.accounts.Current().SetupRequired {
		t.Fatal("成功设置后仍要求首次设置")
	}
	if _, ok := NewAccountStore(path).Login("owner", "independent-password"); !ok {
		t.Fatal("首次设置账号未持久化")
	}
	if _, ok := s.accounts.Login(DefaultUsername, DefaultPassword); ok {
		t.Fatal("新账号创建后仍可用默认账号")
	}
	repeated := setupTestRequest(handler, "POST", "/api/setup", body, "127.0.0.1:1234", "localhost", "http://localhost", "")
	if repeated.Code == http.StatusOK {
		t.Fatal("已初始化部署不应接受重复首次设置")
	}
	// 不含 setupRequired 的历史格式保持可登录，不强制重建已有管理员。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "setupRequired")
	data, _ = json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := NewAccountStore(path)
	if reloaded.Current().SetupRequired {
		t.Fatal("已有账号不应进入首次设置")
	}
	if _, ok := reloaded.Login("owner", "independent-password"); !ok {
		t.Fatal("历史账号登录兼容被破坏")
	}
}

func TestSetupRejectsRemoteAndCrossOrigin(t *testing.T) {
	for _, tc := range []struct{ name, remote, host, origin string }{
		{"remote", "192.0.2.1:1234", "localhost", "http://localhost"},
		{"foreign_origin", "127.0.0.1:1234", "localhost", "https://attacker.example"},
		{"dns_rebinding", "127.0.0.1:1234", "attacker.example", "http://attacker.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, handler, _ := setupTestServer(t)
			response := setupTestRequest(handler, "POST", "/api/setup", []byte(`{"username":"owner","password":"secret-password"}`), tc.remote, tc.host, tc.origin, "")
			if response.Code != http.StatusForbidden || !s.accounts.Current().SetupRequired {
				t.Fatalf("应拒绝不可信首次设置: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestSetupConcurrentInitializeHasOneWinner(t *testing.T) {
	s, handler, path := setupTestServer(t)
	start := make(chan struct{})
	codes := make(chan int, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := setupTestRequest(handler, "POST", "/api/setup", []byte(`{"username":"owner","password":"secret-password"}`), "127.0.0.1:1234", "localhost", "http://localhost", "")
			codes <- r.Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	winners := 0
	for code := range codes {
		if code == http.StatusOK {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("并发首次设置成功次数=%d，期望1", winners)
	}
	if err := s.accounts.Initialize("other", "other-password"); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("Initialize 应拒绝覆盖，got %v", err)
	}
	if _, ok := NewAccountStore(path).Login("owner", "secret-password"); !ok {
		t.Fatal("并发设置最终凭据未正确落盘")
	}
}

func TestSetupWriteFailureRemainsUninitialized(t *testing.T) {
	s, handler, path := setupTestServer(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.accounts.path = filepath.Join(filepath.Dir(path), "missing", "account.json")
	r := setupTestRequest(handler, "POST", "/api/setup", []byte(`{"username":"owner","password":"secret-password"}`), "127.0.0.1:1234", "localhost", "http://localhost", "")
	if r.Code == http.StatusOK || !s.accounts.Current().SetupRequired {
		t.Fatal("保存失败不能完成首次设置")
	}
	if bytes.Contains(r.Body.Bytes(), []byte(`"token"`)) {
		t.Fatal("账号保存失败不能签发登录 token")
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("设置失败破坏了原账号文件: %v", err)
	}
}
