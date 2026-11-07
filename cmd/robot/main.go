// Command robot 是 Friday 个人管家的入口：加载配置、组装 vortexagent Agent、
// 启动任务引擎（单次/多次/循环/智能任务），以文本 REPL、网页聊天、钉钉、飞书
// 多种方式接收用户指令——全局单对话（同一份上下文，SessionPool 落盘重启不失忆），
// 共用同一个对话大脑（Brain）。工具调用经 vortexagent 全局权限层把关：
// 白名单/毁灭性黑名单/审批模式由 config 翻译成规则，未放行的操作进管理后台审批。
//
// 用法：
//
//	docker compose up -d --build   # 部署（数据卷 ./data，密钥放 .env）
//
// 或本地开发：
//
//	export FRIDAY_API_KEY=sk-...            # 通用命名（兼容旧名 DEEPSEEK_API_KEY）
//	cp config.yaml.example config.yaml && vim config.yaml
//	go run ./cmd/robot
//
// 启动后：
//   - 终端 REPL 直接输入对话，内置命令 /help /tasks /cancel <id> /self-test /quit
//   - 网页聊天 http://localhost:8080/（端口同管理后台）
//
// 桌面模式（供 Tauri 外壳拉起，见 desktop/）：
//
//		friday --desktop --data-dir <目录> --port 0
//
//	  - 进程 chdir 到数据目录，config.yaml 与全部数据文件落该目录；
//	  - 无终端 REPL，外壳关闭 stdin 即优雅退出；
//	  - --port 0 自动挑空闲端口，服务就绪后向 stdout 打印一行
//	    FRIDAY_ADDR=http://127.0.0.1:<port>（外壳解析后导航窗口）。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/acp"
	"github.com/capyflow/friday/internal/admin"
	robotagent "github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/audio"
	dingtalk "github.com/capyflow/friday/internal/channel/dingtalk"
	feishu "github.com/capyflow/friday/internal/channel/feishu"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/cost"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/mcp"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/notification"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/plan"
	"github.com/capyflow/friday/internal/skill"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/terminal"
	"github.com/capyflow/friday/internal/todo"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/usage"
	"github.com/capyflow/friday/internal/web"
)

// 心跳任务的固定 ID：重启从任务文件恢复时按 ID 去重，避免重复注册。
const heartbeatTaskID = "heartbeat"

