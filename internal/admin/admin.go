// Package admin 提供Friday 管理后台：基于 vortexv3（Gin）的 REST API 与
// 内嵌单页 Dashboard（go:embed，无外部静态文件依赖）。页面源码在仓库 web/ 目录
// （Vue 3 + Vite），npm run build 产物内联为自包含 HTML 落在 internal/admin/webdist/
// ——产物不入库（.gitignore），新克隆先构建前端再编译（Docker 构建自动包含该步）；
// 未构建时以占位页兜底，go build 始终可过。
//
// 鉴权：账号密码登录换取 JWT（HS256，7 天有效），之后请求带
// Authorization: Bearer <token>；token 缺失/失效返回 403。token 不落库，
// 签名密钥在数据目录（auth-key），换密码即令旧 token 失效。
// 账号来源：启动时由配置文件（admin.username/password）或环境变量
// （FRIDAY_ADMIN_USERNAME/FRIDAY_ADMIN_PASSWORD）预置；两者都未提供时
// 使用默认账号 admin/admin。密码以 bcrypt 哈希落盘。
//
// 路由一览：
//
//	GET    /                  应用外壳（登录 + 聊天/待办/运行状态/自动化/记忆/设置）
//	GET    /palette           全局快捷键弹窗页（桌面外壳加载，公开静态；对话仍走 /api/chat）
//	GET    /wizard            首装向导页（桌面外壳首装流程加载，公开静态；含建号表单）
//	POST   /api/chat          对话接口（需登录，SSE 流式，web.Chat 处理）
//	POST   /api/chat/stop     暂停当前回复（需登录，已生成部分可继续）
//	POST   /api/chat/resume   继续被暂停的对话（需登录，SSE 流式）
//	POST   /api/upload        图片上传（需登录，multipart 字段 file）
//	POST   /api/provider      热更新 LLM baseURL/model（需登录，写回 config.yaml）
//	GET    /api/provider/models 模型库列表与激活条目（未建库时用扁平字段合成一条，需登录）
//	POST   /api/provider/models 保存模型库并激活指定条目（校验/密钥继承/按变化范围热重建，需登录）
//	POST   /api/provider/activate 切换激活模型（聊天页快捷切换，库保持原样，需登录）
//	GET    /api/persona       人设提示词配置（当前生效 ID + 全部预设，需登录）
//	POST   /api/persona       切换人设/保存自定义人设（需登录，热生效并写回 config.yaml）
//	GET    /api/permissions   全局权限模式与审批策略（需登录）
//	POST   /api/permissions   切换权限模式 confirm/acceptEdits/fullAccess（需登录，热生效并写回 config.yaml）
//	GET    /api/runtime       内置管家运行上限（当前工具轮数上限与可填区间，需登录）
//	POST   /api/runtime       保存工具轮数上限（需登录，热生效并写回 config.yaml；maxIterations=0 恢复默认）
//	POST   /api/context/clear 清除对话上下文（需登录）
//	GET    /api/context       会话上下文占用快照（活跃区估算 token/窗口/比例，需登录）
//	POST   /api/context/compact 手动压缩一次上下文（归档旧块并重写活跃历史，需登录）
//	GET    /api/sessions      网页会话列表（主会话 + 后台入口会话，带归属与活跃时刻，需登录）
//	GET    /admin /traces     旧地址，重定向回 /
//	GET    /api/traces        调用轨迹列表（鉴权）
//	GET    /api/traces/:id    单次调用轨迹详情（鉴权）
//	GET    /api/session       会话状态（公开）
//	GET    /api/acp/config    智能体配置（全局参数 + agent 清单，需登录）
//	POST   /api/acp/config    保存智能体配置（写回 config.yaml 并热生效，需登录）
//	GET    /api/acp/threads   工作区列表 + 会话列表 + 配置（需登录）
//	POST   /api/acp/threads   新建会话（在工作区里绑定 agent 与主目录，需登录）
//	POST   /api/acp/workspaces       新建工作区（名字 + 目录列表，需登录）
//	POST   /api/acp/workspaces/:id   工作区改名/置顶/换主目录（字段可选，需登录）
//	POST   /api/acp/workspaces/:id/dirs 工作区加目录 {"path"}（需登录）
//	DELETE /api/acp/workspaces/:id/dirs?path= 工作区移目录（需登录）
//	GET    /api/acp/workspaces/:id/memory 工作区项目记忆列表（需登录）
//	POST   /api/acp/workspaces/:id/memory 新建/更新项目记忆 {"id?","title","body"}（需登录）
//	GET    /api/acp/workspaces/:id/memory/:page 读取一页项目记忆（需登录）
//	DELETE /api/acp/workspaces/:id/memory/:page 删除一页项目记忆（需登录）
//	DELETE /api/acp/workspaces/:id   删工作区连带其会话（需登录）
//	GET    /api/acp/threads/:id      会话详情含投影消息与目录视图（需登录）
//	POST   /api/acp/threads/:id      改会话标题 {"title"}（留空回到自动，需登录）
//	DELETE /api/acp/threads/:id      删除会话（需登录）
//	POST   /api/acp/threads/:id/chat 智能体对话（SSE 流式，帧协议同 /api/chat，需登录）
//	POST   /api/acp/threads/:id/stop 取消进行中的一轮（需登录）
//	GET    /api/acp/threads/:id/options 会话配置项（模型/模式/推理档位下拉，需登录）
//	POST   /api/acp/threads/:id/options 切换配置项取值 {"id","value"}（需登录）
//	GET    /api/acp/threads/:id/files   工作区文件树（?path=相对路径，需登录）
//	GET    /api/acp/threads/:id/changes 某轮改动详情（?turn=，含逐文件 diff，需登录）
//	POST   /api/acp/threads/:id/changes/revert 还原本轮改动 {"turn"}（需登录）
//	POST   /api/acp/threads/:id/handoff 生成交接摘要并新开续会话（需登录）
//	GET    /api/acp/threads/:id/checks  验证命令记录与建议命令（需登录）
//	POST   /api/acp/threads/:id/checks  提交验证命令 {"command","timeoutSec?"}（需登录，进审批队列）
//	GET    /api/acp/threads/:id/checks/:run 单次验证运行状态与输出（需登录）
//	POST   /api/acp/threads/:id/checks/:run/stop 停止验证运行（需登录）
//	GET    /api/runs          运行看板：ACP 轮次/计划/目标/任务/审批/失败（需登录）
//	POST   /api/acp/test      agent 握手诊断（设置页「测试」按钮，需登录）
//	GET    /api/acp/catalog   可安装 agent 目录与本机安装状态（需登录）
//	POST   /api/acp/install   执行目录 agent 的官方安装命令并注册（需登录）
//	GET    /api/acp/dirs      列出某目录的子目录（新建对话选目录用，只读，需登录）
//	GET    /api/terminal      内置终端可用状态（enabled/上限/活跃数，需登录）
//	GET    /api/terminal/ws   内置终端（WebSocket，一条连接一个 PTY；只接受本机回环，需登录）
//	GET    /api/files/preview 只读读取本机文件内容供侧边预览（?path=，需登录）
//	POST   /api/login         登录（公开）
//	POST   /api/logout        退出登录
//	GET    /api/status        运行状态（运行时长/模型/待办任务与记忆数量）
//	GET    /api/tasks         任务列表（含下次触发时间）
//	POST   /api/tasks         手工建任务（参数同 schedule_task 工具：in/when/every/times）
//	DELETE /api/tasks/:id     取消任务
//	GET    /api/entries          闪念列表（笔记+待办，?kind=&q=&tag=&done= 过滤）
//	POST   /api/entries          记入闪念（{"kind":"note"|"todo","text",dueAt?,remindAt?}）
//	PUT    /api/entries/:id      编辑条目（完整替换文本与时间）
//	POST   /api/entries/reconcile 重建待办关联提醒
//	POST   /api/entries/:id      完成/重开待办（{"done":true|false}，缺省 true；笔记拒绝）
//	POST   /api/entries/:id/pin  置顶/取消置顶（{"pinned":true|false}，缺省 true）
//	DELETE /api/entries/:id      删除条目
//	GET    /api/wiki/pages      记忆页列表/搜索（?q= 关键词，命中附片段）
//	POST   /api/wiki/pages      新建记忆页（标题已存在则更新该页）
//	GET    /api/wiki/pages/:id  记忆页详情（含原始 markdown 正文）
//	PUT    /api/wiki/pages/:id  更新记忆页
//	DELETE /api/wiki/pages/:id  删除记忆页
//	GET    /api/usage         token 用量账本（按天明细 + 按模型/来源/会话/运行分桶，需登录）
//	GET    /api/cost          成本态势与预算（三态 + 主动任务暂停标记，需登录）
//	POST   /api/cost/pause    暂停主动型工作（心跳/智能任务/目标；不写配置，需登录）
//	POST   /api/cost/resume   恢复主动型工作（需登录）
//	GET    /api/cost/pricing  模型价格表 + 有用量但未配价的模型（需登录）
//	POST   /api/cost/pricing  保存价格表（校验后原子写 pricing.json 并热加载，需登录）
//	GET    /api/skills        技能列表（含无效文件原因与所在目录，需登录）
//	POST   /api/skills/reload 重新扫描技能目录（需登录）
//	GET    /api/skills/:id    单个技能的摘要与 SKILL.md 原文（需登录）
//	POST   /api/skills        新建技能（校验失败返回原因，不落盘，需登录）
//	PUT    /api/skills/:id    编辑技能（需登录）
//	POST   /api/skills/:id/enabled 启用/停用技能（只改 frontmatter 一个键，需登录）
//	DELETE /api/skills/:id    删除技能（用户目录技能拒绝，需登录）
//	GET    /api/goals         目标列表（进度与预算，需登录）
//	GET    /api/goals/:id   单个目标详情与该目标的执行轮次（需登录）
//	POST   /api/goals/:id/stop 在步边界停止当前目标运行（需登录）
//	GET    /api/plans         计划列表（?status=active|awaiting 过滤，需登录）
//	GET    /api/plans/:id     计划详情（含每步状态与 token，需登录）
//	POST   /api/plans/:id/confirm 确认或否决计划（可带编辑后的步骤，需登录）
//	POST   /api/plans/:id/pause|resume|cancel 暂停/继续/取消计划（需登录）
//	POST   /api/plans/:id/steps/:n/retry|skip 重试或跳过某一步（需登录）
package admin

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/llm"
	vortex "github.com/capyflow/vortexv3"
	vhttp "github.com/capyflow/vortexv3/server/http"
	"github.com/gin-gonic/gin"

	"github.com/capyflow/friday/internal/acp"
	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/cost"
	"github.com/capyflow/friday/internal/mcp"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/notification"
	"github.com/capyflow/friday/internal/plan"
	"github.com/capyflow/friday/internal/skill"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/terminal"
	"github.com/capyflow/friday/internal/todo"
	"github.com/capyflow/friday/internal/tools"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/web"
)

