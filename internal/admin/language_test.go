package admin

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
)

// 回复语言接口：读默认、改语言、写回 config.yaml、再读回来。
func TestLanguageAPIReadsWritesAndPersists(t *testing.T) {
	_, handler, token, path := runtimeTestServer(t)

	// 1. 默认是中文（配置里没写 language 时）。
	rec := setupTestRequest(handler, "GET", "/api/language", nil, "127.0.0.1:1234", "localhost", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取回复语言失败: %d %s", rec.Code, rec.Body)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("中文")) {
		t.Fatalf("默认回复语言应是中文: %s", rec.Body)
	}

	// 2. 改成 English：热生效并写回 config.yaml。
	body := []byte(`{"language":"English"}`)
	rec = setupTestRequest(handler, "POST", "/api/language", body, "127.0.0.1:1234", "localhost", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存回复语言失败: %d %s", rec.Code, rec.Body)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "language: English") {
		t.Fatalf("config.yaml 应写回 language: %s", saved)
	}

	// 3. 再读一次是改后的值。
	rec = setupTestRequest(handler, "GET", "/api/language", nil, "127.0.0.1:1234", "localhost", "", token)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("English")) {
		t.Fatalf("读取应为 English: %d %s", rec.Code, rec.Body)
	}
}

// 未登录不许改语言。
func TestLanguageAPIRequiresAuth(t *testing.T) {
	_, handler, _, _ := runtimeTestServer(t)
	body := []byte(`{"language":"English"}`)
	rec := setupTestRequest(handler, "POST", "/api/language", body, "127.0.0.1:1234", "localhost", "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未登录不该能改语言: %d", rec.Code)
	}
}
