package vws

import (
	"net/http"
	"strings"
	"sync"

	"github.com/capyflow/allspark-go/logx"
	vpkg "github.com/capyflow/vortexv3/pkg"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// MessageHandler 消息回调，携带 sid（设备ID）、uid（用户ID）和消息体
type MessageHandler func(sid, uid string, data *vpkg.VStruct)

// SessionManager 用户会话管理器接口，由业务服务端实现
type SessionManager interface {
	// OnConnect 连接建立时回调，用于业务层记录在线状态
	OnConnect(session *vpkg.VSession) error
	// OnDisconnect 连接断开时回调，用于业务层清理在线状态
	OnDisconnect(session *vpkg.VSession) error
	// GetUserSessions 查询用户所有在线设备的 session 列表
	GetUserSessions(uid string) ([]*vpkg.VSession, error)
	// GetSession 根据设备 SID 查询单个 session
	GetSession(sid string) (*vpkg.VSession, error)
}

// WsManager 管理所有 WebSocket 连接
// 索引结构：
//
//	conns: sid -> WsConn        （通过设备ID快速定位连接）
//	users: uid -> set of sids   （通过用户ID找到其所有设备连接）
type WsManager struct {
	mu                  sync.RWMutex
	conns               map[string]*WsConn         // sid -> conn
	users               map[string]map[string]bool // uid -> set of sid
	Router              *WsRouter                  // 客户端消息路由器（内置路由 + 用户业务路由）
	sendSuccessCallback MessageHandler             // 框架向客户端推送成功的回调函数
	sessionManager      SessionManager             // 业务实现的会话管理器（可选）
}

func (ws *WsManager) WithSendSuccessCallback(callback MessageHandler) *WsManager {
	ws.sendSuccessCallback = callback
	return ws
}

// WithRouter 设置客户端消息路由器
func (ws *WsManager) WithRouter(router *WsRouter) *WsManager {
	ws.Router = router
	return ws
}

// WithSessionManager 设置会话管理器（由业务服务端实现）
func (ws *WsManager) WithSessionManager(mgr SessionManager) *WsManager {
	ws.sessionManager = mgr
	return ws
}

// NewWsManager 创建一个新的 WebSocket 管理器
func NewWsManager() *WsManager {
	return &WsManager{
		conns:  make(map[string]*WsConn),
		users:  make(map[string]map[string]bool),
		Router: NewWsRouter(),
	}
}

// Upgrade 将 HTTP 连接升级为 WebSocket 并注册到管理器
// session 为 JWT 解析出的会话信息，同一 sid 重复连接时关闭旧连接
func (m *WsManager) Upgrade(w http.ResponseWriter, r *http.Request, session *vpkg.VSession) (*WsConn, error) {
	rawConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		if strings.Contains(err.Error(), "not using the websocket protocol") {
			logx.Warnf("vortex|ws|reject|not websocket|path=%s|client=%s", r.URL.Path, r.RemoteAddr)
		} else {
			logx.Errorf("vortex|ws|upgrade failed|%v", err)
		}
		return nil, err
	}
	conn := newWsConn(session, rawConn)
	// 注入推送能力，使业务 handler 可直接通过 conn 发送消息
	conn.sendToUser = m.PushToUser
	conn.sendToDevice = m.PushToDevice
	conn.broadcast = m.Broadcast

	sid := session.SID
	uid := session.UID

	m.mu.Lock()
	if old, ok := m.conns[sid]; ok {
		m.removeFromUser(old.session.UID, sid)
		old.Close()
	}
	m.conns[sid] = conn
	if m.users[uid] == nil {
		m.users[uid] = make(map[string]bool)
	}
	m.users[uid][sid] = true
	m.mu.Unlock()

	// 回调业务层的 OnConnect
	if m.sessionManager != nil {
		if err := m.sessionManager.OnConnect(session); err != nil {
			logx.Errorf("vortex|ws|session onConnect error|sid=%s|uid=%s|err=%v", sid, uid, err)
		}
	}

	onClose := func(s, u string) {
		m.mu.Lock()
		if c, ok := m.conns[s]; ok && c == conn {
			delete(m.conns, s)
			m.removeFromUser(u, s)
		}
		m.mu.Unlock()
		// 回调业务层的 OnDisconnect
		if m.sessionManager != nil {
			if err := m.sessionManager.OnDisconnect(session); err != nil {
				logx.Errorf("vortex|ws|session onDisconnect error|sid=%s|uid=%s|err=%v", s, u, err)
			}
		}
	}

	go conn.writePump()
	go conn.readPump(onClose, func(sid, uid string, data *vpkg.VStruct) {
		m.Router.Route(conn, data)
	})

	return conn, nil
}

