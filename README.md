# Friday 陪伴智能体机器人

用 Go 实现的陪伴辅助机器人：像朋友一样聊天、主动关心用户日常，并能执行"X 分钟后提醒我敷面膜"、
"每天早上 8 点提醒我喝水"这类定时/循环任务。核心大脑基于
[vortexagent](https://github.com/capyflow/vortexagent)（通用 Go agent 框架），
计划部署到能跑 Linux 的开发板（树莓派 / 香橙派 / Luckfox 等），当前在桌面环境开发。

## 功能

- **陪伴对话**：自然聊天、情绪回应、主动关心（LLM 驱动，OpenAI 兼容：DeepSeek / 通义 / Qwen 等）
- **定时提醒**：`10分钟后提示我该敷面膜了` → 到点自动播报 + 屏幕显示
- **循环任务**：`每天早上8点提醒我喝水`、`每周一三五9点播报天气`（自然语言 → cron）
- **立即播报/显示**：`speak_now` / `display_now` 工具
- **语音交互**（可选）：唤醒词 → 录音 → 本地 ASR → 对话 → TTS 播报（sherpa-onnx，见 `README-voice.md`）
- **可扩展工具**：`agent.Tool` 接口实现 4 个方法即可加能力（GPIO、传感器、媒体播放……）

## 架构

```
┌─────────────────────────────────────────────────┐
│ 输入：文本 REPL（开发） / 唤醒词+语音（-tags sherpa）│
└────────────────────┬────────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────┐
│  vortexagent Agent（内部/agent）                  │
│  ├─ LLM Provider：OpenAI 兼容（DeepSeek/Qwen）     │
│  ├─ 工具注册表（内部/tools）：                     │
│  │   schedule_reminder / list_reminders / cancel_reminder
│  │   schedule_recurring / list_recurring / cancel_recurring
│  │   get_time / speak_now / display_now           │
│  └─ Hooks：日志 / 工具调用审计                      │
└────────────────────┬────────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────┐
│  排程引擎（内部/reminder）                         │
│  ├─ 一次性提醒：time.Timer                        │
│  └─ 循环任务：robfig/cron                         │
│  到点 → Speaker（TTS/console）+ Displayer（屏幕）  │
└─────────────────────────────────────────────────┘
```

## 快速开始（文本模式）

```bash
# 1. 依赖
go mod tidy

# 2. 配置 LLM（DeepSeek 示例）
export DEEPSEEK_API_KEY=sk-xxx
cp config.yaml.example config.yaml
# 编辑 config.yaml：确认 baseURL/model 与你的厂商一致

# 3. 运行
go run ./cmd/robot
```

对话示例：

```
🤖 小五已启动（文本模式，provider=deepseek-chat）
> 10分钟后提示我该敷面膜了
已设置提醒 #r1788264xxx：将于 20:12:30 触发。
（10 分钟后）[提醒] 到点：该敷面膜了
[TTS] 该敷面膜了
[屏幕] 该敷面膜了
```

内置命令：`/help` `/reminders` `/cancel <id>` `/self-test`（无需 LLM 的自检演示）`/quit`

## 测试

```bash
go test ./...     # 排程引擎、工具、agent 组装全通过（不依赖真实 LLM）
go vet ./...
```

## 语音模式（唤醒词 + 本地识别 + TTS）

依赖 sherpa-onnx（cgo）与本地模型，需以 `-tags sherpa` 编译。完整安装与模型下载步骤见
**[README-voice.md](README-voice.md)**（由语音层脚本 `scripts/download-models.sh` 配合）。

```bash
go run -tags sherpa ./cmd/robot    # config.yaml 中 asr/wake/tts.engine 设为 sherpa-onnx
```

## 扩展到 Linux 开发板

代码通过接口隔离硬件差异，换板子只需替换实现：

| 抽象 | 开发实现 | 板子上 |
|------|---------|--------|
| `output.Speaker` | Console / SpeakerAdapter(TTS+Player) | 同上（TTS 真实发音） |
| `output.Displayer` | Console | OLED/彩屏驱动（如 SSD1306/ST7789） |
| `audio.Recorder/Player` | portaudio | 同上（ALSA 后端） |
| `asr/wake/tts` | sherpa-onnx | 同上（模型放本地） |

适配步骤：
1. 目标板确认（树莓派生态最全，Go 直接跑；Luckfox 等确认 GPIO/I2C/ALSA 接口）
2. 音频：确认麦克风/喇叭的 ALSA 设备号，填入 `config.yaml` 的 `audio.inputDevice/outputDevice`
3. 屏幕：实现 `output.Displayer`（I2C/SPI 驱动），或先用 console 跑通
4. 交叉编译：`GOOS=linux GOARCH=arm64 go build -tags sherpa ./cmd/robot`

## 添加新能力（示例：控制 LED）

```go
type LedTool struct{ On bool }
func (t *LedTool) Name() string        { return "set_led" }
func (t *LedTool) Description() string { return "开关 LED 灯" }
func (t *LedTool) Schema() map[string]any {
    return map[string]any{"type": "object", "properties": map[string]any{
        "on": map[string]any{"type": "boolean"},
    }, "required": []string{"on"}}
}
func (t *LedTool) Call(_ context.Context, args map[string]any) (string, error) {
    on, _ := args["on"].(bool)
    // 控制 GPIO...
    return fmt.Sprintf("LED 已%s", map[bool]string{true: "打开", false: "关闭"}[on]), nil
}
// 在 agent.RegisterTools 里注册即可，LLM 会自动学会使用。
```

## 目录结构

```
cmd/robot/           入口：文本/语音双模式、信号处理
internal/agent/      vortexagent 组装：Provider、Hooks、系统提示词（陪伴人设）、工具注册
internal/tools/      agent 工具：提醒/循环任务/时间/播报/显示 + 自然语言解析
internal/reminder/   排程引擎：一次性（Timer）+ 循环（cron），带回调
internal/output/     Speaker/Displayer 抽象 + console 实现
internal/audio/      录音/播放抽象（16kHz mono PCM 约定）
internal/asr/        语音识别抽象
internal/tts/        语音合成抽象
internal/wake/       唤醒词抽象
internal/voice/      语音链路编排：唤醒→录音→识别→处理；sherpa 实现（build tag）
internal/config/     YAML 配置加载与校验
```