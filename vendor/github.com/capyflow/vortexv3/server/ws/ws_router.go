package vws

import (
	"encoding/json"
	"time"

	"github.com/capyflow/allspark-go/logx"
	vpkg "github.com/capyflow/vortexv3/pkg"
)

// 框架内置的客户端消息 path
const (
	PathPing = "ping" // 客户端心跳，框架自动回复 pong
)

// WsHandler 客户端消息处理函数
// conn 为当前连接，msg 为解析后的消息体
type WsHandler func(conn *WsConn, msg *vpkg.VStruct) error

// WsRouter 客户端 → 服务端消息路由器
// 收到客户端消息后，先匹配框架内置路由，再匹配用户注册的业务路由
type WsRouter struct {
	builtinRoutes  map[string]WsHandler // 框架内置路由（ping 等）
	userRoutes     map[string]WsHandler // 用户注册的业务路由
	defaultHandler WsHandler            // 未匹配时的兜底处理（可选）
}

// NewWsRouter 创建消息路由器，自动注册框架内置路由
func NewWsRouter() *WsRouter {
	r := &WsRouter{
		builtinRoutes: make(map[string]WsHandler),
		userRoutes:    make(map[string]WsHandler),
	}
	// 注册框架内置路由
	r.builtinRoutes[PathPing] = handlePing
	return r
}

// On 注册业务消息路由
func (r *WsRouter) On(path string, handler WsHandler) {
	r.userRoutes[path] = handler
}

// SetDefaultHandler 设置未匹配路由时的兜底处理函数
func (r *WsRouter) SetDefaultHandler(handler WsHandler) {
	r.defaultHandler = handler
}

// Route 消息路由分发：内置路由 → 用户路由 → 兜底处理
func (r *WsRouter) Route(conn *WsConn, msg *vpkg.VStruct) {
	// 1. 框架内置路由优先
	if handler, ok := r.builtinRoutes[msg.Path]; ok {
		if err := handler(conn, msg); err != nil {
			logx.Errorf("vortex|ws|builtin handler error|sid=%s|uid=%s|path=%s|err=%v",
				conn.session.SID, conn.session.UID, msg.Path, err)
		}
		return
	}
	// 2. 用户注册的业务路由
	if handler, ok := r.userRoutes[msg.Path]; ok {
		if err := handler(conn, msg); err != nil {
			logx.Errorf("vortex|ws|user handler error|sid=%s|uid=%s|path=%s|err=%v",
				conn.session.SID, conn.session.UID, msg.Path, err)
		}
		return
	}
	// 3. 兜底处理
	if r.defaultHandler != nil {
		if err := r.defaultHandler(conn, msg); err != nil {
			logx.Errorf("vortex|ws|default handler error|sid=%s|uid=%s|path=%s|err=%v",
				conn.session.SID, conn.session.UID, msg.Path, err)
		}
		return
	}
	logx.Debugf("vortex|ws|unhandled message|sid=%s|uid=%s|path=%s", conn.session.SID, conn.session.UID, msg.Path)
}

// handlePing 框架内置 ping 处理：回复 pong
func handlePing(conn *WsConn, _ *vpkg.VStruct) error {
	resp := &vpkg.VStruct{
		Path: "pong",
		Head: vpkg.VHead{},
		Body: "pong",
	}
	resp.Head.SetTs(time.Now().UnixMilli())
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	conn.sendBytes(data)
	return nil
}