// 前端构建产物不入库（.gitignore），这里嵌入整个目录（.gitkeep 保证空目录可编译）；
// 未构建时（新克隆直接 go build / go test）回退到下方占位页。
//
//go:embed all:webdist
var webdistFS embed.FS

// placeholderHTML 是产物缺失时的占位页：fresh clone 不跑 npm run build 也能
// 编译、启动，页面明确告知构建方法。
var placeholderHTML = []byte(`<!doctype html><html lang="zh"><meta charset="utf-8">
<title>Friday</title><body style="font-family:system-ui;max-width:560px;margin:12vh auto;padding:0 24px">
<h2>前端尚未构建</h2>
<p>当前二进制内没有网页产物（构建产物不入库）。在仓库根目录执行：</p>
<pre><code>cd web && npm ci && npm run build && cd .. && go build -o friday ./cmd/robot</code></pre>
<p>重新编译后刷新本页即可。或直接使用 Docker：<code>docker compose up -d --build</code>（自动构建前端）。</p>
</body></html>`)

var placeholderPalette = []byte(`<!doctype html><html lang="zh"><meta charset="utf-8">
<title>Friday 快捷面板</title><body style="font-family:system-ui;margin:8vh auto;max-width:480px;padding:0 24px">
<p>前端尚未构建，快捷面板不可用。构建方法见主页面占位说明（<code>cd web && npm run build</code>）。</p>
</body></html>`)