// version 构建时经 -ldflags "-X main.version=vX.Y.Z" 注入。
var version = "dev"

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	desktop := flag.Bool("desktop", false, "桌面模式：数据落独立目录、无终端 REPL、stdin 关闭即退出（供 Tauri 桌面外壳拉起）")
	serviceMode := flag.Bool("service", false, "服务模式（launchd LaunchAgent）：数据目录语义与桌面模式相同，但无 /quit stdin 契约与 ppid 看门狗——内核生命周期归服务管理器")
	dataDir := flag.String("data-dir", "", "数据目录（桌面/服务模式：进程切换到该目录运行，默认取系统配置目录下的 Friday）")
	portFlag := flag.Int("port", -1, "覆盖 admin.port；0 表示自动挑选空闲端口（桌面外壳依赖此参数定位服务地址）")
	hostFlag := flag.String("host", "", "覆盖 admin.host（默认 127.0.0.1；远程部署显式用 0.0.0.0）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	// service 子命令（launchd 服务管理）自带 FlagSet，放 flag 解析前分流。
	if len(os.Args) > 1 && os.Args[1] == "service" {
		runServiceCommand(os.Args[2:])
		return
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	// 桌面/服务模式：先补用户 PATH（Finder/launchd 只有系统目录，npm/npx/uv 会 127），
	// 再落位数据目录（相对路径的 config.yaml 与数据文件全部基于它）。
	if *desktop || *serviceMode {
		mode := "桌面"
		if *serviceMode {
			mode = "服务"
		}
		ensureUserPath()
		dir := strings.TrimSpace(*dataDir)
		if dir == "" {
			base, err := os.UserConfigDir()
			if err != nil {
				log.Fatalf("%s模式无法定位系统配置目录: %v", mode, err)
			}
			dir = filepath.Join(base, "Friday")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Fatalf("%s模式创建数据目录失败: %v", mode, err)
		}
		if err := os.Chdir(dir); err != nil {
			log.Fatalf("%s模式切换数据目录失败: %v", mode, err)
		}
		log.Printf("[%s] 数据目录：%s（config.yaml / 会话 / 待办 / 记忆均在此目录）", mode, dir)
	}

	if err := config.EnsureInitial(*cfgPath); err != nil {
		log.Fatalf("初始配置准备失败: %v", err)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}
	if *portFlag >= 0 {
		if *portFlag == 0 {
			p, err := freePort()
			if err != nil {
				log.Fatalf("挑选空闲端口失败: %v", err)
			}
			cfg.Admin.Port = p
		} else {
			cfg.Admin.Port = *portFlag
		}
	}
	if *hostFlag != "" {
		cfg.Admin.Host = *hostFlag
	}
	if *desktop || *serviceMode {
		// 本机 HTTP 门恒为回环（外壳/本机浏览器走这里）；局域网访问走 TLS
		// 监听（见 admin TLS 配置），两个门是两套独立 listener。
		cfg.Admin.Host = "127.0.0.1"
	}
	if (*desktop || *serviceMode) && !cfg.Admin.IsEnabled() {
		log.Fatalf("桌面/服务模式要求管理后台保持开启（admin.enabled 不能配成 false），外壳窗口与手机端依赖它提供界面")
	}
	log.Printf("[启动] Friday %s（provider=%s）", version, cfg.Provider.Model)
	// 密钥未配置不阻断启动：网页/桌面端均可在「设置」页补配（写回 config.yaml）。
	if cfg.ProviderAPIKey() == "" {
		log.Printf("[配置] 未检测到 LLM API Key：对话功能暂不可用（不影响启动）。可在管理后台「设置」页填写，或设环境变量 FRIDAY_API_KEY（见 config.yaml provider.apiKeyEnv）")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 调用轨迹持久化：防抖落盘，重启后管理后台仍可查看历史调用。
	trace.Init(cfg.Trace.File)
	// token 用量账本：按天+维度累计，落盘（默认 token-usage.json），重启恢复。
	// 文件损坏时保留原文件、本次运行只走内存账本，不覆盖历史数据。
	if err := usage.Init(cfg.UsageFile); err != nil {
		log.Printf("[警告] %v", err)
	}

	// 硬件输出（扬声器/屏幕/音频播放）：Friday 当前与硬件分离（纯软件管家），
	// 默认关闭（config hardware.enabled）。关闭时 speak_now/display_now 等工具不注册，
	// 任务到点的提醒经下方通知渠道（钉钉/飞书）投递；打开开关即恢复本地播报/显示。
	var speaker output.Speaker
	var displayer output.Displayer
	var player audio.Player
	if cfg.Hardware.Enabled {
		speaker = output.NewConsoleSpeaker(os.Stdout)
		displayer = output.NewConsoleDisplayer(os.Stdout)
		player = audio.NewConsolePlayer(os.Stdout)
		log.Printf("[硬件输出] 已开启（console 实现，接 TTS/OLED/喇叭后替换实现）")
	} else {
		log.Printf("[硬件输出] 已关闭（纯软件形态），提醒经通知渠道投递")
	}

	// 通知渠道：普通任务到点与智能任务 [推送] 的投递目标。
	// 仅注册真正有输出的通道；网页通知中心始终保存提醒。
	notifiers := []robotagent.Notifier{}
	if speaker != nil || displayer != nil {
		notifiers = append(notifiers, &robotagent.ConsoleNotifier{Speaker: speaker, Displayer: displayer})
	}
	if cfg.Chan.Dingtalk.PushWebhook != "" {
		notifiers = append(notifiers, &dingtalk.WebhookNotifier{Webhook: cfg.Chan.Dingtalk.PushWebhook})
	}
	if cfg.Chan.Feishu.Enabled && cfg.Chan.Feishu.AppID != "" && len(cfg.Chan.Feishu.PushChatIDs) > 0 {
		notifiers = append(notifiers, feishu.NewPushNotifier(cfg.Chan.Feishu.AppID, cfg.Chan.Feishu.AppSecret, cfg.Chan.Feishu.PushChatIDs))
	}

	// 危险操作审批队列：exec_command 经全局权限层（白名单/黑名单/模式）桥接到
	// 此处审批；write_file/edit_file 在 confirm 模式直接入队。
	// permissions.mode（confirm/acceptEdits/fullAccess）决定全局审批强度：
	// 编辑类内部审批在 acceptEdits/fullAccess 下放行（设置页可在线切换）。
	approver := approval.New(cfg.Tools.Exec.Mode, cfg.Tools.Exec.Allowlist, 0)
	approver.SetEditsFree(cfg.Permissions.EditsAutoApproved())
	log.Printf("[审批] 权限模式=%s，exec 模式=%s，白名单 %d 条，超时 %s",
		cfg.Permissions.EffectiveMode(), approver.Mode(), len(cfg.Tools.Exec.Allowlist), approver.Timeout())

	// 先可靠入队再推进任务；工作器在 Brain 就绪后启动。
	var runner *robotagent.TaskRunner
	var goalRunner *robotagent.GoalRunner
	var todoStore *todo.Store
	acceptReady := make(chan struct{})
	targets := make([]notification.Target, 0, len(notifiers))
	for _, n := range notifiers {
		targets = append(targets, n)
	}
	notices, err := notification.New("notifications.json", func(workCtx context.Context, t task.Task) (text string, push bool, runErr error) {
		if t.TodoID != "" {
			allowed, err := todoStore.ShouldRemind(t)
			if err != nil {
				return "", false, err
			}
			if !allowed {
				return "关联待办已完成、删除或更新，提醒已取消", false, nil
			}
		}
		rctx := logx.WithID(workCtx, logx.NextID())
		rctx = robotagent.WithOrigin(rctx, robotagent.OriginTask)
		id := logx.IDFrom(rctx)
		trace.Begin(id, "task", robotagent.SessionTask, t.Text)
		defer func() {
			if runErr != nil {
				trace.End(id, runErr.Error())
			} else {
				trace.End(id, "")
			}
		}()
		if t.Goal {
			if goalRunner == nil {
				return "", false, fmt.Errorf("目标运行器尚未就绪")
			}
			return goalRunner.Prepare(rctx, t)
		}
		if t.Kind == task.KindAgent {
			if runner == nil {
				return "", false, fmt.Errorf("智能任务运行器尚未就绪")
			}
			return runner.Prepare(rctx, t)
		}
		trace.Record(rctx, "task", "提醒已保存到通知中心：%s", t.Text)
		return t.Text, true, nil
	}, targets)
	if err != nil {
		log.Fatalf("加载通知记录失败: %v", err)
	}
	notices.SetWebEnabled(cfg.Admin.IsEnabled())
	if *desktop {
		notices.SetDesktopDispatch(func(n notification.Notice) error {
			payload, err := json.Marshal(map[string]string{"id": n.ID, "text": n.Text})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(os.Stdout, "FRIDAY_NOTICE=%s\n", payload)
			return err
		})
	}
	eng := task.NewReliableEngineFile(func(_ context.Context, t task.Task) error {
		select {
		case <-acceptReady:
		default:
			return fmt.Errorf("待办关联初始化尚未完成")
		}
		if allowed, err := todoStore.ShouldRemind(t); err != nil {
			return err
		} else if !allowed {
			return nil
		}
		return notices.Enqueue(t)
	}, cfg.TaskFile)
	if err := eng.Err(); err != nil {
		log.Fatalf("加载任务失败: %v", err)
	}
	defer eng.Close()
	log.Printf("[任务] 持久化文件：%s（重启自动恢复）", cfg.TaskFile)

	// 待办清单：JSON 落盘，重启不丢；agent 工具与管理后台"待办"页共用。
	todos := todo.NewStore(cfg.TodoFile)
	todoStore = todos
	notices.SetDeliveryGuard(todos.ShouldRemind)
	if err := todos.Err(); err != nil {
		log.Fatalf("加载待办失败: %v", err)
	}
	log.Printf("[待办] 持久化文件：%s", cfg.TodoFile)
	if err := todos.BindEngine(eng); err != nil {
		log.Printf("[待办] 提醒同步待恢复（门卫已阻止错误触发）: %v", err)
	}
	if err := notices.RestoreSnoozes(eng); err != nil {
		log.Fatalf("恢复稍后提醒失败: %v", err)
	}
	close(acceptReady)

	// 会话持久化：JSON 落盘，重启不失忆；池按 key（web/repl/ding-*/feishu-*/task）隔离。
	store, err := sessionstore.NewJSON(cfg.SessionFile)
	if err != nil {
		log.Fatalf("加载会话文件失败: %v", err)
	}
	pool := robotagent.NewSessionPool(store)
	log.Printf("[会话] 持久化文件：%s（全局单对话，重启自动恢复）", cfg.SessionFile)

	// 工具注册表：MCP 配置变化时会整体重建（rebuild），regHolder 持有当前实例。
	var (
		regMu      sync.Mutex
		currentReg *vxagent.Registry
		mcpMgr     *mcp.Manager
		rebuild    func(string)
	)
	regFn := func() *vxagent.Registry {
		regMu.Lock()
		defer regMu.Unlock()
		return currentReg
	}
	// MCP 管理器先于 Agent 首建创建（mcp_edit 工具依赖它），onChange 触发 rebuild；
	// rebuild 在下方赋值，首次 Apply 发生在 brain 就绪之后，不会提前触发。
	mcpMgr = mcp.NewManager(func() { rebuild("MCP 工具清单变化") })
	// 远程 MCP 的 OAuth 会话落在数据目录里（0600：含 refresh_token 与
	// client_secret）。桌面模式下进程已切到数据目录，相对名即落在该目录。
	mcpMgr.EnableOAuth("mcp-oauth.json")
	memStore := memory.NewWikiStore(cfg.Memory.Dir, cfg.Memory.File)
	log.Printf("[记忆] wiki 页面目录：%s", cfg.Memory.Dir)

	// 上下文引擎：长会话不线性膨胀（摘要区 + 最近块 + 按需检索，原文不可变归档）。
	// 引擎绑定 provider/model，随 Agent 重建路径一起重建（见 rebuild 闭包与热更新）。
	ctxMgr := robotagent.NewContextManager(robotagent.WikiFactSink(memStore))

	// 技能仓库必须在注册工具之前就位：read_skill 工具靠它按需取正文（超预算的技能只把
	// 概览交给模型）。技能层关闭时保持 nil，工具会直接回「技能层未启用」。
	var skillStore *skill.Store
	if cfg.Skills.EnabledOrDefault() {
		skillStore = skill.NewStoreRoots(skillRoots(cfg.Skills.Dir, skill.UserScopeDir, workDir())...)
	}

	deps := robotagent.Deps{
		Skills:     skillStore,
		Engine:     eng,
		Todos:      todos,
		Memory:     memStore,
		Ctx:        ctxMgr,
		Speaker:    speaker,
		Displayer:  displayer,
		Player:     player,
		Config:     cfg,
		Approver:   approver,
		Mcp:        mcpMgr,
		ConfigPath: *cfgPath,
		Search: func() (string, string) {
			s := cfg.Snapshot().Tools.Search
			return s.ProviderOrDefault(), s.APIKey
		},
	}

	// rebuild 重建注册表与 Agent（上下文引擎 + 内置工具 + 当前在线的 MCP 工具），原子替换。
	var brain *robotagent.Brain
	rebuild = func(reason string) {
		if err := cfg.Apply(func(next *config.Config) error {
			return brain.Reconfigure(func() (*vxagent.Agent, error) {
				if err := ctxMgr.Rebuild(next); err != nil {
					return nil, err
				}
				reg := vxagent.NewRegistry()
				localDeps := deps
				localDeps.Config = next
				if err := robotagent.RegisterTools(reg, localDeps); err != nil {
					return nil, err
				}
				if mcpMgr != nil {
					if err := mcpMgr.RegisterTools(reg); err != nil {
						return nil, err
					}
				}
				ag, err := robotagent.Build(next, reg, pool, approver, ctxMgr.Current())
				if err != nil {
					return nil, err
				}
				regMu.Lock()
				currentReg = reg
				regMu.Unlock()
				return ag, nil
			})
		}); err != nil {
			log.Printf("[重建] Agent 重建失败（%s）: %v", reason, err)
			return
		}
		log.Printf("[重建] Agent 已重建（%s）", reason)
	}

	// 上下文引擎首建与启动日志。
	if err := ctxMgr.Rebuild(cfg); err != nil {
		log.Printf("[上下文] 引擎初始化失败（对话将不归档，修复配置后重启生效）: %v", err)
	} else if ctxMgr.Current() != nil {
		log.Printf("[上下文] 已启用：归档 %s，候选事实→%s",
			cfg.Context.ArchiveDir,
			map[bool]string{true: "记忆 wiki", false: "丢弃"}[cfg.Context.FactsEnabled()])
	} else {
		log.Printf("[上下文] 未启用（context.enabled: false），长会话历史完整发送")
	}

	// 首次构建。
	registry := vxagent.NewRegistry()
	if err := robotagent.RegisterTools(registry, deps); err != nil {
		log.Fatalf("注册工具失败: %v", err)
	}
	ag, err := robotagent.Build(cfg, registry, pool, approver, ctxMgr.Current())
	if err != nil {
		log.Fatalf("创建 agent 失败: %v", err)
	}
	brain = robotagent.NewBrain(ag, pool, ctxMgr)
	brain.SetMemory(memStore)            // 会话开始时注入长期记忆速览（清上下文后仍有"印象"）
	brain.SetProjectDir(workDir())       // 会话开始时注入项目上下文（工作目录下的 AGENTS.md / README.md）
	brain.SetCustom(cfg.Custom.Commands) // 自定义指令：触发词 → 提示词展开
	if n := len(cfg.Custom.Commands); n > 0 {
		log.Printf("[自定义指令] 已启用 %d 条（config custom.commands，重启生效）", n)
	}

	// 技能层：SKILL.md 目录（沿用 vortexagent 的框架格式，另加 Friday 的启用/触发键）。
	// 后台"设置 → 技能"增删改即时生效——技能是按次注入的，不需要重建 Agent。
	// 仓库在注册工具前就建好了（read_skill 要用），这里只接给 Brain 并打日志。
	if skillStore != nil {
		brain.SetSkills(skillStore, cfg.Skills.AutoTriggerEnabled())
		list := skillStore.List()
		broken := 0
		for _, s := range list {
			if s.ParseErr != "" {
				broken++
			}
		}
		auto := "关闭"
		if cfg.Skills.AutoTriggerEnabled() {
			auto = "开启"
		}
		log.Printf("[技能] 共 %d 个，自动匹配%s（技能需自身 auto: true 才参与）；发现根：%s",
			len(list), auto, strings.Join(rootSummary(skillStore), "、"))
		if broken > 0 {
			log.Printf("[技能] 其中 %d 个文件无效，已在后台标出原因（原文件保留，不参与选择）", broken)
		}
		if err := skillStore.Err(); err != nil {
			log.Printf("[技能] %v", err)
		}
	}
	regMu.Lock()
	currentReg = registry
	regMu.Unlock()

	// MCP 服务器：随 Friday 拉起，工具动态注册（前缀 mcp_<server>_），崩溃自动重启。
	// 管理器已提前创建（mcp_edit 工具依赖）；此处按配置首次拉起，之后聊天里经
	// mcp_edit 增删改查均可即时生效。
	if len(cfg.Mcp.Servers) > 0 {
		servers := make([]mcp.ServerConfig, 0, len(cfg.Mcp.Servers))
		for _, s := range cfg.Mcp.Servers {
			servers = append(servers, mcp.ServerConfig(s))
		}
		if err := mcpMgr.Apply(ctx, servers); err != nil {
			log.Printf("[MCP] 部分服务器连接失败（不影响其余功能）: %v", err)
		}
	}
	defer mcpMgr.Close()

	// 智能体（ACP）对话空间：独立于 Brain 的平行入口，thread 投影落盘
	// （重启不失历史），进程空闲回收、session/load 重接。默认关闭
	// （acp.enabled: false），关闭时接口拒绝新消息、侧栏入口由前端隐藏。
	acpMgr := acp.NewManager(func() config.AcpConfig { return cfg.Snapshot().Acp.Normalized() },
		approver, "acp-threads.json")
	acpMgr.SetMemory(memStore) // 工作区首轮说明附带项目记忆速览
	// 系统提示桥：审批卡入队、一轮结束这两个时刻，人通常已经切到别的窗口，
	// 页面里的卡片和气泡等于没发生。走通知中心的 PostOnce（按 key 幂等，同一
	// 事件不重复打扰），桌面弹原生通知、浏览器走网页通知，内容都留在通知中心。
	acpMgr.SetNotifier(func(key, text string) {
		if _, err := notices.PostOnce(key, text, nil); err != nil {
			log.Printf("[智能体] 系统提示未落档: %v", err)
		}
	})
	// 内置 Friday 也进智能体清单（工作区新建会话时可选 "friday"）：一轮对话经进程内的
	// ACP 适配器交给这个 Brain，所以外部 agent 那套会话编排、轮前快照、改动还原、
	// 停止与重接对内置会话同样成立。差别写在 builtin.go 顶部（模型/权限是全局的）。
	acpMgr.SetBuiltin(brain)
	defer acpMgr.Close()
	if cfg.Acp.Enabled {
		// 内置条目不算进配置里的 agent 数（它是代码给的，用户没配过）
		log.Printf("[智能体] 已启用（%d 个 agent + 内置 Friday，并发 %d，空闲 %ds 回收）",
			len(cfg.Acp.WithoutBuiltin().Agents), cfg.Acp.MaxConcurrent, cfg.Acp.IdleKillSeconds)
	}

	// 内置终端：服务端 PTY + 界面里的 xterm.js，只服务本机回环（admin 层硬闸，
	// 配置关不掉那条判断）。关掉的是"能不能开新终端"，不是那道闸。
	termCfgFn := func() terminal.Config {
		t := cfg.Snapshot().Terminal
		return terminal.Config{Enabled: t.IsEnabled(), MaxSessions: t.MaxSessions, Shell: t.Shell}
	}
	termMgr := terminal.NewManager(termCfgFn)
	defer termMgr.CloseAll() // 内核退出时收掉进程组，别留下挂在已关 PTY 上的 shell
	if tc := termCfgFn(); tc.Enabled {
		log.Printf("[终端] 内置终端可用（本机回环，并发 %d；远程部署不服务）", tc.Max())
	}

	// 成本闸门：价格表（默认 pricing.json，后台可编辑、热加载）+ 软限额判定。
	// 没有价格表不影响使用——预算按 token 判定，金额留空并在后台提示补价。
	pricer, pricerErr := cost.NewPricer(cfg.Cost.PricingFile)
	if pricerErr != nil {
		log.Printf("[成本] %v（暂只按 token 判预算）", pricerErr)
	}
	guard := cost.NewGuard(cfg, pricer)
	log.Printf("[成本] 预算：%s（软限额，超线只停主动任务）", describeBudget(cfg, pricer))
	go startCostAlert(ctx, guard, notifiers, notices)

	// 计划模式：规划轮先产出步骤清单 → 网页确认 → 按步执行（一步一次提问、
	// 一条独立轨迹、一份独立成本）。执行挂应用级 ctx，不挂某次 HTTP 请求。
	var planStore *plan.Store
	var planPlanner *plan.Planner
	var planRunner *plan.Runner
	if cfg.Plan.EnabledOrDefault() {
		ps, err := plan.NewStore(cfg.PlanFile)
		if err != nil {
			log.Printf("[计划] %v（本次运行不加载历史计划）", err)
		}
		planStore = ps
		planPlanner = &plan.Planner{Ask: brain, Store: planStore, MaxIterations: cfg.Plan.MaxIterations}
		planRunner = plan.NewRunner(ctx, brain, planStore)
		planRunner.MaxIterations = cfg.Plan.MaxIterations
		planRunner.PerPlanTokens = cfg.Cost.Budget.PerPlanTokens
		log.Printf("[计划] 已启用（清单 %s，单步轮数上限 %d，计划 token 预算 %d）",
			cfg.PlanFile, cfg.Plan.MaxIterations, cfg.Cost.Budget.PerPlanTokens)
	}

	// 智能任务运行器：到点把指令（心跳任务附 HEARTBEAT.md 清单）交给 agent 决策，
	// 输出按 [推送]/[静默] 协议投递到上方通知渠道。
	runner = &robotagent.TaskRunner{
		Ask:           brain,
		HeartbeatFile: cfg.Heartbeat.File,
		QuietHours:    cfg.Heartbeat.QuietHours,
		Notifiers:     notifiers,
		Budget:        guard,
	}

	// 目标运行器：goal 任务到点后的多轮自主推进（隔离会话 + 步数/token 预算）。
	goalRunner = robotagent.NewGoalRunner(brain, notifiers)
	goalRunner.Budget = guard
	goalRunner.MaxConcurrent = cfg.Cost.Goals.MaxConcurrent
	goalRunner.ConfigureSlots(cfg.Cost.Goals.MaxConcurrent)
	if cfg.Cost.Budget.PerGoalSteps > 0 {
		goalRunner.DefaultSteps = cfg.Cost.Budget.PerGoalSteps
	}
	if cfg.Cost.Budget.PerGoalTokens > 0 {
		goalRunner.DefaultTokens = cfg.Cost.Budget.PerGoalTokens
	}
	goalRunner.StepIterations = cfg.Plan.MaxIterations
	log.Printf("[目标] 已启用（并发 %d，默认预算 %d 步 / %d token）",
		cfg.Cost.Goals.MaxConcurrent, goalRunner.DefaultSteps, goalRunner.DefaultTokens)

	// 心跳任务（config heartbeat）：到点读检查清单自主巡检。
	if cfg.Heartbeat.Enabled {
		if _, err := registerHeartbeat(eng, cfg); err != nil {
			log.Printf("[心跳] 注册失败: %v", err)
		} else {
			log.Printf("[心跳] 已启用（文件 %s，静默时段 %q）", cfg.Heartbeat.File, cfg.Heartbeat.QuietHours)
		}
	}

	// 消息通道：钉钉 Stream 模式 / 飞书长连接（均免公网 IP）。
	if cfg.Chan.Dingtalk.Enabled && cfg.Chan.Dingtalk.ClientID != "" {
		adapter := dingtalk.New(cfg.Chan.Dingtalk, brain)
		go func() {
			if err := adapter.Start(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[钉钉] 通道退出: %v", err)
			}
		}()
		log.Printf("[钉钉] 通道已启动（Stream 模式，clientID=%s）", cfg.Chan.Dingtalk.ClientID)
	}
	if cfg.Chan.Feishu.Enabled && cfg.Chan.Feishu.AppID != "" {
		adapter := feishu.New(cfg.Chan.Feishu, brain)
		go func() {
			if err := adapter.Start(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[飞书] 通道退出: %v", err)
			}
		}()
		log.Printf("[飞书] 通道已启动（长连接模式，appID=%s）", cfg.Chan.Feishu.AppID)
	}

	if cfg.Admin.IsEnabled() {
		accounts := admin.NewAccountStore(cfg.Admin.AccountFile)
		if err := accounts.Err(); err != nil {
			log.Fatalf("加载管理员账号失败（原文件已保留）: %v", err)
		}
		// 启动预置凭据（配置文件 admin.username/password 或环境变量
		// FRIDAY_ADMIN_USERNAME/FRIDAY_ADMIN_PASSWORD）：提供即覆盖现有账号。
		user, pass, err := cfg.ResolveAdminCredentials()
		if err != nil {
			log.Fatalf("解析管理员启动凭据: %v", err)
		}
		if user != "" {
			if err := accounts.Update(user, pass); err != nil {
				log.Fatalf("按启动配置设置管理员账号失败: %v", err)
			}
			log.Printf("[管理后台] 已按启动配置设置管理员账号 %s（每次启动以配置/环境变量为准）", user)
		}
		chat := web.New(brain, "uploads", cfg.PausedFile)
		chat.EnablePlans(planPlanner, planRunner)
		chat.EnableGoals(eng, func() (bool, string) { return guard.ProactiveAllowed("目标运行") })
		current := cfg.Snapshot()
		adminServer := admin.New(admin.Options{
			Host:          cfg.Admin.Host,
			Port:          cfg.Admin.Port,
			Accounts:      accounts,
			AuthKeyFile:   filepath.Join(filepath.Dir(cfg.Admin.AccountFile), "auth-key"),
			Engine:        eng,
			Todos:         todos,
			Memory:        deps.Memory,
			Info:          admin.Info{Model: current.Provider.Model, BaseURL: current.Provider.BaseURL, TaskFile: cfg.TaskFile, TodoFile: cfg.TodoFile, MemoryDir: cfg.Memory.Dir},
			Chat:          chat,
			Cfg:           cfg,
			ConfigPath:    *cfgPath,
			Registry:      regFn,
			Approver:      approver,
			Mcp:           mcpMgr,
			Acp:           acpMgr,
			Terminal:      termMgr,
			HeartbeatFile: cfg.Heartbeat.File,
			Notifications: notices,
			Pricer:        pricer,
			Guard:         guard,
			Skills:        skillStore,
			Plans:         planStore,
			PlanRunner:    planRunner,
			GoalRunner:    goalRunner,
		})
		// TLS 局域网门（手机直连）：启用时证书落数据目录 tls/，监听 0.0.0.0:<tlsPort>。
		if cfg.Admin.TLS.IsEnabled() {
			adminServer.SetTLS(admin.TLSOptions{Port: cfg.Admin.TLS.Port, Dir: filepath.Join("tls")})
		}
		if err := adminServer.Start(ctx); err != nil {
			log.Fatalf("启动管理后台失败: %v", err)
		}
		addr := "http://" + net.JoinHostPort(cfg.Admin.Host, fmt.Sprint(cfg.Admin.Port))
		log.Printf("[网页入口] %s/（登录后：聊天 / 运行状态 / 自动化 / 通知 / 记忆 / 设置）", addr)
		if user != "" {
			log.Printf("[管理后台] %s/admin（账号 %s，来源：启动配置/环境变量）", addr, user)
		} else if accounts.Current().SetupRequired {
			log.Printf("[管理后台] %s/（请在本机页面创建管理员账号；远程部署可通过启动凭据预置）", addr)
		} else {
			log.Printf("[管理后台] %s/admin（使用已有管理员账号）", addr)
		}
		// 桌面/服务模式：服务就绪后落盘 kernel.addr（外壳冷启动「发现已有内核」
		// 复用，attach 模式）；桌面模式再向 stdout 打印 FRIDAY_ADDR 标记，外壳逐行
		// 解析 stdout 拿到地址后导航主窗口（docs/DESKTOP.md 启动链路第 3/4 步）。
		if *desktop || *serviceMode {
			if !waitReady(cfg.Admin.Port, 30*time.Second) {
				log.Fatalf("等待服务就绪超时（端口 %d）", cfg.Admin.Port)
			}
			addrLocal := fmt.Sprintf("http://127.0.0.1:%d", cfg.Admin.Port)
			writeAddrFile("kernel.addr", addrLocal)
			if *desktop {
				fmt.Printf("FRIDAY_ADDR=%s\n", addrLocal)
			}
		}
	}
	noticeCtx, stopNotices := context.WithCancel(ctx)
	notices.Start(noticeCtx)
	defer func() { eng.Close(); stopNotices(); notices.Wait(); ctxMgr.Close() }()

	// handle 处理一行用户输入：分配请求 ID（链路日志前缀）并挂主会话，
	// 内置命令直接处理，其余经 Brain 交给 agent（流式增量打印到终端）。
	handle := func(ctx context.Context, source, line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		ctx = logx.WithID(ctx, logx.NextID())
		ctx = robotagent.WithSessionKey(ctx, robotagent.SessionWeb)
		ctx = robotagent.WithOrigin(ctx, robotagent.OriginRepl)
		logx.Printf(ctx, "%s: %s", source, line)
		switch {
		case line == "/quit" || line == "/exit":
			fmt.Println("再见 👋")
			stop()
			return
		case line == "/help":
			printHelp()
			return
		case line == "/tasks" || line == "/reminders":
			printTasks(eng)
			return
		case strings.HasPrefix(line, "/cancel "):
			id := strings.TrimSpace(strings.TrimPrefix(line, "/cancel "))
			ok, err := eng.CancelErr(id)
			if err != nil {
				fmt.Printf("取消任务失败: %v\n", err)
			} else if ok {
				fmt.Printf("已取消任务 #%s\n", id)
			} else {
				fmt.Printf("未找到任务 #%s\n", id)
			}
			return
		case line == "/self-test":
			runSelfTest(eng)
			return
		}

		var streamed strings.Builder
		trace.Begin(logx.IDFrom(ctx), "repl", robotagent.SessionWeb, line)
		answer, err := brain.Ask(ctx, line, nil, func(d llm.Delta) {
			fmt.Print(d.Text)
			streamed.WriteString(d.Text)
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logx.Printf(ctx, "agent 出错: %v", err)
			trace.End(logx.IDFrom(ctx), err.Error())
			return
		}
		if streamed.Len() == 0 && answer != "" {
			fmt.Println(answer)
		}
		trace.End(logx.IDFrom(ctx), "")
		fmt.Println()
	}

	// 桌面模式（docs/DESKTOP.md 生命周期一致性三层保证）：无终端 REPL。
	// ① 外壳正常退出 → 向 stdin 写一行 /quit；② 外壳崩溃/被强杀 → stdin 管道 EOF；
	// ③ 看门狗兜底：孤儿进程被过继给 launchd/init（ppid=1）即优雅退出
	// （Windows 无进程过继机制，该检查恒不触发，退出依赖 ②）。
	// 退出统一走 stop() → ctx 取消 → 各组件 defer Close 的优雅退出链。
	// 桌面模式 stdout 只输出 FRIDAY_ADDR 一行（日志走 stderr），供外壳解析。
	if *desktop {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if os.Getppid() == 1 {
						log.Printf("[桌面] 检测到父进程已退出（ppid=1），优雅退出")
						stop()
						return
					}
				}
			}
		}()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "/quit" {
				log.Printf("[桌面] 收到外壳退出指令（/quit），优雅退出")
				break
			}
		}
		stop()
		return
	}

	fmt.Println("🤖 小五（Friday 个人管家）已启动（provider=" + cfg.Snapshot().Provider.Model + "）")
	fmt.Println("对我说\"10分钟后提醒我喝水\"\"记个待办：买牛奶\"\"上海天气\"，或 /help 查看命令。")
	// stdin 在独立 goroutine 中读取：主循环 select 监听 ctx，
	// 保证 SIGTERM/Ctrl+C 时即使阻塞在等待输入也能立即退出（并触发 Close 快照）。
	input := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			input <- scanner.Text()
		}
		close(input)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-input:
			if !ok {
				// stdin 关闭（后台/守护运行）：停用 REPL 但保持服务与定时任务，直到收到退出信号。
				input = nil
				log.Printf("[终端] stdin 已关闭，REPL 停用；网页聊天与任务引擎继续运行")
				continue
			}
			handle(ctx, "[终端] 输入", line)
		}
	}
}

