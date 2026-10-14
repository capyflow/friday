// Package config 负责加载 Friday 的 YAML 配置：LLM 供应商、待办/天气/自定义指令
// 等管家能力、消息通道与工具权限。硬件输出（扬声器/屏幕/音频播放）默认关闭
// （hardware.enabled），Friday 当前是纯软件形态的私人管家服务。
// 语音链路代码已归档至 archive/voice-mode/（见其 README），但 ASR/Wake/TTS/VAD/Audio
// 配置字段仍保留在此，避免恢复语音功能时改动配置结构。
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是 Friday 整体配置。
type Config struct {
	runtimeMu    *sync.RWMutex
	Provider     ProviderConfig    `yaml:"provider"`
	Audio        AudioConfig       `yaml:"audio"`
	ASR          ASRConfig         `yaml:"asr"`
	Wake         WakeConfig        `yaml:"wake"`
	TTS          TTSConfig         `yaml:"tts"`
	VAD          VADConfig         `yaml:"vad"`
	Display      DisplayConfig     `yaml:"display"`
	Hardware     HardwareConfig    `yaml:"hardware"`
	Weather      WeatherConfig     `yaml:"weather"`
	Custom       CustomConfig      `yaml:"custom"`
	Skills       SkillsConfig      `yaml:"skills"`
	Chan         ChanConfig        `yaml:"chan"`
	File         FileConfig        `yaml:"file"`
	Admin        AdminConfig       `yaml:"admin"`
	Memory       MemoryConfig      `yaml:"memory"`
	Context      ContextConfig     `yaml:"context"`
	Tools        ToolsConfig       `yaml:"tools"`
	Permissions  PermissionsConfig `yaml:"permissions"`
	Trace        TraceConfig       `yaml:"trace"`
	Heartbeat    HeartbeatConfig   `yaml:"heartbeat"`
	Mcp          McpConfig         `yaml:"mcp"`
	Persona      PersonaConfig     `yaml:"persona"`
	Cost         CostConfig        `yaml:"cost"`
	SystemPrompt string            `yaml:"systemPrompt"`
	// TaskFile 定时任务持久化 JSON 文件路径（默认 tasks.json，重启自动恢复）。
	TaskFile string `yaml:"taskFile"`
	// TodoFile 待办清单持久化 JSON 文件路径（默认 todos.json，重启自动恢复）。
	TodoFile string `yaml:"todoFile"`
	// SessionFile 对话会话持久化 JSON 文件路径（默认 session.json，重启不失忆）。
	SessionFile string `yaml:"sessionFile"`
	// UsageFile token 用量账本持久化 JSON 文件路径（默认 token-usage.json）。
	UsageFile string `yaml:"usageFile"`
	// PausedFile 聊天暂停现场落盘路径（默认 paused-turns.json）：刷新页面或重启服务后
	// 仍可一键继续，并复用当时的技能。
	PausedFile string `yaml:"pausedFile"`
	// PlanFile 计划持久化 JSON 文件路径（默认 plans.json）。
	PlanFile string `yaml:"planFile"`
	// Plan 计划模式参数。
	Plan PlanConfig `yaml:"plan"`
}

// HardwareConfig 控制硬件输出能力。Friday 当前与硬件分离（纯软件形态）：
// enabled=false（默认）时不注册 speak_now / display_now / play_audio / play_effect
// 工具，提醒经钉钉/飞书等通道推送；将来接 TTS/OLED/喇叭只需打开开关并替换实现。
type HardwareConfig struct {
	Enabled bool `yaml:"enabled"`
}

// WeatherConfig 描述天气查询能力（get_weather 工具，Open-Meteo 免密钥接口）。
type WeatherConfig struct {
	// DefaultCity 用户未指明城市时查询的默认城市（如 "上海"）。
	// 留空时 agent 会先询问用户所在城市，并可用 remember 记住以便下次直接查。
	DefaultCity string `yaml:"defaultCity"`
}

// CustomCommand 是一条自定义指令：聊天中输入 trigger（或"trigger 补充内容"）
// 时自动展开为 prompt 交给 agent 执行，相当于用户自定义的快捷功能。
type CustomCommand struct {
	// Name 指令名（即聊天触发词），如 "早安简报"。
	Name string `yaml:"name"`
	// Description 用途说明（管理后台/文档展示用，不参与匹配）。
	Description string `yaml:"description"`
	// Prompt 展开后的完整提示词。
	Prompt string `yaml:"prompt"`
}

// CustomConfig 是用户自定义指令集（config.yaml custom.commands，重启生效）。
// 更灵活的自定义能力（接外部工具/服务）走 MCP 服务器（mcp.servers，可在线配置）。
type CustomConfig struct {
	Commands []CustomCommand `yaml:"commands"`
}

