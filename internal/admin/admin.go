// Package admin 提供机器人管理后台：基于 vortexv3（Gin）的 REST API 与
// 内嵌单页 Dashboard（go:embed，无外部静态文件依赖）。
//
// 鉴权：账号密码登录换取会话 Cookie（7 天有效）。
// 账号来源：启动时由配置文件（admin.username/password）或环境变量
// （FRIDAY_ADMIN_USERNAME/FRIDAY_ADMIN_PASSWORD）预置；两者都未提供时
// 使用默认账号 admin/admin。密码以 bcrypt 哈希落盘。
//
// 路由一览：
//
//	GET    /                  应用外壳（登录 + 聊天/运行状态/自动化/记忆/设置）
//	POST   /api/chat          对话接口（需登录，SSE 流式，web.Chat 处理）
//	POST   /api/upload        图片上传（需登录，multipart 字段 file）
//	POST   /api/provider      热更新 LLM baseURL/model（需登录，写回 config.yaml）
//	POST   /api/context/clear 清除对话上下文（需登录）
//	GET    /admin /traces     旧地址，重定向回 /
//	GET    /api/traces        调用轨迹列表（鉴权）
//	GET    /api/traces/:id    单次调用轨迹详情（鉴权）
//	GET    /api/session       会话状态（公开）
//	POST   /api/login         登录（公开）
//	POST   /api/logout        退出登录
//	GET    /api/status        运行状态（运行时长/模型/任务与记忆数量）
//	GET    /api/tasks         任务列表（含下次触发时间）
//	POST   /api/tasks         手工建任务（参数同 schedule_task 工具：in/when/every/times）
//	DELETE /api/tasks/:id     取消任务
//	GET    /api/memories?q=   长期记忆列表/搜索
//	POST   /api/memories      新增记忆
//	DELETE /api/memories/:id  删除记忆
package admin

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	vortex "github.com/capyflow/vortexv3"
	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/mcp"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/tools"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/web"
)

//go:embed index.html
var indexHTML []byte