// registerHeartbeat 按配置注册心跳任务：schedule（cron）优先，否则 interval（@every）。
// ID 固定，重启恢复时已存在则跳过。
func registerHeartbeat(eng *task.Engine, cfg *config.Config) (task.Task, error) {
	spec := strings.TrimSpace(cfg.Heartbeat.Schedule)
	if spec == "" {
		interval := strings.TrimSpace(cfg.Heartbeat.Interval)
		if interval == "" {
			return task.Task{}, fmt.Errorf("heartbeat.enabled 已开启，但未配置 schedule（cron）或 interval（如 30m）")
		}
		if _, err := time.ParseDuration(interval); err != nil {
			return task.Task{}, fmt.Errorf("heartbeat.interval %q 无法解析为时长: %w", interval, err)
		}
		spec = "@every " + interval
	}
	// 已恢复的心跳任务不重复注册（重启场景）。
	for _, t := range eng.List() {
		if t.ID == heartbeatTaskID {
			return t, nil
		}
	}
	return eng.Schedule(task.Task{
		ID:        heartbeatTaskID,
		Kind:      task.KindAgent,
		Heartbeat: true,
		Cron:      spec,
		Text:      "按心跳检查清单执行例行巡检，判断是否有需要主动告知用户的事项；没有就 [静默]。",
	})
}