// PushToUser 推送消息给指定用户的所有在线设备
func (m *WsManager) PushToUser(uid string, data *vpkg.VStruct) error {
	if len(uid) == 0 {
		return vpkg.VortexErrors.ErrUidIsEmpty
	}
	payload, err := data.Marshal()
	if err != nil {
		return err
	}
	m.mu.RLock()
	var conns []*WsConn
	for s := range m.users[uid] {
		if c, ok := m.conns[s]; ok {
			conns = append(conns, c)
		}
	}
	m.mu.RUnlock()

	for _, c := range conns {
		if c.sendBytes(payload) && m.sendSuccessCallback != nil {
			m.sendSuccessCallback(c.session.SID, c.session.UID, data)
		}
	}
	return nil
}

// PushToDevice 推送消息给指定用户的特定设备
func (m *WsManager) PushToDevice(uid, sid string, data *vpkg.VStruct) error {
	if len(uid) == 0 {
		return vpkg.VortexErrors.ErrUidIsEmpty
	}
	payload, err := data.Marshal()
	if err != nil {
		return err
	}
	m.mu.RLock()
	c, ok := m.conns[sid]
	m.mu.RUnlock()

	if !ok || c.session.UID != uid {
		return nil
	}
	if c.sendBytes(payload) && m.sendSuccessCallback != nil {
		m.sendSuccessCallback(c.session.SID, c.session.UID, data)
	}
	return nil
}

// Broadcast 广播消息给所有在线连接
func (m *WsManager) Broadcast(data *vpkg.VStruct) error {
	payload, err := data.Marshal()
	if err != nil {
		return err
	}
	m.mu.RLock()
	conns := make([]*WsConn, 0, len(m.conns))
	for _, c := range m.conns {
		conns = append(conns, c)
	}
	m.mu.RUnlock()

	for _, c := range conns {
		if c.sendBytes(payload) && m.sendSuccessCallback != nil {
			m.sendSuccessCallback(c.session.SID, c.session.UID, data)
		}
	}
	return nil
}

// KickUser 踢掉指定用户连接
// sid 不为空时只踢该设备，为空时踢掉该用户所有设备
func (m *WsManager) KickUser(uid, sid string) error {
	if len(uid) == 0 {
		return vpkg.VortexErrors.ErrUidIsEmpty
	}
	m.mu.Lock()
	var conns []*WsConn
	if sid != "" {
		if c, ok := m.conns[sid]; ok && c.session.UID == uid {
			conns = []*WsConn{c}
			delete(m.conns, sid)
			m.removeFromUser(uid, sid)
		}
	} else {
		for s := range m.users[uid] {
			if c, ok := m.conns[s]; ok {
				conns = append(conns, c)
				delete(m.conns, s)
			}
		}
		delete(m.users, uid)
	}
	m.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	return nil
}

// removeFromUser 从 uid 索引中移除 sid（需在锁内调用）
func (m *WsManager) removeFromUser(uid, sid string) {
	sids := m.users[uid]
	if sids == nil {
		return
	}
	delete(sids, sid)
	if len(sids) == 0 {
		delete(m.users, uid)
	}
}

// Count 返回当前在线连接总数（设备数）
func (m *WsManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.conns)
}

// UserCount 返回当前在线用户数（去重 uid 数）
func (m *WsManager) UserCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users)
}

// IsUserOnline 检查指定 uid 是否有在线的 WebSocket 连接
func (m *WsManager) IsUserOnline(uid string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sids, ok := m.users[uid]
	return ok && len(sids) > 0
}