// AdminConfig 描述管理后台（vortexv3 HTTP 服务）。
type AdminConfig struct {
	// Host 默认仅允许本机访问；Docker/远程部署显式配置 0.0.0.0。
	Host string `yaml:"host"`
	// Enabled 默认开启；配置里写 enabled: false 显式关闭。
	Enabled *bool `yaml:"enabled"`
	// Port 监听端口（默认 8080）。
	Port int `yaml:"port"`
	// AccountFile 管理员账号文件（默认 admin-account.json）。
	// 首次初始化默认账号 admin/admin，启动配置可预置用户名与密码；
	// 账号以 bcrypt 哈希落盘，重启后以设置的账号为准。
	AccountFile string `yaml:"accountFile"`
	// Username/Password 启动时预置的管理员账号（可选，两者须同时提供）。
	// 提供后每次启动都会以它覆盖现有账号并取消"首次登录强制改密"；
	// 环境变量 FRIDAY_ADMIN_USERNAME / FRIDAY_ADMIN_PASSWORD 优先于这两个字段。
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// 管理员凭据的环境变量名（优先于配置文件中的 username/password）。
const (
	AdminUsernameEnv = "FRIDAY_ADMIN_USERNAME"
	AdminPasswordEnv = "FRIDAY_ADMIN_PASSWORD"
)

// ResolveAdminCredentials 解析启动预置的管理员凭据：环境变量优先，其次配置文件。
// 两者都未提供返回空串（走默认 admin/admin + 首次改密流程）；
// 只提供其一返回错误（防止误配一半导致账号处于意外状态）。
func (c *Config) ResolveAdminCredentials() (username, password string, err error) {
	username = strings.TrimSpace(os.Getenv(AdminUsernameEnv))
	password = os.Getenv(AdminPasswordEnv)
	if username == "" {
		username = strings.TrimSpace(c.Admin.Username)
	}
	if password == "" {
		password = c.Admin.Password
	}
	if username == "" && password == "" {
		return "", "", nil
	}
	if username == "" || password == "" {
		return "", "", fmt.Errorf("config: 管理员账号密码须同时提供（%s/%s 或 admin.username/admin.password，当前只配了一项）",
			AdminUsernameEnv, AdminPasswordEnv)
	}
	return username, password, nil
}

// IsEnabled 返回后台是否启用（未配置时默认开启）。
func (a AdminConfig) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

// ChanConfig 描述外部消息通道（钉钉/飞书…），让助手住进用户的聊天软件。
type ChanConfig struct {
	Dingtalk DingtalkConfig `yaml:"dingtalk"`
	Feishu   FeishuConfig   `yaml:"feishu"`
}

// DingtalkConfig 是钉钉 Stream 模式通道（WebSocket，免公网 IP）。
type DingtalkConfig struct {
	// Enabled 是否启用钉钉通道。
	Enabled bool `yaml:"enabled"`
	// ClientID 应用 AppKey（开发者后台创建应用后获取）。
	ClientID string `yaml:"clientId"`
	// ClientSecret 应用 AppSecret。
	ClientSecret string `yaml:"clientSecret"`
	// AllowUserIDs 发送者白名单（senderId）；为空表示允许所有联系人。
	AllowUserIDs []string `yaml:"allowUserIDs"`
	// PushWebhook 可选：自定义机器人 webhook 地址（群机器人"Webhook 接入"），
	// 用于心跳等主动消息推送；留空则钉钉侧不接收主动推送。
	PushWebhook string `yaml:"pushWebhook"`
}

// FeishuConfig 是飞书长连接通道（larkws WebSocket，免公网 IP）。
type FeishuConfig struct {
	// Enabled 是否启用飞书通道。
	Enabled bool `yaml:"enabled"`
	// AppID 企业自建应用 App ID（飞书开放平台创建）。
	AppID string `yaml:"appID"`
	// AppSecret 企业自建应用 App Secret。
	AppSecret string `yaml:"appSecret"`
	// AllowOpenIDs 发送者白名单（open_id）；为空表示允许所有联系人。
	AllowOpenIDs []string `yaml:"allowOpenIDs"`
	// PushChatIDs 可选：主动消息（心跳等）的投递目标 chat_id 列表
	//（应用需已在对应群/单聊中且有发消息权限）。
	PushChatIDs []string `yaml:"pushChatIDs"`
}

// FileConfig 描述文件工具（read_file/list_files）的访问边界。
type FileConfig struct {
	// AllowDirs 允许 agent 读取的目录白名单；相对路径基于服务运行目录解析。
	// 为空时默认仅允许运行目录（"."）。
	AllowDirs []string `yaml:"allowDirs"`
}

// MemoryConfig 描述长期记忆（wiki 页面仓库）。
type MemoryConfig struct {
	// Dir 记忆 wiki 页面目录，每页一个 <id>.md 文件（默认 memories）。
	Dir string `yaml:"dir"`
	// File 旧版扁平记忆 JSON 文件路径（默认 memories.json）；
	// wiki 目录为空时启动自动一次性迁入《用户档案》页，原文件保留。
	File string `yaml:"file"`
}

// ContextConfig 描述单会话上下文引擎（ctxengine）：长会话不再线性膨胀——
// 活跃上下文 = 交接摘要区 + 最近块原文 + 按需检索，原始历史不可变归档本地目录。
// 比例/块数等 0 值交由框架默认（软阈值 0.7 / 硬阈值 0.95 / 摘要区 10% /
// 保留最近 12 块 / 检索 5 条）。注意框架的发送前硬校验（hardRatio）在当前
// 依赖版本未实际生效，实际防线是软阈值轮末归档 + 超窗自动裁剪重试一次。
type ContextConfig struct {
	// Enabled 是否启用（未配置默认开启）。关闭后行为与旧版一致：
	// 每轮发送完整历史，不归档、不注册检索工具。
	Enabled *bool `yaml:"enabled"`
	// ArchiveDir 归档目录（默认 archive）：原始块与摘要按会话分子目录落盘，
	// 桌面/容器模式随工作目录落在本机数据目录内。
	ArchiveDir string `yaml:"archiveDir"`
	// CandidateFacts 归档时抽取的候选事实是否写入记忆 wiki（默认开启，
	// Source 标记为 agent，可在后台查看/编辑/删除）。显式 memory_save 仍是主通道。
	CandidateFacts *bool `yaml:"candidateFacts"`
	// WindowTokens 上下文窗口 token 数（0 = 按模型名查表，未知型号默认 128k；
	// 框架仅精确匹配常见型号名，用非标准命名时建议显式配置）。
	WindowTokens int `yaml:"windowTokens"`
	// SoftRatio 归档触发软阈值（估算用量占比，默认 0.7）。
	SoftRatio float64 `yaml:"softRatio"`
	// HardRatio 发送前硬校验阈值（默认 0.95；见上：当前依赖版本未生效）。
	HardRatio float64 `yaml:"hardRatio"`
	// SummaryBudget 摘要区预算（占窗口比例，默认 0.10）。
	SummaryBudget float64 `yaml:"summaryBudget"`
	// KeepRecentBlocks 归档保留的最近块数（默认 12）。
	KeepRecentBlocks int `yaml:"keepRecentBlocks"`
	// RetrievalLimit search_history 默认返回条数（默认 5，上限 20）。
	RetrievalLimit int `yaml:"retrievalLimit"`
	// RetentionDays 归档保留天数（0 = 永久保留）。
	RetentionDays int `yaml:"retentionDays"`
}

// IsEnabled 返回上下文引擎是否启用（未配置时默认开启）。
func (c ContextConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// FactsEnabled 返回候选事实是否写入记忆 wiki（未配置时默认开启）。
func (c ContextConfig) FactsEnabled() bool {
	return c.CandidateFacts == nil || *c.CandidateFacts
}

// ToolsConfig 描述执行类工具。
type ToolsConfig struct {
	Exec ExecConfig `yaml:"exec"`
}

// ExecConfig 描述 exec_command 工具的审批与安全边界。策略经
// internal/agent/permission.go 翻译成 vortexagent 全局权限层规则生效。
type ExecConfig struct {
	// Mode 审批模式：auto（白名单内直接执行，其余拒绝，映射权限层 whitelist）/
	// confirm（白名单内直行，其余进网页审批队列，批准后执行，超时自动拒绝）/
	// off（不注册 exec_command，整体禁用）。默认 confirm。
	Mode string `yaml:"mode"`
	// Allowlist 免审批命令白名单，如 ["uname -a", "ls", "date"]。
	// 匹配规则：按 shell 命令语义做段级校验——复合命令（; && || | 及换行）拆段
	// 后每一段都须命中白名单项（整条相等或词边界前缀），感知引号。
	Allowlist []string `yaml:"allowlist"`
	// Timeout 默认超时秒数（默认 30，上限 600；单次调用可用 timeout 参数覆盖，仍受上限约束）。
	Timeout int `yaml:"timeout"`
}

// TimeoutSeconds 返回规范化后的默认超时秒数（默认 30，上限 600）。
func (e ExecConfig) TimeoutSeconds() int {
	if e.Timeout <= 0 {
		return 30
	}
	if e.Timeout > 600 {
		return 600
	}
	return e.Timeout
}

// 权限模式（permissions.mode，参照 zcode 的多档权限设计）。
const (
	// PermModeConfirm 默认：写文件/改 MCP 配置进网页审批，exec 按 tools.exec.mode 策略。
	PermModeConfirm = "confirm"
	// PermModeAcceptEdits 编辑类操作（write_file/edit_file/mcp_edit 变更）免审批直行，
	// exec_command 白名单外仍进网页审批——"日常改东西不问，跑命令仍把关"。
	PermModeAcceptEdits = "acceptEdits"
	// PermModeFullAccess 全部免审批直行；毁灭性命令黑名单（deny 规则）仍强制拦截。
	PermModeFullAccess = "fullAccess"
)

// PermissionsConfig 描述全局工具权限层（vortexagent PermissionChecker）的附加// 规则。基础策略由 tools.exec 派生（模式/白名单/毁灭性黑名单/安全工具直通），
// 此处只做补充与例外，对所有工具（含 MCP）生效。
type PermissionsConfig struct {
	// Mode 全局权限模式：confirm（默认，危险操作网页审批）| acceptEdits（编辑类
	// 免审批，执行命令仍审批）| fullAccess（全部免审批，黑名单仍拦截）。
	// 管理后台"设置"页可在线切换（热生效）。
	Mode string `yaml:"mode"`
	// MCP 控制 MCP 工具（mcp_<server>_<tool>）是否需要网页审批：
	// allow（默认，免审批）| confirm（每次调用进审批队列）。
	MCP string `yaml:"mcp"`
	// Allow 附加全局放行规则："工具名" 或 "工具名:参数模式"（如 "mcp_muse_*"）。
	Allow []string `yaml:"allow"`
	// Deny 附加全局拒绝规则，任何模式（含用户批准）下命中即拒。
	Deny []string `yaml:"deny"`
}

// NormalizePermissionMode 规范化权限模式：空串视为 confirm，容忍 accept-edits /
// full_access / bypass-permissions 等写法别名；非法值返回错误。
func NormalizePermissionMode(s string) (string, error) {
	flat := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.TrimSpace(s)))
	switch flat {
	case "", "confirm", "default":
		return PermModeConfirm, nil
	case "acceptedits":
		return PermModeAcceptEdits, nil
	case "fullaccess", "bypasspermissions", "bypass":
		return PermModeFullAccess, nil
	default:
		return "", fmt.Errorf("未知权限模式 %q（可选 confirm / acceptEdits / fullAccess）", s)
	}
}