// startCostAlert 周期检查成本态势：每天首次超预算时经通知渠道推送一次告警，
// 并在通知中心留档（同一天不重复，重启后当日已提醒过则不再打扰）。
func startCostAlert(ctx context.Context, guard *cost.Guard, notifiers []robotagent.Notifier, notices *notification.Center) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !guard.TakeOverAlert() {
				continue
			}
			text := "成本预算已用满：心跳巡检 / 智能任务 / 目标启动暂时停用（对话与定时提醒不受影响）。" +
				"用量与恢复入口见管理后台「运行状态」页。"
			var delivered []string
			alertCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			for _, n := range notifiers {
				if err := n.Notify(alertCtx, text); err == nil {
					delivered = append(delivered, n.Name())
				}
			}
			cancel()
			if _, err := notices.PostManual(text, delivered); err != nil {
				log.Printf("[成本] 超预算告警落档失败: %v", err)
			}
			log.Printf("[成本] 本日预算已用满，主动任务暂停（推送通道: %v）", delivered)
		}
	}
}

// describeBudget 把预算配置写成一行启动日志，方便无人值守时确认闸门到底开没开。
func describeBudget(cfg *config.Config, pricer *cost.Pricer) string {
	snapshot := cfg.Snapshot()
	b := snapshot.Cost.Budget
	parts := []string{}
	if b.DailyTokens > 0 {
		parts = append(parts, fmt.Sprintf("日 token %d", b.DailyTokens))
	}
	if b.MonthlyTokens > 0 {
		parts = append(parts, fmt.Sprintf("月 token %d", b.MonthlyTokens))
	}
	if b.DailyCost > 0 {
		parts = append(parts, fmt.Sprintf("日金额 %.2f %s", b.DailyCost, pricer.Currency()))
	}
	if b.MonthlyCost > 0 {
		parts = append(parts, fmt.Sprintf("月金额 %.2f %s", b.MonthlyCost, pricer.Currency()))
	}
	if len(parts) == 0 {
		return "未设置（只统计不拦截）"
	}
	if !snapshot.Cost.PauseProactiveEnabled() {
		return strings.Join(parts, " / ") + "，仅警示不暂停"
	}
	return strings.Join(parts, " / ")
}

