package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/acp"
	"github.com/capyflow/friday/internal/config"
)

// dirsBody 发一次 GET /api/acp/dirs 并解出 { body: ... } 信封。
func dirsBody(t *testing.T, handler http.Handler, token string, rawQuery string) (int, map[string]any) {
	t.Helper()
	rec := setupTestRequest(handler, http.MethodGet, "/api/acp/dirs?"+rawQuery, nil, "127.0.0.1:1234", "localhost", "", token)
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析 dirs 响应 %s: %v", rec.Body.String(), err)
	}
	return rec.Code, envelope.Body
}

func TestAcpUpgradeUpdatesLaunchConfiguration(t *testing.T) {
	// 默认 acp 基目录是相对路径 <数据目录>/acp：chdir 到临时目录隔离真实数据目录。
	tmp := t.TempDir()
	t.Chdir(tmp)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\necho upgraded\n"))
	}))
	defer srv.Close()
	base := filepath.Join(tmp, "acp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := map[string]any{
		"fetchedAt": time.Now(),
		"index": map[string]any{"version": "1", "agents": []any{map[string]any{
			"id": "upgrade-test", "name": "Upgrade Test", "version": "2.0.0",
			"distribution": map[string]any{"binary": map[string]any{
				"darwin-aarch64": map[string]any{"archive": srv.URL + "/bin", "cmd": "agent", "args": []string{"--new"}},
				"linux-x86_64":   map[string]any{"archive": srv.URL + "/bin", "cmd": "agent", "args": []string{"--new"}},
				"windows-x86_64": map[string]any{"archive": srv.URL + "/bin", "cmd": "agent", "args": []string{"--new"}},
			}},
		}}},
	}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "registry-cache.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, token, _, _ := manageTestServer(t)
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(s.configPath, []byte("accounts: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Acp = config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{{
		Name: "upgrade-test", Command: "/old/agent", Args: []string{"--old"}, Version: "1.0.0",
		Env: map[string]string{"CUSTOM": "kept"}, Workspace: "/custom/workspace",
	}}}
	m := acp.NewManager(func() config.AcpConfig { return s.cfg.Snapshot().Acp }, nil, filepath.Join(t.TempDir(), "threads.json"))
	s.acp = m
	t.Cleanup(m.Close)
	rec := setupTestRequest(s.Handler(), http.MethodPost, "/api/acp/install", []byte(`{"id":"upgrade-test"}`), "127.0.0.1:1234", "localhost", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("升级失败: %d %s", rec.Code, rec.Body.String())
	}
	got := s.cfg.Snapshot().Acp.Agents[0]
	want := filepath.Join("acp", "agents", "upgrade-test", "2.0.0", "agent")
	if got.Command != want || len(got.Args) != 1 || got.Args[0] != "--new" || got.Version != "2.0.0" {
		t.Fatalf("升级后启动配置未更新: %+v", got)
	}
	if got.Env["CUSTOM"] != "kept" || got.Workspace != "/custom/workspace" {
		t.Fatalf("升级覆盖了用户配置: %+v", got)
	}
}

func TestAcpDirsListsSubdirectoriesOnly(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	m := acp.NewManager(func() config.AcpConfig { return config.AcpConfig{} }, nil, filepath.Join(t.TempDir(), "threads.json"))
	s.acp = m
	t.Cleanup(m.Close)
	handler := s.Handler()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b-dir", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body := dirsBody(t, handler, token, "path="+url.QueryEscape(root))
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %v", code, body)
	}
	if body["path"] != root {
		t.Fatalf("应回显绝对路径 %s，实际 %v", root, body["path"])
	}
	if parent := body["parent"]; parent != filepath.Dir(root) {
		t.Fatalf("parent 应为 %s，实际 %v", filepath.Dir(root), parent)
	}
	dirs, ok := body["dirs"].([]any)
	if !ok || len(dirs) != 2 {
		t.Fatalf("应只列 2 个子目录（不含文件、不递归），实际 %v", body["dirs"])
	}
	first := dirs[0].(map[string]any)
	second := dirs[1].(map[string]any)
	if first["name"] != "a-dir" || second["name"] != "b-dir" {
		t.Fatalf("应按名称排序：a-dir, b-dir；实际 %v, %v", first, second)
	}
	if first["path"] != filepath.Join(root, "a-dir") {
		t.Fatalf("子目录 path 应为绝对路径，实际 %v", first["path"])
	}

	// 缺省 path = HOME（只要求可用，不假设内容）。
	code, body = dirsBody(t, handler, token, "")
	home, _ := os.UserHomeDir()
	if code != http.StatusOK || body["path"] != home {
		t.Fatalf("缺省 path 应列 HOME %s，状态 %d，实际 %v", home, code, body["path"])
	}

	// 不存在的目录 / 文件路径报 400。
	code, _ = dirsBody(t, handler, token, "path="+url.QueryEscape(filepath.Join(root, "nope")))
	if code != http.StatusBadRequest {
		t.Fatalf("不存在的目录应报 400，实际 %d", code)
	}
	code, _ = dirsBody(t, handler, token, "path="+url.QueryEscape(filepath.Join(root, "file.txt")))
	if code != http.StatusBadRequest {
		t.Fatalf("文件路径应报 400，实际 %d", code)
	}
}
