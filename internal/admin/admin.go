// Package admin 提供机器人管理后台：基于 vortexv3（Gin）的 REST API 与
// 内嵌单页 Dashboard（go:embed，无外部静态文件依赖）。
//
// 鉴权：账号密码登录换取会话 Cookie（7 天有效）。
// 内置默认账号 admin/admin，首次登录强制重新设置用户名与密码，
// 之后一切登录均以该账号为准（账号文件落盘，重启不变）。
//
// 路由一览：
//
//	GET    /                  管理页面（静态，不鉴权）
//	GET    /api/session       会话状态（公开）
//	POST   /api/login         登录（公开）
//	POST   /api/logout        退出登录
//	POST   /api/account       重新设置账号（用户名+密码）
//	GET    /api/status        运行状态（运行时长/模型/任务与记忆数量/语音模式）
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
	"encoding/hex"
	_ "embed"
	"net/http"
	"strings"
	"sync"
	"time"

	vortex "github.com/capyflow/vortexv3"
	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/tools"
)

//go:embed dashboard.html
var dashboardHTML []byte

const (
	sessionCookie = "friday_admin_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// Info 是由 main 注入的机器人静态运行信息（展示用）。
type Info struct {
	VoiceEnabled bool
	Keywords     []string
	Model        string
	BaseURL      string
	TaskFile     string
	MemoryFile   string
}

// Server 是管理后台。
type Server struct {
	port      int
	accounts  *AccountStore
	sessMu    sync.Mutex
	sessions  map[string]time.Time // 会话 token → 过期时刻（内存态，重启需重新登录）
	engine    *task.Engine
	memory    *memory.Store
	schedule  *tools.ScheduleTaskTool // 复用 LLM 工具的时间解析与建任务逻辑
	info      Info
	startedAt time.Time
}

// New 创建管理后台。accounts 为账号仓库（默认 admin/admin，首次登录强制改号）。
func New(port int, accounts *AccountStore, eng *task.Engine, mem *memory.Store, info Info) *Server {
	return &Server{
		port:      port,
		accounts:  accounts,
		sessions:  make(map[string]time.Time),
		engine:    eng,
		memory:    mem,
		schedule:  &tools.ScheduleTaskTool{Engine: eng},
		info:      info,
		startedAt: time.Now(),
	}
}

// Start 启动 HTTP 服务（vortex 在后台 goroutine 中运行，不阻塞）。
func (s *Server) Start(_ context.Context) {
	root := vhttp.NewRootGroup("/")
	root.AddStaticRouter("/", s.handleDashboard)
	root.AddRouter([]string{http.MethodGet}, "/api/session", s.wrapPublic(s.handleSession))
	root.AddRouter([]string{http.MethodPost}, "/api/login", s.wrapPublic(s.handleLogin))
	root.AddRouter([]string{http.MethodPost}, "/api/logout", s.wrap(s.handleLogout))
	root.AddRouter([]string{http.MethodPost}, "/api/account", s.wrap(s.handleUpdateAccount))
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
			"authenticated": true, "username": acc.Username, "mustChange": acc.MustChange,
		})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"authenticated": false})
}

// handleLogin 账号密码登录；首次（默认账号）登录返回 mustChange=true，
// 前端强制进入"重新设置账号"流程。
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
		"username":   acc.Username,
		"mustChange": acc.MustChange,
	})
}

// handleLogout 退出登录，销毁会话并清 Cookie。
func (s *Server) handleLogout(ctx *vhttp.Context) error {
	s.dropSession(s.sessionToken(ctx))
	ctx.GinContext().SetCookie(sessionCookie, "", -1, "/", "", false, true)
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已退出"})
}

// handleUpdateAccount 重新设置账号（首次登录强制；之后也可随时修改）。
func (s *Server) handleUpdateAccount(ctx *vhttp.Context) error {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if err := s.accounts.Update(req.Username, req.Password); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message":    "账号已更新",
		"username":   strings.TrimSpace(req.Username),
		"mustChange": false,
	})
}

// ---------- 页面 ----------

// handleDashboard 返回内嵌的管理页面。
func (s *Server) handleDashboard(ctx *vhttp.Context) error {
	ctx.GinContext().Data(http.StatusOK, "text/html; charset=utf-8", dashboardHTML)
	return nil
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
		"voice":         map[string]any{"enabled": s.info.VoiceEnabled, "keywords": s.info.Keywords},
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