var placeholderWizard = []byte(`<!doctype html><html lang="zh"><meta charset="utf-8">
<title>Friday 初始设置</title><body style="font-family:system-ui;margin:8vh auto;max-width:480px;padding:0 24px">
<p>前端尚未构建，首装向导不可用。构建方法见主页面占位说明（<code>cd web && npm run build</code>）。</p>
</body></html>`)

// webdistFile 读取内嵌产物，缺失（未构建）返回 false。
func webdistFile(name string) ([]byte, bool) {
	b, err := webdistFS.ReadFile("webdist/" + name)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// Info 是由 main 注入的管家静态运行信息（展示用）。
type Info struct {
	Model     string
	BaseURL   string
	TaskFile  string
	TodoFile  string
	MemoryDir string
}

// Server 是管理后台，同时承载网页聊天（web.Chat 挂载在同一端口）。
type Server struct {
	host          string
	port          int
	accounts      *AccountStore
	authKey       []byte // JWT 签名密钥（落盘在数据目录，重启后仍有效）
	authMu        sync.Mutex
	revoked       map[string]time.Time // 已退出登录的 token → 过期时刻（内存态）
	engine        *task.Engine
	todos         *todo.Store
	memory        *memory.WikiStore
	schedule      *tools.ScheduleTaskTool  // 复用 LLM 工具的时间解析与建任务逻辑
	chat          *web.Chat                // 网页聊天（可为 nil：不挂载聊天入口）
	cfg           *config.Config           // 运行配置（热更新通过串行事务发布快照）
	regFn         func() *vxagent.Registry // 工具注册表 getter（MCP 重建后指针会变）
	configPath    string                   // config.yaml 路径（热更新写回）
	approver      *approval.Queue          // 危险操作审批队列（可为 nil）
	mcp           *mcp.Manager             // MCP 服务器管理器（可为 nil）
	acp           *acp.Manager             // 智能体（ACP）管理器（可为 nil）
	terminal      *terminal.Manager        // 内置终端管理器（可为 nil：不挂载 /api/terminal*）
	mcpUpdateMu   sync.Mutex
	acpUpdateMu   sync.Mutex
	skillUpdateMu sync.Mutex
	notifications *notification.Center
	heartbeatFile string // HEARTBEAT.md 路径
	pricer        *cost.Pricer
	guard         *cost.Guard
	skills        *skill.Store
	plans         *plan.Store
	planRunner    *plan.Runner
	goalRunner    *agent.GoalRunner
	info          Info
	startedAt     time.Time
}

// Options 汇总管理后台的构建参数。
type Options struct {
	Host        string
	Port        int
	Accounts    *AccountStore
	AuthKeyFile string // JWT 签名密钥文件；为空则用进程内随机密钥（测试）
	Engine      *task.Engine
	Todos       *todo.Store
	Memory      *memory.WikiStore
	Info        Info
	Chat        *web.Chat
	Cfg         *config.Config
	ConfigPath  string
	Registry    func() *vxagent.Registry // 注册表 getter（热更新/MCP 重建用）
	Approver    *approval.Queue
	Mcp         *mcp.Manager
	// Acp 智能体管理器（可为 nil：不挂载 /api/acp*，侧栏入口由前端按配置隐藏）。
	Acp *acp.Manager
	// Terminal 内置终端管理器（可为 nil：不挂载 /api/terminal*）。它只服务本机回环，
	// 见 terminal.go 的硬闸说明。
	Terminal      *terminal.Manager
	HeartbeatFile string
	Notifications *notification.Center
	// Pricer/Guard 支撑成本面板与软限额接口（可为 nil：不挂载 /api/cost*）。
	Pricer *cost.Pricer
	Guard  *cost.Guard
	// Skills 技能仓库（可为 nil：不挂载 /api/skills*，技能层整体关闭）。
	Skills *skill.Store
	// Plans/PlanRunner 支撑计划模式（任一为 nil 则不挂载 /api/plans*）；
	// GoalRunner 支撑目标模式的查看与软停（nil 则不挂载 /api/goals*）。
	Plans      *plan.Store
	PlanRunner *plan.Runner
	GoalRunner *agent.GoalRunner
}

// New 创建管理后台。accounts 为账号仓库（默认 admin/admin，可通过启动配置预置）；
// chat 为网页聊天服务（可为 nil）；cfg/registry/configPath 支撑模型热更新
// （三者任一为零值则不挂载热更新接口）；approver/mcp 支撑审批卡片与 MCP 状态页（可为 nil）。
func New(o Options) *Server {
	if strings.TrimSpace(o.Host) == "" {
		o.Host = "127.0.0.1"
	}
	return &Server{
		host:          o.Host,
		port:          o.Port,
		accounts:      o.Accounts,
		authKey:       loadOrCreateAuthKey(o.AuthKeyFile),
		revoked:       make(map[string]time.Time),
		engine:        o.Engine,
		todos:         o.Todos,
		memory:        o.Memory,
		schedule:      &tools.ScheduleTaskTool{Engine: o.Engine},
		chat:          o.Chat,
		cfg:           o.Cfg,
		regFn:         o.Registry,
		configPath:    o.ConfigPath,
		approver:      o.Approver,
		mcp:           o.Mcp,
		acp:           o.Acp,
		terminal:      o.Terminal,
		heartbeatFile: o.HeartbeatFile,
		info:          o.Info,
		startedAt:     time.Now(),
		notifications: o.Notifications,
		pricer:        o.Pricer,
		guard:         o.Guard,
		skills:        o.Skills,
		plans:         o.Plans,
		planRunner:    o.PlanRunner,
		goalRunner:    o.GoalRunner,
	}
}

// Handler 保留 vortexv3 路由/响应约定，由本服务管理监听地址和关闭生命周期。
func (s *Server) Handler() http.Handler {
	root := vhttp.NewRootGroup("/")
	// 应用外壳（登录 + 聊天/运行状态/自动化/记忆/设置）：公开下发，数据接口各自鉴权。
	root.AddStaticRouter("/", s.handleIndex)
	// 全局快捷键弹窗页（桌面外壳加载）：公开下发，对话仍走需登录的 /api/chat。
	root.AddStaticRouter("/palette", s.handlePalette)
	// 首装向导页（桌面外壳首装流程加载）：公开下发——向导本身就是建号入口。
	root.AddStaticRouter("/wizard", s.handleWizard)
	if s.chat != nil {
		root.AddRouter([]string{http.MethodPost}, "/api/chat", s.wrap(s.chat.HandleSSE))
		root.AddRouter([]string{http.MethodPost}, "/api/chat/stop", s.wrap(s.chat.HandleStop))
		root.AddRouter([]string{http.MethodPost}, "/api/chat/resume", s.wrap(s.chat.HandleResume))
		root.AddRouter([]string{http.MethodPost}, "/api/upload", s.wrap(s.chat.HandleUpload))
		root.AddRouter([]string{http.MethodGet}, "/api/upload/:id", s.wrapMedia(s.handleUploadGet))
		root.AddRouter([]string{http.MethodPost}, "/api/context/clear", s.wrap(s.handleClearContext))
		root.AddRouter([]string{http.MethodGet}, "/api/context", s.wrap(s.handleContextUsage))
		root.AddRouter([]string{http.MethodPost}, "/api/context/compact", s.wrap(s.handleContextCompact))
		root.AddRouter([]string{http.MethodGet}, "/api/sessions", s.wrap(s.handleListSessions))
		root.AddRouter([]string{http.MethodGet}, "/api/chat/history", s.wrap(s.handleChatHistory))
	}
	if s.cfg != nil && s.regFn != nil && s.configPath != "" {
		root.AddRouter([]string{http.MethodPost}, "/api/provider", s.wrap(s.handleUpdateProvider))
		root.AddRouter([]string{http.MethodGet}, "/api/provider/models", s.wrap(s.handleProviderModelsGet))
		if s.chat != nil {
			root.AddRouter([]string{http.MethodPost}, "/api/provider/models", s.wrap(s.handleProviderModelsSet))
			root.AddRouter([]string{http.MethodPost}, "/api/provider/activate", s.wrap(s.handleProviderActivate))
			root.AddRouter([]string{http.MethodGet}, "/api/persona", s.wrap(s.handleGetPersona))
			root.AddRouter([]string{http.MethodPost}, "/api/persona", s.wrap(s.handleSetPersona))
			root.AddRouter([]string{http.MethodGet}, "/api/permissions", s.wrap(s.handleGetPermissions))
			root.AddRouter([]string{http.MethodPost}, "/api/permissions", s.wrap(s.handleSetPermissions))
			// 运行上限（设置 → 能力与行为 → 运行上限）：单轮工具调用轮数。
			root.AddRouter([]string{http.MethodGet}, "/api/runtime", s.wrap(s.handleGetRuntime))
			root.AddRouter([]string{http.MethodPost}, "/api/runtime", s.wrap(s.handleSetRuntime))
		}
	}
	if s.cfg != nil {
		// 联网搜索配置（设置 → 扩展 → 工具）：读/存 tools.search + 真搜一次的测试端点。
		root.AddRouter([]string{http.MethodGet}, "/api/tools/search", s.wrap(s.handleToolsSearchGet))
		root.AddRouter([]string{http.MethodPost}, "/api/tools/search", s.wrap(s.handleToolsSearchSet))
		root.AddRouter([]string{http.MethodPost}, "/api/tools/search/test", s.wrap(s.handleToolsSearchTest))
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
		// OAuth 鉴权：发起浏览器授权、查状态（前端轮询）、断开（撤销令牌）。
		root.AddRouter([]string{http.MethodPost}, "/api/mcp/:name/authorize", s.wrap(s.handleMCPAuthorize))
		root.AddRouter([]string{http.MethodGet}, "/api/mcp/:name/auth", s.wrap(s.handleMCPAuthStatus))
		root.AddRouter([]string{http.MethodPost}, "/api/mcp/:name/disconnect", s.wrap(s.handleMCPDisconnect))
	}
	// 文件预览：时间线里输出的本机文件路径点开侧边预览（只读，需登录）。
	root.AddRouter([]string{http.MethodGet}, "/api/files/preview", s.wrap(s.handleFilePreview))
	// 智能体（ACP）工作区与对话：工作区 CRUD、会话 CRUD、SSE 对话流、停止、
	// 配置与握手诊断。
	if s.acp != nil {
		root.AddRouter([]string{http.MethodGet}, "/api/acp/config", s.wrap(s.handleAcpConfigGet))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/config", s.wrap(s.handleAcpConfigSet))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/threads", s.wrap(s.handleAcpThreads))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/threads", s.wrap(s.handleAcpThreadCreate))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/workspaces", s.wrap(s.handleAcpWorkspacesCreate))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/workspaces/:id", s.wrap(s.handleAcpWorkspacesUpdate))
		root.AddRouter([]string{http.MethodDelete}, "/api/acp/workspaces/:id", s.wrap(s.handleAcpWorkspaceDelete))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/workspaces/:id/dirs", s.wrap(s.handleAcpWorkspaceDirAdd))
		root.AddRouter([]string{http.MethodDelete}, "/api/acp/workspaces/:id/dirs", s.wrap(s.handleAcpWorkspaceDirRemove))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/workspaces/:id/memory", s.wrap(s.handleAcpWorkspaceMemoryList))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/workspaces/:id/memory", s.wrap(s.handleAcpWorkspaceMemorySave))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/workspaces/:id/memory/:page", s.wrap(s.handleAcpWorkspaceMemoryGet))
		root.AddRouter([]string{http.MethodDelete}, "/api/acp/workspaces/:id/memory/:page", s.wrap(s.handleAcpWorkspaceMemoryDelete))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/threads/:id", s.wrap(s.handleAcpThreadGet))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/threads/:id", s.wrap(s.handleAcpThreadRename))
		root.AddRouter([]string{http.MethodDelete}, "/api/acp/threads/:id", s.wrap(s.handleAcpThreadDelete))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/threads/:id/chat", s.wrap(s.handleAcpChat))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/threads/:id/stop", s.wrap(s.handleAcpStop))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/threads/:id/options", s.wrap(s.handleAcpOptionsGet))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/threads/:id/options", s.wrap(s.handleAcpOptionsSet))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/threads/:id/files", s.wrap(s.handleAcpThreadFiles))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/threads/:id/changes", s.wrap(s.handleAcpThreadChanges))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/threads/:id/changes/revert", s.wrap(s.handleAcpThreadChangesRevert))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/threads/:id/handoff", s.wrap(s.handleAcpThreadHandoff))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/threads/:id/checks", s.wrap(s.handleAcpThreadChecks))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/threads/:id/checks", s.wrap(s.handleAcpThreadCheckStart))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/threads/:id/checks/:run", s.wrap(s.handleAcpThreadCheckRun))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/threads/:id/checks/:run/stop", s.wrap(s.handleAcpThreadCheckStop))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/test", s.wrap(s.handleAcpTest))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/catalog", s.wrap(s.handleAcpCatalog))
		root.AddRouter([]string{http.MethodPost}, "/api/acp/install", s.wrap(s.handleAcpInstall))
		root.AddRouter([]string{http.MethodGet}, "/api/acp/dirs", s.wrap(s.handleAcpDirs))
	}
	if s.terminal != nil {
		// 终端要长连接：token 走 ?token=（浏览器 WebSocket 加不了请求头），
		// 所以用 wrapMedia 这层"头或查询参数都认"的鉴权。
		root.AddRouter([]string{http.MethodGet}, "/api/terminal", s.wrap(s.handleTerminalInfo))
		root.AddRouter([]string{http.MethodGet}, "/api/terminal/ws", s.wrapMedia(s.handleTerminalWS))
	}
	// 运行看板：只读聚合；具体动作仍走各自接口。
	root.AddRouter([]string{http.MethodGet}, "/api/runs", s.wrap(s.handleRuns))
	// 成本面板 / 预算闸门 / 价格表（未装配时跳过）。
	s.registerCostRoutes(root)
	// 技能管理（技能层关闭时跳过）。
	s.registerSkillRoutes(root)
	// 计划模式（未启用时跳过）。
	s.registerPlanRoutes(root)
	// 目标模式（未启用时跳过）。
	s.registerGoalRoutes(root)
	// 旧页面地址兼容：统一回到首页外壳。
	root.AddStaticRouter("/admin", s.handleRedirectRoot)
	root.AddStaticRouter("/traces", s.handleRedirectRoot)
	root.AddRouter([]string{http.MethodGet}, "/api/session", s.wrapPublic(s.handleSession))
	root.AddRouter([]string{http.MethodGet}, "/api/setup/status", s.wrapPublic(s.handleSetupStatus))
	root.AddRouter([]string{http.MethodPost}, "/api/setup", s.wrapPublic(s.handleSetup))
	root.AddRouter([]string{http.MethodPost}, "/api/provider/test", s.wrap(s.handleProviderTest))
	root.AddRouter([]string{http.MethodPost}, "/api/login", s.wrapPublic(s.handleLogin))
	root.AddRouter([]string{http.MethodGet}, "/api/traces", s.wrap(s.handleListTraces))
	root.AddRouter([]string{http.MethodGet}, "/api/traces/:id", s.wrap(s.handleGetTrace))
	root.AddRouter([]string{http.MethodGet}, "/api/usage", s.wrap(s.handleUsage))
	root.AddRouter([]string{http.MethodPost}, "/api/logout", s.wrap(s.handleLogout))
	root.AddRouter([]string{http.MethodGet}, "/api/status", s.wrap(s.handleStatus))
	root.AddRouter([]string{http.MethodGet}, "/api/tasks", s.wrap(s.handleListTasks))
	root.AddRouter([]string{http.MethodPost}, "/api/tasks", s.wrap(s.handleCreateTask))
	root.AddRouter([]string{http.MethodPut}, "/api/tasks/:id", s.wrap(s.handleUpdateTask))
	root.AddRouter([]string{http.MethodPost}, "/api/tasks/:id/pause", s.wrap(s.handlePauseTask))
	root.AddRouter([]string{http.MethodPost}, "/api/tasks/:id/resume", s.wrap(s.handleResumeTask))
	root.AddRouter([]string{http.MethodDelete}, "/api/tasks/:id", s.wrap(s.handleCancelTask))
	root.AddRouter([]string{http.MethodGet}, "/api/entries", s.wrap(s.handleListEntries))
	root.AddRouter([]string{http.MethodPost}, "/api/entries", s.wrap(s.handleAddEntry))
	root.AddRouter([]string{http.MethodPut}, "/api/entries/:id", s.wrap(s.handleUpdateEntry))
	root.AddRouter([]string{http.MethodPost}, "/api/entries/reconcile", s.wrap(s.handleReconcileEntries))
	root.AddRouter([]string{http.MethodPost}, "/api/entries/:id", s.wrap(s.handleCompleteEntry))
	root.AddRouter([]string{http.MethodPost}, "/api/entries/:id/pin", s.wrap(s.handlePinEntry))
	root.AddRouter([]string{http.MethodDelete}, "/api/entries/:id", s.wrap(s.handleDeleteEntry))
	root.AddRouter([]string{http.MethodGet}, "/api/wiki/pages", s.wrap(s.handleListWikiPages))
	root.AddRouter([]string{http.MethodPost}, "/api/wiki/pages", s.wrap(s.handleCreateWikiPage))
	root.AddRouter([]string{http.MethodGet}, "/api/wiki/pages/:id", s.wrap(s.handleGetWikiPage))
	root.AddRouter([]string{http.MethodPut}, "/api/wiki/pages/:id", s.wrap(s.handleUpdateWikiPage))
	root.AddRouter([]string{http.MethodDelete}, "/api/wiki/pages/:id", s.wrap(s.handleDeleteWikiPage))
	if s.notifications != nil {
		root.AddRouter([]string{http.MethodGet}, "/api/notifications", s.wrap(s.handleNotifications))
		root.AddRouter([]string{http.MethodPost}, "/api/notifications/:id/retry", s.wrap(s.handleRetryNotification))
		root.AddRouter([]string{http.MethodPost}, "/api/notifications/:id/read", s.wrap(s.handleReadNotification))
		root.AddRouter([]string{http.MethodPost}, "/api/notifications/:id/snooze", s.wrap(s.handleSnoozeNotification))
	}

	var handler *gin.Engine
	_ = vortex.NewVortexEngine(context.Background(),
		vortex.WithPort(s.port),
		vortex.WithHttpServerOptions(func(e *gin.Engine) {
			handler = e
			_ = e.SetTrustedProxies([]string{"127.0.0.1", "::1"})
		}),
		vortex.WithHttpRouterRootGroup(root),
	)
	return handler
}

