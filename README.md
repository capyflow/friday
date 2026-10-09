# Friday 个人管家 Agent

用 Go 实现的**个人管家 agent**：帮你打理日常琐事——待办清单、定时提醒、天气查询、
长期记忆，以及自定义功能；通过网页、终端、钉钉、飞书随时使唤，纯软件形态，
部署在自己的服务器或电脑上即可，**当前与硬件分离**。核心大脑基于
[vortexagent](https://github.com/capyflow/vortexagent)（通用 Go agent 框架），
管理后台基于 [vortexv3](https://github.com/capyflow/vortexv3)（Gin）。

## 功能

- **Web 界面**：浏览器打开 `http://localhost:8080/`，登录后进入统一界面，
  左侧导航：**聊天**（SSE 流式对话，打字机效果，可切换会话/清除上下文）、**运行状态**、**自动化**（定时/智能任务）、
  **待办**（清单增删改/勾选完成）、**记忆**、**设置**（模型热更新 / 人设提示词 / 会话管理 / 心跳清单编辑 / MCP 服务器状态）；
  聊天页点"调用轨迹"在右侧弹出抽屉查看每次调用的 LLM/工具/回答链路
- **待办清单**："记个待办：周三前交房租""我有什么待办""房租交了""这条不用了"——
  `add_todo` / `list_todos` / `complete_todo` / `delete_todo` 四个工具，
  清单落盘 `todos.json` 重启不丢，管理后台"待办"页可直接增删勾选
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
  飞书（pushChatIDs），硬件输出开启时叠加本地播报/显示；全程留痕调用轨迹
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
- **自定义功能**：两层扩展机制——
  1. **自定义指令**（`custom.commands`）：聊天触发词 → 提示词展开（如"早安简报"
     = 日期 + 天气 + 待办汇报），网页/终端/钉钉/飞书全入口生效；
  2. **MCP 服务器**（`mcp.servers`，管理后台在线配置热生效）：本地命令（stdio）或
    远程 URL（Streamable HTTP）均可，工具以 `mcp_<服务器>_<工具>` 前缀动态注册进 agent，
    解锁整个 MCP 工具生态（日历、笔记、智能家居……）
- **管理后台**：查看运行状态、管理待办/任务/记忆、审批执行类操作、在线配 MCP。
  账号密码登录：默认账号 `admin`/`admin`；建议在**启动时预置账号**——配置文件
  `admin.username`/`admin.password`，或环境变量 `FRIDAY_ADMIN_USERNAME`/`FRIDAY_ADMIN_PASSWORD`
  （优先），两者须成对提供，每次启动以它覆盖账号文件（bcrypt 哈希落盘）。
  登录页只提供登录，改账号通过启动配置完成
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

# 2. 首次运行会自动生成 data/config.yaml 与 .env：
#    - data/config.yaml：补全 provider 段（baseURL / model / apiKeyEnv），
#      建议顺手配 weather.defaultCity（默认城市）
#    - .env：填入真实密钥（变量名与 apiKeyEnv 对应，如 STEP_API_KEY=sk-xxx）
#    改完重启生效：docker compose restart
```

服务地址：`http://<主机IP>:8090`（管理后台 `/admin`；端口由 `data/config.yaml` 的
`admin.port` 决定，compose 里 `ports`/`ADMIN_PORT` 同步调整）。日常运维：

```bash
docker compose logs -f        # 看日志
docker compose restart        # 改配置后重启（会话/任务/待办自动恢复）
docker compose down           # 停止（数据都在 ./data，不丢）
```

## 本地开发（go run）

不走 Docker 时：

```bash
go mod tidy
export STEP_API_KEY=sk-xxx                 # 密钥环境变量（名与 config.yaml apiKeyEnv 一致）
cp config.yaml.example config.yaml         # 编辑确认 baseURL/model
go run ./cmd/robot                         # 或 go build -o friday ./cmd/robot && ./friday
```

### 前端（web/，Vue 3 + Vite）

网页界面源码在 `web/`，`npm run build` 把 JS/CSS 内联为自包含 HTML，
产物落在 `internal/admin/webdist/`，经 go:embed 打进二进制。
仓库提交了构建产物，纯 Go 开发不需要 Node；改前端时：

```bash
cd web
npm install      # 首次
npm run dev      # 热更开发（http://localhost:5173，/api 代理到 8080 的 Go 服务）
npm run build    # 产物写回 internal/admin/webdist/，重新 go build 即生效
```

`npm run build` 串两次 vite：先主应用（index.html），后快捷面板（palette.html，
`emptyOutDir=false` 不清掉前者）——分开构建是为了两页 CSS 互不污染（palette
要求 body 透明，供 Tauri 透明窗口用）。

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

**MCP 服务器**（管理后台"设置 → MCP 服务器"在线配置，保存即热生效）：

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
  推送目标：钉钉 webhook（`chan.dingtalk.pushWebhook`）+ 飞书（`chan.feishu.pushChatIDs`）
  + 本地播报/显示（仅 `hardware.enabled` 时）
- **心跳**：`heartbeat.enabled` 开启后按 `schedule`（cron）或 `interval` 触发，
  agent 读 `HEARTBEAT.md` 清单自主巡检（管理后台可编辑清单）；
  `quietHours` 静默时段照常执行但不推送

## 演进计划

见 [docs/ROADMAP.md](docs/ROADMAP.md)。当前定位为纯软件个人管家；
硬件形态（TTS 音箱/屏幕终端）作为后续可选方向保留（`hardware.enabled` 开关 + 归档的语音链路）。