// EffectiveMode 返回规范化后的权限模式（未配置视为 confirm）。
func (p PermissionsConfig) EffectiveMode() string {
	m, err := NormalizePermissionMode(p.Mode)
	if err != nil {
		return PermModeConfirm
	}
	return m
}

// EditsAutoApproved 返回编辑类操作（write_file/edit_file/mcp_edit 变更）是否免审批。
func (p PermissionsConfig) EditsAutoApproved() bool {
	return p.EffectiveMode() != PermModeConfirm
}

// BypassesAll 返回是否全部免审批（fullAccess；deny 黑名单不受影响）。
func (p PermissionsConfig) BypassesAll() bool {
	return p.EffectiveMode() == PermModeFullAccess
}

// TraceConfig 描述调用轨迹的持久化。
type TraceConfig struct {
	// File 轨迹落盘文件（默认 traces.json，重启自动恢复最近 200 次调用）。
	File string `yaml:"file"`
}

// HeartbeatConfig 描述心跳式主动任务：agent 定期醒来读检查清单，自主决策
// 要不要做事/打扰用户（对标 OpenClaw HEARTBEAT.md）。
type HeartbeatConfig struct {
	// Enabled 是否启用心跳。
	Enabled bool `yaml:"enabled"`
	// Schedule 心跳触发时刻，标准 5 段 cron（分 时 日 月 周），如 "0 8 * * *"（每天 8 点）。
	Schedule string `yaml:"schedule"`
	// Interval 触发间隔时长（如 "30m"、"1h"）。与 Schedule 二选一，都填时 Schedule 优先。
	Interval string `yaml:"interval"`
	// QuietHours 静默时段 "HH:MM-HH:MM"（如 "23:00-07:00"，支持跨零点）：
	// 心跳照常执行但只记轨迹不推送。留空关闭静默。
	QuietHours string `yaml:"quietHours"`
	// File 检查清单文件路径（默认工作目录 HEARTBEAT.md，可在管理后台编辑）。
	File string `yaml:"file"`
}