// printTasks 打印当前所有待触发任务（单次/多次/循环/智能，按下次触发时间排序）。
func printTasks(eng *task.Engine) {
	tasks := eng.List()
	if len(tasks) == 0 {
		fmt.Println("当前没有待触发的任务。")
		return
	}
	fmt.Println("待触发任务：")
	for _, t := range tasks {
		next := t.Next.Format("01-02 15:04:05")
		switch t.Kind {
		case task.KindOnce:
			fmt.Printf("  #%-11s [单次] %s  %s\n", t.ID, next, t.Text)
		case task.KindRepeat:
			fmt.Printf("  #%-11s [多次] 每%s一次 剩%d/%d  下次 %s  %s\n",
				t.ID, task.FormatDuration(t.Interval), t.Remaining, t.Times, next, t.Text)
		case task.KindCron:
			fmt.Printf("  #%-11s [循环] cron=%-12s 下次 %s  %s\n", t.ID, t.Cron, next, t.Text)
		case task.KindAgent:
			mark := ""
			if t.Heartbeat {
				mark = "（心跳）"
			}
			fmt.Printf("  #%-11s [智能]%s cron=%-12s 下次 %s  %s\n", t.ID, mark, t.Cron, next, t.Text)
		}
	}
}

// runSelfTest 不依赖 LLM 演示"定时任务 → 到点投递提醒"完整链路。
func runSelfTest(eng *task.Engine) {
	const delay = 5 * time.Second
	fmt.Printf("自检：5 秒后触发任务\"该浇水了\"……（观察日志中的到点投递）\n")
	tsk, err := eng.Schedule(task.Task{Kind: task.KindOnce, When: time.Now().Add(delay), Text: "该浇水了"})
	if err != nil {
		log.Printf("自检失败: %v", err)
		return
	}
	fmt.Printf("已注册任务 #%s，%s 后触发。\n", tsk.ID, delay)
}

