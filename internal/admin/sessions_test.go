// sessions_test.go 是网页会话清单接口的用例：列表只分主会话与后台入口两类，
// 已移除的创建/改名/删除端点不再可达，以及"没登记过的会话 key 不能靠发消息
// 或停止操作混进来"。
package admin

import (
	"net/http"
	"testing"
)

func TestSessionRoutes(t *testing.T) {
	_, handler, token, _ := runtimeTestServer(t)

	// 列表：只有主会话一条，且标成 life。
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

	// 创建/改名/删除端点已随聊天页入口收敛移除，访问应 404 而不是半途报错。
	for _, call := range []struct{ method, path string }{
		{"POST", "/api/sessions"},
		{"POST", "/api/sessions/work-1"},
		{"DELETE", "/api/sessions/work-1"},
	} {
		code, _ = acpCall(t, handler, token, call.method, call.path, nil)
		if code != http.StatusNotFound {
			t.Fatalf("%s %s 已移除的端点应 404，实际 %d", call.method, call.path, code)
		}
	}
}

// TestSessionRoutesRejectAnonymous 清单是读动作但也要登录；本项目口径是
// 403 + auth 标记，前端据此回登录页。
func TestSessionRoutesRejectAnonymous(t *testing.T) {
	_, handler, _, _ := runtimeTestServer(t)
	code, _ := acpCall(t, handler, "", "GET", "/api/sessions", nil)
	if code != http.StatusForbidden {
		t.Fatalf("未登录应 403，实际 %d", code)
	}
}

// TestChatRejectsUnregisteredSession 发消息与停止都不能凭空造会话
// （曾经的 work- 会话 key 形状也不放行）。
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
}
