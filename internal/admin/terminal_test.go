// terminal_test.go 覆盖内置终端的接口层：WebSocket 帧协议、要登录、以及"只服务本机
// 回环"这道硬闸。PTY 本身在 internal/terminal 里用真 /bin/sh 测过，这里只验搬运。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/capyflow/friday/internal/terminal"
)

// terminalTestServer 装一个只挂了终端管理器的后台（shell 固定 /bin/sh，用例可改上限）。
func terminalTestServer(t *testing.T, cfg terminal.Config) (*Server, string) {
	t.Helper()
	if cfg.Shell == "" {
		cfg.Shell = "/bin/sh"
	}
	s, _, _ := setupTestServer(t)
	mgr := terminal.NewManager(func() terminal.Config { return cfg })
	t.Cleanup(mgr.CloseAll)
	s.terminal = mgr
	return s, testAuthToken(t, s)
}

// dialTerminal 连上终端通道（httptest 的 RemoteAddr 就是 127.0.0.1，本机判断自然通过）。
func dialTerminal(t *testing.T, s *Server, token string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/terminal/ws?token=" + token
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("连终端通道失败: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readTermMsg 读一帧并解出控制消息（二进制帧返回 kind="data" 与原文）。
func readTermMsg(t *testing.T, conn *websocket.Conn, within time.Duration) (string, map[string]any, []byte) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	mt, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读终端帧失败: %v", err)
	}
	if mt == websocket.BinaryMessage {
		return "data", nil, payload
	}
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("控制帧不是 JSON: %s", payload)
	}
	kind, _ := msg["t"].(string)
	return kind, msg, nil
}

// waitTermData 一直读到出现 want 的输出（跳过控制帧）。PTY 会回显敲进去的命令，
// 所以用例等的都是"算出来的结果"，不是命令里的字面量。
func waitTermData(t *testing.T, conn *websocket.Conn, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var got strings.Builder
	for time.Now().Before(deadline) {
		kind, _, data := readTermMsg(t, conn, time.Until(deadline))
		if kind != "data" {
			continue
		}
		got.Write(data)
		if strings.Contains(got.String(), want) {
			return got.String()
		}
	}
	t.Fatalf("终端输出里没等到 %q，实际: %q", want, got.String())
	return ""
}

// waitTermKind 一直读到某类控制帧为止（中间的输出帧照常读掉，不丢就行）。
func waitTermKind(t *testing.T, conn *websocket.Conn, kindWant string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		kind, msg, _ := readTermMsg(t, conn, time.Until(deadline))
		if kind == kindWant {
			return msg
		}
	}
	t.Fatalf("没等到 %s 帧", kindWant)
	return nil
}

func TestTerminalWSRoundTrip(t *testing.T) {
	s, token := terminalTestServer(t, terminal.Config{Enabled: true, MaxSessions: 4})
	conn := dialTerminal(t, s, token)
	dir := t.TempDir()

	if err := conn.WriteJSON(map[string]any{"t": "start", "cols": 80, "rows": 24, "cwd": dir}); err != nil {
		t.Fatal(err)
	}
	kind, ready, _ := readTermMsg(t, conn, 10*time.Second)
	if kind != "ready" || ready["shell"] != "/bin/sh" || ready["cwd"] != dir {
		t.Fatalf("第一帧应是 ready 并交代 shell 与目录: %q %+v", kind, ready)
	}
	if id, _ := ready["id"].(string); !strings.HasPrefix(id, "t") {
		t.Fatalf("ready 应带会话 id: %+v", ready)
	}

	if err := conn.WriteJSON(map[string]any{"t": "input", "d": "pwd\n"}); err != nil {
		t.Fatal(err)
	}
	if out := waitTermData(t, conn, filepath.Base(dir), 10*time.Second); !strings.Contains(out, filepath.Base(dir)) {
		t.Fatalf("起始目录没生效: %q", out)
	}

	if err := conn.WriteJSON(map[string]any{"t": "resize", "cols": 40, "rows": 12}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]any{"t": "input", "d": "stty size\n"}); err != nil {
		t.Fatal(err)
	}
	if out := waitTermData(t, conn, "12 40", 10*time.Second); !strings.Contains(out, "12 40") {
		t.Fatalf("尺寸没同步: %q", out)
	}

	// 退出：shell 自己结束时回一条 exit 帧，带退出码
	if err := conn.WriteJSON(map[string]any{"t": "input", "d": "exit 3\n"}); err != nil {
		t.Fatal(err)
	}
	ex := waitTermKind(t, conn, "exit", 10*time.Second)
	if ex["code"] != float64(3) {
		t.Fatalf("应收到 exit code=3，实际 %+v", ex)
	}
}