func printHelp() {
	fmt.Println(`命令：
  <任意指令>      提问或安排事项，如"10分钟后提醒我喝水""记个待办：买牛奶""上海天气"
  /tasks          查看待触发任务（单次/多次/循环/智能）
  /cancel <id>    取消任务
  /self-test      演示定时任务链路（无需 LLM）
  /quit           退出`)
}

// freePort 挑一个当前空闲的 TCP 端口（监听 :0 由内核分配后立刻关闭）。
// 桌面外壳传 --port 0 依赖它：端口在占用检查与服务监听之间可能被抢走，
// 但对单机个人应用是可接受的竞态。
// workDir 是"Friday 此刻在哪个目录里干活"的唯一答案：今天就是进程的工作目录
// （桌面模式启动时已 chdir 到数据目录，非桌面模式是启动时的当前目录）。
// 后续做"选一个目录工作"的模式时改这一处即可——技能的 .agents/skills 发现根与
// AGENTS.md / README.md 项目上下文都从这里取根。
func workDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// skillRoots 按优先级从高到低列出技能发现根：
//  1. <数据目录>/skills            后台"设置 → 技能"可写，改哪条生效哪条
//  2. <工作目录>/.agents/skills     项目共享（约定地址，只读）
//  3. ~/.agents/skills             个人全局（约定地址，只读）
//  4. <数据目录>/skills-user       个人技能（后台只读）
//
// 后台可写的那条排在最前：用户在界面上编辑的技能不该被某个看不见的共享文件悄悄压住。
// 约定地址不存在是常态（没建过就是没有），空目录不会报错，也不会被创建。
func skillRoots(dir, userDir, work string) []skill.Root {
	roots := []skill.Root{
		{Dir: dir, Scope: skill.ScopeProject, Source: "Friday 技能目录", Writable: true},
	}
	if p := filepath.Join(work, ".agents", "skills"); abs(p) != abs(dir) {
		roots = append(roots, skill.Root{Dir: p, Scope: skill.ScopeProject, Source: "项目 .agents/skills"})
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".agents", "skills")
		if abs(p) != abs(dir) {
			roots = append(roots, skill.Root{Dir: p, Scope: skill.ScopeUser, Source: "公共 ~/.agents/skills"})
		}
	}
	roots = append(roots, skill.Root{Dir: userDir, Scope: skill.ScopeUser, Source: "个人技能目录"})
	return roots
}