// Start 在绑定失败时同步返回错误，默认仅监听本机；取消 ctx 时关闭 HTTP 服务。
func (s *Server) Start(ctx context.Context) error {
	l, err := net.Listen("tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		if err := server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[管理后台] HTTP 服务退出: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	return nil
}

// ---------- 会话 ----------

// wrap 要求已登录（Authorization: Bearer JWT）。
func (s *Server) wrap(fn func(*vhttp.Context) error) func(*vhttp.Context) error {
	return func(ctx *vhttp.Context) error {
		if !s.authenticated(bearerToken(ctx.GinContext().Request)) {
			return forbidden(ctx)
		}
		return fn(ctx)
	}
}

// wrapMedia 用于 <img> 这类无法带自定义头的读取：允许 header，也允许 ?token=
// （仅附件回读走这条路，token 只在本机页面 URL 上出现）。
func (s *Server) wrapMedia(fn func(*vhttp.Context) error) func(*vhttp.Context) error {
	return func(ctx *vhttp.Context) error {
		raw := bearerToken(ctx.GinContext().Request)
		if raw == "" {
			raw = strings.TrimSpace(ctx.GinContext().Query("token"))
		}
		if !s.authenticated(raw) {
			return forbidden(ctx)
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
	if s.authenticated(bearerToken(ctx.GinContext().Request)) {
		acc := s.accounts.Current()
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
			"authenticated": true, "username": acc.Username,
		})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"authenticated": false})
}