const (
	sessionCookie = "friday_admin_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// Info 是由 main 注入的机器人静态运行信息（展示用）。
type Info struct {
	Model      string
	BaseURL    string
	TaskFile   string
	MemoryFile string
}

// Server 是管理后台，同时承载网页聊天（web.Chat 挂载在同一端口）。
type Server struct {
	port          int
	accounts      *AccountStore
	sessMu        sync.Mutex
	sessions      map[string]time.Time // 会话 token → 过期时刻（内存态，重启需重新登录）
	engine        *task.Engine
	memory        *memory.Store
	schedule      *tools.ScheduleTaskTool  // 复用 LLM 工具的时间解析与建任务逻辑
	chat          *web.Chat                // 网页聊天（可为 nil：不挂载聊天入口）
	cfg           *config.Config           // 运行配置（热更新直接改写其中的 provider 字段）
	regFn         func() *vxagent.Registry // 工具注册表 getter（MCP 重建后指针会变）
	configPath    string                   // config.yaml 路径（热更新写回）
	approver      *approval.Queue          // 危险操作审批队列（可为 nil）
	mcp           *mcp.Manager             // MCP 服务器管理器（可为 nil）
	heartbeatFile string                   // HEARTBEAT.md 路径
	info          Info
	startedAt     time.Time
}

// Options 汇总管理后台的构建参数。
type Options struct {
	Port          int
	Accounts      *AccountStore
	Engine        *task.Engine
	Memory        *memory.Store
	Info          Info
	Chat          *web.Chat
	Cfg           *config.Config
	ConfigPath    string
	Registry      func() *vxagent.Registry // 注册表 getter（热更新/MCP 重建用）
	Approver      *approval.Queue
	Mcp           *mcp.Manager
	HeartbeatFile string
}

// New 创建管理后台。accounts 为账号仓库（默认 admin/admin，首次登录强制改号）；
// chat 为网页聊天服务（可为 nil）；cfg/registry/configPath 支撑模型热更新
// （三者任一为零值则不挂载热更新接口）；approver/mcp 支撑审批卡片与 MCP 状态页（可为 nil）。
func New(o Options) *Server {
	return &Server{
		port:          o.Port,
		accounts:      o.Accounts,
		sessions:      make(map[string]time.Time),
		engine:        o.Engine,
		memory:        o.Memory,
		schedule:      &tools.ScheduleTaskTool{Engine: o.Engine},
		chat:          o.Chat,
		cfg:           o.Cfg,
		regFn:         o.Registry,
		configPath:    o.ConfigPath,
		approver:      o.Approver,
		mcp:           o.Mcp,
		heartbeatFile: o.HeartbeatFile,
		info:          o.Info,
		startedAt:     time.Now(),
	}
}

// Start 启动 HTTP 服务（vortex 在后台 goroutine 中运行，不阻塞）。
func (s *Server) Start(_ context.Context) {
	root := vhttp.NewRootGroup("/")
	// 应用外壳（登录 + 聊天/运行状态/自动化/记忆/设置）：公开下发，数据接口各自鉴权。
	root.AddStaticRouter("/", s.handleIndex)
	if s.chat != nil {
		root.AddRouter([]string{http.MethodPost}, "/api/chat", s.wrap(s.chat.HandleSSE))
		root.AddRouter([]string{http.MethodPost}, "/api/upload", s.wrap(s.chat.HandleUpload))
		root.AddRouter([]string{http.MethodPost}, "/api/context/clear", s.wrap(s.handleClearContext))
		root.AddRouter([]string{http.MethodGet}, "/api/sessions", s.wrap(s.handleListSessions))
		root.AddRouter([]string{http.MethodGet}, "/api/chat/history", s.wrap(s.handleChatHistory))
	}
	if s.cfg != nil && s.regFn != nil && s.configPath != "" {
		root.AddRouter([]string{http.MethodPost}, "/api/provider", s.wrap(s.handleUpdateProvider))
	}
	if s.heartbeatFile != "" {
		root.AddRouter([]string{http.MethodGet}, "/api/heartbeat", s.wrap(s.handleGetHeartbeat))
		root.AddRouter([]string{http.MethodPost}, "/api/heartbeat", s.wrap(s.handleSetHeartbeat))
	}
	if s.approver != nil {
		root.AddRouter([]string{http.MethodGet}, "/api/approvals", s.wrap(s.handleListApprovals))
		root.AddRouter([]string{http.MethodPost}, "/api/approvals/:id", s.wrap(s.handleDecideApproval))
	}
	if s.mcp != nil {
		root.AddRouter([]string{http.MethodGet}, "/api/mcp", s.wrap(s.handleMCPStatus))
		if s.cfg != nil && s.configPath != "" {
			root.AddRouter([]string{http.MethodGet}, "/api/mcp/config", s.wrap(s.handleMCPConfigGet))
			root.AddRouter([]string{http.MethodPost}, "/api/mcp/config", s.wrap(s.handleMCPConfigSet))
		}
		root.AddRouter([]string{http.MethodPost}, "/api/mcp/:name/reconnect", s.wrap(s.handleMCPReconnect))
	}
	// 旧页面地址兼容：统一回到首页外壳。
	root.AddStaticRouter("/admin", s.handleRedirectRoot)
	root.AddStaticRouter("/traces", s.handleRedirectRoot)
	root.AddRouter([]string{http.MethodGet}, "/api/session", s.wrapPublic(s.handleSession))
	root.AddRouter([]string{http.MethodPost}, "/api/login", s.wrapPublic(s.handleLogin))
	root.AddRouter([]string{http.MethodGet}, "/api/traces", s.wrap(s.handleListTraces))
	root.AddRouter([]string{http.MethodGet}, "/api/traces/:id", s.wrap(s.handleGetTrace))
	root.AddRouter([]string{http.MethodPost}, "/api/logout", s.wrap(s.handleLogout))
	root.AddRouter([]string{http.MethodGet}, "/api/status", s.wrap(s.handleStatus))
	root.AddRouter([]string{http.MethodGet}, "/api/tasks", s.wrap(s.handleListTasks))
	root.AddRouter([]string{http.MethodPost}, "/api/tasks", s.wrap(s.handleCreateTask))
	root.AddRouter([]string{http.MethodDelete}, "/api/tasks/:id", s.wrap(s.handleCancelTask))
	root.AddRouter([]string{http.MethodGet}, "/api/memories", s.wrap(s.handleListMemories))
	root.AddRouter([]string{http.MethodPost}, "/api/memories", s.wrap(s.handleAddMemory))
	root.AddRouter([]string{http.MethodDelete}, "/api/memories/:id", s.wrap(s.handleDeleteMemory))

	eng := vortex.NewVortexEngine(context.Background(),
		vortex.WithPort(s.port),
		vortex.WithHttpRouterRootGroup(root),
	)
	eng.Start()
}

// ---------- 会话 ----------

// newSession 生成会话 token 并登记过期时刻。
func (s *Server) newSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("admin: 生成会话 token 失败: " + err.Error())
	}
	token := hex.EncodeToString(b)
	s.sessMu.Lock()
	s.sessions[token] = time.Now().Add(sessionTTL)
	s.sessMu.Unlock()
	return token
}