// CostConfig 描述成本观测与软限额。
//
// 限额是"软"的：超预算时停止的是**主动型工作**（心跳巡检、智能任务、目标启动），
// 绝不取消进行中的用户对话轮，也不影响定时提醒投递（提醒不调模型、零成本）。
// 框架只有事后的 OnLLMCall 钩子，事前拦截需要 ctx 取消，故本轮不做硬停。
type CostConfig struct {
	// Currency 金额展示币种（默认 CNY）。价格表自带币种时以价格表为准。
	Currency string `yaml:"currency"`
	// PricingFile 模型价格表路径（默认 pricing.json）：后台可编辑、热加载，
	// 不进 config.yaml 以免占用配置热更新链路。
	PricingFile string     `yaml:"pricingFile"`
	Budget      CostBudget `yaml:"budget"`
	// PauseProactive 超预算时是否暂停主动型工作（默认开启）。关闭即"只警示不停手"。
	PauseProactive *bool       `yaml:"pauseProactive"`
	Goals          GoalsConfig `yaml:"goals"`
}

// CostBudget 是各档预算上限。0 表示该项不限。
type CostBudget struct {
	// DailyTokens / MonthlyTokens 计费 token（输入+输出）额度。
	DailyTokens   int64 `yaml:"dailyTokens"`
	MonthlyTokens int64 `yaml:"monthlyTokens"`
	// DailyCost / MonthlyCost 金额额度，需价格表命中；未配价的模型不计入。
	DailyCost   float64 `yaml:"dailyCost"`
	MonthlyCost float64 `yaml:"monthlyCost"`
	// SoftRatio 预警阈值（默认 0.8）：用量占比达到它即 warn，达到 1 即 over。
	SoftRatio float64 `yaml:"softRatio"`
	// PerPlanTokens 单次计划（全部步骤合计）的软预算，超限则在步边界暂停。
	PerPlanTokens int64 `yaml:"perPlanTokens"`
	// PerGoalTokens / PerGoalSteps 单个目标的默认预算（任务可逐项覆盖）。
	PerGoalTokens int64 `yaml:"perGoalTokens"`
	PerGoalSteps  int   `yaml:"perGoalSteps"`
}

// GoalsConfig 是目标模式（长程自主任务）的运行参数。
type GoalsConfig struct {
	// MaxConcurrent 同时运行的目标数（默认 1）：目标轮次长、成本大，串行更安全。
	MaxConcurrent int `yaml:"maxConcurrent"`
}

// PauseProactiveEnabled 返回超预算时是否暂停主动型工作（缺省开启）。
func (c CostConfig) PauseProactiveEnabled() bool {
	return c.PauseProactive == nil || *c.PauseProactive
}

// McpConfig 描述 MCP（Model Context Protocol）外部工具服务器。
type McpConfig struct {
	// Servers 声明的 MCP 服务器列表（stdio transport：本机子进程）。
	Servers []McpServerConfig `yaml:"servers" json:"servers"`
}

