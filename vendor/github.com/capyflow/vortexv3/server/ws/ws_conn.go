package vws

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/capyflow/allspark-go/logx"
	vpkg "github.com/capyflow/vortexv3/pkg"
	"github.com/gorilla/websocket"
)

// WsConn 单个 WebSocket 连接的封装
type WsConn struct {
	session *vpkg.VSession // 完整会话信息
	conn    *websocket.Conn
	send    chan []byte
	mu      sync.Mutex
	closed  bool
	// 推送能力（由 WsManager 注入，用于向其他连接发送消息）
	sendToUser   func(uid string, data *vpkg.VStruct) error
	sendToDevice func(uid, sid string, data *vpkg.VStruct) error
	broadcast    func(data *vpkg.VStruct) error
}

func newWsConn(session *vpkg.VSession, conn *websocket.Conn) *WsConn {
	return &WsConn{
		session: session,
		conn:    conn,
		send:    make(chan []byte, 256),
	}
}

// Session 返回完整会话信息
func (c *WsConn) Session() *vpkg.VSession { return c.session }

// Sid 返回设备/会话ID
func (c *WsConn) Sid() string { return c.session.SID }

// Uid 返回用户ID
func (c *WsConn) Uid() string { return c.session.UID }

// Reply 回复消息给当前连接（自己）
func (c *WsConn) Reply(data *vpkg.VStruct) error {
	payload, err := data.Marshal()
	if err != nil {
		return err
	}
	c.sendBytes(payload)
	return nil
}

// SendToUser 推送消息给指定用户的所有设备
func (c *WsConn) SendToUser(uid string, data *vpkg.VStruct) error {
	if c.sendToUser == nil {
		return nil
	}
	return c.sendToUser(uid, data)
}

// SendToDevice 推送消息给指定用户的特定设备
func (c *WsConn) SendToDevice(uid, sid string, data *vpkg.VStruct) error {
	if c.sendToDevice == nil {
		return nil
	}
	return c.sendToDevice(uid, sid, data)
}

// Broadcast 广播消息给所有在线连接
func (c *WsConn) Broadcast(data *vpkg.VStruct) error {
	if c.broadcast == nil {
		return nil
	}
	return c.broadcast(data)
}

// sendBytes 向该连接发送原始字节（仅 manager 内部使用）
func (c *WsConn) sendBytes(data []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.send <- data:
		return true
	default:
		return false
	}
}

// Close 关闭连接
func (c *WsConn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.send)
		c.conn.Close()
	}
}

// writePump 将 send channel 中的消息写入 WebSocket
func (c *WsConn) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				logx.Debugf("vortex|ws|ping failed|sid=%s|uid=%s|err=%v", c.session.SID, c.session.UID, err)
				return
			}
			logx.Debugf("vortex|ws|ping sent|sid=%s|uid=%s", c.session.SID, c.session.UID)
		}
	}
}

// readPump 读取客户端消息，将字节解析为 VStruct 后通过回调传给业务层
func (c *WsConn) readPump(onClose func(sid, uid string), onMessage func(sid, uid string, data *vpkg.VStruct)) {
	defer func() {
		onClose(c.session.SID, c.session.UID)
		c.Close()
	}()
	c.conn.SetReadLimit(512 * 1024)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		logx.Debugf("vortex|ws|pong received|sid=%s|uid=%s", c.session.SID, c.session.UID)
		return nil
	})
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		if onMessage == nil {
			continue
		}
		var msg vpkg.VStruct
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		onMessage(c.session.SID, c.session.UID, &msg)
	}
}