// validSession 校验会话 token；有效则顺带滑动续期。
func (s *Server) validSession(token string) bool {
	if token == "" {
		return false
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	exp, ok := s.sessions[token]
	if !ok || time.Now().After(exp) {
		delete(s.sessions, token)
		return false
	}
	s.sessions[token] = time.Now().Add(sessionTTL)
	return true
}

func (s *Server) dropSession(token string) {
	s.sessMu.Lock()
	delete(s.sessions, token)
	s.sessMu.Unlock()
}

func (s *Server) sessionToken(ctx *vhttp.Context) string {
	tok, err := ctx.GinContext().Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return tok
}

// wrap 要求已登录（Cookie 会话）。
func (s *Server) wrap(fn func(*vhttp.Context) error) func(*vhttp.Context) error {
	return func(ctx *vhttp.Context) error {
		if !s.validSession(s.sessionToken(ctx)) {
			return ctx.JsonResponse(vhttp.Codes.Unauthorized,
				map[string]string{"error": "未登录或会话已过期"})
		}
		return fn(ctx)
	}
}

// wrapPublic 公开接口（登录/会话状态），不要求登录。
func (s *Server) wrapPublic(fn func(*vhttp.Context) error) func(*vhttp.Context) error {
	return fn
}

// ---------- 鉴权接口 ----------

// handleSession 返回当前登录状态，前端据此显示登录页或主界面。
func (s *Server) handleSession(ctx *vhttp.Context) error {
	if s.validSession(s.sessionToken(ctx)) {
		acc := s.accounts.Current()
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
			"authenticated": true, "username": acc.Username,
		})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"authenticated": false})
}

// handleLogin 账号密码登录。
func (s *Server) handleLogin(ctx *vhttp.Context) error {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	acc, ok := s.accounts.Login(req.Username, req.Password)
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.Unauthorized, map[string]string{"error": "用户名或密码错误"})
	}
	token := s.newSession()
	ctx.GinContext().SetCookie(sessionCookie, token, int(sessionTTL.Seconds()), "/", "", false, true)
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"username": acc.Username,
	})
}

// handleLogout 退出登录，销毁会话并清 Cookie。
func (s *Server) handleLogout(ctx *vhttp.Context) error {
	s.dropSession(s.sessionToken(ctx))
	ctx.GinContext().SetCookie(sessionCookie, "", -1, "/", "", false, true)
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已退出"})
}

// ---------- 页面 ----------

// handleIndex 返回应用外壳（登录 + 聊天/运行状态/自动化/记忆/设置）。
func (s *Server) handleIndex(ctx *vhttp.Context) error {
	noCache(ctx)
	ctx.GinContext().Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	return nil
}

