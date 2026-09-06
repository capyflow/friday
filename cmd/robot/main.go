// Command robot 是陪伴智能体机器人的入口：加载配置、组装 vortexagent Agent、
// 启动任务引擎（单次/多次/循环），以文本 REPL 或语音（sherpa-onnx，-tags sherpa）
// 方式接收用户指令。
//
// 用法：
//
//	export DEEPSEEK_API_KEY=sk-...
//	cp config.yaml.example config.yaml && vim config.yaml
//	go run ./cmd/robot                      # 文本模式（开发）
//	go run -tags sherpa ./cmd/robot          # 语音模式（需安装 sherpa-onnx 与模型）
//
// 文本模式内置命令：/help /tasks /cancel <id> /self-test /quit
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
	"syscall"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/llm"

	robotagent "github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/admin"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/voice"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	displayer := output.NewConsoleDisplayer(os.Stdout)
	speaker, vc, err := buildVoice(cfg)
	if err != nil {
		log.Printf("语音不可用，回退文本模式: %v", err)
	}
	if speaker == nil {
		speaker = output.NewConsoleSpeaker(os.Stdout)
	}
	player := audio.Player(audio.NewConsolePlayer(os.Stdout))
	if vc != nil {
		player = vc.Player
	}
	voiceEnabled := vc != nil
	keywords := cfg.Wake.Keywords
	if voiceEnabled && len(vc.Keywords) > 0 {
		keywords = vc.Keywords
	}

	mem := memory.NewStore(cfg.Memory.File)
	eng := task.NewEngineFile(func(t task.Task) {
		log.Printf("[任务] 到点：%s", t.Text)
		if err := speaker.Speak(ctx, t.Text); err != nil {
			log.Printf("播报失败: %v", err)
		}
		if err := displayer.Display(ctx, t.Text); err != nil {
			log.Printf("显示失败: %v", err)
		}
	}, cfg.TaskFile)
	defer eng.Close()
	log.Printf("[任务] 持久化文件：%s（重启自动恢复）", cfg.TaskFile)

	if cfg.Admin.IsEnabled() {
		accounts := admin.NewAccountStore(cfg.Admin.AccountFile)
		adminServer := admin.New(cfg.Admin.Port, accounts, eng, mem, admin.Info{
			VoiceEnabled: voiceEnabled,
			Keywords:     keywords,
			Model:        cfg.Provider.Model,
			BaseURL:      cfg.Provider.BaseURL,
			TaskFile:     cfg.TaskFile,
			MemoryFile:   cfg.Memory.File,
		})
		adminServer.Start(ctx)
		log.Printf("[管理后台] http://0.0.0.0:%d（账号文件 %s，首次登录账号 admin/admin 需重新设置）",
			cfg.Admin.Port, cfg.Admin.AccountFile)
	}

	registry := vxagent.NewRegistry()
	if err := robotagent.RegisterTools(registry, eng, mem, speaker, displayer, player); err != nil {
		log.Fatalf("注册工具失败: %v", err)
	}

	var streamed strings.Builder
	ag, err := robotagent.Build(cfg, registry, func(d llm.Delta) {
		fmt.Print(d.Text)
		streamed.WriteString(d.Text)
	})
	if err != nil {
		log.Fatalf("创建 agent 失败: %v", err)
	}

	session := robotagent.NewSession()
	handle := func(ctx context.Context, line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
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

		streamed.Reset()
		answer, err := ag.Ask(ctx, session, line)
		if err != nil {
			log.Printf("agent 出错: %v", err)
			return
		}
		if streamed.Len() == 0 && answer != "" {
			fmt.Println(answer)
		}
		// 语音模式下把最终回复播报出来（文本模式无 TTS，仅打印）。
		if vc != nil {
			spoken := answer
			if spoken == "" {
				spoken = streamed.String()
			}
			if s := stripMarkdown(spoken); strings.TrimSpace(s) != "" {
				if err := speaker.Speak(ctx, s); err != nil {
					log.Printf("语音播报失败: %v", err)
				}
			}
		}
		fmt.Println()
	}

	if vc != nil {
		kw := vc.Keywords
		if len(kw) == 0 {
			kw = cfg.Wake.Keywords
		}
		fmt.Println("🎤 语音模式已启用：说出唤醒词开始对话（" + strings.Join(kw, "/") + "）")
		loop := voice.VoiceLoop(vc, 8*time.Second)
		if err := loop.Run(ctx, func(c context.Context, text string) error {
			log.Printf("[语音] 识别到: %s", text)
			handle(c, text)
			return nil
		}); err != nil && ctx.Err() == nil {
			log.Fatalf("语音循环退出: %v", err)
		}
		return
	}

	fmt.Println("🤖 小五已启动（文本模式，provider=" + cfg.Provider.Model + "）")
	fmt.Println("输入指令，如：10分钟后提示我该敷面膜了。/help 查看命令。")
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
				return
			}
			handle(ctx, line)
		}
	}
}

// buildVoice 按配置尝试构建语音组件；文本模式返回 (nil, nil, nil)。
// 语音模式需要 -tags sherpa 编译。
func buildVoice(cfg *config.Config) (output.Speaker, *voice.Components, error) {
	if cfg.Wake.Engine == "none" && cfg.ASR.Engine == "none" && cfg.TTS.Engine == "none" {
		return nil, nil, nil
	}
	vc, err := voice.NewComponents(cfg)
	if err != nil {
		return nil, nil, err
	}
	return &voice.SpeakerAdapter{Synth: vc.TTS, Player: vc.Player}, vc, nil
}

// stripMarkdown 去掉 LLM 回复中的 Markdown 符号，避免 TTS 把星号井号读出来。
func stripMarkdown(s string) string {
	return strings.NewReplacer("*", "", "#", "", "`", "", "_", "", "~", "", ">", "").Replace(s)
}

// printTasks 打印当前所有待触发任务（单次/多次/循环，按下次触发时间排序）。
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
		}
	}
}

// runSelfTest 不依赖 LLM 演示"定时任务 → 到点播报+显示"完整链路。
func runSelfTest(eng *task.Engine) {
	const delay = 5 * time.Second
	fmt.Printf("自检：5 秒后触发任务“该敷面膜了”……（观察 [TTS]/[屏幕] 输出）\n")
	tsk, err := eng.Schedule(task.Task{Kind: task.KindOnce, When: time.Now().Add(delay), Text: "该敷面膜了"})
	if err != nil {
		log.Printf("自检失败: %v", err)
		return
	}
	fmt.Printf("已注册任务 #%s，%s 后触发。\n", tsk.ID, delay)
}

func printHelp() {
	fmt.Println(`命令：
  <任意指令>      提问，如"10分钟后提示我该敷面膜了"
  /tasks          查看待触发任务（单次/多次/循环）
  /cancel <id>    取消任务
  /self-test      演示定时任务链路（无需 LLM）
  /quit           退出`)
}
