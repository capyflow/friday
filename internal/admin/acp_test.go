package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/capyflow/friday/internal/acp"
	"github.com/capyflow/friday/internal/config"
)

// dirsBody 发一次 GET /api/acp/dirs 并解出 { body: ... } 信封。
func dirsBody(t *testing.T, handler http.Handler, cookie *http.Cookie, rawQuery string) (int, map[string]any) {
	t.Helper()
	rec := setupTestRequest(handler, http.MethodGet, "/api/acp/dirs?"+rawQuery, nil, "127.0.0.1:1234", "localhost", "", cookie)
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析 dirs 响应 %s: %v", rec.Body.String(), err)
	}
	return rec.Code, envelope.Body
}

func TestAcpDirsListsSubdirectoriesOnly(t *testing.T) {
	s, _, cookie, _, _ := manageTestServer(t)
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

	code, body := dirsBody(t, handler, cookie, "path="+url.QueryEscape(root))
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
	code, body = dirsBody(t, handler, cookie, "")
	home, _ := os.UserHomeDir()
	if code != http.StatusOK || body["path"] != home {
		t.Fatalf("缺省 path 应列 HOME %s，状态 %d，实际 %v", home, code, body["path"])
	}

	// 不存在的目录 / 文件路径报 400。
	code, _ = dirsBody(t, handler, cookie, "path="+url.QueryEscape(filepath.Join(root, "nope")))
	if code != http.StatusBadRequest {
		t.Fatalf("不存在的目录应报 400，实际 %d", code)
	}
	code, _ = dirsBody(t, handler, cookie, "path="+url.QueryEscape(filepath.Join(root, "file.txt")))
	if code != http.StatusBadRequest {
		t.Fatalf("文件路径应报 400，实际 %d", code)
	}
}
