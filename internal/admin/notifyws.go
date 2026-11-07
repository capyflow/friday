// notifyws.go 是通知长链接：手机端页面与内核保持一条 WebSocket，新通知落库
// 与投递状态变化实时推过去，替代桌面页面的 5 秒轮询（远端页面轮询更浪费）。
//
// 与终端通道（terminal.go）的差别：终端能执行命令所以有本机回环硬闸；这里
// 只读推送、无回环闸，信任面就是登录 token（?token= 或 Authorization 头，
// 走 wrapMedia）——远程部署的手机端也能连。
//
// 帧约定（全部文本帧 JSON）：
//
//	连接建立推  {"t":"hello","unread":N}
//	事件推送    {"t":"notice","notice":{...}}   新通知落库或投递状态变化
//	保活        服务端每 25s 发协议层 ping，60s 没收到 pong 断开（浏览器自动回 pong）
//
// 推送只保证"有变化"不保证完整列表：客户端收到事件后按 id 并入本地列表，
// 重连或怀疑失步时全量拉 GET /api/notifications 兜底。
package admin

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/notification"
)

// 通道参数：写入超时、等待 pong 的读超时、ping 间隔、并发上限与单连接事件缓冲。
const (
	noticeWSWriteGap = 10 * time.Second
	noticeWSReadGap  = 60 * time.Second
	noticeWSPingGap  = 25 * time.Second
	// noticeWSMaxConns 是并发长链接上限：手机端一般一条，多开页面也够用；
	// 防失控（泄漏连接不释放）比容量更重要。
	noticeWSMaxConns = 16
	// noticeWSQueue 是单连接的事件缓冲：满即丢（该连接网络已堵），重连后全量拉取兜底。
	noticeWSQueue = 32
)

// noticeWSUpgrader 不做 Origin 校验：终端通道要求回环是防"任意网页驱动本机
// 命令"，这里只有拿着登录 token 才能建立连接，同源与否不改变攻击面——
// 桌面端与手机端的页面本来就都由内核同源下发。
var noticeWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// noticeClient 是一条已通过鉴权的长链接。
type noticeClient struct {
	conn *websocket.Conn
	send chan notification.Notice // 事件缓冲，writer 独占消费
	done chan struct{}            // 出册时关闭，writer 退出
}

// noticeHub 收敛所有长链接。push 由通知中心的钩子在 Center 锁内调用
// （契约：非阻塞），事件经缓冲队列交给 run 派发，各连接的 writer 串行写出。
type noticeHub struct {
	unread func() int
	events chan notification.Notice
	mu     sync.Mutex
	conns  map[*noticeClient]struct{}
	closed chan struct{}
}

func newNoticeHub(unread func() int) *noticeHub {
	h := &noticeHub{
		unread: unread,
		events: make(chan notification.Notice, noticeWSMaxConns*4),
		conns:  map[*noticeClient]struct{}{},
		closed: make(chan struct{}),
	}
	go h.run()
	return h
}

// push 投递一条通知事件；队列满说明派发已堵，丢弃（客户端重连后全量拉取兜底）。
// 在通知中心的锁内被调用，必须非阻塞。
func (h *noticeHub) push(n notification.Notice) {
	select {
	case h.events <- n:
	default:
	}
}

func (h *noticeHub) add(c *noticeClient) bool {
	select {
	case <-h.closed:
		return false
	default:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.conns) >= noticeWSMaxConns {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

// remove 出册并叫停 writer；只有"在册→出册"这一次才关 done，不会 double-close。
func (h *noticeHub) remove(c *noticeClient) {
	h.mu.Lock()
	_, ok := h.conns[c]
	delete(h.conns, c)
	h.mu.Unlock()
	if ok {
		close(c.done)
	}
}

func (h *noticeHub) run() {
	for {
		select {
		case <-h.closed:
			return
		case n := <-h.events:
			h.mu.Lock()
			for c := range h.conns {
				select {
				case c.send <- n:
				case <-c.done:
				default: // 该连接队列满：跳过，不让一条堵线拖住全局派发
				}
			}
			h.mu.Unlock()
		}
	}
}

// write 是 writer 独占的写出：gorilla 不允许并发写同一连接，事件帧与保活
// ping 都从这里走。出错关连接，reader 读到断开自然收尾。
func writeNoticeFrame(c *noticeClient, v any) error {
	c.conn.SetWriteDeadline(time.Now().Add(noticeWSWriteGap))
	return c.conn.WriteJSON(v)
}

// handleNotificationsWS 挂一条通知长链接：
// GET /api/notifications/ws?token=…（wrapMedia 鉴权，手机端可用）。
func (s *Server) handleNotificationsWS(ctx *vhttp.Context) error {
	hub := s.noticeHub
	if hub == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "通知中心未启用"})
	}
	conn, err := noticeWSUpgrader.Upgrade(ctx.GinContext().Writer, ctx.GinContext().Request, nil)
	if err != nil {
		return nil // Upgrade 失败时响应已写出，不再落 JSON 错误
	}
	c := &noticeClient{conn: conn, send: make(chan notification.Notice, noticeWSQueue), done: make(chan struct{})}
	if !hub.add(c) {
		conn.Close()
		return nil
	}
	defer hub.remove(c)

	// writer：独占这条连接的写。先发 hello（带当前未读数，客户端据此校准角标），
	// 之后派发事件帧与保活 ping。
	unread := 0
	if hub.unread != nil {
		unread = hub.unread()
	}
	if err := writeNoticeFrame(c, map[string]any{"t": "hello", "unread": unread}); err != nil {
		conn.Close()
		return nil
	}
	go func() {
		ping := time.NewTicker(noticeWSPingGap)
		defer ping.Stop()
		for {
			select {
			case n := <-c.send:
				if err := writeNoticeFrame(c, map[string]any{"t": "notice", "notice": n}); err != nil {
					conn.Close()
					return
				}
			case <-ping.C:
				conn.SetWriteDeadline(time.Now().Add(noticeWSWriteGap))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					conn.Close()
					return
				}
			case <-c.done:
				return
			}
		}
	}()

	// reader：本 handler 就是读者。客户端没有上行语义，读到什么都是忽略，
	// 协议层 pong 由浏览器自动回（读超时刷新靠 PongHandler）。读挂即断线收尾。
	defer conn.Close()
	conn.SetReadLimit(512) // 防恶意大帧；正常客户端本就不该发东西
	conn.SetReadDeadline(time.Now().Add(noticeWSReadGap))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(noticeWSReadGap))
	})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return nil
		}
	}
}