// rootSummary 把发现根写成"来源=路径(状态)"一串，启动日志里能一眼看出扫了哪几个目录、
// 哪个可写。约定地址读不到（还没建）时标"未建"，免得被当成故障。
func rootSummary(store *skill.Store) []string {
	out := make([]string, 0, 4)
	for _, r := range store.Roots() {
		state := "只读"
		if r.Writable {
			state = "可写"
		}
		if _, err := os.Stat(r.Dir); err != nil {
			state = "未建"
		}
		out = append(out, fmt.Sprintf("%s %s[%s]", r.Source, r.Dir, state))
	}
	return out
}

func abs(p string) string {
	if p == "" {
		return ""
	}
	if out, err := filepath.Abs(p); err == nil {
		return out
	}
	return p
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitReady 轮询本地管理后台直到 GET / 返回 200（服务真正开始接收请求）。
// 桌面外壳依赖就绪后打印的 FRIDAY_ADDR 标记，过早打印会让外壳导航到白屏。
func waitReady(port int, timeout time.Duration) bool {
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// writeAddrFile 把内核实际服务地址原子落盘（数据目录下 kernel.addr），供桌面
// 外壳冷启动「发现已有内核」复用（attach 模式）。写失败只记日志——它只是发现
// 通道之一，外壳探测失败会退回拉起 sidecar 的老路。
func writeAddrFile(path, addr string) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(addr), 0o644); err != nil {
		log.Printf("[地址] 落盘 %s 失败: %v", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[地址] 落盘 %s 失败: %v", path, err)
		_ = os.Remove(tmp)
	}
}
