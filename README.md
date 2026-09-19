# Friday 陪伴智能体机器人

用 Go 实现的陪伴辅助机器人：像朋友一样聊天、主动关心用户日常，并能执行"X 分钟后提醒我敷面膜"、
"每天早上 8 点提醒我喝水"这类定时/循环任务。核心大脑基于
[vortexagent](https://github.com/capyflow/vortexagent)（通用 Go agent 框架），
计划部署到能跑 Linux 的开发板（树莓派 / 香橙派 / Luckfox 等），当前在桌面环境开发。

## 功能

- **Web 界面**：浏览器打开 `http://<设备IP>:8080/`，登录后进入统一界面，
  左侧导航：**聊天**（SSE 流式对话，打字机效果，可切换会话/清除上下文）、**运行状态**、**自动化**（定时/智能任务）、
  **记忆**、**设置**（模型热更新 / 会话管理 / 心跳清单编辑 / MCP 服务器状态）；
  聊天页点"调用轨迹"在右侧弹出抽屉查看每次调用的 LLM/工具/回答链路
- **陪伴对话**：自然聊天、情绪回应、主动关心（LLM 驱动，OpenAI 兼容：DeepSeek / 通义 / Qwen 等）
- **定时任务**（统一模型，三类）：
  - 单次：`10分钟后提示我该敷面膜了` → 到点自动播报 + 屏幕显示
  - 多次：`每10分钟提醒我喝水，共3次` → 按间隔触发指定次数
  - 循环：`每天早上8点提醒我喝水`、`每周一三五9点播报天气`（自然语言 → cron）
- **智能任务**（第四类）：`schedule_task` 传 `agent: true`，到点把指令交给 agent
  自主决策与行动（如"每天早上8点看看我的备忘录，把今天要做的事告诉我"），
  输出首行 `[推送]`/`[静默]` 决定是否打扰用户
- **心跳巡检**（对标 OpenClaw HEARTBEAT.md）：agent 定期醒来读 `HEARTBEAT.md` 清单自主检查
  （清单在管理后台"设置"页可编辑）；静默时段（`heartbeat.quietHours`，如 23:00-07:00）
  照常执行但只记轨迹不推送
- **任务管理**：`schedule_task` / `list_tasks` / `cancel_task` 工具，单次/多次/循环/智能通用
- **任务持久化**：任务落到本地 JSON（`taskFile`，默认 `tasks.json`），注册/取消/触发即原子写回，
  重启自动恢复；停机期间过期的单次任务在启动时立即补触发一次
- **长期记忆**：`remember` / `recall` / `forget` 工具让机器人记住用户偏好与习惯
  （`memories.json` 落盘，重启不丢），对话中按需回忆
- **管理后台**：基于 [vortexv3](https://github.com/capyflow/vortexv3)（Gin）的 Web 控制台
  （`http://<设备IP>:8080/admin`，与聊天页同一服务），查看运行状态、管理任务与长期记忆。
  账号密码登录：默认账号 `admin`/`admin`；建议在**启动时预置账号**——配置文件
  `admin.username`/`admin.password`，或环境变量 `FRIDAY_ADMIN_USERNAME`/`FRIDAY_ADMIN_PASSWORD`
  （优先），两者须成对提供，每次启动以它覆盖账号文件（bcrypt 哈希落盘）。
  登录页只提供登录，改账号通过启动配置完成
- **调用轨迹**：聊天页点"调用轨迹"弹出右侧抽屉，按请求查看 agent 的完整调用链：
  每轮 LLM 调用（token/耗时）、模型请求的工具与参数、工具返回结果、最终回答与错误；
  任务到点触发同样留痕。数据存内存（最近 200 条），重启清空
- **立即播报/显示**：`speak_now` / `display_now` 工具
- **声音播放**：`play_audio`（播放本地 16-bit PCM wav）`play_effect`（内置提示音 ding/dingdong/alert）工具
- **文件能力**：`read_file` / `list_files` 工具（目录白名单内读文本文件、按行分段、PDF 按页抽取），
  聊天可发送图片（`/api/upload`，多模态消息，需模型支持视觉）
- **执行类工具 + 审批**：`write_file`（自动备份 .bak）/ `edit_file`（old 唯一匹配防误改）/
  `exec`（30s 超时上限 120s、输出截断 32KB）；
  审批三模式（`tools.exec.mode`）：`auto`（白名单内直接执行）/ `confirm`（进网页审批队列，
  管理后台弹卡片，5 分钟未处理自动拒绝）/ `off`（禁用 exec）；
  `rm -rf`、`mkfs` 等毁灭性命令有内置黑名单兜底，任何模式都拦截
- **多会话 + 持久化**：网页/终端/钉钉/飞书聊天/智能任务各自拥有独立会话
  （`web` / `repl` / `ding-<chatID>` / `feishu-<chatID>` / `task`），不同会话并行对话互不阻塞，
  同一会话串行保证上下文连续；历史落盘 `session.json`，重启不失忆；
  管理后台可查看会话列表、按会话清除
- **消息通道**：钉钉 Stream 模式 + 飞书长连接（均 WebSocket 免公网 IP），单聊/群聊 @机器人、
  白名单、2000 字符安全分段、自动重连；心跳/智能任务输出可推送（钉钉自定义机器人 webhook /
  飞书 pushChatIDs）
- **MCP 客户端**：连接外部 MCP 服务器（stdio 子进程），工具以 `mcp_<服务器>_<工具>` 前缀
  动态注册进 agent，解锁整个 MCP 工具生态；进程随机器人启停、崩溃数秒内自动重启并恢复工具，
  管理后台展示连接状态并支持手动重连
- **可扩展工具**：`agent.Tool` 接口实现 4 个方法即可加能力（GPIO、传感器、媒体播放……）

> **语音交互已归档**：原唤醒词 → 录音 → 本地 ASR → 对话 → TTS 链路（sherpa-onnx）
> 移至 [`archive/voice-mode/`](archive/voice-mode/)，默认构建不再依赖任何 cgo；
> 恢复方法见该目录 README。

## 架构

```
┌─────────────────────────────────────────────────┐
│ 输入：网页聊天 / 终端 REPL / 钉钉 / 飞书 / 智能任务    │
│   （共用 Brain：按会话 key 独立会话，同会话串行、      │
│     跨会话并行；历史经 SessionPool 落盘重启恢复）      │
└────────────────────┬────────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────┐
│  vortexagent Agent（内部/agent）                 │
│  ├─ LLM Provider：OpenAI 兼容（DeepSeek/Qwen）     │
│  ├─ 工具注册表（内部/tools）：                     │
│  │   schedule_task（单次/多次/循环/智能任务）        │
│  │   remember / recall / forget（长期记忆）        │
│  │   read_file / list_files / write_file / edit_file │
│  │   exec（审批：auto/confirm/off + 黑名单兜底）     │
│  │   get_time / speak_now / display_now           │
│  │   play_audio / play_effect                     │
│  │   mcp_<server>_<tool>（MCP 动态注册，内部/mcp）   │
│  └─ Hooks：日志 / 工具调用审计（轨迹含会话归属）      │
└────────────────────┬────────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────┐
│  任务引擎（内部/task）                              │
│  ├─ 单次/多次：可重挂 time.Timer（无累积漂移）        │
│  ├─ 循环/心跳：robfig/cron（支持 @every）           │
│  ├─ 持久化：JSON 原子写回（taskFile），重启恢复       │
│  普通任务 → Speaker + Displayer                     │
│  智能任务 → Brain 自主决策 → [推送]/[静默] 投递       │
└─────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────┐
│  HTTP 服务（内部/admin + 内部/web，vortexv3/Gin）    │
│  /chat      网页聊天（go:embed 单页，公开）          │
│  /api/chat  SSE 流式对话接口                        │
│  /          管理后台 Dashboard（go:embed，鉴权）      │
│  /traces    调用轨迹页（go:embed；数据接口鉴权）        │
│  /api/sessions /api/approvals /api/mcp /api/heartbeat│
│  /api/status /api/tasks /api/memories /api/traces   │
│  与机器人共用 tasks.json / memories.json / session.json│
└─────────────────────────────────────────────────┘
```

## 快速开始

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

启动后两种入口：

- **网页聊天**：浏览器打开 `http://localhost:8080/chat`（端口同管理后台）
- **终端 REPL**：直接在运行终端输入

终端对话示例：

```
🤖 小五已启动（provider=deepseek-chat）
> 10分钟后提示我该敷面膜了
已安排单次任务 #t1788264xxx："该敷面膜了"，将于 20:12:30 触发。
（10 分钟后）[任务] 到点：该敷面膜了
[TTS] 该敷面膜了
[屏幕] 该敷面膜了
```

内置命令：`/help` `/tasks` `/cancel <id>` `/self-test`（无需 LLM 的自检演示）`/quit`

## 通道接入（钉钉 / 飞书）

两者均为 WebSocket 长连接，**无需公网 IP**，配置见 `config.yaml.example` 注释：

- **钉钉**：[open-dev.dingtalk.com](https://open-dev.dingtalk.com) 创建企业内部应用 →
  消息接收模式选"Stream 模式" → 开通"企业内部机器人" → 填 `chan.dingtalk.clientId/clientSecret`
- **飞书**：[open.feishu.cn](https://open.feishu.cn) 创建企业自建应用 → 开启机器人能力 →
  订阅 `im.message.receive_v1` 事件（长连接方式）→ 申请 `im:message` 读写权限 →
  填 `chan.feishu.appID/appSecret`

每个入口拥有独立会话（`ding-<chatID>` / `feishu-<chatID>`），与网页/终端互不干扰；
`allowUserIDs` / `allowOpenIDs` 白名单留空 = 允许所有联系人。

## 执行类工具与审批（安全边界）

agent 能写文件、跑命令，危险操作受审批机制约束（`internal/approval`）：

| 模式 | exec | write_file / edit_file |
|------|------|------------------------|
| `auto` | 白名单内直接执行，白名单外拒绝 | 直接执行 |
| `confirm`（默认） | 白名单内直接执行；白名单外进**网页审批队列**（管理后台右下角弹卡片，5 分钟未处理自动拒绝） | 需网页批准后执行 |
| `off` | 禁用 | 不受影响 |

审批与拦截全程留痕（调用轨迹可查）；黑名单命令（`rm -rf` / `mkfs` / `shutdown` 等）
任何模式都强制拦截；白名单匹配拒绝 `; | & ` ` > <` 等链式字符，防拼接绕过。

## 心跳巡检与智能任务

- **智能任务**：`schedule_task` 传 `agent: true`（或管理后台"自动化"表单选"智能任务"），
  到点把指令交给 agent 自主决策，输出首行 `[推送]`/`[静默]` 决定是否投递；
  推送目标：本地播报/屏幕 + 钉钉 webhook（`chan.dingtalk.pushWebhook`）+ 飞书（`chan.feishu.pushChatIDs`）
- **心跳**：`heartbeat.enabled: true` + `schedule`（cron）或 `interval`（如 `30m`）；
  每轮把 `HEARTBEAT.md` 清单喂给 agent 自主巡检；静默时段（`quietHours`）只记轨迹不推送；
  清单在管理后台"设置"页直接编辑

## MCP 工具生态

在 `config.yaml` 的 `mcp.servers` 声明 MCP 服务器（stdio 子进程），重启即自动拉起并握手，
工具以 `mcp_<服务器>_<工具>` 前缀注册进 agent：

```yaml
mcp:
  servers:
    - name: filesystem
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/home/user/shared"]
```

进程崩溃数秒内自动重启并恢复工具；管理后台"设置 → MCP 服务器"展示连接状态并支持手动重连。

## 测试

```bash
go test ./...         # 全部测试通过（不依赖真实 LLM/网络）
go test -race ./...   # 并发安全（会话池/审批队列/MCP 管理器）
go vet ./...
```

覆盖范围：会话池与 Brain 持久化（假 OpenAI 服务器全链路）、审批模式/超时/黑名单、
exec/write_file/edit_file 工具（白名单、逃逸、备份、审批往返）、智能任务触发与输出协议、
静默时段、飞书 adapter（@ 剥离/分段/白名单/轨迹）、MCP 客户端握手与生命周期
（自动重启、配置增减）、任务引擎 agent 类持久化恢复。

## 语音模式（已归档）

原 sherpa-onnx 语音链路（唤醒词 + 本地识别 + TTS + VAD + voicecheck 诊断）已整体移至
**[`archive/voice-mode/`](archive/voice-mode/)**，恢复方法见其 README。
归档后主项目为纯 Go 构建，不再需要 portaudio/sherpa-onnx 等 cgo 依赖与本地语音模型。

## 扩展到 Linux 开发板

代码通过接口隔离硬件差异，换板子只需替换实现：

| 抽象 | 开发实现 | 板子上 |
|------|---------|--------|
| `output.Speaker` | Console（后续接 TTS） | TTS 真实发音 |
| `output.Displayer` | Console | OLED/彩屏驱动（如 SSD1306/ST7789） |

适配步骤：
1. 目标板确认（树莓派生态最全，Go 直接跑；Luckfox 等确认 GPIO/I2C/ALSA 接口）
2. 屏幕：实现 `output.Displayer`（I2C/SPI 驱动），或先用 console 跑通
3. 交叉编译：`GOOS=linux GOARCH=arm64 go build ./cmd/robot`

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
cmd/robot/           入口：REPL + 网页聊天双入口、信号处理
internal/agent/      vortexagent 组装：Provider、Hooks、系统提示词（陪伴人设）、
                     工具注册；Brain（REPL/网页共享会话，互斥 + 流式增量分发）
internal/tools/      agent 工具：任务（单次/多次/循环）/时间/播报/显示 + 自然语言时间解析
internal/task/       统一任务引擎：单次/多次（可重挂 Timer）+ 循环（cron）+ JSON 落盘恢复
internal/memory/     长期记忆仓库：JSON 落盘、关键词搜索
internal/web/        网页聊天：内嵌聊天页（go:embed）+ SSE 流式 /api/chat
internal/admin/      管理后台：vortexv3（Gin）REST API + 内嵌 Dashboard/调用轨迹单页
internal/trace/      agent 调用轨迹：按请求 ID 归档 LLM/工具/回答/错误（内存环形缓冲）
internal/output/     Speaker/Displayer 抽象 + console 实现
internal/audio/      播放抽象 + wav 解析 + 内置音效（录音/唤醒等语音实现已归档）
internal/config/     YAML 配置加载与校验（语音配置字段保留，便于恢复归档功能）
archive/voice-mode/  已归档的语音链路：asr/tts/wake/vad/voice/voicecheck/portaudio
```