// SkillsConfig 是技能层配置。技能本身是磁盘上的 SKILL.md（框架格式），
// 后台"设置 → 技能"可增删改并即时生效，不进 config.yaml。
type SkillsConfig struct {
	// Enabled 总开关：关闭时不加载技能目录，也不提供任何技能注入。
	Enabled *bool `yaml:"enabled"`
	// Dir 技能目录（默认 skills，相对数据目录）。
	Dir string `yaml:"dir"`
	// AutoTrigger 是否允许按消息内容自动选用技能（默认开启）。
	// 单个技能仍要写 auto: true 才参与——两层都是 opt-in。
	AutoTrigger *bool `yaml:"autoTrigger"`
}

// EnabledOrDefault 返回技能层是否启用（缺省启用：目录里没有文件时等于没有效果）。
func (c SkillsConfig) EnabledOrDefault() bool {
	return c.Enabled == nil || *c.Enabled
}

// AutoTriggerEnabled 返回是否允许自动匹配（缺省允许，仍需技能自身 auto: true）。
func (c SkillsConfig) AutoTriggerEnabled() bool {
	return c.AutoTrigger == nil || *c.AutoTrigger
}

// PlanConfig 是计划模式参数。计划本身存在 planFile 里，后台"自动化 → 计划"可管。
type PlanConfig struct {
	// Enabled 关闭时聊天里的"计划"入口退化为普通对话（后端直接拒绝并说明）。
	Enabled *bool `yaml:"enabled"`
	// MaxIterations 规划轮与每一步各自允许的工具调用轮数（默认 12）。
	// 框架默认只有 10 轮，多步计划里单步很容易撞上限。
	MaxIterations int `yaml:"maxIterations"`
}

// EnabledOrDefault 返回计划模式是否启用（缺省启用）。
func (c PlanConfig) EnabledOrDefault() bool {
	return c.Enabled == nil || *c.Enabled
}

// Persona 是一套机器人人设：系统提示词中"性格与语气"段的可切换文案。
type Persona struct {
	// ID 人设唯一标识（内置：default/playful/rigorous/warm/concise；自定义另起名）。
	ID string `yaml:"id" json:"id"`
	// Name 显示名（管理后台下拉展示），如 "活泼调皮"。
	Name string `yaml:"name" json:"name"`
	// Tagline 一句话描述（管理后台展示用）。
	Tagline string `yaml:"tagline" json:"tagline"`
	// Prompt 人设正文（性格与语气描述，拼入系统提示词的人设段）。
	Prompt string `yaml:"prompt" json:"prompt"`
}

// PersonaConfig 描述人设提示词配置：Active 指定当前生效的人设，
// Presets 为用户自定义/覆盖预设（与内置预设同 ID 时覆盖内置文案）。
type PersonaConfig struct {
	// Active 当前生效的人设 ID（默认 default）。
	Active string `yaml:"active" json:"active"`
	// Presets 自定义或覆盖预设（管理后台在线编辑，写回 config.yaml）。
	Presets []Persona `yaml:"presets" json:"presets"`
}

// McpServerConfig 是一台 MCP 服务器的启动配置（视为可信配置，明文声明在 config.yaml；
// 管理后台在线编辑后经 UpdateMcpServers 写回）。
type McpServerConfig struct {
	// Name 服务器名（工具名前缀 mcp_<name>_，防冲突）。
	Name string `yaml:"name" json:"name"`
	// Command 启动命令，如 npx / ./mcp-server。
	Command string `yaml:"command" json:"command"`
	// Args 启动参数。
	Args []string `yaml:"args" json:"args"`
	// Env 额外环境变量（在继承进程环境的基础上追加）。
	Env map[string]string `yaml:"env" json:"env"`
	// URL 远程 MCP 服务器地址（Streamable HTTP），与 Command 二选一。
	URL string `yaml:"url" json:"url"`
	// Headers 远程请求头（鉴权等，如 Authorization: Bearer xxx）。
	Headers map[string]string `yaml:"headers" json:"headers"`
}

// ProviderConfig 描述 LLM 供应商。
type ProviderConfig struct {
	// Name 固定为 "openai"（vortexagent 的 OpenAI 兼容层，DeepSeek/Qwen/智谱等通用）。
	Name string `yaml:"name"`
	// BaseURL OpenAI 兼容服务地址，如 https://api.deepseek.com/v1。
	BaseURL string `yaml:"baseURL"`
	// Model 模型名，如 deepseek-chat。
	Model string `yaml:"model"`
	// APIKeyEnv 环境变量名，运行时从该变量读取 API 密钥（不落盘）。
	// 通用命名（默认 FRIDAY_API_KEY），任意 OpenAI 兼容服务商通用；
	// 未设置时兼容回退旧名 DEEPSEEK_API_KEY。
	APIKeyEnv string `yaml:"apiKeyEnv"`
	// APIKey 直接配置的 API 密钥（可选）：优先于环境变量；可经管理后台
	// 「设置」页在线填写并写回 config.yaml（数据目录用户私有）。
	APIKey string `yaml:"apiKey"`
	// Temperature 采样温度（可选，默认 0.7）。
	Temperature float64 `yaml:"temperature"`
	// MaxTokens 单次输出最大 token（可选，默认 1024）。
	MaxTokens int `yaml:"maxTokens"`
	// StreamUsage 流式请求是否索取 usage 统计（缺省开启）。
	// 关掉后网页聊天的 token 账本会归零（仅"运行状态"页 ZeroCalls 计数可见），
	// 只用于个别不接受 stream_options 字段的兼容网关排障。
	StreamUsage *bool `yaml:"streamUsage"`
}