// handleLogin 账号密码登录：校验通过后下发 JWT，前端自行保存并随请求携带。
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
	token, err := s.issueToken(acc.Username)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "签发登录凭据失败，请重试"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"username": acc.Username,
		"token":    token,
	})
}

// handleLogout 退出登录：吊销当前 token（前端同时清掉本地保存）。
func (s *Server) handleLogout(ctx *vhttp.Context) error {
	s.revokeToken(bearerToken(ctx.GinContext().Request))
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已退出"})
}

// ---------- 页面 ----------

// handleIndex 返回应用外壳（登录 + 聊天/运行状态/自动化/记忆/设置）。
func (s *Server) handleIndex(ctx *vhttp.Context) error {
	noCache(ctx)
	if b, ok := webdistFile("index.html"); ok {
		ctx.GinContext().Data(http.StatusOK, "text/html; charset=utf-8", b)
	} else {
		ctx.GinContext().Data(http.StatusOK, "text/html; charset=utf-8", placeholderHTML)
	}
	return nil
}

// handlePalette 返回全局快捷键弹窗页（桌面外壳 Alt+Space 的透明窗口加载；
// 与主窗口同源共享登录会话，浏览器直接访问 :port/palette 亦可用）。
func (s *Server) handlePalette(ctx *vhttp.Context) error {
	noCache(ctx)
	if b, ok := webdistFile("palette.html"); ok {
		ctx.GinContext().Data(http.StatusOK, "text/html; charset=utf-8", b)
	} else {
		ctx.GinContext().Data(http.StatusOK, "text/html; charset=utf-8", placeholderPalette)
	}
	return nil
}

