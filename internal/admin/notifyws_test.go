// notifyws_test.go 覆盖通知长链接：要登录、hello 帧带未读数、新通知实时推送，
// 以及已读这类局部更新不触发推送。终端通道的回环闸在 terminal_test.go 验证，
// 这里特意不设闸——手机端连的就是这条线。
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/capyflow/friday/internal/notification"
	"github.com/capyflow/friday/internal/task"
)

// noticeTestServer 装一个带通知中心与长链接枢纽的后台。manageTestServer 的通知
// 中心没有 resolver（Prepared/Push 永远 false，Unread 恒为 0），这里换成带
// resolver 的：种子通知经 ProcessOne 走完投递后 Unread 才会计数（口径见 center.go）。
func noticeTestServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	s, _, token, _, _ := manageTestServer(t)
	c, err := notification.New(filepath.Join(t.TempDir(), "notifications.json"),
		func(_ context.Context, tk task.Task) (string, bool, error) { return tk.Text, true, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.notifications = c
	s.noticeHub = newNoticeHub(c.Unread)
	c.SetNoticeHook(s.noticeHub.push)
	return s, s.Handler(), token
}

func dialNoticeWS(t *testing.T, handler http.Handler, rawQuery string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/notifications/ws" + rawQuery
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("连通知长链接失败(%d): %v", code, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readNoticeFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("帧不是 JSON: %v", err)
	}
	return frame
}

func TestNoticeWSRequiresAuth(t *testing.T) {
	_, handler, _ := noticeTestServer(t)
	rec := setupTestRequest(handler, http.MethodGet, "/api/notifications/ws", nil, "10.1.2.3:5678", "phone.example", "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未登录应 403，实际 %d", rec.Code)
	}
}

func TestNoticeWSServesRemoteOriginAndPushes(t *testing.T) {
	s, handler, token := noticeTestServer(t)

	// 预置一条已投递（Prepared+Push）的通知，hello 的未读数应把它数进去。
	seed := task.Task{ID: "seed", Text: "已存在的提醒", When: time.Now()}
	if err := s.notifications.Enqueue(seed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.notifications.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 远端地址（非回环）+ token 查询参数：手机端的标准连法，必须放行。
	conn := dialNoticeWS(t, handler, "?token="+url.QueryEscape(token))

	hello := readNoticeFrame(t, conn)
	if hello["t"] != "hello" {
		t.Fatalf("hello 帧不符: %v", hello)
	}
	if got := int(hello["unread"].(float64)); got != 1 {
		t.Fatalf("hello 未读数应含种子通知，实际 %d", got)
	}

	// 新通知落库 → 实时收到 notice 帧，内容带正文。
	if err := s.notifications.Enqueue(task.Task{ID: "t1", Text: "报告跑完了", When: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	frame := readNoticeFrame(t, conn)
	if frame["t"] != "notice" {
		t.Fatalf("应推 notice 帧: %v", frame)
	}
	n := frame["notice"].(map[string]any)
	if n["text"] != "报告跑完了" {
		t.Fatalf("通知正文不符: %v", n)
	}

	// 已读这种局部更新不该触发推送（否则自己点已读又收到一遍）。
	if err := s.notifications.MarkRead(n["id"].(string)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatalf("已读更新不应推送")
	}
}

func TestNoticeWSRejectsWithoutTokenFromRemote(t *testing.T) {
	_, handler, _ := noticeTestServer(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/notifications/ws"
	if _, resp, err := websocket.DefaultDialer.Dial(wsURL, nil); err == nil {
		t.Fatalf("无 token 远端连接应被拒")
	} else if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("应 403，实际 %d", resp.StatusCode)
	}
}

// TestNoticeHookFiresOnEnqueue 验证 Center 的通知钩子在入队时同步回调并拿到
// 最新快照——这是 notifyws.go push 契约（非阻塞、不回叫 Center）的前提。
func TestNoticeHookFiresOnEnqueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.json")
	seen := make(chan notification.Notice, 4)
	c, err := notification.New(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetNoticeHook(func(n notification.Notice) { seen <- n })
	if err := c.Enqueue(task.Task{ID: "a", Text: "正文", When: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-seen:
		if n.Text != "正文" || n.Status != "pending" {
			t.Fatalf("钩子快照不符: %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatalf("入队未触发钩子")
	}
}
