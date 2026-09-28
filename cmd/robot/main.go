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
//	export DEEPSEEK_API_KEY=sk-...
//	cp config.yaml.example config.yaml && vim config.yaml
//	go run ./cmd/robot
//
// 启动后：
//   - 终端 REPL 直接输入对话，内置命令 /help /tasks /cancel <id> /self-test /quit
//   - 网页聊天 http://localhost:8080/（端口同管理后台）
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/admin"
	robotagent "github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/audio"
	dingtalk "github.com/capyflow/friday/internal/channel/dingtalk"
	feishu "github.com/capyflow/friday/internal/channel/feishu"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/mcp"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/todo"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/usage"
	"github.com/capyflow/friday/internal/web"
)

// 心跳任务的固定 ID：重启从任务文件恢复时按 ID 去重，避免重复注册。
const heartbeatTaskID = "heartbeat"

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 调用轨迹持久化：防抖落盘，重启后管理后台仍可查看历史调用。
	trace.Init(cfg.Trace.File)
	// token 用量账本：按天累计，落盘 token-usage.json，重启恢复。
	usage.Init("token-usage.json")

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
	// console 负责本地可见（硬件输出关闭时自动降级为空实现）；钉钉/飞书按配置挂载。
	notifiers := []robotagent.Notifier{&robotagent.ConsoleNotifier{Speaker: speaker, Displayer: displayer}}
	if cfg.Chan.Dingtalk.PushWebhook != "" {
		notifiers = append(notifiers, &dingtalk.WebhookNotifier{Webhook: cfg.Chan.Dingtalk.PushWebhook})
	}
	if cfg.Chan.Feishu.Enabled && cfg.Chan.Feishu.AppID != "" && len(cfg.Chan.Feishu.PushChatIDs) > 0 {
		notifiers = append(notifiers, feishu.NewPushNotifier(cfg.Chan.Feishu.AppID, cfg.Chan.Feishu.AppSecret, cfg.Chan.Feishu.PushChatIDs))
	}

	// 危险操作审批队列：exec_command 经全局权限层（白名单/黑名单/模式）桥接到
	// 此处审批；write_file/edit_file 在 confirm 模式直接入队。
	approver := approval.New(cfg.Tools.Exec.Mode, cfg.Tools.Exec.Allowlist, 0)
	log.Printf("[审批] exec 模式=%s，白名单 %d 条，超时 %s", approver.Mode(), len(cfg.Tools.Exec.Allowlist), approver.Timeout())

	// 智能任务运行器在 Brain 就绪后赋值（引擎回调经闭包引用）。
	var runner *robotagent.TaskRunner
	eng := task.NewEngineFile(func(t task.Task) {
		// 每次任务触发视为独立请求：分配请求 ID，投递日志串在同一条链路上。
		rctx := logx.WithID(ctx, logx.NextID())
		id := logx.IDFrom(rctx)
		trace.Begin(id, "task", robotagent.SessionTask, t.Text)
		if t.Kind == task.KindAgent {
			logx.Printf(rctx, "[智能任务] 到点：%s", t.Text)
			if runner != nil {
				runner.Run(rctx, t)
			}
			trace.End(id, "")
			return
		}
		logx.Printf(rctx, "[任务] 到点：%s", t.Text)
		delivered := 0
		for _, n := range notifiers {
			if err := n.Notify(rctx, t.Text); err != nil {
				logx.Printf(rctx, "[任务] 投递到 %s 失败: %v", n.Name(), err)
				trace.Record(rctx, "error", "投递到 %s 失败：%v", n.Name(), err)
				continue
			}
			delivered++
		}
		if delivered > 0 {
			trace.Record(rctx, "task", "提醒已投递到 %d 个通知渠道", delivered)
		} else {
			trace.Record(rctx, "task", "无可用通知渠道（未开硬件输出且未配置钉钉/飞书推送），提醒仅记录日志")
		}
		trace.End(id, "")
	}, cfg.TaskFile)
	defer eng.Close()
	log.Printf("[任务] 持久化文件：%s（重启自动恢复）", cfg.TaskFile)

	// 待办清单：JSON 落盘，重启不丢；agent 工具与管理后台"待办"页共用。
	todos := todo.NewStore(cfg.TodoFile)
	log.Printf("[待办] 持久化文件：%s", cfg.TodoFile)

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
	)
	regFn := func() *vxagent.Registry {
		regMu.Lock()
		defer regMu.Unlock()
		return currentReg
	}
	memStore := memory.NewWikiStore(cfg.Memory.Dir, cfg.Memory.File)
	log.Printf("[记忆] wiki 页面目录：%s", cfg.Memory.Dir)
	deps := robotagent.Deps{
		Engine:    eng,
		Todos:     todos,
		Memory:    memStore,
		Speaker:   speaker,
		Displayer: displayer,
		Player:    player,
		Config:    cfg,
		Approver:  approver,
	}

	// rebuild 重建注册表与 Agent（内置工具 + 当前在线的 MCP 工具），原子替换。
	var brain *robotagent.Brain
	rebuild := func(reason string) {
		reg := vxagent.NewRegistry()
		if err := robotagent.RegisterTools(reg, deps); err != nil {
			log.Printf("[重建] 注册内置工具失败（%s）: %v", reason, err)
			return
		}
		if mcpMgr != nil {
			if err := mcpMgr.RegisterTools(reg); err != nil {
				log.Printf("[重建] 注册 MCP 工具失败（%s）: %v", reason, err)
			}
		}
		ag, err := robotagent.Build(cfg, reg, pool, approver)
		if err != nil {
			log.Printf("[重建] 构建 Agent 失败（%s）: %v", reason, err)
			return
		}
		brain.SetAgent(ag)
		regMu.Lock()
		currentReg = reg
		regMu.Unlock()
		log.Printf("[重建] Agent 已重建（%s）", reason)
	}

	// 首次构建。
	registry := vxagent.NewRegistry()
	if err := robotagent.RegisterTools(registry, deps); err != nil {
		log.Fatalf("注册工具失败: %v", err)
	}
	ag, err := robotagent.Build(cfg, registry, pool, approver)
	if err != nil {
		log.Fatalf("创建 agent 失败: %v", err)
	}
	brain = robotagent.NewBrain(ag, pool)
	brain.SetCustom(cfg.Custom.Commands) // 自定义指令：触发词 → 提示词展开
	if n := len(cfg.Custom.Commands); n > 0 {
		log.Printf("[自定义指令] 已启用 %d 条（config custom.commands，重启生效）", n)
	}
	regMu.Lock()
	currentReg = registry
	regMu.Unlock()

	// MCP 服务器：随 Friday 拉起，工具动态注册（前缀 mcp_<server>_），崩溃自动重启。
	// 管理器始终创建（无配置时为空跑）：管理后台在线添加 MCP 服务器可即时生效。
	mcpMgr = mcp.NewManager(func() { rebuild("MCP 工具清单变化") })
	if len(cfg.Mcp.Servers) > 0 {
		servers := make([]mcp.ServerConfig, 0, len(cfg.Mcp.Servers))
		for _, s := range cfg.Mcp.Servers {
			servers = append(servers, mcp.ServerConfig{
				Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env,
				URL: s.URL, Headers: s.Headers,
			})
		}
		if err := mcpMgr.Apply(ctx, servers); err != nil {
			log.Printf("[MCP] 部分服务器连接失败（不影响其余功能）: %v", err)
		}
	}
	defer mcpMgr.Close()

	// 智能任务运行器：到点把指令（心跳任务附 HEARTBEAT.md 清单）交给 agent 决策，
	// 输出按 [推送]/[静默] 协议投递到上方通知渠道。
	runner = &robotagent.TaskRunner{
		Ask:           brain,
		HeartbeatFile: cfg.Heartbeat.File,
		QuietHours:    cfg.Heartbeat.QuietHours,
		Notifiers:     notifiers,
	}

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
		chat := web.New(brain, "uploads")
		adminServer := admin.New(admin.Options{
			Port:          cfg.Admin.Port,
			Accounts:      accounts,
			Engine:        eng,
			Todos:         todos,
			Memory:        deps.Memory,
			Info:          admin.Info{Model: cfg.Provider.Model, BaseURL: cfg.Provider.BaseURL, TaskFile: cfg.TaskFile, TodoFile: cfg.TodoFile, MemoryDir: cfg.Memory.Dir},
			Chat:          chat,
			Cfg:           cfg,
			ConfigPath:    *cfgPath,
			Registry:      regFn,
			Approver:      approver,
			Mcp:           mcpMgr,
			HeartbeatFile: cfg.Heartbeat.File,
		})
		adminServer.Start(ctx)
		log.Printf("[网页入口] http://0.0.0.0:%d/（登录后：聊天 / 运行状态 / 自动化 / 记忆 / 设置）", cfg.Admin.Port)
		if user != "" {
			log.Printf("[管理后台] http://0.0.0.0:%d/admin（账号 %s，来源：启动配置/环境变量）", cfg.Admin.Port, user)
		} else {
			log.Printf("[管理后台] http://0.0.0.0:%d/admin（默认账号 admin/admin；可通过 FRIDAY_ADMIN_USERNAME/FRIDAY_ADMIN_PASSWORD 或 config.yaml admin.username/password 预置）",
				cfg.Admin.Port)
		}
	}

	// handle 处理一行用户输入：分配请求 ID（链路日志前缀）并挂主会话，
	// 内置命令直接处理，其余经 Brain 交给 agent（流式增量打印到终端）。
	handle := func(ctx context.Context, source, line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		ctx = logx.WithID(ctx, logx.NextID())
		ctx = robotagent.WithSessionKey(ctx, robotagent.SessionWeb)
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
			if eng.Cancel(id) {
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

	fmt.Println("🤖 小五（Friday 个人管家）已启动（provider=" + cfg.Provider.Model + "）")
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