// handleWizard 返回首装向导页（桌面外壳首次运行时的账号创建与数据目录确认；
// 与 /api/setup 同级公开下发——向导本身就是建号入口）。
func (s *Server) handleWizard(ctx *vhttp.Context) error {
	noCache(ctx)
	if b, ok := webdistFile("wizard.html"); ok {
		ctx.GinContext().Data(http.StatusOK, "text/html; charset=utf-8", b)
	} else {
		ctx.GinContext().Data(http.StatusOK, "text/html; charset=utf-8", placeholderWizard)
	}
	return nil
}

// handleUpdateProvider 热更新 LLM 供应商配置（baseURL/model）：
// 立即生效（重建 Agent，会话连续）并写回 config.yaml。
func (s *Server) handleUpdateProvider(ctx *vhttp.Context) error {
	var req struct {
		BaseURL string `json:"baseURL"`
		Model   string `json:"model"`
		APIKey  string `json:"apiKey"` // 可选：留空保持已有密钥不变
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if err := agent.ReloadProvider(s.cfg, s.configPath, s.regFn(), s.chat.Brain(), s.approver, req.BaseURL, req.Model, req.APIKey); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	// 已建模型库时把改动同步回激活条目（库内条目与扁平投影保持一致），失败只追加提示，
	// 不影响本次已生效的热更新。
	msg := "模型配置已热生效，并已写回 config.yaml"
	if req.APIKey != "" {
		msg = "模型配置与 API 密钥已热生效，并已写回 config.yaml"
	}
	if err := s.cfg.Apply(func(next *config.Config) error {
		if next.Provider.Active == "" {
			return nil
		}
		for i := range next.Provider.Models {
			if next.Provider.Models[i].Name != next.Provider.Active {
				continue
			}
			next.Provider.Models[i].BaseURL = req.BaseURL
			next.Provider.Models[i].Model = req.Model
			if req.APIKey != "" {
				next.Provider.Models[i].APIKey = req.APIKey
			}
		}
		return config.UpdateProviderModels(s.configPath, next.Provider)
	}); err != nil {
		msg += "；同步模型库条目失败：" + err.Error()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message":  msg,
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

// handleListTraces 返回全局调用事件流（最新在前，默认最近 300 条事件）
// 与全量统计。轨迹生命周期跟随对话历史：清除上下文时一并清除。
func (s *Server) handleListTraces(ctx *vhttp.Context) error {
	limit := 300
	if n, err := strconv.Atoi(ctx.GinContext().Query("limit")); err == nil && n > 0 {
		limit = n
	}
	// ?session=plan-<id> / ?session=goal-<id> 只看一次多步运行的全部轮次。
	session := strings.TrimSpace(ctx.GinContext().Query("session"))
	events, stats := trace.Events(limit, session)
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"events": events, "stats": stats})
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

// handleStatus 返回管家运行状态。
func (s *Server) handleStatus(ctx *vhttp.Context) error {
	if err := s.engine.Err(); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	tasks := s.engine.List()
	byKind := map[string]int{}
	for _, t := range tasks {
		byKind[string(t.Kind)]++
	}
	var pending int
	if s.todos != nil {
		if err := s.todos.Err(); err != nil {
			return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
		}
		pending = s.todos.CountPending()
	}
	model, baseURL, keyConfigured := s.info.Model, s.info.BaseURL, false
	if s.cfg != nil {
		cfg := s.cfg.Snapshot()
		model, baseURL, keyConfigured = cfg.Provider.Model, cfg.Provider.BaseURL, cfg.ProviderAPIKey() != ""
		// 首次启动种子里的示例占位不算已配置模型：状态卡显示「未配置模型」。
		if (config.ModelProfile{BaseURL: baseURL, Model: model}).IsPlaceholder() {
			model, baseURL = "", ""
		}
	}
	// 输入格式与窗口取激活条目（未建库时缺省全支持 + 按模型名查表）。
	inputs := []string{config.InputText, config.InputImage}
	entryWindow := 0
	if s.cfg != nil {
		if entry, ok := s.cfg.Snapshot().Provider.ActiveModel(); ok {
			inputs = entry.EffectiveInputs()
			entryWindow = entry.ContextWindow
		}
	}
	if entryWindow <= 0 {
		entryWindow = llm.GetContextWindow(model)
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"startedAt":        s.startedAt.Format(time.RFC3339),
		"uptimeSeconds":    int(time.Since(s.startedAt).Seconds()),
		"provider":         map[string]any{"model": model, "baseURL": baseURL, "inputs": inputs, "contextWindow": entryWindow},
		"apiKeyConfigured": keyConfigured,
		"tasks":            map[string]any{"total": len(tasks), "byKind": byKind},
		"todos":            map[string]any{"pending": pending},
		"memories":         s.memory.Count(),
		"files":            map[string]string{"tasks": s.info.TaskFile, "todos": s.info.TodoFile, "memories": s.info.MemoryDir},
		"cost":             s.costStatus(),
	})
}

// costStatus 返回成本态势摘要（闸门未装配时为 nil，前端不显示横幅）。
func (s *Server) costStatus() map[string]any {
	if s.guard == nil {
		return nil
	}
	v := s.guard.Evaluate()
	return map[string]any{
		"state":           v.State,
		"ratio":           v.Ratio,
		"tightest":        v.Tightest,
		"todayTokens":     v.TodayTokens,
		"todayCost":       v.TodayCost,
		"monthlyTokens":   v.MonthlyTokens,
		"monthlyCost":     v.MonthlyCost,
		"currency":        v.Currency,
		"manualPaused":    v.ManualPaused,
		"proactivePaused": v.ProactivePaused,
		"unknownModels":   v.UnknownModels,
		"zeroCalls":       v.ZeroCalls,
	}
}

// handleListTasks 返回任务列表。
func (s *Server) handleListTasks(ctx *vhttp.Context) error {
	if err := s.engine.Err(); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
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
	target, found := s.engine.Get(id)
	ok, err := s.engine.CancelErr(id)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到任务 #" + id})
	}
	if found && target.Goal {
		s.releaseGoalSession(id)
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"message": "已取消任务 #" + id,
		"tasks":   s.engine.List(),
	})
}

