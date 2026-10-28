package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// previewBody 发一次 GET /api/files/preview 并解出 { body: ... } 信封。
func previewBody(t *testing.T, handler http.Handler, token string, rawQuery string) (int, map[string]any) {
	t.Helper()
	rec := setupTestRequest(handler, http.MethodGet, "/api/files/preview?"+rawQuery, nil, "127.0.0.1:1234", "localhost", "", token)
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析 preview 响应 %s: %v", rec.Body.String(), err)
	}
	return rec.Code, envelope.Body
}

func TestFilePreviewReturnsTextContent(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	handler := s.Handler()

	dir := t.TempDir()
	file := filepath.Join(dir, "计划.md")
	if err := os.WriteFile(file, []byte("# 标题\n\n正文"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := previewBody(t, handler, token, "path="+url.QueryEscape(file))
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %v", code, body)
	}
	if body["kind"] != "text" {
		t.Fatalf("markdown 文件应为 text，实际 %v", body["kind"])
	}
	if body["content"] != "# 标题\n\n正文" {
		t.Fatalf("content 不符: %v", body["content"])
	}
	if body["name"] != "计划.md" || body["path"] != file {
		t.Fatalf("name/path 不符: %v / %v", body["name"], body["path"])
	}
	if body["mime"] != "text/markdown" {
		t.Fatalf("扩展名应给 mime 标签 text/markdown，实际 %v", body["mime"])
	}
	if body["truncated"] != false {
		t.Fatalf("小文件不应截断: %v", body["truncated"])
	}
}

func TestFilePreviewRejectsMissingAndNonRegular(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	handler := s.Handler()

	dir := t.TempDir()
	for _, raw := range []string{filepath.Join(dir, "没有.md"), dir} {
		code, body := previewBody(t, handler, token, "path="+url.QueryEscape(raw))
		if code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d: %v", raw, code, body)
		}
	}
	code, _ := previewBody(t, handler, token, "")
	if code != http.StatusBadRequest {
		t.Fatalf("缺 path 应 400，实际 %d", code)
	}
}

func TestFilePreviewTruncatesLargeText(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	handler := s.Handler()

	file := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(file, bytes.Repeat([]byte("a"), filePreviewTextMax+4096), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := previewBody(t, handler, token, "path="+url.QueryEscape(file))
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %v", code, body)
	}
	if body["truncated"] != true {
		t.Fatalf("超限文件应标记 truncated: %v", body["truncated"])
	}
	if got := body["content"].(string); len(got) != filePreviewTextMax {
		t.Fatalf("content 应只含前 %d 字节，实际 %d", filePreviewTextMax, len(got))
	}
}

func TestFilePreviewDetectsBinaryAndImage(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	handler := s.Handler()

	dir := t.TempDir()
	// PNG magic number → image + dataUrl。
	png := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(png, append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := previewBody(t, handler, token, "path="+url.QueryEscape(png))
	if code != http.StatusOK || body["kind"] != "image" {
		t.Fatalf("PNG 应识别为 image: %d %v", code, body)
	}
	if dataURL, _ := body["dataUrl"].(string); !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Fatalf("dataUrl 前缀不符: %v", body["dataUrl"])
	}
	// 含 NUL 字节 → binary，不回内容。
	bin := filepath.Join(dir, "raw.bin")
	if err := os.WriteFile(bin, []byte("text\x00text"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body = previewBody(t, handler, token, "path="+url.QueryEscape(bin))
	if code != http.StatusOK || body["kind"] != "binary" {
		t.Fatalf("含 NUL 应识别为 binary: %d %v", code, body)
	}
	if _, has := body["content"]; has {
		t.Fatalf("binary 不应回 content: %v", body)
	}
	if body["message"] == "" {
		t.Fatalf("binary 应带说明: %v", body)
	}
}

func TestFilePreviewExpandsHomePrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _, token, _, _ := manageTestServer(t)
	handler := s.Handler()

	file := filepath.Join(home, "home.txt")
	if err := os.WriteFile(file, []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := previewBody(t, handler, token, "path="+url.QueryEscape("~/home.txt"))
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %v", code, body)
	}
	if body["path"] != file || body["content"] != "home" {
		t.Fatalf("~/ 应展开为 HOME: %v", body)
	}
}
