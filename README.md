# Friday 个人管家 Agent

用 Go 实现的**个人管家 agent**：帮你打理日常琐事——待办清单、定时提醒、天气查询、
长期记忆，以及自定义功能；通过网页、终端、钉钉、飞书随时使唤，纯软件形态，
部署在自己的服务器或电脑上即可，**当前与硬件分离**。核心大脑基于
[vortexagent](https://github.com/capyflow/vortexagent)（通用 Go agent 框架），
管理后台基于 [vortexv3](https://github.com/capyflow/vortexv3)（Gin）。

## 功能

- **Web 界面**：浏览器打开 `http://localhost:8080/`，登录后进入统一界面，
  左侧导航：**聊天**（SSE 流式对话，打字机效果，可切换会话/清除上下文）、**运行状态**（含 token 用量多维视图与成本预算）、
  **自动化**（定时/智能任务/目标、计划与执行历史）、
  **闪念**（笔记+待办统一时间线：#标签、搜索、置顶、勾选完成、已完成视图）、**记忆**、**设置**（模型热更新 / 权限模式 / 成本与预算 / 人设提示词 / 会话管理 / 心跳清单编辑 / MCP 服务器 / 技能）；
  聊天页点"调用轨迹"在右侧弹出抽屉查看每次调用的 LLM/工具/回答链路；
  回复中展示**思考过程**（推理内容 + 工具调用步骤，按 LLM 轮次分块、可折叠，刷新后原样回显），
  每轮末尾一行本轮 token 与耗时；发送键在回复中变**暂停**——
  点击即止损，已生成部分可一键**继续**接着完成；暂停期间输入新问题则开启新对话
- **闪念（笔记与待办）**：简易 Blinko——随手想法记成笔记（支持 Markdown 与 #标签），
  要做的事记成待办（截止/提醒/勾选完成）；`add_note` / `list_notes` / `update_note` / `delete_note`
  与 `add_todo` / `list_todos` / `complete_todo` / `update_todo` / `delete_todo` / `pin_entry`（置顶，两类通用），
  统一落盘 `todos.json` 重启不丢；网页"闪念"页时间线按天分组，置顶固定顶部，
  已完成待办有专门视图，可按标签/关键词筛选
- **天气查询**："上海天气怎么样""明天要带伞吗"——`get_weather` 工具
  （Open-Meteo 免密钥接口，全球城市中文地理编码，当前实况 + 未来三天预报）；
  配 `weather.defaultCity` 后不报城市名也能查，没配则管家会先问一次并记住
- **定时任务**（统一模型，四类）：
  - 单次：`10分钟后提醒我喝水` → 到点经通知渠道投递
  - 多次：`每10分钟提醒我喝水，共3次` → 按间隔触发指定次数
  - 循环：`每天早上8点提醒我喝水`、`每周一三五9点播报天气`（自然语言 → cron）
  - 智能任务：`schedule_task` 传 `agent: true`，到点把指令交给 agent
    自主决策与行动（如"每天早上8点给我一份晨报：日期、天气、今日待办"），
    输出首行 `[推送]`/`[静默]` 决定是否打扰用户
- **提醒投递**：任务到点/智能任务推送统一走通知渠道——钉钉（pushWebhook）/
  飞书（pushChatIDs），硬件输出开启时叠加本地播报/显示；启用网页时内容始终保留在
  **通知**页（`notifications.json`）。任务先可靠入队再推进次数；远程投递失败最多自动
  重试 5 次，之后可在通知页重试，跳过已成功记录的通道；全程留痕调用轨迹
- **心跳巡检**（对标 OpenClaw HEARTBEAT.md）：agent 定期醒来读 `HEARTBEAT.md` 清单自主检查
  （清单在管理后台"设置"页可编辑）；静默时段（`heartbeat.quietHours`，如 23:00-07:00）
  照常执行但只记轨迹不推送
- **长期记忆（wiki）**：`remember` / `recall` / `forget` 工具让管家把记忆按主题维护成
  wiki 页面（`memories/` 目录，每页一个 `<id>.md`，落盘重启不丢）；记新信息先查相关
  主题页、合并后再保存，避免新旧矛盾。管理后台"记忆"页可像看 wiki 文档一样浏览与编辑
  （旧版扁平 `memories.json` 首次启动自动迁入《用户档案》页）
- **人设提示词**：多套可切换的性格与语气——内置**贴心管家**（默认）/ **活泼调皮** /
  **严谨可靠** / **温柔陪伴** / **简洁高效**，管理后台"设置 → 人设提示词"在线切换
  （热生效，对话上下文保持连续）；提示词可自由编辑，支持新增/删除自定义人设
  （写回 config.yaml `persona` 段）。人设只改变小五怎么说话，能力与工具规则不变，
  且所有人设都保留**人文关怀**底线（深夜关怀、天气添衣带伞、低落时的安慰）
- **自定义功能**：三层扩展机制——
  1. **自定义指令**（`custom.commands`）：聊天触发词 → 提示词展开（如"早安简报"
     = 日期 + 天气 + 待办汇报），网页/终端/钉钉/飞书全入口生效；
  2. **技能**（`data/skills/<id>/SKILL.md`，管理后台"设置 → 技能"可增删改并即时生效）：
     沿用 vortexagent 的 SKILL.md 格式（`name`/`description`/`model`/`allowed-tools`/`temperature`
     + 正文指令）。与自定义指令的差别是技能还能换模型、调温度、**限定本轮可用工具**；
     触发方式三种：显式点名（`/skill 名 正文` 或 `$名 正文`）、触发词命中、按描述打分自动匹配
     （技能要写 `auto: true` 且全局开关开启才参与，命中依据在"思考过程"与轨迹里可见）；
     `allowed-tools` **只能收窄不能提权**——权限模式与黑名单照旧把关；
  3. **MCP 服务器**（`mcp.servers`，管理后台在线配置或对话里让 agent 用 `mcp_edit` 增删改，热生效）：
     本地命令（stdio）或远程 URL（Streamable HTTP）均可，工具以 `mcp_<服务器>_<工具>` 前缀
     动态注册进 agent，解锁整个 MCP 工具生态（日历、笔记、智能家居……）
- **计划模式**："帮我准备周末采购"→ 选「计划」→ 小五先给出**可审阅的步骤清单**
  （每步做什么、完成标准、预计用到的工具），网页确认后**按步执行**：一步一次独立提问、
  一条独立轨迹、一份独立 token 成本，可随时暂停/继续、对失败步骤重试或跳过。
  规划轮只许用读类工具（不许"边规划边动手"）；进程重启后计划原样保留并降为暂停，
  由你决定要不要继续（工具副作用不可知，不自动续跑）
- **目标模式**："把这个月的报销办完"→ 选「目标」（或对话里直接说"把 X 办成"）→
  登记为长程自主任务：在**独立会话**里多轮推进，直到 `[达成]`/`[受阻]`/用完步数或 token 预算，
  每步都在步边界检查预算与成本闸门。达成才主动推送，受阻/超预算只留档可查；
  进展、已用 token 与执行轮次在"自动化"页可看（一键跳到该目标的调用轨迹）
- **成本与预算**：token 账本按**模型/来源/会话/运行**分桶（含缓存命中、以及"网关未回传用量"
  的哨兵计数），价格表在后台可编辑并热生效，未配价的模型只计 token 不计金额并显式提示补价。
  预算是**软限额**：超线停的是心跳巡检、智能任务与目标启动这类主动工作——
  **你正在进行的对话与定时提醒绝不受影响**（框架只有事后 `OnLLMCall` 钩子，
  事前拦截需 ctx 取消，与"不打断用户"的取向冲突，故不做硬取消）
- **管理后台**：查看运行状态、管理闪念/任务/记忆、审批执行类操作、在线配 MCP。
  账号密码登录：新部署通过本机首次设置向导创建账号，已有账号继续有效；
  远程部署在**启动时预置账号**——配置文件
  `admin.username`/`admin.password`，或环境变量 `FRIDAY_ADMIN_USERNAME`/`FRIDAY_ADMIN_PASSWORD`
  （优先），两者须成对提供，每次启动以它覆盖账号文件（bcrypt 哈希落盘）。
  模型配置支持真实连接诊断，在「设置 → 模型」里填写（首次向导只负责创建账号，不引导配模型）
  本地与桌面默认只监听 `127.0.0.1`；远程部署显式设置 `admin.host` 或 `--host`。
  账号文件损坏/无法读取会拒绝启动并保留原文件，不再自动重置默认密码。
- **调用轨迹**：聊天页点"调用轨迹"弹出右侧抽屉，按请求查看 agent 的完整调用链：
  每轮 LLM 调用（token/耗时）、模型请求的工具与参数、工具返回结果、最终回答与错误；
  任务到点触发同样留痕。数据落盘 `traces.json`（最近 200 条）
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
  白名单、2000 字符安全分段、自动重连

> **硬件输出已解耦（`hardware.enabled`，默认关闭）**：`speak_now` / `display_now` /
> `play_audio` / `play_effect`（扬声器/屏幕/提示音）默认不注册，提醒一律经通知渠道投递；
> 原唤醒词 → 录音 → 本地 ASR → 对话 → TTS 链路（sherpa-onnx）更早前已移至
> [`archive/voice-mode/`](archive/voice-mode/)（默认构建无 cgo）。将来要做硬件形态
> （TTS 音箱/屏幕终端），打开开关并替换 output/audio 实现即可。

## 架构

```
┌─────────────────────────────────────────────────┐
│ 输入：网页聊天 / 终端 REPL / 钉钉 / 飞书 / 自定义指令 │
│   （共用 Brain：全局单对话，各渠道同一份上下文；        │
│     历史经 SessionPool 落盘重启恢复；心跳用独立会话）    │
└────────────────────┬────────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────┐
│  vortexagent Agent（内部/agent）                 │
│  ├─ LLM Provider：OpenAI 兼容（DeepSeek/Qwen）     │
│  ├─ 工具注册表（内部/tools）：                     │
│  │   add_todo / list_todos / complete_todo / delete_todo（待办）│
│  │   add_note / list_notes / update_note / delete_note / pin_entry（闪念）│
│  │   schedule_task（单次/多次/循环/智能任务）        │
│  │   get_weather（Open-Meteo 天气查询）             │
│  │   remember / recall / forget（长期记忆）        │
│  │   read_file / list_files / write_file / edit_file │
│  │   exec_command（全局权限层：白名单/黑名单/审批）     │
│  │   get_time；speak_now / display_now / play_audio / │
│  │   play_effect（仅 hardware.enabled 时注册）         │
│  │   mcp_<server>_<tool>（MCP 动态注册，内部/mcp）   │
│  ├─ Permissions：全局工具权限层（白名单/黑名单/审批模式，│
│  │   未放行调用桥接审批队列，网页批准后执行）           │
│  └─ Hooks：日志 / 工具调用审计（轨迹含会话归属）      │
└────────────────────┬────────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────┐
│  任务引擎（内部/task）+ 待办仓库（内部/todo）         │
│  ├─ 调度内核复用 vortexagent autonomous：            │
│  │   Scheduler 计算下次唤醒时刻（最近目标 NextRunAt，  │
│  │   maxSleep 封顶）与到期集合（按优先级）             │
│  ├─ 触发器：单次延迟 / 固定间隔 / cron（含 @every）    │
│  ├─ 持久化：JSON 原子写回（taskFile/todoFile），       │
│  │   格式与旧引擎兼容，重启自动恢复                   │
│  普通任务 → 通知渠道（钉钉/飞书 + 可选硬件输出）        │
│  智能任务 → Brain 自主决策 → [推送]/[静默] 投递       │
└─────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────┐
│  HTTP 服务（内部/admin + 内部/web，vortexv3/Gin）    │
│  /chat      网页聊天（go:embed 单页，公开）          │
│  /api/chat  SSE 流式对话接口                        │
│  /          管理后台 Dashboard（go:embed，鉴权）      │
│  /traces    调用轨迹页（go:embed；数据接口鉴权）        │
│  /api/sessions /api/approvals /api/mcp /api/heartbeat│
│  /api/status /api/tasks /api/todos /api/memories /api/traces│
│  与管家共用 tasks.json / todos.json / memories.json / session.json│
└─────────────────────────────────────────────────┘
```

## 快速开始（Docker 部署）

```bash
# 1. 构建并启动（capyflow/* 为私有 Go 模块，构建需传 SSH 凭据）
docker compose up -d --build

# 2. 首次运行会自动复制 data/config.yaml：
#    - data/config.yaml：补全 provider 段（baseURL / model / apiKeyEnv），
#      建议顺手配 weather.defaultCity（默认城市）
#    - 手动创建 .env：填入真实密钥（默认 FRIDAY_API_KEY=sk-xxx），
#      并预置 FRIDAY_ADMIN_USERNAME / FRIDAY_ADMIN_PASSWORD
#    改完重启生效：docker compose restart
```

服务地址：`http://<主机IP>:8090`（管理后台 `/admin`）。默认宿主 8090 → 容器 8080；
`FRIDAY_HOST_PORT` 控制宿主端口，`ADMIN_PORT` 同时控制容器实际监听与健康检查，并覆盖
配置文件中的 `admin.port`。容器显式使用 `ADMIN_HOST=0.0.0.0`。日常运维：

```bash
docker compose logs -f        # 看日志
docker compose restart        # 改配置后重启（会话/任务/待办自动恢复）
docker compose down           # 停止（数据都在 ./data，不丢）
```

## 本地开发（go run）

不走 Docker 时：

```bash
go mod tidy
export FRIDAY_API_KEY=sk-xxx               # 密钥环境变量（名与 config.yaml apiKeyEnv 一致）
cp config.yaml.example config.yaml         # 编辑确认 baseURL/model
go run ./cmd/robot                         # 或 go build -o friday ./cmd/robot && ./friday
```

### 前端（web/，Vue 3 + Vite）

网页界面源码在 `web/`，`npm run build` 把 JS/CSS 内联为自包含 HTML，
产物落在 `internal/admin/webdist/`，经 go:embed 打进二进制。
仓库提交了构建产物，纯 Go 开发不需要 Node；改前端时：

```bash
cd web
npm ci           # 首次，使用 lock 文件固定依赖
npm run dev      # 热更开发（http://localhost:5173，/api 代理到 8080 的 Go 服务）
npm run check    # ESLint + 安全/聊天/审批/通知组件回归测试
npm run build    # 产物写回 internal/admin/webdist/，重新 go build 即生效
```

`npm run build` 串两次 vite：先主应用（index.html），后快捷面板（palette.html，
`emptyOutDir=false` 不清掉前者）——分开构建是为了两页 CSS 互不污染（palette
要求 body 透明，供 Tauri 透明窗口用）。

稳定性修复的行为与故障恢复方式见 [docs/STABILITY.md](docs/STABILITY.md)。
完整使用说明见 [USAGE.md](USAGE.md)，下一阶段交付清单见 [P0 待办](docs/P0-TODO.md)。

启动后两种入口：

- **网页聊天**：浏览器打开 `http://localhost:8080/`（端口同管理后台）
- **终端 REPL**：直接在运行终端输入

终端对话示例：

```
🤖 小五（Friday 个人管家）已启动（provider=deepseek-chat）
> 记个待办：周三前交房租
已记入待办 #t1788264xxx：周三前交房租（当前未完成 1 项）
> 10分钟后提醒我喝水
已安排单次任务 #t1788265xxx："该喝水了"，将于 20:12:30 触发。
（10 分钟后）[任务] 到点：该喝水了
```

内置命令：`/help` `/tasks` `/cancel <id>` `/self-test`（无需 LLM 的自检演示）`/quit`

## 自定义功能

**自定义指令**（config.yaml，重启生效）：

```yaml
custom:
  commands:
    - name: 早安简报
      description: 日期 + 天气 + 今日待办
      prompt: |
        请给我一份早安简报：先说今天的日期与星期，然后查默认城市的天气并提醒带伞与否，
        最后把待办清单里未完成的事项汇报一遍。
```

之后在任何入口输入"早安简报"（或"早安简报 只看天气"）即自动展开执行。

**MCP 服务器**（管理后台"设置 → MCP 服务器"在线配置，保存即热生效；
对话里直接说"帮我接入这个 mcp"也行——agent 会用 `mcp_edit` 工具增删改查并热生效）：

```yaml
mcp:
  servers:
    - name: filesystem                      # 方式一：本地命令（stdio）
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/home/user/shared"]
    - name: remote-tools                    # 方式二：远程 URL（Streamable HTTP）
      url: https://mcp.example.com/mcp
      headers: {"Authorization": "Bearer xxx"}
```

**扩展工具**（Go 侧）：实现 `agent.Tool` 接口的 4 个方法即可注册新能力。

## 通道接入（钉钉 / 飞书）

两者均为 WebSocket 长连接，**无需公网 IP**，配置见 `config.yaml.example` 注释：

- **钉钉**：[open-dev.dingtalk.com](https://open-dev.dingtalk.com) 创建企业内部应用 →
  消息接收模式选"Stream 模式" → 开通"企业内部机器人" → 填 `chan.dingtalk.clientId/clientSecret`
- **飞书**：[open.feishu.cn](https://open.feishu.cn) 创建企业自建应用 → 开启机器人能力 →
  订阅 `im.message.receive_v1` 事件（长连接方式）→ 申请 `im:message` 读写权限 →
  填 `chan.feishu.appID/appSecret`

钉钉/飞书与网页/终端共享同一份对话上下文（全局单对话）；
`allowUserIDs` / `allowOpenIDs` 白名单留空 = 允许所有联系人。
要让**定时任务/心跳的主动提醒**送达钉钉/飞书，另配
`chan.dingtalk.pushWebhook`（群机器人 webhook）或 `chan.feishu.pushChatIDs`（应用所在会话）。

## 执行类工具与审批（安全边界）

agent 能写文件、跑命令，危险操作经 vortexagent **全局权限层**把关
（`tools.exec` 翻译成权限规则，见 `internal/agent/permission.go`），未放行的调用
桥接到审批队列（`internal/approval`），在管理后台网页批准。

**全局权限模式**（`permissions.mode`，参照 zcode 多档权限，设置页"权限模式"在线切换热生效）：

| 模式 | 行为 |
|------|------|
| `confirm`（默认） | 写文件/改 MCP 配置与白名单外命令逐次网页批准（可"批准并记住"） |
| `acceptEdits` | 日常编辑免审批直行（write_file / edit_file / mcp_edit 变更），执行命令仍把关 |
| `fullAccess` | 全部免审批直行，仅毁灭性命令黑名单仍强制拦截——适合本机私人使用 |

按次维度的细则：

| 模式 | exec_command | write_file / edit_file | mcp_edit（add/update/remove） |
|------|------|------------------------|------|
| `auto` | 白名单内直接执行，白名单外拒绝 | 直接执行 | 直接执行 |
| `confirm`（默认） | 白名单内直接执行；白名单外进**网页审批队列**（管理后台右下角弹卡片，5 分钟未处理自动拒绝，可"批准并记住"——相同调用本进程内免审批） | 需网页批准后执行 | 需网页批准后执行（查询与 restart 免审批） |
| `off` | 不注册 exec_command | 不受影响 | 不受影响 |

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
  推送目标：钉钉 webhook（`chan.dingtalk.pushWebhook`）+ 飞书（`chan.feishu.pushChatIDs`）
  + 本地播报/显示（仅 `hardware.enabled` 时）
- **心跳**：`heartbeat.enabled` 开启后按 `schedule`（cron）或 `interval` 触发，
  agent 读 `HEARTBEAT.md` 清单自主巡检（管理后台可编辑清单）；
  `quietHours` 静默时段照常执行但不推送

## 演进计划

见 [docs/ROADMAP.md](docs/ROADMAP.md)。当前定位为纯软件个人管家；
硬件形态（TTS 音箱/屏幕终端）作为后续可选方向保留（`hardware.enabled` 开关 + 归档的语音链路）。
