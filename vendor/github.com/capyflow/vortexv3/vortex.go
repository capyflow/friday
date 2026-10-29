package vortex

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/capyflow/allspark-go/logx"
	vpkg "github.com/capyflow/vortexv3/pkg"
	vhttp "github.com/capyflow/vortexv3/server/http"
	vws "github.com/capyflow/vortexv3/server/ws"
	"github.com/gin-gonic/gin"
)

type VortexEngine struct {
	ctx context.Context
	// 启动协议
	enableProtocol []string
	port           int // 监听端口
	ginEngine      *gin.Engine
	WsManager      *vws.WsManager // WebSocket 连接管理器
}

func NewVortexEngine(ctx context.Context, opts ...VortexOption) *VortexEngine {
	options := &VortexOptions{
		port: 8080,
		protocols: []string{
			vpkg.HTTP,
		},
		httpServerOptions: []gin.OptionFunc{
			func(e *gin.Engine) {
				e.SetTrustedProxies([]string{"127.0.0.1", "::1"})
			},
		},
		i18n: make(map[string]string),
	}
	for _, opt := range opts {
		opt(options)
	}
	// 设置i18n映射
	vpkg.SetI18nMap(options.i18n)
	if len(options.ginMode) == 0 {
		options.ginMode = gin.ReleaseMode
	}
	gin.SetMode(options.ginMode)
	ginEngine := gin.Default()
	for _, opt := range options.httpServerOptions {
		// 应用HTTP服务器选项
		opt(ginEngine)
	}
	if len(options.staticDir) > 0 {
		ginEngine.LoadHTMLGlob(options.staticDir)
	}
	wsManager := vws.NewWsManager()
	if options.WsRouter != nil {
		wsManager.WithRouter(options.WsRouter)
	}

	if options.WsSendMessage != nil {
		wsManager.WithSendSuccessCallback(options.WsSendMessage)
	}
	if options.SessionManager != nil {
		wsManager.WithSessionManager(options.SessionManager)
	}
	mountRouters(ginEngine, options.httpRouterRootGroup, options, wsManager)
	return &VortexEngine{
		ctx:            ctx,
		port:           options.port,
		ginEngine:      ginEngine,
		enableProtocol: options.protocols,
		WsManager:      wsManager,
	}
}

// Start 启动Vortex引擎
func (v *VortexEngine) Start() {
	fmt.Printf("Vortex Engine is running on port %d\n", v.port)
	go v.ginEngine.Run(":" + strconv.FormatInt(int64(v.port), 10))
	go v.wsStatsLoop()
}

// wsStatsLoop 定期打印WebSocket连接统计
func (v *VortexEngine) wsStatsLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-v.ctx.Done():
			return
		case <-ticker.C:
			logx.Debugf("vortex|ws|stats|devices=%d|users=%d",
				v.WsManager.Count(), v.WsManager.UserCount())
		}
	}
}

// mountRouters 挂载所有路由，包括 WebSocket 路由和用户定义的 HTTP 路由
func mountRouters(engine *gin.Engine, rootRouter *vhttp.VortexHttpRouterGroup, options *VortexOptions, wsManager *vws.WsManager) {
	if options == nil {
		panic(errors.New("options is nil"))
	}
	// 挂载 WebSocket 升级路由 /v1/websocket
	for _, p := range options.protocols {
		if p == vpkg.WEBSOCKET {
			middlewares := buildJwtMiddlewares(options, false, false)
			handler := vhttp.WrapHandleFunc(func(ctx *vhttp.Context) error {
				session := ctx.GetVSession()
				if session == nil {
					logx.Errorf("vortex|ws|reject|no session|ip=%s", ctx.GinContext().ClientIP())
					return ctx.JsonResponse(vhttp.Codes.Unauthorized, vpkg.VMsgResponse{"error": "unauthorized"})
				}
				if _, err := wsManager.Upgrade(ctx.GinContext().Writer, ctx.GinContext().Request, session); err != nil {
					logx.Errorf("vortex|ws|upgrade failed|uid=%s|sid=%s|ip=%s|err=%v",
						session.UID, session.SID, ctx.GinContext().ClientIP(), err)
					return ctx.JsonResponse(vhttp.Codes.InternalError, vpkg.VMsgResponse{"error": err.Error()})
				}
				logx.Debugf("vortex|ws|connected|uid=%s|sid=%s", session.UID, session.SID)
				return nil
			})
			engine.GET("/v1/websocket", append(middlewares, handler)...)
			break
		}
	}
	// 挂载用户定义的路由组
	mountRouterGroup(engine, rootRouter, options)
}

// mountRouterGroup 递归挂载路由组
func mountRouterGroup(engine *gin.Engine, group *vhttp.VortexHttpRouterGroup, options *VortexOptions) {
	for _, childGroup := range group.GetChildGroups() {
		mountRouterGroup(engine, childGroup, options)
	}
	for _, router := range group.GetRouters() {
		// 每个 router 独立计算自己的中间件列表。
		// 修复：之前 middlewares 在 router 循环外声明，导致 JWT 中间件跨 router 累积，
		// 使得同组内位于「需要鉴权路由」之后注册的「跳过 JWT 路由」会被错误地挂上 JWT 校验。
		middlewares := make([]gin.HandlerFunc, 0)
		if !router.IsSkipJwtVerify() && !router.IsStatic() {
			middlewares = append(middlewares, buildJwtMiddlewares(options, router.IsSkipExpireCheck(), router.IsConsoleRouter())...)
		}
		for _, method := range router.GetMethods() {
			method = strings.ToUpper(method)
			if method == "ANY" || method == "" {
				continue
			}
			engine.Handle(method, router.GetPath(), append(middlewares, router.GetHandles()...)...)
		}
	}
}

// buildJwtMiddlewares 构建 JWT 中间件列表
func buildJwtMiddlewares(options *VortexOptions, skipExpireCheck bool, isConsoleRouter bool) []gin.HandlerFunc {
	middlewares := make([]gin.HandlerFunc, 0)
	if !isConsoleRouter && options.JwtOption != nil {
		skip := skipExpireCheck || options.JwtOption.SkipExpiredVerfiy
		middlewares = append(middlewares, vhttp.WrapHandleFunc(vhttp.JwtMiddleWareFunc(options.JwtOption, skip)))
	}
	return middlewares
}