// AudioConfig 描述麦克风/喇叭设备。
type AudioConfig struct {
	InputDevice  string `yaml:"inputDevice"`  // 留空自动选择
	OutputDevice string `yaml:"outputDevice"` // 留空自动选择
	SampleRate   int    `yaml:"sampleRate"`   // 默认 16000
	Channels     int    `yaml:"channels"`     // 默认 1
	// SilenceRMS 静音门限：整段录音 RMS 低于该值视为没人说话，跳过 ASR
	// （防止离线识别对环境噪声的幻觉）。0 视为默认 150；调小（如 1）关闭。
	// 运行日志会打印实测 RMS，便于按麦克风灵敏度调整。
	SilenceRMS float64 `yaml:"silenceRMS"`
}

// ASRConfig 描述语音识别。
type ASRConfig struct {
	// Engine: "none"(文本模式) 或 "sherpa-onnx"。
	Engine string `yaml:"engine"`
	// ModelDir sherpa-onnx 模型目录（离线识别模型）。
	ModelDir string `yaml:"modelDir"`
}

// WakeConfig 描述唤醒词。
type WakeConfig struct {
	// Engine: "none" 或 "sherpa-onnx"。
	Engine string `yaml:"engine"`
	// ModelDir 唤醒词模型目录（KWS 模型）。
	ModelDir string `yaml:"modelDir"`
	// Keywords 唤醒词列表，如 ["小助手"]。
	Keywords []string `yaml:"keywords"`
}

// TTSConfig 描述语音合成。
type TTSConfig struct {
	// Engine: "none" 或 "sherpa-onnx"。
	Engine string `yaml:"engine"`
	// ModelDir TTS 模型目录。
	ModelDir string `yaml:"modelDir"`
	// Voice 说话人/音色名（留空用模型默认）。
	Voice string `yaml:"voice"`
	// Speed 语速倍率（1.0 正常，越大越快；留空/0 视为 1.0）。
	Speed float64 `yaml:"speed"`
}

// VADConfig 描述录音断句（silero VAD）：说完自动停止录音，替代固定时长等待。
type VADConfig struct {
	// ModelPath silero_vad.onnx 路径；为空或文件不存在时回退为固定时长录音。
	ModelPath string `yaml:"modelPath"`
	// Threshold 语音概率阈值（0~1，越大越不敏感，默认 0.5）。
	Threshold float32 `yaml:"threshold"`
	// MinSilence 静音持续多少秒判定"说完"（默认 0.6）。
	MinSilence float32 `yaml:"minSilence"`
	// MinSpeech 语音持续多少秒才算"说过话"（默认 0.25，滤除短噪声）。
	MinSpeech float32 `yaml:"minSpeech"`
}

// DisplayConfig 描述屏幕。
type DisplayConfig struct {
	// Driver: "console"(开发) 或后续 "oled"/"st7789" 等。
	Driver string `yaml:"driver"`
}

// Load 从 path 读取 YAML 配置，并对缺失字段填充默认值。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// UpdateProvider 热更新 config.yaml 中的 provider.baseURL / provider.model，
// 以及 provider.apiKey（apiKey 为空串表示不动已有密钥）：以 yaml.Node 方式编辑
// （保留注释与原有结构），临时文件 + rename 原子写回。
func UpdateProvider(path, baseURL, model, apiKey string) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("解析配置失败: %w", err)
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("配置文件为空")
	}
	prov := mappingValue(doc.Content[0], "provider")
	if prov == nil {
		return fmt.Errorf("配置文件中未找到 provider 段")
	}
	setMappingKey(prov, "baseURL", baseURL)
	setMappingKey(prov, "model", model)
	if apiKey != "" {
		setMappingKey(prov, "apiKey", apiKey)
	}

	out, err := marshalNode(&doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	return os.Rename(tmp, path)
}

