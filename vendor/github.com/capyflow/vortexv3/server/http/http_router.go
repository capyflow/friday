package vhttp

import (
	"path"

	"github.com/gin-gonic/gin"
)

type (
	VortexHttpRouterHandle  = handleFunc
	VortexHttpMiddleware    = handleFunc
	VortexHttpRouterOptions = func(router *VortexHttpRouter)
)

// VortexHttpRouterGroup 路由组
type VortexHttpRouterGroup struct {
	relativePath string
	middlewares  []VortexHttpMiddleware
	childGroups  []*VortexHttpRouterGroup
	routers      []*VortexHttpRouter
}

// GetChildGroups 获取子路由组
func (group *VortexHttpRouterGroup) GetChildGroups() []*VortexHttpRouterGroup {
	return group.childGroups
}

// GetRouters 获取路由
func (group *VortexHttpRouterGroup) GetRouters() []*VortexHttpRouter {
	return group.routers
}

// AddRouter 添加路由
func (group *VortexHttpRouterGroup) AddRouter(methods []string, routerPath string, handle VortexHttpRouterHandle, options ...VortexHttpRouterOptions) *VortexHttpRouterGroup {
	router := &VortexHttpRouter{
		methods: methods,
		path:    path.Join(group.relativePath, routerPath),
		handles: make([]gin.HandlerFunc, 0),
	}
	for _, opt := range options {
		opt(router)
	}
	// 合并路由组中间件和路由级中间件
	allMiddlewares := make([]VortexHttpMiddleware, 0, len(group.middlewares)+len(router.middlewares))
	allMiddlewares = append(allMiddlewares, group.middlewares...)  // 先添加路由组中间件
	allMiddlewares = append(allMiddlewares, router.middlewares...) // 再添加路由级中间件

	mws := make([]gin.HandlerFunc, 0, len(allMiddlewares))
	for _, middleware := range allMiddlewares {
		mws = append(mws, WrapHandleFunc(middleware))
	}
	router.handles = append(router.handles, mws...)
	router.handles = append(router.handles, WrapHandleFunc(handle))
	group.routers = append(group.routers, router)
	return group
}

// AddStaticRouter 添加静态资源
func (group *VortexHttpRouterGroup) AddStaticRouter(routerPath string, handle VortexHttpRouterHandle, options ...VortexHttpRouterOptions) *VortexHttpRouterGroup {
	router := &VortexHttpRouter{
		methods:  []string{"GET", "HEAD"}, // 静态资源通常只支持 GET 和 HEAD 方法
		path:     path.Join(group.relativePath, routerPath),
		handles:  make([]gin.HandlerFunc, 0),
		isStatic: true, // 标记为静态路由
	}
	for _, opt := range options {
		opt(router)
	}
	// 合并路由组中间件和路由级中间件
	allMiddlewares := make([]VortexHttpMiddleware, 0, len(group.middlewares)+len(router.middlewares))
	allMiddlewares = append(allMiddlewares, group.middlewares...)  // 先添加路由组中间件
	allMiddlewares = append(allMiddlewares, router.middlewares...) // 再添加路由级中间件

	mws := make([]gin.HandlerFunc, 0, len(allMiddlewares))
	for _, middleware := range allMiddlewares {
		mws = append(mws, WrapHandleFunc(middleware))
	}
	router.handles = append(router.handles, mws...)
	router.handles = append(router.handles, WrapHandleFunc(handle))
	group.routers = append(group.routers, router)
	return group
}

// 创建子路由组
func (group *VortexHttpRouterGroup) AddGroup(relativePath string, middlewares ...VortexHttpMiddleware) *VortexHttpRouterGroup {
	childGroup := &VortexHttpRouterGroup{
		relativePath: path.Join(group.relativePath, relativePath),
		middlewares:  append(group.middlewares, middlewares...),
	}
	group.childGroups = append(group.childGroups, childGroup)
	return childGroup
}

// VortexHttpRouter 路由
type VortexHttpRouter struct {
	methods         []string
	path            string
	handles         []gin.HandlerFunc
	skipExpireCheck bool // 是否跳过过期检查
	isConsoleRouter bool // 是否是控制台路由
	skipJwtVerify   bool // 是否跳过JWT验证
	middlewares     []VortexHttpMiddleware
	isStatic        bool // 是否是静态文件
}

// WithSkipExpireCheck 设置是否跳过过期检查
func WithSkipExpireCheck(skipExpireCheck ...bool) VortexHttpRouterOptions {
	return func(router *VortexHttpRouter) {
		if len(skipExpireCheck) > 0 {
			router.skipExpireCheck = skipExpireCheck[0]
		} else {
			router.skipExpireCheck = true
		}
	}
}

// WithSkipJwtVerify 设置是否跳过JWT验证
func WithSkipJwtVerify(skipJwtVerify ...bool) VortexHttpRouterOptions {
	return func(router *VortexHttpRouter) {
		if len(skipJwtVerify) > 0 {
			router.skipJwtVerify = skipJwtVerify[0]
		} else {
			router.skipJwtVerify = true
		}
	}
}

// WithConsoleRouter 设置是否是控制台路由
func WithConsoleRouter() VortexHttpRouterOptions {
	return func(router *VortexHttpRouter) {
		router.isConsoleRouter = true
	}
}

// WithMiddlewares 设置中间件
func WithMiddlewares(middlewares ...VortexHttpMiddleware) VortexHttpRouterOptions {
	return func(router *VortexHttpRouter) {
		router.middlewares = append(router.middlewares, middlewares...)
	}
}

func (router *VortexHttpRouter) IsSkipExpireCheck() bool {
	return router.skipExpireCheck
}

func (router *VortexHttpRouter) IsConsoleRouter() bool {
	return router.isConsoleRouter
}

func (router *VortexHttpRouter) IsSkipJwtVerify() bool {
	return router.skipJwtVerify
}

// 是静态资源
func (router *VortexHttpRouter) IsStatic() bool {
	return router.isStatic
}

// GetPath 获取路由路径
func (router *VortexHttpRouter) GetPath() string {
	return router.path
}

// GetMethods 获取路由方法
func (router *VortexHttpRouter) GetMethods() []string {
	return router.methods
}

// GetHandles 获取路由处理函数
func (router *VortexHttpRouter) GetHandles() []gin.HandlerFunc {
	return router.handles
}

// 创建根路由组
func NewRootGroup(relativePath string, middlewares ...VortexHttpMiddleware) *VortexHttpRouterGroup {
	return &VortexHttpRouterGroup{
		relativePath: relativePath,
		middlewares:  middlewares,
	}
}
