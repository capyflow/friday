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
  `exec_command`（shell 执行，默认 30s 超时上限 10min、输出截断 100KB）；
  经 vortexagent 全局权限层把关，审批三模式（`tools.exec.mode`）：`auto`（白名单内直接执行）/
  `confirm`（进网页审批队列，管理后台弹卡片，5 分钟未处理自动拒绝，可"批准并记住"）/
  `off`（不注册 exec_command）；
  白名单按 shell 语义段级校验（复合命令每段都须命中）；`rm -rf`、`mkfs` 等毁灭性命令有
  内置黑名单兜底（含藏在命令替换里的片段），任何模式都拦截
- **全局单对话 + 持久化**：网页/终端/钉钉/飞书共享同一个小五（同一份对话上下文）——
  网页说过的话在终端能接着聊；心跳/智能任务用独立会话，巡检内容不污染主对话；
  历史落盘 `session.json`，重启不失忆；管理后台可查看会话、按会话清除
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
│   （共用 Brain：全局单对话，各渠道同一份上下文；        │
│     历史经 SessionPool 落盘重启恢复；心跳用独立会话）    │
└────────────────────┬────────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────┐
│  vortexagent Agent（内部/agent）                 │
│  ├─ LLM Provider：OpenAI 兼容（DeepSeek/Qwen）     │
│  ├─ 工具注册表（内部/tools）：                     │
│  │   schedule_task（单次/多次/循环/智能任务）        │
│  │   remember / recall / forget（长期记忆）        │
│  │   read_file / list_files / write_file / edit_file │
│  │   exec_command（全局权限层：白名单/黑名单/审批）     │
│  │   get_time / speak_now / display_now           │
│  │   play_audio / play_effect                     │
│  │   mcp_<server>_<tool>（MCP 动态注册，内部/mcp）   │
│  ├─ Permissions：全局工具权限层（白名单/黑名单/审批模式，│
│  │   未放行调用桥接审批队列，网页批准后执行）           │
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

## 快速开始（Docker 部署）

```bash
# 1. 构建并启动（capyflow/* 为私有 Go 模块，构建需传 SSH 凭据）
docker compose up -d --build

# 2. 首次运行会自动生成 data/config.yaml 与 .env：
#    - data/config.yaml：补全 provider 段（baseURL / model / apiKeyEnv）
#    - .env：填入真实密钥（变量名与 apiKeyEnv 对应，如 STEP_API_KEY=sk-xxx）
#    改完重启生效：docker compose restart
```

服务地址：`http://<主机IP>:8090`（管理后台 `/admin`；端口由 `data/config.yaml` 的
`admin.port` 决定，compose 里 `ports`/`ADMIN_PORT` 同步调整）。日常运维：

```bash
docker compose logs -f        # 看日志
docker compose restart        # 改配置后重启（会话/任务自动恢复）
docker compose down           # 停止（数据都在 ./data，不丢）
```

## 本地开发（go run）

不走 Docker 时：