// UpdateMcpServers 热更新 config.yaml 中的 mcp.servers 列表：以 yaml.Node 方式编辑
// （保留文件其余部分的注释与结构；mcp 段不存在时追加），临时文件 + rename 原子写回。
// servers 允许为空（写空列表，表示清空全部 MCP 服务器）。
func UpdateMcpServers(path string, servers []McpServerConfig) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("解析配置失败: %w", err)
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("配置文件为空")
	}
	root := doc.Content[0]
	mcpNode := mappingValue(root, "mcp")
	if mcpNode == nil || mcpNode.Kind != yaml.MappingNode {
		mcpNode = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "mcp"},
			mcpNode)
	}

	// 构造 servers 序列（映射列表：name/command/args/env）。
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, s := range servers {
		item := &yaml.Node{Kind: yaml.MappingNode}
		strPair := func(k, v string) {
			item.Content = append(item.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: k},
				&yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"})
		}
		strPair("name", s.Name)
		if s.URL != "" {
			// 远程服务器：写 url + headers。
			strPair("url", s.URL)
			if len(s.Headers) > 0 {
				hdrMap := &yaml.Node{Kind: yaml.MappingNode}
				keys := make([]string, 0, len(s.Headers))
				for k := range s.Headers {
					keys = append(keys, k)
				}
				sort.Strings(keys) // 固定顺序，避免每次保存随机重排
				for _, k := range keys {
					hdrMap.Content = append(hdrMap.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Value: k},
						&yaml.Node{Kind: yaml.ScalarNode, Value: s.Headers[k], Tag: "!!str"})
				}
				item.Content = append(item.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "headers"}, hdrMap)
			}
		} else {
			// 本地命令服务器：写 command + args + env。
			strPair("command", s.Command)
			if len(s.Args) > 0 {
				argsSeq := &yaml.Node{Kind: yaml.SequenceNode}
				for _, a := range s.Args {
					argsSeq.Content = append(argsSeq.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Value: a, Tag: "!!str"})
				}
				item.Content = append(item.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "args"}, argsSeq)
			}
			if len(s.Env) > 0 {
				envMap := &yaml.Node{Kind: yaml.MappingNode}
				keys := make([]string, 0, len(s.Env))
				for k := range s.Env {
					keys = append(keys, k)
				}
				sort.Strings(keys) // 固定顺序，避免每次保存时 env 行随机重排
				for _, k := range keys {
					envMap.Content = append(envMap.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Value: k},
						&yaml.Node{Kind: yaml.ScalarNode, Value: s.Env[k], Tag: "!!str"})
				}
				item.Content = append(item.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "env"}, envMap)
			}
		}
		seq.Content = append(seq.Content, item)
	}
	setMappingNode(mcpNode, "servers", seq)

	out, err := marshalNode(&doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	return os.Rename(tmp, path)
}

// UpdatePermissionMode 热更新 config.yaml 中的 permissions.mode：以 yaml.Node
// 方式编辑（保留注释与原有结构；permissions 段不存在时追加），临时文件 + rename
// 原子写回。mode 须为已校验的规范值（confirm / acceptEdits / fullAccess）。
func UpdatePermissionMode(path, mode string) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("解析配置失败: %w", err)
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("配置文件为空")
	}
	root := doc.Content[0]
	perms := mappingValue(root, "permissions")
	if perms == nil || perms.Kind != yaml.MappingNode {
		perms = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "permissions"},
			perms)
	}
	setMappingKey(perms, "mode", mode)

	out, err := marshalNode(&doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	return os.Rename(tmp, path)
}

// UpdatePersona 热更新 config.yaml 中的 persona 配置（active + 自定义预设列表）：
// 以 yaml.Node 方式编辑（保留文件其余部分的注释与结构；persona 段不存在时追加），
// 临时文件 + rename 原子写回。presets 允许为空（写空列表，表示清空自定义覆盖）。
func UpdatePersona(path, active string, presets []Persona) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("解析配置失败: %w", err)
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("配置文件为空")
	}
	root := doc.Content[0]
	personaNode := mappingValue(root, "persona")
	if personaNode == nil || personaNode.Kind != yaml.MappingNode {
		personaNode = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "persona"},
			personaNode)
	}

	setMappingKey(personaNode, "active", active)
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, p := range presets {
		item := &yaml.Node{Kind: yaml.MappingNode}
		strPair := func(k, v string) {
			item.Content = append(item.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: k},
				&yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"})
		}
		strPair("id", p.ID)
		strPair("name", p.Name)
		strPair("tagline", p.Tagline)
		strPair("prompt", p.Prompt)
		seq.Content = append(seq.Content, item)
	}
	setMappingNode(personaNode, "presets", seq)

	out, err := marshalNode(&doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	return os.Rename(tmp, path)
}

// setMappingNode 更新或新增映射中一个键的任意值节点（保留键节点上的注释）。
func setMappingNode(mapping *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = val
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key}, val)
}

// mappingValue 在映射节点中按 key 取值节点。
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// setMappingKey 更新或新增映射中的字符串键。
func setMappingKey(mapping *yaml.Node, key, val string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			if len(mapping.Content[i+1].Content) == 0 { // 标量才直接改值
				mapping.Content[i+1].Value = val
				mapping.Content[i+1].Tag = "!!str"
			}
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: val, Tag: "!!str"})
}

// marshalNode 以 2 空格缩进序列化。
func marshalNode(doc *yaml.Node) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	enc.Close()
	return []byte(b.String()), nil
}