func TestTerminalWSFirstFrameMustBeStart(t *testing.T) {
	s, token := terminalTestServer(t, terminal.Config{Enabled: true, MaxSessions: 4})
	conn := dialTerminal(t, s, token)
	if err := conn.WriteJSON(map[string]any{"t": "input", "d": "ls\n"}); err != nil {
		t.Fatal(err)
	}
	kind, msg, _ := readTermMsg(t, conn, 6*time.Second)
	if kind != "error" || !strings.Contains(msg["message"].(string), "start") {
		t.Fatalf("没先发 start 应报错并说清协议: %q %+v", kind, msg)
	}
	if s.terminal.Count() != 0 {
		t.Fatal("协议没走对也不该起进程")
	}
}

func TestTerminalWSCapReportsError(t *testing.T) {
	s, token := terminalTestServer(t, terminal.Config{Enabled: true, MaxSessions: 1})
	first := dialTerminal(t, s, token)
	if err := first.WriteJSON(map[string]any{"t": "start", "cols": 80, "rows": 24}); err != nil {
		t.Fatal(err)
	}
	if kind, _, _ := readTermMsg(t, first, 10*time.Second); kind != "ready" {
		t.Fatalf("第一个终端应起得来: %q", kind)
	}

	second := dialTerminal(t, s, token)
	if err := second.WriteJSON(map[string]any{"t": "start", "cols": 80, "rows": 24}); err != nil {
		t.Fatal(err)
	}
	kind, msg, _ := readTermMsg(t, second, 10*time.Second)
	if kind != "error" || !strings.Contains(msg["message"].(string), "上限 1") {
		t.Fatalf("第二个应被上限拦住: %q %+v", kind, msg)
	}
}

// TestTerminalWSRejectsRemoteAndAnon 两道闸：非本机回环直接 403（这条不看配置），
// 没登录也 403（token 走查询参数，浏览器 WebSocket 加不了请求头）。
func TestTerminalWSRejectsRemoteAndAnon(t *testing.T) {
	s, token := terminalTestServer(t, terminal.Config{Enabled: true, MaxSessions: 4})
	handler := s.Handler()

	rec := setupTestRequest(handler, "GET", "/api/terminal/ws", nil, "203.0.113.9:52233", "localhost", "", token)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "只允许本机") {
		t.Fatalf("非本机应 403 并说清原因，实际 %d: %s", rec.Code, rec.Body)
	}

	rec = setupTestRequest(handler, "GET", "/api/terminal/ws", nil, "127.0.0.1:52233", "localhost", "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未登录应 403，实际 %d: %s", rec.Code, rec.Body)
	}
	if s.terminal.Count() != 0 {
		t.Fatal("被拒的请求不该起任何进程")
	}
}

func TestTerminalWSDisabled(t *testing.T) {
	s, token := terminalTestServer(t, terminal.Config{Enabled: false, MaxSessions: 4})
	conn := dialTerminal(t, s, token)
	if err := conn.WriteJSON(map[string]any{"t": "start", "cols": 80, "rows": 24}); err != nil {
		t.Fatal(err)
	}
	kind, msg, _ := readTermMsg(t, conn, 6*time.Second)
	if kind != "error" || !strings.Contains(msg["message"].(string), "未开启") {
		t.Fatalf("关掉后应回错误帧并指路设置: %q %+v", kind, msg)
	}
}

// TestTerminalInfoReportsAvailability 状态接口是前端决定"给不给入口、不给说什么"的依据：
// 本机 + 开启才算可用，远程那侧要能看出是被本机闸拦住的。
func TestTerminalInfoReportsAvailability(t *testing.T) {
	s, token := terminalTestServer(t, terminal.Config{Enabled: true, MaxSessions: 2})
	handler := s.Handler()

	rec := setupTestRequest(handler, "GET", "/api/terminal", nil, "127.0.0.1:52233", "localhost", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态接口应 200，实际 %d: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	info, _ := body["body"].(map[string]any)
	if info["enabled"] != true || info["local"] != true || info["maxSessions"] != float64(2) {
		t.Fatalf("本机且开启时应报可用并带上限: %+v", info)
	}

	rec = setupTestRequest(handler, "GET", "/api/terminal", nil, "203.0.113.9:52233", "203.0.113.9:8080", "", token)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	info, _ = body["body"].(map[string]any)
	if info["enabled"] != false || info["local"] != false || !strings.Contains(info["reason"].(string), "只允许本机") {
		t.Fatalf("远程那侧应报不可用并说清原因: %+v", info)
	}
}