// handleUpdateProvider 热更新 LLM 供应商配置（baseURL/model）：
// 立即生效（重建 Agent，会话连续）并写回 config.yaml。
func (s *Server) handleUpdateProvider(ctx *vhttp.Context) error {
	var req struct {
		BaseURL string `json:"baseURL"`
		Model   string `json:"model"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if err := agent.ReloadProvider(s.cfg, s.configPath, s.regFn(), s.chat.Brain(), req.BaseURL, req.Model); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	s.info.Model, s.info.BaseURL = req.Model, req.BaseURL
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message":  "模型配置已热生效，并已写回 config.yaml",
		"provider": map[string]string{"model": req.Model, "baseURL": req.BaseURL},
	})
}

// handleRedirectRoot 旧页面地址统一回到首页外壳。
func (s *Server) handleRedirectRoot(ctx *vhttp.Context) error {
	ctx.GinContext().Redirect(http.StatusMovedPermanently, "/")
	return nil
}

// noCache 禁止浏览器缓存内嵌页面：页面随二进制内嵌更新，避免客户端用到旧版。
func noCache(ctx *vhttp.Context) {
	ctx.GinContext().Header("Cache-Control", "no-cache")
}

// handleListTraces 返回调用轨迹列表（摘要，不含逐条事件）。
func (s *Server) handleListTraces(ctx *vhttp.Context) error {
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"traces": trace.List()})
}

// handleGetTrace 返回单次调用的完整轨迹（含逐条事件）。
func (s *Server) handleGetTrace(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	t, ok := trace.Get(id)
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到轨迹 " + id})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"trace": t})
}

// ---------- 业务接口 ----------

// handleStatus 返回机器人运行状态。
func (s *Server) handleStatus(ctx *vhttp.Context) error {
	tasks := s.engine.List()
	byKind := map[string]int{}
	for _, t := range tasks {
		byKind[string(t.Kind)]++
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"startedAt":     s.startedAt.Format(time.RFC3339),
		"uptimeSeconds": int(time.Since(s.startedAt).Seconds()),
		"provider":      map[string]string{"model": s.info.Model, "baseURL": s.info.BaseURL},
		"tasks":         map[string]any{"total": len(tasks), "byKind": byKind},
		"memories":      s.memory.Count(),
		"files":         map[string]string{"tasks": s.info.TaskFile, "memories": s.info.MemoryFile},
	})
}

// handleListTasks 返回任务列表。
func (s *Server) handleListTasks(ctx *vhttp.Context) error {
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"tasks": s.engine.List()})
}

// handleCreateTask 手工建任务，参数与 schedule_task 工具一致
// （text 必填；in/when/every 三选一；times 配合 in 表示多次任务）。
func (s *Server) handleCreateTask(ctx *vhttp.Context) error {
	var args map[string]any
	if err := ctx.UnmarshalBody(&args); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	msg, err := s.schedule.Call(ctx.Context(), args)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": msg,
		"tasks":   s.engine.List(),
	})
}

// handleCancelTask 取消一条任务。
func (s *Server) handleCancelTask(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	if !s.engine.Cancel(id) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到任务 #" + id})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "已取消任务 #" + id,
		"tasks":   s.engine.List(),
	})
}

// handleListMemories 记忆列表/搜索（?q= 关键词，留空返回全部）。
func (s *Server) handleListMemories(ctx *vhttp.Context) error {
	q := strings.TrimSpace(ctx.GinContext().Query("q"))
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"memories": s.memory.Search(q)})
}

// handleAddMemory 新增一条记忆。
func (s *Server) handleAddMemory(ctx *vhttp.Context) error {
	var req struct {
		Content string `json:"content"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	m, err := s.memory.Add(req.Content, "admin")
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"memory": m})
}

// handleDeleteMemory 删除一条记忆。
func (s *Server) handleDeleteMemory(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	if !s.memory.Delete(id) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到记忆 #" + id})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已删除记忆 #" + id})
}
