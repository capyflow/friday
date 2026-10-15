# M6 一期：ACP agent 调用链路实施方案

状态：待评审 ｜ 前置文档：[ACP-AGENTS.md](./ACP-AGENTS.md)（二期，含 13 条实测硬约束 C1–C13）
本文拆 `ACP-AGENTS.md` §〇 的一期前置，落到具体文件与接缝。约束编号 C* 一律引用二期文档，不重复论证。

## 一、一期目标

用最小代价回答一个问题：**"Friday 把一件自包含的活甩给外部 agent，几分钟后拿回结果并投递给用户"这件事，值不值得继续投入。**

一期不做审批、不做写类工具。理由不是省事，而是实测决定：审批在二期才有承载它的东西（写类工具），而一期最大的不确定性——进程模型、事件量级、作业生命周期、门面能否鉴权——全都可以在没有审批的情况下验证完。

**判定标准（一期结束时回答）**：真实用过 3 次以上派活后，结果是"有用但不够"（进二期）还是"我自己用 exec_command + 一轮总结就够了"（停在这里，只留 probe）。

## 二、设计决策

### D1 进程模型：每作业一个子进程 + 一条连接，跑完即杀

- 全局并发上限 `maxConcurrent`（默认 2），形状照 `internal/agent/goal.go:325` 的信号量槽位。
- 超上限的作业排队（`queued`），排队期间可取消。
- **不做崩溃自动重启**——`internal/mcp/manager.go:194` 的 2s 自动重启在 ACP 上是错语义：进程重拉即会话丢失，重启后静默续跑比直接失败更坏。崩溃一律判 `failed` 并保留 stderr 尾部。
- 代价与理由：冷启实测 7.3s（opencode）~18.5s（codex-acp），相对 turn 12~209s 可接受；换来的是一作业一故障域，直接规避 C6（SDK 通知是单 goroutine 串行队列，一条连接上多会话会被彼此拖死）。

### D2 同一 session 同一时刻只允许一个 in-flight prompt

一期"一作业一会话"，天然不会并发两轮，因此**只需要锁，不需要 pending 队列**：用 `SessionPool.Lock(key)`（`internal/agent/pool.go:113`，现成的按 key 惰性互斥锁）包住 `Prompt` 调用。

这是对二期文档 S3 的下界修正：二期的唤醒轮才需要真正的 pending 队列（把"立刻发"改成"排队发"）。一期把锁建好，二期只在其上加一层队列语义。

### D3 作业落盘只记历史，不恢复执行

`acp-jobs.json`，持久化形状照 `internal/task/reliable.go:18` 的 `fileStore`（load/save、写失败不消费次数）。Friday 重启后：

- 已完成的作业仍可在后台/`acp_status` 里查到；
- 重启前处于 `running` 的一律转 `interrupted`，**不尝试 `session/load` 恢复**（`loadSession=true` 实测存在但从未验证过恢复保真度，不赌）。

### D4 事件聚合器一期就建

不做聚合的代价是实测数字：一轮 54.6s 的作业 = 153 条 `session/update`，其中 80 条 thought + 52 条 message 碎片，另有单条 25003 B 的 `available_commands_update`。直接喂 `PublishStep`（`internal/agent/events.go:53`）会把网页时间线刷爆。

规则（全部落在 `internal/acp/aggregate.go`）：

| 输入 | 处理 |
|---|---|
| `available_commands_update`、`config_option_update`、`session_info_update` | 丢弃，只记字节数 |
| `agent_message_chunk` | 按 `messageId` 累积，flush 条件「≥1200 字 或 ≥3s 或 turn 结束」，投为一条 `Step{Kind:"acp"}` |
| `agent_thought_chunk` | 默认不投，`showThought` 可配 |
| `tool_call` / `tool_call_update` | 只取 `kind` + `title`，附后缀标记「文案来自子 agent」；**不取 `locations`**（C11 实测上报假路径） |
| `usage_update` | 不投时间线，进账本 |

聚合器出口**只允许入队**，任何阻塞都会经 C6 冻住整条连接的事件流。

### D5 门面只放三个只读工具，但 token 机制一期就做

一期门面工具：`recall`、`get_time`、`list_tasks`。写类（`remember`/`schedule_task`/`display_now`）全部留二期——一期门面被外部 agent 调用时**没有任何人在中间把关**（C1/C2 实测审批通道没人用），只读是唯一诚实的边界。

但作业级 token 一期就实现：二期文档把"token over `Headers` 实测"列为 S1 第一件事，它其实是一期能不能挂门面的地基，挪到一期（见 P1-E）。

### D6 三级超时，各自独立

