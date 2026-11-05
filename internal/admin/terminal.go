// terminal.go 是内置终端的接口层：把一条 WebSocket 接到 terminal.Session 上，
// 键盘与尺寸进来、终端输出出去。
//
// 帧约定（文本帧 = JSON 控制消息，二进制帧 = 终端原始输出）：
//
//	客户端先发   {"t":"start","cols":80,"rows":24,"cwd":"可选"}
//	之后         {"t":"input","d":"ls -l\n"} / {"t":"resize","cols":..,"rows":..}
//	服务端回     {"t":"ready","id":..,"shell":..,"cwd":..}
//	            {"t":"error","message":".."} / {"t":"exit","code":..}
//	            二进制帧就是 PTY 读到的字节（不做 base64，省三成体积）
//
// 输出用二进制帧还有一层原因：终端字节流里本来就有任意控制字符，塞进 JSON 字符串
// 再解出来等于给自己找转义 bug。
//
// 一条连接一个终端：连接断开（含刷新页面、关标签）就把那条 shell 的进程组收掉，
// 不留"没人看却还在跑"的进程。上限与开关在 terminal.Manager 里判，这里只管搬运。
//
// 谁能连：先过 wrapMedia 的登录态（浏览器 WebSocket 加不了请求头，所以 token 走
// ?token=），再过本机回环这道硬闸——远程部署里这个接口不可用，配置也打不开它。
package admin

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	vhttp "github.com/capyflow/vortexv3/server/http"
)

// termReadLimit 是单条控制帧的上限：终端输入本来就小，64KB 足够一次性粘贴长命令。
const termReadLimit = 64 << 10

// termClientMsg 是一条客户端控制消息（start/input/resize）。
type termClientMsg struct {
	T    string `json:"t"`
	D    string `json:"d,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Cwd  string `json:"cwd,omitempty"`
}

// termUpgrader 的 Origin 检查直接用本机判断：非回环来的握手根本升不起来。
var termUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return localRequest(r) },
}

// handleTerminalInfo 返回内置终端的可用状态（GET /api/terminal）：前端据此决定
// 侧栏要不要给入口、不给的时候说什么原因。enabled 是"配置开了 且 这次请求来自本机"
// 两者的与——远程部署里这个功能整体不服务，不是降级可用。
func (s *Server) handleTerminalInfo(ctx *vhttp.Context) error {
	if s.terminal == nil {
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
			"enabled": false, "reason": "内置终端未装配"})
	}
	cfg := s.terminal.Config()
	local := localRequest(ctx.GinContext().Request)
	body := map[string]any{
		"enabled":     cfg.Enabled && local,
		"local":       local,
		"active":      s.terminal.Count(),
		"maxSessions": cfg.Max(),
	}
	switch {
	case !cfg.Enabled:
		body["reason"] = "内置终端未开启（config.yaml 里 terminal.enabled 设为 true 后重启）"
	case !local:
		body["reason"] = "内置终端只允许本机访问"
	}
	return ctx.JsonResponse(vhttp.Codes.Success, body)
}

// handleTerminalWS 接管一条终端连接。handler 一直阻塞到连接结束——与 SSE 同一种
// 长连接写法，路由层不会提前替它收尾。
func (s *Server) handleTerminalWS(ctx *vhttp.Context) error {
	req := ctx.GinContext().Request
	if !localRequest(req) {
		return ctx.JsonResponse(vhttp.Code{Code: http.StatusForbidden, MsgKey: "forbidden"},
			map[string]string{"error": "内置终端只允许本机访问：远程部署请在本机做端口转发，" +
				"或在配置里把 terminal.enabled 设为 false 关掉这个入口"})
	}
	if s.terminal == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "内置终端未装配"})
	}
	conn, err := termUpgrader.Upgrade(ctx.GinContext().Writer, req, nil)
	if err != nil {
		return nil // 握手没成（多半是 Origin 被拒），gorilla 已经把响应写掉了
	}
	defer conn.Close()
	conn.SetReadLimit(termReadLimit)

	// 第一帧必须是 start：尺寸要先到 PTY，否则 shell 按 80x24 排版，界面宽度不一样
	// 就会看到错位的历史。
	var start termClientMsg
	if err := conn.ReadJSON(&start); err != nil || start.T != "start" {
		writeTermErr(conn, `第一条消息须是 {"t":"start","cols":列数,"rows":行数}`)
		return nil
	}
	sess, err := s.terminal.Start(start.Cols, start.Rows, start.Cwd)
	if err != nil {
		writeTermErr(conn, err.Error())
		return nil
	}
	defer sess.Close()

	var wmu sync.Mutex // gorilla 一次只允许一个写者：输出泵与控制回复共用一条连接
	write := func(mt int, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return conn.WriteMessage(mt, payload)
	}
	if err := write(websocket.TextMessage, termJSON(map[string]any{
		"t": "ready", "id": sess.ID, "shell": sess.Shell, "cwd": sess.Cwd,
	})); err != nil {
		return nil
	}

	// 输出与退出：先把 PTY 的输出搬到连接上（写失败 = 客户端已经不在了，顺手把
	// 终端收掉，不然 shell 会一直等输入），输出通道关闭（进程结束、PTY EOF）之后
	// 再回一条 exit 帧——顺序反了会把最后几行输出丢掉。
	go func() {
		for chunk := range sess.Output() {
			if err := write(websocket.BinaryMessage, chunk); err != nil {
				sess.Close()
				return
			}
		}
		ex, ok := <-sess.Done()
		if !ok {
			return
		}
		msg := map[string]any{"t": "exit", "code": ex.Code}
		if ex.Err != nil {
			msg["error"] = ex.Err.Error()
		}
		_ = write(websocket.TextMessage, termJSON(msg))
	}()

	// 读侧：input/resize，读到断连为止。
	for {
		var msg termClientMsg
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		switch msg.T {
		case "input":
			if _, err := sess.Write([]byte(msg.D)); err != nil {
				writeTermErr(conn, "写入终端失败，连接将关闭: "+err.Error())
				return nil
			}
		case "resize":
			if err := sess.Resize(msg.Cols, msg.Rows); err != nil {
				writeTermErr(conn, err.Error())
			}
		case "close":
			return nil // 主动关标签：defer 里收掉进程组
		default:
			writeTermErr(conn, "未知终端指令: "+msg.T)
		}
	}
	return nil
}

func writeTermErr(conn *websocket.Conn, message string) {
	_ = conn.WriteMessage(websocket.TextMessage, termJSON(map[string]any{"t": "error", "message": message}))
}

func termJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// 只有内部字段进这里，真序列化不了就是代码问题；给一条能看懂的错误帧总比什么都不回强
		b, _ = json.Marshal(map[string]any{"t": "error", "message": "终端消息编码失败"})
	}
	return b
}
