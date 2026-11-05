// sessions_test.go 是网页会话接口的用例：列表归属、新建/改名/删除，
// 以及"没登记过的会话 key 不能靠发消息或停止操作混进来"。
package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionRoutes(t *testing.T) {
	_, handler, token, _ := runtimeTestServer(t)

	// 列表：还没有工作会话时只有主会话一条，且标成 life。
	code, body := acpCall(t, handler, token, "GET", "/api/sessions", nil)
	if code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d: %v", code, body)
	}
	list, _ := body["sessions"].([]any)
	if len(list) != 1 {
		t.Fatalf("应只有主会话: %v", body["sessions"])
	}
	first, _ := list[0].(map[string]any)
	if first["key"] != "web" || first["kind"] != "life" || first["title"] != "主会话" {
		t.Fatalf("主会话应带 life 归属与固定标题: %v", first)
	}

	// 新建：不想起名字也建得出来，标题回落「新会话」。
	code, body = acpCall(t, handler, token, "POST", "/api/sessions", map[string]any{})
	session, _ := body["session"].(map[string]any)
	key, _ := session["key"].(string)
	if code != http.StatusOK || !strings.HasPrefix(key, "work-") {
		t.Fatalf("新建应 200 并回带 work- 前缀的 key，实际 %d: %v", code, body)
	}
	if session["kind"] != "work" || session["title"] != "新会话" {
		t.Fatalf("新会话应是未起名的 work 会话: %v", session)
	}

	// 改名。
	code, body = acpCall(t, handler, token, "POST", "/api/sessions/"+key, map[string]any{"title": " 周报 "})
	session, _ = body["session"].(map[string]any)
	if code != http.StatusOK || session["title"] != "周报" {
		t.Fatalf("改名应 200 并去空白，实际 %d: %v", code, body)
	}

	// 列表里能看见这条名字与工作归属。
	code, body = acpCall(t, handler, token, "GET", "/api/sessions", nil)
	if code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", code)
	}
	if !sessionNamed(body["sessions"], key, "周报") {
		t.Fatalf("列表应带上改名后的会话: %v", body["sessions"])
	}

	// 删除。
	code, body = acpCall(t, handler, token, "DELETE", "/api/sessions/"+key, nil)
	if code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d: %v", code, body)
	}
	code, body = acpCall(t, handler, token, "GET", "/api/sessions", nil)
	if sessionNamed(body["sessions"], key, "周报") {
		t.Fatalf("删除后列表不该还有: %v", body["sessions"])
	}

	// 主会话删不掉，且给出下一步。
	code, body = acpCall(t, handler, token, "DELETE", "/api/sessions/web", nil)
	errText, _ := body["error"].(string)
	if code != http.StatusBadRequest || !strings.Contains(errText, "清除上下文") {
		t.Fatalf("删主会话应 400 并说明用清除上下文，实际 %d: %v", code, body)
	}

	// 没登记过的 key 改不了名。
	code, body = acpCall(t, handler, token, "POST", "/api/sessions/work-没建过", map[string]any{"title": "x"})
	if code != http.StatusBadRequest {
		t.Fatalf("未登记的会话改名应 400，实际 %d: %v", code, body)
	}
}

// TestSessionRoutesRejectAnonymous 新建/改名/删除都是写动作，未登录一律拦住
// （本项目的口径是 403 + auth 标记，前端据此回登录页）。
func TestSessionRoutesRejectAnonymous(t *testing.T) {
	_, handler, _, _ := runtimeTestServer(t)
	for _, call := range []struct{ method, path string }{
		{"GET", "/api/sessions"},
		{"POST", "/api/sessions"},
		{"POST", "/api/sessions/work-1"},
		{"DELETE", "/api/sessions/work-1"},
	} {
		code, _ := acpCall(t, handler, "", call.method, call.path, nil)
		if code != http.StatusForbidden {
			t.Fatalf("%s %s 未登录应 403，实际 %d", call.method, call.path, code)
		}
	}
}

// TestChatRejectsUnregisteredSession 发消息与停止都不能凭空造会话。
func TestChatRejectsUnregisteredSession(t *testing.T) {
	_, handler, token, _ := runtimeTestServer(t)

	for _, path := range []string{"/api/chat", "/api/chat/stop", "/api/chat/resume"} {
		code, body := acpCall(t, handler, token, "POST", path,
			map[string]any{"message": "说一句", "session": "work-20260101-000000-dead"})
		errText, _ := body["error"].(string)
		if code != http.StatusBadRequest || errText == "" {
			t.Fatalf("%s 带未登记的会话应 400 并说明原因，实际 %d: %v", path, code, body)
		}
	}

	// 停一条没在跑的会话：不是"会话不存在"，而是"没有进行中的对话"。
	code, body := acpCall(t, handler, token, "POST", "/api/sessions", map[string]any{"title": "没在跑"})
	session, _ := body["session"].(map[string]any)
	key, _ := session["key"].(string)
	if code != http.StatusOK || key == "" {
		t.Fatalf("新建应 200: %d %v", code, body)
	}
	code, body = acpCall(t, handler, token, "POST", "/api/chat/stop", map[string]any{"session": key})
	errText, _ := body["error"].(string)
	if code != http.StatusBadRequest || !strings.Contains(errText, "没有进行中的对话") {
		t.Fatalf("登记过的会话应走到停止判断，实际 %d: %v", code, body)
	}
}

// sessionNamed 在列表响应里找一条会话，名字对上才算数。
func sessionNamed(v any, key, title string) bool {
	list, _ := v.([]any)
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		if item["key"] == key {
			return item["title"] == title
		}
	}
	return false
}

// TestSessionCreateWithWorkDir 建会话时可以带一个工作目录：带上了就原样回带并
// 存进登记表，带错了（不存在/是文件）是 400 并说明下一步，不是 500。
func TestSessionCreateWithWorkDir(t *testing.T) {
	_, handler, token, _ := runtimeTestServer(t)
	proj := t.TempDir()

	code, body := acpCall(t, handler, token, "POST", "/api/sessions",
		map[string]any{"title": "改这个项目", "dir": proj})
	session, _ := body["session"].(map[string]any)
	key, _ := session["key"].(string)
	if code != http.StatusOK || session["dir"] != proj {
		t.Fatalf("带目录建会话应 200 并回带目录，实际 %d: %v", code, body)
	}

	code, body = acpCall(t, handler, token, "GET", "/api/sessions", nil)
	list, _ := body["sessions"].([]any)
	var found bool
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		if item["key"] == key {
			found = item["dir"] == proj
		}
	}
	if !found {
		t.Fatalf("列表里这条会话应带目录: %v", list)
	}

	for _, bad := range []string{filepath.Join(proj, "没这个目录"), filepath.Join(proj, "a.txt")} {
		if err := os.WriteFile(filepath.Join(proj, "a.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, body = acpCall(t, handler, token, "POST", "/api/sessions", map[string]any{"dir": bad})
		errText, _ := body["error"].(string)
		if code != http.StatusBadRequest || errText == "" {
			t.Fatalf("目录 %q 应 400 并说明原因，实际 %d: %v", bad, code, body)
		}
	}
}