| 档 | 值 | 依据 |
|---|---|---|
| 握手（`initialize` + `session/new`） | 10s | C7：`gemini --acp` 静默挂起 3 分钟零字节 |
| 单轮 turn | 5min，按 agent 可配 | 实测 12~209s，同 prompt 方差极大 |
| 门面单次工具调用 | 15s 硬上限 | C1：对方 MCP 客户端 60.1s 放弃 |

三档不共用一个 ctx 预算。握手超时必须能产出可诊断的错误（附 stderr 尾部 + agent 名），否则"没登录"这类故障在 Friday 侧表现为莫名超时。

### D7 默认关闭

`agents.enabled: false`。理由：默认开启会让 Friday 启动路径依赖 `npx`/`HOME` 下有登录态（C13），而这两件事在 launchd/无 TTY 环境里最容易碎。配置里未启用时，三个模型可见工具不注册。

### D8 `homePolicy` 一期只实现 `inherit`

`docs/ACP-AGENTS.md` §六 的三档里，一期只做 `inherit`（默认，继承 `HOME` 与用户 env），但**作业记录与后台状态页必须显示这一档**，避免"以为隔离了其实没有"。`sandbox-home` 留二期，因为一期就切过去会顺手失去所有 agent 的登录态，把"能不能用"和"能不能隔离"两个问题混在一起测。

## 三、组件与落点

| 位置 | 内容 | 复用的既有形状 | 预估 |
|---|---|---|---|
| `internal/acp/config.go` | `AcpConfig{Enabled, MaxConcurrent, HandshakeTimeout, TurnTimeout, HomePolicy, WorkspaceRoot, Agents []AcpAgentConfig}` | `McpServerConfig`（`internal/config/config.go:465`）逐字段对齐 | ~90 行 |
| `internal/acp/proc.go` | 子进程生命周期：`LookPath`→`exec.CommandContext`→pipes→握手→`Done()` 监视→kill | `internal/mcp/client.go:75` 的 Start/Close，但**不含自动重启** | ~180 行 |
| `internal/acp/client.go` | 实现 `acp.Client` 9 个稳定方法：fs×2 与 terminal×5 返回 not-supported 桩；`SessionUpdate` 只入队；`RequestPermission` 一期一律 `cancelled` 并记日志 | `internal/mcp/client.go` 的连接形状 + SDK `NewClientSideConnection(client, peerInput io.Writer, peerOutput io.Reader)` | ~160 行 |
| `internal/acp/aggregate.go` | D4 聚合器 | `internal/agent/events.go` 的 `Step`/`PublishStep` | ~140 行 |
| `internal/acp/jobs.go` | Job 状态机 + `acp-jobs.json` 落盘 + 信号量 | `internal/task/reliable.go` 的 fileStore、`goal.go:325` 的 sem | ~200 行 |
| `internal/acp/facade.go` | mcp-go streamable-HTTP 门面，挂 `/mcp/agents`，只读三工具 | 框架已依赖 `github.com/mark3labs/mcp-go@v0.44.1`（含 server 包） | ~150 行 |
| `internal/acp/token.go` | 作业级 token：`{jobID, agent, allowedTools, TTL}` + HMAC；经 `McpServer.Http.Headers` 下发 | 无现成件，新写 | ~90 行 |
| `internal/tools/acp_agent.go` | 模型可见工具 `acp_dispatch` / `acp_status` / `acp_cancel` | `vxagent.Tool` 接口 + `internal/tools/mcp_edit.go` 的工具形状 | ~170 行 |
| `internal/admin/acp.go` | `GET /api/acp`（agent+作业状态）、`GET/POST /api/acp/config`、`POST /api/acp/jobs/:id/cancel` | `internal/admin/admin.go:257-262` 的 `root.AddRouter` + `s.wrap`；handler 形状照 `ext.go:288` | ~140 行 |
| `cmd/robot/main.go` | 管理器构造 + `rebuild` 注册 + 首次 Apply | 直接插在 `:310` 的 `mcp.NewManager` 与 `:334` 的 `rebuild` 闭包旁 | ~40 行 |

装配注意：`rebuild` 会在配置热更新时整体重建注册表（`main.go:332-347`），ACP 管理器的 `RegisterTools` 必须一并进这条路径，否则改配置后 `acp_*` 工具消失。

## 四、模型可见工具面

| 工具 | 参数 | 返回 |
|---|---|---|
| `acp_dispatch` | `agent`（配置名）、`prompt`、`workspace?`（默认一次性目录）、`timeout?` | 立即返回 `job_id`，不阻塞 |
| `acp_status` | `job_id?`（省略则列近期） | 状态、耗时、stopReason、token 用量、结果文本前 N 字 |
| `acp_cancel` | `job_id` | 显式 `session/cancel` 结果 |

三条必须同时进 `internal/agent/permission.go:38` 的 `passThroughTools`，否则 `permission_test.go:213` 的 guard 直接红。