// releaseGoalSession 释放目标占用的内存会话并清理其轨迹。
// SessionPool 只增不减，不释放会随目标数量线性膨胀；轨迹与对话历史同生命周期
// （与 /api/context/clear 一致）。目标仍在推进时跳过——清会话要等当前这一步结束，
// 会把取消请求挂住几分钟，此时留到目标跑完后再删。
func (s *Server) releaseGoalSession(id string) {
	if s.chat == nil || (s.goalRunner != nil && s.goalRunner.Running(id)) {
		return
	}
	b := s.chat.Brain()
	if b == nil || b.ReleaseGoalSession(id) != nil {
		return
	}
	trace.ClearSession(agent.GoalSessionKey(id))
}

// wikiPageReq 是新建/更新记忆页的请求体。
type wikiPageReq struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// handleListWikiPages 记忆页列表/搜索（?q= 关键词，留空返回全部）。
func (s *Server) handleListWikiPages(ctx *vhttp.Context) error {
	q := strings.TrimSpace(ctx.GinContext().Query("q"))
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"pages": s.memory.Search(q)})
}

// handleGetWikiPage 返回一页完整内容（含原始 markdown 正文）。
func (s *Server) handleGetWikiPage(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	p, ok := s.memory.Read(id)
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到记忆页 #" + id})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"page": p})
}

