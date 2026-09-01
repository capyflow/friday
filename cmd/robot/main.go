// Command robot 是陪伴智能体机器人的入口：加载配置、组装 vortexagent Agent、
// 启动提醒引擎（一次性 + 循环任务），以文本 REPL 或语音（sherpa-onnx，-tags sherpa）
// 方式接收用户指令。
//
// 用法：
//
//	export DEEPSEEK_API_KEY=sk-...
//	cp config.yaml.example config.yaml && vim config.yaml
//	go run ./cmd/robot                      # 文本模式（开发）
//	go run -tags sherpa ./cmd/robot          # 语音模式（需安装 sherpa-onnx 与模型）
//
// 文本模式内置命令：/help /reminders /cancel <id> /self-test /quit
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
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/reminder"
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

	eng := reminder.NewEngine(func(r reminder.Reminder) {
		log.Printf("[提醒] 到点：%s", r.Text)
		if err := speaker.Speak(ctx, r.Text); err != nil {
			log.Printf("播报失败: %v", err)
		}
		if err := displayer.Display(ctx, r.Text); err != nil {
			log.Printf("显示失败: %v", err)
		}
	})
	defer eng.Close()

	registry := vxagent.NewRegistry()
	if err := robotagent.RegisterTools(registry, eng, speaker, displayer); err != nil {
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
		case line == "/reminders":
			printReminders(eng)
			return
		case strings.HasPrefix(line, "/cancel "):
			id := strings.TrimSpace(strings.TrimPrefix(line, "/cancel "))
			if eng.Cancel(id) {
				fmt.Printf("已取消提醒 #%s\n", id)
			} else {
				fmt.Printf("未找到提醒 #%s\n", id)
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
		fmt.Println()
	}

	if vc != nil {
		fmt.Println("🎤 语音模式已启用：说出唤醒词开始对话（" + strings.Join(cfg.Wake.Keywords, "/") + "）")
		loop := voice.VoiceLoop(vc, 8*time.Second)
		if err := loop.Run(ctx, func(c context.Context, text string) error {
			handle(c, text)
			return nil
		}); err != nil && ctx.Err() == nil {
			log.Fatalf("语音循环退出: %v", err)
		}
		return
	}

	fmt.Println("🤖 小五已启动（文本模式，provider=" + cfg.Provider.Model + "）")
	fmt.Println("输入指令，如：10分钟后提示我该敷面膜了。/help 查看命令。")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		handle(ctx, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Printf("读取输入失败: %v", err)
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

// printReminders 打印当前所有未触发提醒与循环任务。
func printReminders(eng *reminder.Engine) {
	rs := eng.List()
	if len(rs) == 0 {
		fmt.Println("当前没有待触发的提醒。")
	} else {
		fmt.Println("待触发提醒：")
		for _, r := range rs {
			fmt.Printf("  #%-8s %s  %s\n", r.ID, r.When.Format("01-02 15:04:05"), r.Text)
		}
	}
	recs := eng.ListRecurring()
	if len(recs) > 0 {
		fmt.Println("循环任务：")
		for _, r := range recs {
			fmt.Printf("  #%-8s cron=%s  %s\n", r.ID, r.Cron, r.Text)
		}
	}
}

// runSelfTest 不依赖 LLM 演示"定时提醒 → 到点播报+显示"完整链路。
func runSelfTest(eng *reminder.Engine) {
	const delay = 5 * time.Second
	fmt.Printf("自检：5 秒后触发提醒“该敷面膜了”……（观察 [TTS]/[屏幕] 输出）\n")
	id, err := eng.Schedule("", time.Now().Add(delay), "该敷面膜了")
	if err != nil {
		log.Printf("自检失败: %v", err)
		return
	}
	fmt.Printf("已注册提醒 #%s，%s 后触发。\n", id, delay)
}

func printHelp() {
	fmt.Println(`命令：
  <任意指令>      提问，如"10分钟后提示我该敷面膜了"
  /reminders      查看待触发提醒与循环任务
  /cancel <id>    取消提醒
  /self-test      演示定时提醒链路（无需 LLM）
  /quit           退出`)
}
