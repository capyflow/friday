package vortex

import (
	vpkg "github.com/capyflow/vortexv3/pkg"
	vhttp "github.com/capyflow/vortexv3/server/http"
	vws "github.com/capyflow/vortexv3/server/ws"
	"github.com/gin-gonic/gin"
)

type VortexOption func(*VortexOptions)

type VortexOptions struct {
	port                int // 监听端口
	httpServerOptions   []gin.OptionFunc
	protocols           []string                     // 启用的通讯协议
	httpRouterRootGroup *vhttp.VortexHttpRouterGroup // HTTP路由组
	ginMode             string                       // Gin模式
	JwtOption           *vpkg.JwtOption              // JWT选项
	i18n                map[string]string            // i18n映射
	staticDir           string                       // 静态文件的目录
	WsRouter            *vws.WsRouter                // WebSocket 客户端消息路由器
	WsSendMessage       vws.MessageHandler           // WebSocket发送消息处理函数
	SessionManager      vws.SessionManager           // 会话管理器（由业务服务端实现）
}

// WithSessionManager 设置会话管理器，框架在连接/断开时回调
func WithSessionManager(mgr vws.SessionManager) VortexOption {
	return func(v *VortexOptions) {
		v.SessionManager = mgr
	}
}

// WithWsRouter 设置 WebSocket 客户端消息路由器
// 用户通过 router.On("path", handler) 注册业务消息处理函数
// 框架内置路由（如 ping）会被自动处理，无需用户关心
func WithWsRouter(router *vws.WsRouter) VortexOption {
	return func(v *VortexOptions) {
		v.WsRouter = router
	}
}

// 设置WebSocket发送消息处理函数
func WithWsSendMessage(handler vws.MessageHandler) VortexOption {
	return func(v *VortexOptions) {
		v.WsSendMessage = handler
	}
}

// 设置启用的通讯协议
func WithEnableProtocol(protocol []string) VortexOption {
	return func(v *VortexOptions) {
		v.protocols = protocol
	}
}

func WithStaticDir(dir string) VortexOption {
	return func(v *VortexOptions) {
		v.staticDir = dir
	}
}

// 设置i18n映射
func WithI18n(i18n map[string]string) VortexOption {
	return func(v *VortexOptions) {
		v.i18n = i18n
	}
}

// 设置Gin模式
func WithGinMode(mode string) VortexOption {
	return func(v *VortexOptions) {
		v.ginMode = mode
	}
}

// 设置HTTP路由组
func WithHttpRouterRootGroup(routers *vhttp.VortexHttpRouterGroup) VortexOption {
	return func(v *VortexOptions) {
		v.httpRouterRootGroup = routers
	}
}

// 设置端口
func WithPort(port int) VortexOption {
	return func(v *VortexOptions) {
		v.port = port
	}
}

// 设置HTTP服务器选项
func WithHttpServerOptions(opts ...gin.OptionFunc) VortexOption {
	return func(v *VortexOptions) {
		v.httpServerOptions = opts
	}
}

// 设置JWT选项
func WithJwtOption(option *vpkg.JwtOption) VortexOption {
	return func(v *VortexOptions) {
		v.JwtOption = option
	}
}