// ProviderAPIKey 解析当前生效的 API 密钥：直填 apiKey 优先，其次 apiKeyEnv
// 指定的环境变量，再回退通用名 FRIDAY_API_KEY 与旧名 DEEPSEEK_API_KEY（兼容）。
// 全部为空表示未配置——允许启动，对话在配置密钥前不可用。
func (c *Config) ProviderAPIKey() string {
	p := c.Snapshot().Provider
	if p.APIKey != "" {
		return p.APIKey
	}
	for _, name := range []string{p.APIKeyEnv, "FRIDAY_API_KEY", "DEEPSEEK_API_KEY"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

// applyDefaults 填充未配置字段的合理默认值。
func (c *Config) applyDefaults() {
	if c.Provider.Name == "" {
		c.Provider.Name = "openai"
	}
	if c.Provider.APIKeyEnv == "" {
		c.Provider.APIKeyEnv = "FRIDAY_API_KEY"
	}
	if c.Provider.Temperature == 0 {
		c.Provider.Temperature = 0.7
	}
	if c.Provider.MaxTokens == 0 {
		c.Provider.MaxTokens = 1024
	}
	if c.Audio.SampleRate == 0 {
		c.Audio.SampleRate = 16000
	}
	if c.Audio.Channels == 0 {
		c.Audio.Channels = 1
	}
	if c.ASR.Engine == "" {
		c.ASR.Engine = "none"
	}
	if c.Wake.Engine == "" {
		c.Wake.Engine = "none"
	}
	if c.TTS.Engine == "" {
		c.TTS.Engine = "none"
	}
	if c.VAD.Threshold == 0 {
		c.VAD.Threshold = 0.5
	}
	if c.VAD.MinSilence == 0 {
		c.VAD.MinSilence = 0.6
	}
	if c.VAD.MinSpeech == 0 {
		c.VAD.MinSpeech = 0.25
	}
	if c.Display.Driver == "" {
		c.Display.Driver = "console"
	}
	if len(c.File.AllowDirs) == 0 {
		c.File.AllowDirs = []string{"."}
	}
	if c.TaskFile == "" {
		c.TaskFile = "tasks.json"
	}
	if c.TodoFile == "" {
		c.TodoFile = "todos.json"
	}
	if c.Admin.Port == 0 {
		c.Admin.Port = 8080
	}
	if c.Admin.Host == "" {
		c.Admin.Host = "127.0.0.1"
	}
	if c.Admin.AccountFile == "" {
		c.Admin.AccountFile = "admin-account.json"
	}
	if c.Memory.File == "" {
		c.Memory.File = "memories.json"
	}
	if c.Memory.Dir == "" {
		c.Memory.Dir = "memories"
	}
	if c.Context.ArchiveDir == "" {
		c.Context.ArchiveDir = "archive"
	}
	if c.SessionFile == "" {
		c.SessionFile = "session.json"
	}
	if c.UsageFile == "" {
		c.UsageFile = "token-usage.json"
	}
	if c.PausedFile == "" {
		c.PausedFile = "paused-turns.json"
	}
	if c.PlanFile == "" {
		c.PlanFile = "plans.json"
	}
	if c.Plan.MaxIterations <= 0 {
		c.Plan.MaxIterations = 12
	}
	if c.Cost.Currency == "" {
		c.Cost.Currency = "CNY"
	}
	if c.Cost.PricingFile == "" {
		c.Cost.PricingFile = "pricing.json"
	}
	if c.Cost.Budget.SoftRatio == 0 {
		c.Cost.Budget.SoftRatio = 0.8
	}
	if c.Cost.Goals.MaxConcurrent <= 0 {
		c.Cost.Goals.MaxConcurrent = 1
	}
	if c.Skills.Dir == "" {
		c.Skills.Dir = "skills"
	}
	if c.Heartbeat.File == "" {
		c.Heartbeat.File = "HEARTBEAT.md"
	}
	if c.Tools.Exec.Mode == "" {
		c.Tools.Exec.Mode = "confirm"
	}
	if c.Permissions.MCP == "" {
		c.Permissions.MCP = "allow"
	}
	if c.Trace.File == "" {
		c.Trace.File = "traces.json"
	}
}

// Validate 校验必填项。
func (c *Config) Validate() error {
	// API 密钥不在此校验：未配置允许启动（对话前经设置页/环境变量补配），
	// 运行时由 ProviderAPIKey() 解析，为空时 LLM 请求报错并给出指引。
	if c.Provider.BaseURL == "" {
		return fmt.Errorf("config: provider.baseURL 不能为空（如 https://api.deepseek.com/v1）")
	}
	if c.Provider.Model == "" {
		return fmt.Errorf("config: provider.model 不能为空（如 deepseek-chat）")
	}
	if c.Provider.Name != "openai" {
		return fmt.Errorf("config: provider.name 仅支持 openai（OpenAI 兼容层），当前为 %q", c.Provider.Name)
	}
	if _, err := NormalizePermissionMode(c.Permissions.Mode); err != nil {
		return fmt.Errorf("config: permissions.mode 非法: %v", err)
	}
	if r := c.Cost.Budget.SoftRatio; r <= 0 || r > 1 {
		return fmt.Errorf("config: cost.budget.softRatio 需落在 (0,1] 区间，当前 %g", r)
	}
	if c.Cost.Budget.DailyTokens < 0 || c.Cost.Budget.MonthlyTokens < 0 ||
		c.Cost.Budget.DailyCost < 0 || c.Cost.Budget.MonthlyCost < 0 ||
		c.Cost.Budget.PerPlanTokens < 0 || c.Cost.Budget.PerGoalTokens < 0 ||
		c.Cost.Budget.PerGoalSteps < 0 {
		return fmt.Errorf("config: cost.budget 各项额度不能为负（0 表示该项不限）")
	}
	// 心跳每轮都要过一遍模型且无人监督，间隔过短等于失控烧钱（ROADMAP 运维提示
	// 建议 ≥30m）；这里挡住明显的误配，启动即报错好过半夜账单。
	if c.Heartbeat.Enabled && c.Heartbeat.Schedule == "" && strings.TrimSpace(c.Heartbeat.Interval) != "" {
		interval := strings.TrimSpace(c.Heartbeat.Interval)
		d, err := time.ParseDuration(interval)
		if err != nil {
			return fmt.Errorf("config: heartbeat.interval %q 无法解析为时长（如 \"30m\"）: %w", interval, err)
		}
		if d < time.Minute {
			return fmt.Errorf("config: heartbeat.interval 不得短于 1m（当前 %q）——心跳每轮都调用模型，过短会失控烧钱", interval)
		}
	}
	return nil
}