`acp_dispatch` 的描述里要写**什么时候别用它**（C10/能力重叠）：Friday 已有 `exec_command` 与文件工具，"看一眼这个目录""跑条命令"不该派给外部 agent。这是描述问题不是机制问题，但实测模型确实会选错。

## 五、任务拆分

| 步 | 内容 | 验收 |
|---|---|---|
| P1-A | SDK 封装 + **fake ACP agent 测试脚手架**（用 `coder/acp-go-sdk` 的 agent 侧起 stdio 假 agent） | 假 agent 能完成 initialize/session/new/prompt，能按需注入：token 级碎片流、25KB `available_commands_update`、崩溃、静默挂起、慢 turn。此步不写业务，纯建地基——后面每步都靠它验收，否则每条回归都要烧别人配额、耗时 12~209s 且不可复现 |
| P1-B | 配置 + 进程管理 + 握手超时 + stderr 落盘 | 真 agent（opencode）冷启到 `session/new` 成功；未登录 agent 在 10s 内给出带 stderr 尾部的错误，而不是莫名 3 分钟 |
| P1-C | 作业模型 + 落盘 + 三个工具 + 直通名单 | `acp_dispatch` 后 `acp_status` 能查到；Friday 重启后 `running` 作业变 `interrupted` 且历史可查；guard 测试绿 |
| P1-D | 聚合器 + 结果投递（通知中心）+ 取消 | 用 P1-A 的注入回放：153 条 raw → `Step` 条数 <30；`session/cancel` 后读 `stopReason="cancelled"` 且不误判为失败（C8）；投递文本来自 `agent_message_chunk` 聚合，不含内部错误串 |
| P1-E | 门面三工具 + token 实测 | **一期最关键的一步**：真 agent 带 `Authorization: Bearer <token>` 连上门面并调通 `recall`；错 token / 越权工具名被拒；MCP 协议版本协商对 opencode（`2025-11-25`）与 codex（`2025-06-18`）都通（C12） |
| P1-F | 后台状态页 + 设置页 + `docs/` 与 ROADMAP 收口 | 后台能看到 agent 清单/状态/作业列表并可取消；设置页可增删 agent 并热更新生效 |

建议顺序 A→B→C→E→D→F：把 P1-E 提到 D 之前，因为"门面挂得上"是整条二期路线的存亡条件，越早证伪越省。

## 六、明确不做（一期）

审批子系统与审批卡；写类门面工具；`fs/*`、`terminal/*` 真实实现（桩返回 not-supported）；ACP server 方向；`session/load` 恢复；model/mode 切换等 Unstable 方法；`sandbox-home` 隔离档；把子 agent 成本并入 `cost.NewGuard` 的自家预算（实测 `cost.amount` 恒为 0，配额在对面）。

## 七、风险

1. **token over `Headers` 未验证**（P1-E）——若某家 agent 的 MCP 客户端不带自定义 header，一期门面就只能裸奔在 127.0.0.1 上，鉴权要改方案（每作业独立端口或 stdio 门面进程）。
2. **PATH/无 TTY**：`npx` 在常驻服务环境里未必可解析（C13 的运维面），P1-B 要顺手把 `LookPath` 失败做成显式错误并支持绝对路径配置。
3. **`homePolicy=inherit` 的副作用是双向的**：子 agent 会读用户 MCP server 与 skill 表，`available_commands_update` 里那些内容会进入 Friday 的日志与报文——日志脱敏口径要先定（`evidence/` 不入库的同一理由）。
4. **同 prompt 方差 12~209s**：turn 超时既不能太短也拦不住 codex 那种 5 分钟不返的；一期接受"超时判 failed、对方可能仍在跑"这一不一致，并在作业记录里标注。
5. **一期的价值可能是负的**：外部 agent 花它自己的订阅、烧它自己的配额，Friday 只拿到文本与耗时，能力面又与自带工具重叠。这是 P1 结束时要诚实回答的，不是技术风险。

## 八、Definition of Done

- `go build ./... && go vet ./... && go test ./... -count=1` 全绿，`-race` 过（聚合器与作业表都有并发访问）。
- 三条 fake-agent 回归固化：60s 门面超时坏案例、并发混流坏案例、握手静默挂起案例。
- 真机跑通至少两个 agent（opencode + codex-acp）各一次完整派活，结果经通知中心投递。
- 文档同步：`docs/ACP-AGENTS.md` §〇 指向本文、ROADMAP 增 M6 一节、`changelog/` 按提交日期补条目。

## 九、与二期的衔接（对 `ACP-AGENTS.md` 的两处修订）

1. 二期 **S1 的 token 实测已前移到本文 P1-E**；二期 S1 收窄为"门面写类工具接入"。
2. 二期 **S3 的 turn 队列建立在本文 D2 的锁之上**，新增的是 pending 语义（把"立刻发"改成"排队发"），不是从零建串行。