```bash
go mod tidy
export STEP_API_KEY=sk-xxx                 # 密钥环境变量（名与 config.yaml apiKeyEnv 一致）
cp config.yaml.example config.yaml         # 编辑确认 baseURL/model
go run ./cmd/robot                         # 或 go build -o robot ./cmd/robot && ./robot
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

钉钉/飞书与网页/终端共享同一份对话上下文（全局单对话）；
`allowUserIDs` / `allowOpenIDs` 白名单留空 = 允许所有联系人。

## 执行类工具与审批（安全边界）

agent 能写文件、跑命令，危险操作经 vortexagent **全局权限层**把关
（`tools.exec` 翻译成权限规则，见 `internal/agent/permission.go`），未放行的调用
桥接到审批队列（`internal/approval`），在管理后台网页批准：

| 模式 | exec_command | write_file / edit_file |
|------|------|------------------------|
| `auto` | 白名单内直接执行，白名单外拒绝 | 直接执行 |
| `confirm`（默认） | 白名单内直接执行；白名单外进**网页审批队列**（管理后台右下角弹卡片，5 分钟未处理自动拒绝，可"批准并记住"——相同调用本进程内免审批） | 需网页批准后执行 |
| `off` | 不注册 exec_command | 不受影响 |

审批与拦截全程留痕（调用轨迹可查）；毁灭性命令黑名单（`rm -rf` / `mkfs` /
`shutdown` 等）翻译成 deny 规则，任何模式（含用户批准）都强制拦截，且对整条命令
做词边界扫描——藏在命令替换里的片段（如 `echo $(rm -rf /tmp/x)`）同样拦得住。
白名单按 shell 语义段级校验：复合命令（`; && || |`）拆段后每一段都须命中白名单项。
MCP 工具默认免审批，可用 `permissions.mcp: confirm` 收紧进审批，`permissions.allow`
/ `deny` 可配置任意附加规则。

> 注意：权限规则是字符串级过滤，防模型误用，不防蓄意构造的绕过（如 base64 解码
> 后执行）；高危环境请配合操作系统级沙箱使用。

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

进程崩溃数秒内自动重启并恢复工具；管理后台"设置 → MCP 服务器"可**在线增删改配置**
（保存即热生效并写回 config.yaml）、查看状态并手动重连。

**两种接入方式**（二选一）：本地命令（stdio 子进程，如上）；远程 URL（Streamable HTTP，
适合只有地址的托管服务）：

```yaml
mcp:
  servers:
    - name: remote-tools
      url: https://mcp.example.com/mcp
      headers: {"Authorization": "Bearer xxx"}   # 可选鉴权头
```

## Docker 部署

部署编排见 [`docker-compose.yml`](docker-compose.yml)：多阶段构建（Go 1.25 编译 +
alpine 运行时，最终镜像约 40MB）、`./data:/data` 数据卷、`restart: unless-stopped`、
内置健康检查。`capyflow/*` 是私有 Go 模块，构建时 compose 已配置把 SSH 凭据传给
BuildKit（依赖 `~/.ssh` 里能访问 GitHub 的密钥）。

```bash
docker compose up -d --build   # 构建并启动
```

目录约定（迁移自旧的宿主机直接运行方式，运行数据全部收口在 `./data`）：

```
data/
  config.yaml          # 主配置（首次启动自动播种模板）
  .env（仓库根）        # API 密钥等敏感环境变量，不入库（变量名对应 provider.apiKeyEnv）
  session.json / tasks.json / memories.json / admin-account.json / uploads/ / HEARTBEAT.md
```

- 非 root 用户（UID 1000）运行；bind mount 场景 `./data` 属主保持 1000 即可
- 管理员可用 `FRIDAY_ADMIN_USERNAME` / `FRIDAY_ADMIN_PASSWORD` 预置（.env 里配）
- 时区默认 `Asia/Shanghai`（定时任务/心跳 cron 依赖），compose 里 `-e TZ` 覆盖
- 改了 `data/config.yaml` 的 `admin.port` 需同步 compose 的 `ports` 与 `ADMIN_PORT`
- 容器自带 busybox `sh`，`exec_command` 开箱可用；MCP 远程 URL 开箱即用，
  本地 stdio 服务器需自行扩展镜像安装对应运行时（如 node）

## 测试

```bash
go test ./...         # 全部测试通过（不依赖真实 LLM/网络）
go test -race ./...   # 并发安全（会话池/审批队列/MCP 管理器）
go vet ./...
```

覆盖范围：会话池与 Brain 持久化（假 OpenAI 服务器全链路）、审批队列（批准/拒绝/
超时/记住）、权限层规则翻译（白名单/黑名单/直通名单/审批桥接）、
write_file/edit_file 工具（逃逸、备份、审批往返）、智能任务触发与输出协议、
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
