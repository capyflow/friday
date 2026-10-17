# Friday 完整计划书（对标 OpenClaw / Hermes Agent）

> 2026-09 制定。对标 [OpenClaw](https://docs.openclaw.ai)（消息通道型个人助手，前身 Clawdbot/Moltbot）
> 与 [Hermes Agent](https://hermes-agent.nousresearch.com)（Nous Research，cron + 记忆 + 多 bot），
> 结合 Friday 的定位给出分阶段演进计划。
> 每个里程碑独立可交付、可验收；文档随实施进度更新。
>
> **2026-10-10 当前状态**：Vue 3 前端、双主题、Markdown 安全清洗、失败请求恢复、
> 可靠任务入队与通知中心已落地；配置热更新串行提交，会话清除等待在途请求落盘。
> 下文 2026-09 的里程碑验收描述保留为历史记录；当前会话策略以本节及 README 为准。
>
> **2026-09-24 定位调整**：Friday 重新定位为**纯软件的个人管家 agent**（待办/定时任务/天气/
> 长期记忆/自定义能力，部署在用户自己的服务器或电脑上），暂时与硬件分离——硬件输出
> （扬声器/屏幕）默认关闭（`hardware.enabled`），语音链路保持归档，作为后续可选方向。
> 下文涉及"开发板/家庭陪伴硬件"的表述均按此理解。
>
> **2026-09-19 交付**：M1.2 飞书 / M2 执行工具+审批 / M3 心跳主动任务 / M4 会话持久化+多会话 /
> M5 MCP 客户端 全部完成（单测 + race 全绿，假 LLM 端到端验证），详见各节状态与第七节决策记录。

---

## 一、当前基线（已完成）

| 能力 | 状态 |
|------|------|
| 对话入口 | 网页聊天（SSE 流式、图片上传）+ 终端 REPL |
| 管理后台 | 登录、运行状态、任务/记忆管理、模型热更新（写回 config.yaml，注释保留） |
| 通知中心 | 提醒正文与投递状态持久化；失败有限重试、人工重试与已确认通道去重 |
| 调用轨迹 | 每轮 LLM 输入快照（完整上下文 JSON）+ 原始输出 + 工具调用/结果 + token 统计（按模型/来源/会话/运行归属） |
| 技能 | `SKILL.md` 提示词技能（显式点名 / 触发词 / 描述打分自动匹配，opt-in），可覆盖模型与温度、限定本轮工具面；后台可管、即时生效 |
| 长程执行 | 计划模式（清单 → 网页确认 → 按步执行，每步独立会话/轨迹/成本，可暂停重试）；目标模式（隔离会话多轮自主推进 + 步数/token 预算） |
| 成本 | 价格表热加载、金额估算、预算三态（ok/warn/over）与软限额（超线只停主动型工作） |
| 任务引擎 | 单次 / 多次 / 循环（cron），JSON 落盘，重启恢复 |
| 长期记忆 | wiki 页面仓库（`memories/<id>.md`）+ remember/recall/forget 工具，agent 先查相关页再合并整编；管理后台 wiki 化浏览/编辑 |
| 文件工具 | read_file / list_files（目录白名单、行分段、PDF 按页抽取） |
| 图片 | 上传（浏览器端统一转 JPEG + 降采样）、多模态消息（依赖模型支持视觉） |
| 模型 | OpenAI 兼容层，baseURL/model 热更新 |
| 消息通道 | **钉钉 Stream 模式**（WebSocket 免公网，已交付：单聊/群聊 @、白名单、rune 安全分段） |

**当前架构与后续方向**：
- 用户对话由 SessionPool 持久化，网页/终端/钉钉/飞书共用主会话；智能任务使用独立会话。
- 普通提醒与智能任务先进入可靠通知队列；模型决策和真实投递分开，支持恢复结果和失败重试。
- 三层扩展：自定义指令（触发词→提示词展开）、**技能**（SKILL.md，可覆盖模型/温度并限定本轮工具面）、
  MCP 动态工具（`mcp_edit` 支持对话内增删改，热生效）。技能「携带」Go 工具/脚本仍属后续方向。
- **计划模式**已交付：规划轮产结构化清单 → 网页确认 → 按步执行（一步一次提问、一条轨迹、一份成本，
  可暂停/继续/取消，失败步可重试或跳过）。**目标模式**已交付：独立会话多轮推进 + 步数/token 预算 + 成本闸门。
  余项：计划步骤间的条件分支，以及执行中途由用户插话改道。
- 后续优先完善首次配置/模型诊断、桌面系统通知、任务编辑/暂停、记忆版本恢复与预算。

P0-01～04 的首次配置、主动提醒、任务管理与待办联动已完成本地开发和模拟验收。真实服务与正式发布验收见 [P0-TODO.md](P0-TODO.md)，当前使用方法见 [P0-USAGE.md](P0-USAGE.md)。

---

## 二、差距分析（对标 OpenClaw / Hermes）

### P0 —— 决定性差距

| # | 差距 | OpenClaw / Hermes 的做法 | 对应里程碑 |
|---|------|------------------------|-----------|
| 1 | 消息通道 | OpenClaw：WhatsApp/Telegram/Signal/Slack/Discord/iMessage 等十余通道；Hermes：cron 结果投递任意通道 | M1 |
| 2 | 执行类工具 | OpenClaw agent 可跑 shell；Hermes 可操作 GitHub（审 PR、理 issue） | M2 |
| 3 | 主动式心跳 | OpenClaw HEARTBEAT.md：agent 定期醒来按清单自主决策要不要行动/打扰 | M3 |

### P1 —— 重要差距

| # | 差距 | 对标做法 | 对应里程碑 |
|---|------|---------|-----------|
| 4 | 会话持久化 + 多会话 | 多会话并行、per-channel 路由、历史可回溯 | M4 |
| 5 | MCP 集成 | Hermes 统一 MCP 命令中心（生态扩张核心） | M5 |
| 6 | 记忆升级 | Hermes 可插拔记忆 provider（wiki 页面 + agent 自维护整编已落地，余项：语义检索/自动去重） | P2 池 |
| 7 | 技能/插件系统 | OpenClaw skills 目录（2026-10-14 已交付提示词级技能：SKILL.md + 触发/自动匹配 + 后台管理；余项：技能携带工具/脚本、一轮多技能叠加） | ✅ / P2 池 |
| 8 | 群聊/多 bot 协作 | Hermes bot 社会、群聊 | P2 池 |

### P2 —— 锦上添花（长期池）

多模型路由/故障转移（Friday 已有单模型热更新的地基）、成本面板（✅ 2026-10-14 交付：流式 token 记账修复 +
按模型/来源/会话/运行归属 + 价格表 + 软限额；按决策只做软限，预算硬取消需要框架侧事前 hook，列为余项）、
CLI/TUI、Webhook/事件触发、Docker 沙箱、语音（已归档可恢复）、RAG 知识库。

---

## 三、里程碑计划

### M1：国内消息通道（钉钉 ✅ 已交付 / 飞书 ✅ 已交付）

**目标**：助手住进国内主流办公与聊天软件。统一走 `internal/channel` 通道层
（`Inbound` / `Asker` / `Sender` / `Dispatch` 串行分发器），各平台只写 adapter。

#### M1.1 钉钉 Stream 模式 ✅ 已交付

- `internal/channel/dingtalk`：WebSocket 接入（**免公网 IP**）、群聊 @ 前缀剥离、
  发送者白名单、rune 安全分段（2000 字符/段）、断线自动重连
- config：`chan.dingtalk.enabled/clientId/clientSecret/allowUserIDs`
- 状态：代码与单测完成；启用待在 `config.yaml` 填入钉钉应用凭证
  （[开发者后台](https://open-dev.dingtalk.com) 建应用 → Stream 模式 → 企业内部机器人）

#### M1.2 飞书长连接 ✅ 已交付（2026-09-19）

**目标**：飞书单聊与群聊 @ 机器人对话，能力与钉钉对齐。

**设计要点**：
- SDK：`github.com/larksuite/oapi-sdk-go` v3——`larkws` 长连接客户端收事件（**免公网 IP**），
  `lark` API 客户端发消息（`im/v1/messages`，receive_id_type=chat_id/open_id）
- 接入准备：[飞书开放平台](https://open.feishu.cn) 创建企业自建应用 → 开启机器人能力 →
  订阅 `im.message.receive_v1` 事件（选长连接）→ 申请 `im:message` 读取与以应用身份发消息权限
- 消息处理：事件体解析 `content`（JSON `{"text":"..."}`）；群聊消息同样剥 @机器人 前缀；
  单聊直接对话
- 回复上下文：`Inbound.Reply` 携带 `chatID`（+ 可选 messageId 用于引用回复），
  Sender 调发送 API；沿用 2000 字符分段
- 白名单：`allowOpenIDs`（飞书 open_id），语义与钉钉一致（空 = 允许所有）
- config：`chan.feishu.enabled/appID/appSecret/allowOpenIDs`

**任务清单**：
1. `internal/channel/feishu/feishu.go`：ws 长连接 adapter（收消息/@ 剥离/回复/分段）
2. `internal/config`：`chan.feishu` 段
3. main.go 接线（与钉钉并列）
4. 单测：消息解析、@ 剥离、白名单、分段

**交付内容**（2026-09-19）：
- `internal/channel/feishu/feishu.go`：larkws 长连接 adapter（文本消息解析 / @ 前缀剥离 /
  chat_id 回发 / 2000 字符 rune 分段）；`lark.go`：发消息 API 客户端 + 心跳推送 Notifier
- `internal/config`：`chan.feishu` 段（appID/appSecret/allowOpenIDs/pushChatIDs）
- main.go 接线（与钉钉并列）；`channel.Dispatch` 统一补上请求留痕（source=通道名，
  session=<通道>-<chatID>）与会话 key 注入
- 单测：content 解析、@ 剥离、rune 分段还原、群聊事件转 Inbound、非文本忽略、
  分段回发、轨迹 source=feishu、推送器错误聚合

**验收标准**：
- [x] 飞书单聊发"现在几点" → 正确回复（轨迹 source=feishu）——Dispatch 统一留痕，单测覆盖
- [x] 群聊 @机器人 正常回复且 @ 前缀被剥除——单测 TestOnReceiveGroupMessage / TestStripAt
- [x] ~~网页与飞书共享上下文~~ **由 M4 多会话设计取代**：网页与飞书各自独立会话
      （`web` / `feishu-<chatID>`），互不干扰（M4 验收项之一）；allowOpenIDs 过滤复用 Dispatch 白名单
- [x] 断线自动重连——larkws SDK `WithAutoReconnect(true)` 开启

**工作量**：约 1 天 ｜ **风险**：低（国内直连；事件订阅的权限申请要在后台点一遍）
**启用待办**：在飞书开放平台建应用、订阅事件、申请权限后把凭证填入 `config.yaml`。

#### （预留）M6：企业微信可交互接入

自建应用可收可发，但**回调需要公网 URL**——家用 NAT 场景需内网穿透（frp / Cloudflare Tunnel）。
架构不变（channel 层加回调路由），等有公网条件或部署形态变化时再做。

---

### M2：执行类工具 + 审批机制 ✅ 已交付（2026-09-19）（让 agent 能"干活"）

**目标**：agent 从"只读"升级为"能写文件、能执行命令"，危险操作经网页审批。

**设计要点**：
- 工具三件套（`internal/tools/`）：
  - `write_file`（path + content，白名单同 read_file；写入前自动备份原文件为 `.bak`）
  - `edit_file`（path + old + new，精确替换；要求 old 唯一匹配，防止误改）
  - `exec`（command + timeout，默认 30s 上限 120s；输出截断 32KB）
- **审批机制**（`internal/approval/`）：`auto`（白名单命令直接执行）/ `confirm`（进待审批队列，
  网页弹卡片，5 分钟未处理自动拒绝，回复模型"用户未批准"）/ `off`（禁用 exec）
- 黑名单兜底（`rm -rf` 类即使误入白名单也拦截）；轨迹：write/edit/exec 与审批动作全部留痕

**交付内容**（2026-09-19）：
- `internal/tools/write_file.go`（write_file + edit_file：resolveWrite 写入解析、
  原文件 .bak 备份、写失败自动还原、old 唯一匹配校验、软链逃逸防护）
- `internal/tools/exec.go`（exec：/bin/sh -c、超时默认 30s 钳制 120s、输出截断 32KB、
  白名单匹配 + 链式字符拒绝、黑名单兜底）
- `internal/approval/`（审批队列：auto/confirm/off 三模式、5 分钟超时、Pending/Decide）
- 网页审批卡片（index.html 轮询 `/api/approvals`，右下角弹卡批准/拒绝）+
  审批动作全程写入调用轨迹
- config：`tools.exec.mode/allowlist/timeout`（默认 confirm）

**验收标准**：
- [x] edit_file 在 confirm 模式产生审批卡片 → 批准后生效——单测 TestWriteFileConfirmApproval、
      TestExecConfirmApprove，e2e 走通"exec → 审批 API → 执行 → 回答"
- [x] 白名单命令（`uname -a`）直接执行并返回结果——TestExecWhitelistPasses
- [x] 非白名单命令审批超时/拒绝 → 模型收到"用户未批准"——TestConfirmTimeoutDenies、
      TestExecConfirmReject、TestEditFileApprovalDenied
- [x] `rm -rf` 类命令即使误入白名单也被黑名单拦截——TestExecBlacklistEvenIfAllowlisted

**工作量**：2 天 ｜ **风险**：中（exec 的安全边界需谨慎）→ 实际风险受控：黑名单/链式字符/路径逃逸均有单测。

---

### M3：心跳式主动任务 ✅ 已交付（2026-09-19）（从"提醒器"变"管家"）

**目标**：定时到点不再是"复读预设文案"，而是把指令交给 agent 自主决策与行动，
输出投递到钉钉/飞书/网页。

**设计要点**：
- **任务引擎扩展**：`task.Task` 新增 `Kind: agent`——到点回调不是 speaker.Speak(text)，
  而是：构造 prompt（任务指令 + 当前时间 + 可用能力说明）→ `brain.Ask` → 拿到回答
- **HEARTBEAT.md 模式**（参考 OpenClaw）：工作目录放检查清单文件，心跳任务每轮把清单内容喂给 agent，
  agent 自主判断"要不要做事、要不要打扰用户"
- **输出协议**：回答首行 `[推送]` / `[静默]`，引擎据此决定投递目标；静默时段
  （如 23:00–07:00，config 可配）期间心跳照跑但只记轨迹不推送
- **与现有任务共存**：单次/多次/循环任务保持原样，`agent` 是新增的第四类；
  管理后台任务表单加"智能任务"选项（指令文本 + 可选推送通道）
- **上限**：心跳任务复用 Brain 全局互斥，与用户对话天然排队，不会并发打架

**交付内容**（2026-09-19）：
- `internal/task`：`KindAgent` 类型（once/repeat/cron 三种定时语义共用，落盘恢复保留 Kind）、
  `schedule_task` 工具与管理后台表单支持"智能任务"
- `internal/agent/heartbeat.go`：TaskRunner（构造 prompt = 指令 + 当前时间 + 输出协议
  [+ HEARTBEAT.md 清单] → Brain.Ask（会话 key=task）→ 解析 `[推送]`/`[静默]` →
  按 quietHours 与任务通道过滤投递）；Notifier 接口（console 本地播报/屏幕、
  钉钉 WebhookNotifier、飞书 PushNotifier）
- `HEARTBEAT.md` 示例文件 + 管理后台"设置"页编辑（GET/POST /api/heartbeat）
- config：`heartbeat.enabled/schedule/interval/quietHours/file`（interval 转 `@every`，cron
  解析器补上 Descriptor 支持）；心跳任务固定 ID `heartbeat`，重启恢复不重复注册
- 轨迹：智能任务完整决策链（source=task，session=task）

**验收标准**：
- [x] 配置"每天 8:00 心跳"→ 到点 agent 读 HEARTBEAT.md 清单自主巡检并推送——
      e2e 验证（interval=3s 心跳 → exec 工具 → 审批 → [推送] 内容经本地通道播报）；
      单测 TestTaskRunnerHeartbeatChecklist
- [x] 静默时段内心跳只写轨迹不推送——TestTaskRunnerQuietHours（含跨零点 23:00-07:00）
- [x] agent 输出 `[静默]` 时即使非静默时段也不打扰——TestTaskRunnerOutputProtocol
- [x] 心跳执行期间用户发消息不交叉——M4 落地后智能任务走独立会话 `task`，与各用户会话
      天然隔离（取代原"全局互斥排队"设计，见决策记录 2026-09-19-d）

**工作量**：1–2 天 ｜ **依赖**：M1（推送目标）、M2（agent 有手可干） ｜ **风险**：低

---

### M4：会话持久化 + 多会话 ✅ 已交付（2026-09-19）（重启不失忆，多入口各自连续）

**目标**：对话历史落盘重启恢复；网页/钉钉/飞书各自拥有独立会话并可切换。

**设计要点**：
- **持久化**：vortexagent 自带 `sessionstore.NewJSON(path)`——把 `SessionRef` 从内存会话
  换成 JSON 存储会话（`session.json` 落盘），代码改动极小；会话过大时框架自带 offload 机制
- **多会话**：`SessionRef`（单会话）升级为 `SessionPool`（按 key 持有会话）：
  - key 约定：`web`（网页）、`ding-<chatID>`（每个钉钉聊天）、`feishu-<chatID>`、`repl`
  - Brain 互斥从全局锁改为 per-key 锁（不同通道并行对话互不阻塞）
  - 轨迹 Request 增加 `session` 字段，抽屉可按会话过滤
- **管理**：清除上下文按会话粒度；设置页展示会话列表（key、消息数、最后活跃）
- **降级**：多会话改造期间保留 `pool.single()` 兼容路径，REPL/旧调用无感

**交付内容**（2026-09-19）：
- `internal/agent/pool.go`：SessionPool（Get/Reset/List/per-key 锁/key 归一化），
  会话 ID 固定 `friday-<key>`，经 `sessionstore.NewJSON` 落盘（session.json），惰性恢复
- Brain 重构：SessionRef/DeltaBus → SessionPool + 按请求流式路由（框架 Agent.WithDelta），
  同会话串行（per-key 锁）、跨会话并行（RWMutex 保护 Agent 热替换）；
  会话 key 经 ctx 注入（Dispatch 按通道+chatID、REPL=repl、网页=web、智能任务=task）
- 轨迹 Request 增加 `session` 字段，抽屉可按会话归属查看
- 网页：聊天页会话切换器（localStorage 记忆，多会话 UI 地基）
- 管理后台：设置页会话列表（key/消息数/最后活跃）+ 按会话清除（POST /api/context/clear 带 session）

**验收标准**：
- [x] 重启服务后能接上历史——单测 TestBrainPersistenceAcrossRestart（假 LLM 校验第二轮
      上下文包含重启前历史）；e2e 重启二进制后 web 会话惰性恢复并继续追加
- [x] 钉钉/飞书会话与网页会话上下文相互独立——TestBrainSessionIsolation
- [x] 两个会话同时发消息 → 并行处理不互相阻塞——TestSessionPoolParallelKeys
- [x] 清除上下文只影响指定会话——TestBrainReset + e2e /api/context/clear 按会话清除

**工作量**：2 天 ｜ **风险**：低

---

### M5：MCP 客户端 ✅ 已交付（2026-09-19）（接入外部工具生态）

**目标**：Friday 作为 MCP 客户端连接外部 MCP 服务器，其工具动态注册进 agent——解锁整个 MCP 生态。

**设计要点**：
- **协议**：优先 stdio transport（本机子进程，开发板友好）；config 声明 MCP 服务器列表
  （command + args + env），启动时拉起并完成 MCP 握手（本会话驱动 chrome-devtools-mcp
  的实践可直接复用）
- **动态注册**：`tools/list` 拿到工具清单 → 适配成 `vxagent.Tool`（Name 加 `mcp_<server>_`
  前缀防冲突）→ 注册进 Registry；工具调用转发为 MCP `tools/call`
- **生命周期**：MCP 进程随 Friday 启停、崩溃自动重启；连接状态展示在设置页
- **安全**：MCP 服务器视为可信配置（config 明文声明），但 exec 类工具同样纳入 M2 审批机制
- **热加载**：config 变更后重连（复用模型热更新思路）

**交付内容**（2026-09-19）：
- `internal/mcp/client.go`：stdio JSON-RPC 2.0 客户端（initialize → notifications/initialized →
  tools/list → tools/call；读循环分发响应、8MB 单帧上限、120s 调用超时、进程退出唤醒等待者）
- `internal/mcp/manager.go`：生命周期管理器（Apply 按配置增减/变更重启、RegisterTools 以
  `mcp_<server>_<tool>` 前缀注册 vxagent.Tool、进程退出 2s 后自动重启直至成功、
  Restart 手动重连、StatusList 状态、Close 停止全部）
- 工具清单变化 → 触发注册表 + Agent 整体重建（复用模型热更新思路，原子替换等待在途请求）
- config：`mcp.servers[]`（name/command/args/env）
- 设置页：MCP 服务器状态（在线/工具数/重启次数/最后错误）+ 重连按钮
  （GET /api/mcp、POST /api/mcp/:name/reconnect）

**验收标准**：
- [x] config 声明的 MCP 服务器随 Friday 自动拉起，工具出现在 agent 可用列表——
      单测 TestManagerLifecycle（工具前缀注册 + 经注册表调用成功）；
      e2e 真实二进制 + python MCP 服务器：`/api/mcp` 显示 running + 2 工具
- [x] agent 能调用 MCP 工具完成任务，轨迹中可见完整调用链——MCP 工具经统一注册表进入
      agent，与内置工具同走 Hooks 留痕（tool_req/tool/answer 全记录）
- [x] MCP 进程被 kill 后 30 秒内自动重启并恢复工具——单测（2s 退避重试）与 e2e
      （kill → restarts:1、tools:2）均验证
- [x] 移除 config 中的服务器 → 工具消失——TestManagerLifecycle 第 3 步（Apply(nil) 后
      新注册表无任何 mcp_ 前缀工具）；定义变更（命令/参数/环境变化）也会触发重启

**工作量**：2–3 天 ｜ **风险**：低（MCP 协议版本兼容）

---

## 四、里程碑总览与依赖关系

```
M1 钉钉✅ ──┬──→ M1.2 飞书 ✅（并列扩展）
M2 执行工具+审批 ✅ ──┬→ M3 心跳主动任务 ✅（需要推送目标 + 有手可干）
M4 会话持久化 ✅（独立）
M5 MCP 客户端 ✅（独立）
```

| 里程碑 | 主题 | 工作量 | 状态 |
|--------|------|--------|------|
| M1.1 | 钉钉通道 | 2–3 天 | ✅ 已交付（待填凭证启用） |
| M1.2 | 飞书通道 | 约 1 天 | ✅ 已交付（2026-09-19，待填凭证启用） |
| M2 | 执行工具 + 审批 | 2 天 | ✅ 已交付（2026-09-19） |
| M3 | 心跳主动任务 | 1–2 天 | ✅ 已交付（2026-09-19，依赖 M1/M2） |
| M4 | 会话持久化 + 多会话 | 2 天 | ✅ 已交付（2026-09-19） |
| M5 | MCP 客户端 | 2–3 天 | ✅ 已交付（2026-09-19） |

## 五、风险与约束

- **消息通道**：钉钉/飞书国内直连免公网；企业微信可交互需公网回调（内网穿透或部署形态变化）
- **微信个人号**：无官方 API，第三方方案有封号风险——**明确不做**
- **开发板资源**：Go 单二进制 + JSON 存储在 1GB 内存板够用；MCP/exec 引入的子进程需关注内存水位
- **上游依赖**：多模态输入受 vortexagent `Ask(string)` 签名约束（已用预注入方案绕过）；
  M4 的多会话若框架 API 不够用，优先给上游提 PR
- **安全底线**：exec / MCP 等高危能力必须依赖 M2 审批机制，不得裸奔
- **模型能力**：图片输入需要支持视觉的模型（step-router-v1 不支持，UI 已有明确提示）

## 六、长期演进池（P2，按需启动）

多模型路由与故障转移（热更新地基已备）、预算硬取消（当前为软限额）、技能携带 Go 工具/脚本与一轮多技能叠加、
多 bot 协作、Webhook/事件触发、Docker 沙箱、语音链路恢复（archive/voice-mode/）、RAG 知识库、
CLI/TUI、记忆语义检索（本地 embedding + 余弦相似度）与自动去重、
MCP 配置热加载（当前修改 mcp.servers 后需重启服务；连接管理/重建机制已就绪）。

**运维提示（M4 落地后新增）**：会话历史全量落盘 `session.json`，每次对话原子写回；
智能任务/心跳共用 `task` 会话——**心跳间隔不宜过短（建议 ≥30m）**，否则会话与轨迹膨胀。
会话过大可按会话清除（管理后台"设置 → 会话管理"）。

## 七、决策记录

- **2026-10-17**：ACP 一期方向由「派活作业模型」改写为「智能体独立对话空间」（侧栏新页，形状对齐
  Zed agent panel），7 项边界逐条经用户确认：thread 创建时绑定 agent 与**真实工作目录**、不可切换；
  进程空闲 10 分钟回收，重开经 `session/load` 重接上下文（协议全量重放语义），resume 保真度实测不过
  的 agent 走「上下文已重置」降级；`request_permission` 接入审批 UI（30 分钟无人处理自动拒绝，
  有效性依赖调紧 agent 自身审批模式）；时间线自 ChatView 抽共享组件；thought 默认显示；
  图片 v1 纳入（前置 probe 实测）；侧栏与设置节命名「智能体」。作业模型（acp_dispatch/门面/token）
  挪「后续可选」。`homePolicy=inherit` 边界对对话空间同样适用。见 docs/ACP-AGENTS-P1.md §二
- **2026-10-15**：ACP（Agent Client Protocol）接入定为 client 方向 + 长程作业模型；一期不含审批、
  门面只放只读工具。`homePolicy` 默认 `inherit`（子 agent 继承用户 HOME 及其登录态、MCP server、
  skill 表），一期**无人工闸门**已由用户认可——安全边界仅为一次性工作目录 + 门面只读 + 任务自包含，
  不承诺拦截子 agent 在其目录内的 shell 与写操作。依据与 13 条实测硬约束见 docs/ACP-AGENTS.md，
  一期拆解见 docs/ACP-AGENTS-P1.md；方案在未合并的 explore/acp-agents 分支，尚未开工
- **2026-09-19（交付日）**：M4 多会话采用"ctx 注入会话 key + SessionPool per-key 锁"取代
  原 SessionRef 单会话；流式增量改用框架 `Agent.WithDelta` 按请求路由，DeltaBus 废除。
  由此 M3 的"心跳与用户对话全局互斥排队"升级为"智能任务独立会话 `task`，与用户会话天然隔离"；
  M1.2 验收项"网页与飞书共享上下文"相应由"各自独立会话"取代（详见各节说明）
- **2026-09-19（交付日）**：MCP 服务器配置视为可信（config 明文声明），工具直接注册；
  危险操作安全边界由 M2 审批机制覆盖（exec/write_file/edit_file），黑名单兜底对任何工具来源生效
- **2026-09-19（交付日）**：M2 审批范围定为 exec（mode 三选一）+ confirm 模式下的
  write_file/edit_file（整体进审批队列）；off 仅禁用 exec（与文档字面语义一致）
- **2026-09-19**：M1 通道选型从 Telegram 改为国内软件——钉钉 Stream 模式优先（免公网、官方 Go SDK）、
  飞书长连接次之、企业微信推送兜底；微信个人号因合规与封号风险明确排除
- **2026-09-19**：取消 M1.3 企业微信群机器人推送（仅出站价值有限，交互需求走预留 M6）；
  飞书（M1.2）为当前通道侧唯一进行项
- **2026-09-19**：M1.1 钉钉通道代码交付（internal/channel + dingtalk adapter，10 个测试包全绿）；
  飞书排入 M1.2，企业微信推送排入 M1.3，执行工具/审批、心跳、多会话、MCP 依次排入 M2–M5

## 八、明确不做（当前版本）

微信个人号接入、企业微信群机器人推送（原 M1.3）、语音链路（已归档，见 archive/voice-mode/）、
Docker 沙箱、多模型故障转移、RAG 知识库。
