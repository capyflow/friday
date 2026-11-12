// events_test.go 覆盖会话事件流订阅端点：鉴权、事件实时送达，以及空闲到上限
// 主动断开（回收客户端已经忘了的僵尸订阅——它们会占满浏览器的同源连接池，
// 表现是「页面收不到任何事件，刷新一下又好了」）。
package admin

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/eventbus"
)

// eventStreamServer 装一个带事件总线的后台（会话事件流按 session key 路由）。
func eventStreamServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	s, _, token, _, _ := manageTestServer(t)
	s.bus = eventbus.New(0, 0)
	return s, s.Handler(), token
}

// readEventFrame 从 SSE 响应里读一帧（顶层平铺 JSON）。
func readEventFrame(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("读事件帧失败: %v", err)
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var frame map[string]any
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("事件帧不是 JSON: %v", err)
		}
		// SSE 帧后面还跟一个空行（帧分隔符），一并读掉，
		// 否则下一次读会读到那个空行而不是真正的新内容。
		if _, err := r.ReadString('\n'); err != nil {
			t.Fatalf("读帧分隔符失败: %v", err)
		}
		return frame
	}
}

// openEventStream 打开一条事件流连接，返回响应体与读帧器。
func openEventStream(t *testing.T, handler http.Handler, query string) (*bufio.Reader, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/events/stream?" + query)
	if err != nil {
		t.Fatalf("连事件流失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("事件流应返回 200: %d", resp.StatusCode)
	}
	return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
}

func TestEventStreamRequiresAuth(t *testing.T) {
	_, handler, _ := eventStreamServer(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/events/stream?session=web&since=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未登录不该订阅到事件流: %d", resp.StatusCode)
	}
}

func TestEventStreamDeliversPublishedEvents(t *testing.T) {
	orig := sseIdleTimeout
	sseIdleTimeout = 30 * time.Second
	defer func() { sseIdleTimeout = orig }()
	s, handler, token := eventStreamServer(t)
	r, closeBody := openEventStream(t, handler, "session=web&since=0&token="+token)
	defer closeBody()

	hello := readEventFrame(t, r)
	if hello["type"] != "hello" {
		t.Fatalf("第一帧应是 hello: %v", hello)
	}

	s.bus.Publish("web", eventbus.TypeDelta, "t1", eventbus.Origin{}, map[string]any{"delta": "你好"})
	frame := readEventFrame(t, r)
	if frame["type"] != "delta" || frame["delta"] != "你好" || frame["turnId"] != "t1" {
		t.Fatalf("事件帧不对: %v", frame)
	}
}

// 客户端断开：服务端要立刻退订，不能把订阅留在内核里。
//
// 为什么重要：页面每刷新一次就多开一条订阅，旧的那条如果没人回收，内核里会
// 攒下一堆「没人读的订阅」——客户端那边连接位早还回去了，服务端这边还在占。
func TestEventStreamUnsubscribesWhenClientDisconnects(t *testing.T) {
	s, handler, token := eventStreamServer(t)
	r, closeBody := openEventStream(t, handler, "session=web&since=0&token="+token)
	if hello := readEventFrame(t, r); hello["type"] != "hello" {
		t.Fatalf("第一帧应是 hello: %v", hello)
	}
	if n := s.bus.ActiveCount("web"); n != 1 {
		t.Fatalf("应有 1 条订阅: %d", n)
	}
	closeBody()
	// 等服务端察觉断开（请求上下文取消）并退订。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.bus.ActiveCount("web") == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("客户端断开后订阅没退订: %d", s.bus.ActiveCount("web"))
}

// 空闲到上限：服务端主动断开这条连接。
//
// 为什么要有这个行为：客户端漏关的旧连接在服务端看仍是活着的订阅，浏览器那边
// 却早把它忘了——它一直占着浏览器的同源连接池（上限 6 条）。池子满了以后新连接
// 建不起来，页面就再也收不到事件。断开后客户端会带 since 重连，内容不丢。
func TestEventStreamClosesWhenIdle(t *testing.T) {
	orig := sseIdleTimeout
	sseIdleTimeout = 300 * time.Millisecond
	defer func() { sseIdleTimeout = orig }()

	_, handler, token := eventStreamServer(t)
	r, closeBody := openEventStream(t, handler, "session=web&since=0&token="+token)
	defer closeBody()

	if hello := readEventFrame(t, r); hello["type"] != "hello" {
		t.Fatalf("第一帧应是 hello: %v", hello)
	}
	// 之后不再有任何事件：空闲到上限后连接被服务端关掉（读到 EOF）。
	done := make(chan error, 1)
	go func() {
		_, err := r.ReadString('\n')
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("空闲连接应被服务端断开")
		}
		if err != io.EOF {
			t.Fatalf("空闲连接应以 EOF 结束: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("空闲连接没有被断开")
	}
}

// 空闲计时会被事件重置：有事件就不算空闲，连接继续留着。
func TestEventStreamIdleTimerResetsOnEvent(t *testing.T) {
	orig := sseIdleTimeout
	sseIdleTimeout = 600 * time.Millisecond
	defer func() { sseIdleTimeout = orig }()

	s, handler, token := eventStreamServer(t)
	r, closeBody := openEventStream(t, handler, "session=web&since=0&token="+token)
	defer closeBody()

	if hello := readEventFrame(t, r); hello["type"] != "hello" {
		t.Fatalf("第一帧应是 hello: %v", hello)
	}
	// 每 200ms 推一条，总共超过空闲上限——连接必须还活着。
	for i := 0; i < 5; i++ {
		time.Sleep(200 * time.Millisecond)
		s.bus.Publish("web", eventbus.TypeStatus, "t1", eventbus.Origin{}, map[string]any{"status": "在"})
		frame := readEventFrame(t, r)
		if frame["type"] != "status" {
			t.Fatalf("第 %d 帧应是 status: %v", i, frame)
		}
	}
}