// handleCreateWikiPage 新建记忆页（标题已存在时更新该页）。
func (s *Server) handleCreateWikiPage(ctx *vhttp.Context) error {
	var req wikiPageReq
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	p, created, err := s.memory.Save("", req.Title, req.Body, "admin")
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	msg := fmt.Sprintf("已更新记忆页《%s》", p.Title)
	if created {
		msg = fmt.Sprintf("已新建记忆页《%s》", p.Title)
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"page": p, "message": msg, "created": created})
}

// handleUpdateWikiPage 更新指定记忆页。
func (s *Server) handleUpdateWikiPage(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	var req wikiPageReq
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	p, _, err := s.memory.Save(id, req.Title, req.Body, "admin")
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"page":    p,
		"message": fmt.Sprintf("已保存记忆页《%s》", p.Title),
	})
}

// handleDeleteWikiPage 删除一页记忆。
func (s *Server) handleDeleteWikiPage(ctx *vhttp.Context) error {
	id := ctx.GinContext().Param("id")
	if !s.memory.Delete(id) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到记忆页 #" + id})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已删除记忆页 #" + id})
}

// handleListEntries 闪念列表（置顶在前，其余按创建时间倒序；?kind=&q=&tag=&done= 过滤）。
func (s *Server) handleListEntries(ctx *vhttp.Context) error {
	if s.todos == nil {
		return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"entries": []todo.Todo{}, "tags": []string{}})
	}
	if err := s.todos.LoadError(); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	f := todo.EntryFilter{Kind: ctx.GinContext().Query("kind"), Q: ctx.GinContext().Query("q"), Tag: ctx.GinContext().Query("tag")}
	if done := ctx.GinContext().Query("done"); done != "" {
		value := done == "true" || done == "1"
		f.Done = &value
	}
	syncError := ""
	if err := s.todos.SyncError(); err != nil {
		syncError = err.Error()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"entries": s.todos.ListEntries(f), "tags": s.todos.Tags(), "syncError": syncError})
}

// handleAddEntry 记入一条闪念（kind 缺省 todo；笔记忽略时间字段）。
func (s *Server) handleAddEntry(ctx *vhttp.Context) error {
	if s.todos == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "闪念功能未启用"})
	}
	var req struct {
		Kind     string     `json:"kind"`
		Text     string     `json:"text"`
		DueAt    *time.Time `json:"dueAt"`
		RemindAt *time.Time `json:"remindAt"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	entry, err := s.todos.AddEntry(req.Kind, req.Text, req.DueAt, req.RemindAt)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"entry": entry})
}

// handleCompleteEntry 完成/重开一条待办（body {"done":false} 表示重开，缺省完成）；笔记拒绝。
func (s *Server) handleCompleteEntry(ctx *vhttp.Context) error {
	if s.todos == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "闪念功能未启用"})
	}
	done := true
	var req struct {
		Done *bool `json:"done"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil && !errors.Is(err, io.EOF) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象，done须为布尔值"})
	} else if req.Done != nil {
		done = *req.Done
	}
	id := ctx.GinContext().Param("id")
	if entry, ok := s.todos.Get(id); ok && entry.Kind == todo.KindNote {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "笔记没有完成状态"})
	}
	entry, ok, err := s.todos.Complete(id, done)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到条目 #" + id})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"entry": entry})
}

// handlePinEntry 置顶/取消置顶一条闪念（body {"pinned":false} 取消，缺省置顶）。
func (s *Server) handlePinEntry(ctx *vhttp.Context) error {
	if s.todos == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "闪念功能未启用"})
	}
	pinned := true
	var req struct {
		Pinned *bool `json:"pinned"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil && !errors.Is(err, io.EOF) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象，pinned须为布尔值"})
	} else if req.Pinned != nil {
		pinned = *req.Pinned
	}
	id := ctx.GinContext().Param("id")
	entry, ok, err := s.todos.SetPinned(id, pinned)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到条目 #" + id})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"entry": entry})
}

// handleDeleteEntry 删除一条闪念（笔记与待办通用）。
func (s *Server) handleDeleteEntry(ctx *vhttp.Context) error {
	if s.todos == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "闪念功能未启用"})
	}
	id := ctx.GinContext().Param("id")
	ok, err := s.todos.Delete(id)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": err.Error()})
	}
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "未找到条目 #" + id})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已删除条目 #" + id})
}
