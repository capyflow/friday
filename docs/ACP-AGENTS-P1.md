# M6 一期：智能体（ACP agent）独立对话空间实施方案

状态：已评审（2026-10-17 用户逐条确认 §二 全部决策）｜ 前置文档：[ACP-AGENTS.md](./ACP-AGENTS.md)（作业模型路线，二期/后续可选，含 13 条实测硬约束 C1–C13）
依据：约束编号 C* 一律引用前置文档，不重复论证；本文新增约束以 N* 编号。

> **2026-10-17 方向改写**：本文由「派活作业模型」（acp_dispatch/status/cancel 三工具 + 门面 + token）
> 原地改写为「智能体独立对话空间」。作业模型整体挪至「后续可选」，仍由 ACP-AGENTS.md 承载；
> C1–C13 约束两条路线通用（C9 随门面搁置）。旧版全文见 git 历史。

## 一、一期目标

侧栏新增「智能体」页，形状对齐 Zed 的 agent panel：**thread 列表 + 对话时间线**。
thread 创建时绑定一个外部 agent 与一个真实工作目录，用户与 agent 直接对话——agent 用它自己的
工具（shell/文件）在自己的目录里干活，Friday 提供宿主：进程生命周期、时间线、审批卡、用量记账。

一期不做作业模型、不做门面。判定标准：真实使用若干次后回答——
**"在 Friday 里直接和外部 agent 对话干活，比另开一个终端值不值。"**

## 二、已拍板决策（2026-10-17 逐条经用户确认）

| # | 决策 |
|---|---|
| D1 | thread 创建时绑定 agent + 工作目录，**之后不可切换**；换 agent 或换目录 = 新开 thread。绕开一切"中途换脑子"的上下文续接问题 |
| D2 | 工作目录用**真实目录**（用户选择，按 agent 记住上次选择），不用一次性沙箱——外部 agent 的价值就是在真实项目里干活。thread 头部常驻显示当前路径。**补（2026-10-20）**：目录可留空——UI 点「选择目录」经 `GET /api/acp/dirs`（只读、只列子目录、逐层懒列举）在弹窗里浏览，路径栏可直达；留空回退 `~/.vortex/acp-sessions/<threadID>`（每会话一个子目录）。不用浏览器 showDirectoryPicker：它出于安全只回文件夹名，拿不到后端要的绝对路径（whatwg/fs #59 提案被隐私理由否决）；也不走 Tauri 原生对话框——一套 web 选择器全形态（浏览器/桌面 App/Docker）通用，壳保持无业务 |
| D3 | 进程空闲 **10 分钟**自动回收；回收**不再等于上下文丢失**——重开 thread 时经 `session/load` 重接（协议要求 agent 全量重放对话，见 N2）；不支持或恢复失败的 agent 走降级：新开 session，thread 标注「上下文已重置」，记录仍可见 |
| D4 | 时间线**抽共享组件**：消息块渲染器从 ChatView 抽出复用，一次小型重构（带现有 16 个测试文件做），换取单份维护 |
| D5 | thought 流（`agent_thought_chunk`）**默认显示**——独立 agent 页的心智是"看着 agent 干活"；设置留全局开关 |
| D6 | **图片 v1 纳入**：gate 在 P1-B2（probe 先实测两家经 ACP 的图片 prompt），通过才做，上传链路复用聊天页 |
| D7 | `request_permission` **接入 Friday 审批 UI**（复用 `internal/approval` 队列与审批卡）：审批卡 30 分钟无人处理自动答复拒绝；审批等待期间 turn 超时暂停计时（N3）。**有效性依赖用户调紧各家 agent 自身的审批模式**（N6），写进使用文档 |
| D8 | 侧栏与设置节命名统一**「智能体」**，不暴露 ACP 协议词 |
| D9 | （继承 2026-10-15 拍板）`homePolicy=inherit`：子 agent 继承 HOME、登录态、MCP server 与 skill 表；作业/对话记录显式标注隔离档位 |

## 三、约束

C1–C13 照用（见 ACP-AGENTS.md §一），其中 C9（门面返回值指令化）随门面搁置。
本文新增：

| # | 约束 | 依据 |
|---|---|---|
| N1 | 杀进程必须杀**进程组**（`Setpgid` + kill `-pgid`），并显式关闭 stdio 管道。`npx`→node→agent 自 spawn 的工具子进程是一棵树，只杀直接子进程会留孤儿占着工作目录 | 设计评审确定，P1-B 加杀树断言测试 |
| N2 | `session/load` 恢复时协议**强制 agent 全量重放对话**（逐条 `session/update` 流回，放完才回空结果）；重接时长与历史长度成正比，重开长对话必须有占位与进度反馈 | ACP session-setup 协议语义 |
| N3 | 审批卡等待期间 **turn 超时暂停计时**，否则人还没点卡、turn 先被判死 | D7 连带 |
| N4 | agent 会在工作目录自落状态文件（实测 opencode 写 `.omo/run-continuation/<session>.json`）；D2 改用真实目录后，这件事从"沙箱内小事"变成"弄脏用户项目"，须在文档中写明哪些文件是 agent 落的 | C-系列探测记录 |
| N5 | stderr 与 `available_commands_update` 含用户 skill 全表与本机路径（单条实测 25003 B），落日志前必须过脱敏；脱敏口径与 `evidence/` 不入库同一理由 | C13 / P1 风险 3 |
| N6 | `request_permission` 实测触发 0 次——agent 按各自配置自审批（如 codex `approval_mode="approve"`）。**审批闸门只在 agent 肯问时有效**，要真起作用需用户调紧 agent 自身配置 | 探测记录 + C13 |

## 四、组件与落点

| 位置 | 内容 | 复用的既有形状 | 预估 |
|---|---|---|---|
| `internal/acp/config.go` | `AcpConfig{Enabled, MaxConcurrent, IdleKill, TurnTimeout, Agents []AcpAgentConfig}`；`AcpAgentConfig{name, command, args, env, enabled, handshakeTimeout, turnTimeout, workspace}` | `McpServerConfig`（`internal/config/config.go:465`）逐字段对齐 | ~90 行 |
| `internal/acp/proc.go` | 子进程生命周期：`LookPath` 预检→`exec.CommandContext`（`Setpgid`）→握手→`Done()` 监视→**进程组击杀**（N1）；空闲回收 timer；stderr 脱敏落盘 | `internal/mcp/client.go:75` 的 Start/Close，**不含自动重启**（进程重拉即会话丢失） | ~200 行 |
| `internal/acp/client.go` | 实现 `acp.Client` 9 个稳定方法：fs×2 与 terminal×5 返回 not-supported 桩；`SessionUpdate` 只入队（C6）；**`RequestPermission` → `approval.Queue`（D7，不再是一律 cancelled）** | SDK `NewClientSideConnection` + `internal/approval/approval.go:145` 的 `Queue.Request` | ~180 行 |
| `internal/acp/aggregate.go` | 事件聚合器：`available_commands_update` 等丢弃只记字节；`agent_message_chunk` 按 messageId 累积（≥1200 字或 ≥3s 或 turn 结束 flush）；`agent_thought_chunk` 默认投（D5，开关可配）；`tool_call` 只取 kind+title 带子 agent 文案后缀，不取 locations（C11）；`usage_update` 进账本 | `internal/agent/events.go:53` 的 `Step`/`PublishStep` | ~140 行 |
| `internal/acp/threads.go` | thread 元数据 `{id, agent, workspace, title, state, sessionId}` + 对话投影落盘（`acp-threads.json`，写失败不消费次数）+ sessionId 记账 + **session/load 重接编排**（重放流入聚合器补投影；失败降级）+ 每 thread 串行锁（一 thread 一进程一连接，C3 结构性满足） | `internal/task/reliable.go:18` 的 fileStore | ~220 行 |
| `internal/admin/acp.go` | `GET/POST /api/acp/threads`（CRUD）、`GET /api/acp/threads/:id/stream`（SSE）、`POST /api/acp/threads/:id/stop`；审批列表复用现有通道（`internal/admin/ext.go:338`） | SSE 骨架照 `internal/web/chat.go:461` 的 `stream`，摘掉 Brain | ~200 行 |
| `web/src/components/agent/` | `AgentView`（thread 列表 + 时间线 + 输入区）+ **时间线共享组件**（自 ChatView 抽出，D4） | ChatView 的时间线渲染（261016 版） | 重构 + ~600 行 |
| `cmd/robot/main.go` | 管理器构造 + rebuild 注册 + 首次 Apply | 插在 `main.go:310` 的 `mcp.NewManager` 与 rebuild 闭包旁；**必须进 rebuild 路径**，否则热更新后丢注册 | ~40 行 |

不再有：`internal/tools/acp_*.go`（三个作业工具）、`facade.go`、`token.go`——随作业模型挪「后续可选」。

## 五、关键流程

### 5.1 thread 生命周期

```
创建（绑定 agent+workspace，D1）
  → 首条消息拉起进程 → initialize（10s 超时，C7）→ session/new
  → turn 循环（每 thread 串行；发消息时进程不在场则先拉起）
  → 空闲 10min 回收（D3）
重开 thread：
  进程不在 → 拉起 → agent 声明 loadSession?
    ├─ 是 → session/load(sessionId)（N2：全量重放流入聚合器补投影）→ 续聊无缝
    └─ 否/失败 → session/new 新会话，thread 标「上下文已重置」，投影保留
Friday 重启：thread 状态一律转 interrupted（不赌自动恢复），重开走同一条重接路径
```

### 5.2 request_permission → 审批卡（D7）

```
agent ──request_permission{kind,title,options}──▶ client（只入队，C6）
client ：approval.Queue 入队 → 网页现有审批卡出卡（字段模板化，附「来自子 agent」标注）
用户点同意/拒绝 ──▶ 答复对应 option，agent 继续
30min 无处理 ──▶ 自动答复拒绝选项（N3：等待期间 turn 超时暂停计时）
```

与 ACP-AGENTS.md §5.1 两拍审批的区别：`request_permission` 是正经异步协议请求、agent 会等；
两拍（C1/C2）是门面 `tools/call` 被 15s 超时逼出的形状，只在作业模型路线需要。

### 5.3 取消

`POST /api/acp/threads/:id/stop` → 显式 `session/cancel` → 读 `stopReason`（C8，"cancelled" 不判失败）。
**没有 PausedTurn 机制**：部分回答已在 agent 自己的 session 里，cancel 后用户直接发下一句即可续聊——
比主聊天（internal/web/chat.go 的暂停/继续）少一整套现场保存，这是独立会话的简化红利。

### 5.4 图片（D6，gate P1-B2）

复用聊天页上传链路（`internal/web/chat.go:192` 的 HandleUpload）；发送时图片经 prompt 的
ContentBlock image 下发。实测不过则整体挪后续，前端不出现入口。

## 六、模型可见工具面

**无。** 一期不注册任何 `acp_*` 工具，主聊天的 Brain 与工具注册表零改动；智能体页是平行入口，
不碰 provider、不碰摘要器/心跳/计划/目标模式（原边界③问题结构性消失）。

## 七、任务拆分（每步可独立验证）

| 步 | 内容 | 验收 |
|---|---|---|
| P1-A | SDK 封装 + **fake ACP agent 测试脚手架**（注入场景：碎片流、25KB update、崩溃、静默挂起、慢 turn、request_permission、session/load 全量重放、图片块）；**并做真机 resume 保真度实测**：opencode 与 codex-acp 各做「杀进程→load→对照」 | 假 agent 可完成全部注入场景；两家真机 resume 保真度有结论，不可靠者配置标 `degradeResume` 走降级 |
| P1-B | 配置 + 进程管理（进程组击杀 N1、空闲回收 10min、LookPath 预检与绝对路径支持）+ 握手超时 + stderr 脱敏落盘 | 未登录 agent 10s 内给出带 stderr 尾部的可诊断错误；杀进程后无孤儿（杀树断言测试） |
| P1-C | thread 模型 + 投影落盘 + session/load 重接与降级 | 重启后 thread 历史可查、状态 interrupted；重开重接成功续聊；降级路径出现「上下文已重置」标注 |
| P1-D | 审批接入（D7）：RequestPermission → 审批卡 → 答复；deadline 30min；turn 计时暂停 | fake agent 发 request_permission 出卡、点同意后 agent 拿到放行；30min 超时自动答复拒绝 |
| P1-E | SSE 端点 + 聚合器接时间线 + 用量记账（`acp:<agent>` 维度，金额缺显「配额在对面」）+ 轨迹 kind=acp | fake agent 注入 153 条 raw → Step 条数 <30；turn 结束帧带 {in,out,ms} |
| P1-F | 前端：时间线共享组件抽取（D4，带现有 16 个测试文件）+ AgentView + 设置节「智能体」（总开关默认关 D7/D9、agent 行卡片含「测试」按钮做握手诊断、删除 agent 的 thread 转只读）+ 侧栏入口 + 文档收口 | web 全量测试绿、eslint 干净；设置改动经 rebuild 路径热生效 |

顺序：**A → B → C → D → E → F**，B2（图片实测）可与 C/D 并行，通过则并入 F 的前端范围。

## 八、测试策略

1. fake ACP agent 为唯一回归载体（不烧真实配额、结果可复现），场景清单见 P1-A；
2. 杀树断言：拉起带子进程的 fake agent，杀进程组后断言无存活后代；
3. 重放回归：fake agent 按录制序列重放，断言投影补齐与顺序；
4. 审批 deadline：时钟注入测 30min 自动拒绝；
5. 脱敏：断言 stderr/`available_commands_update` 落盘前已滤除 skill 表与绝对路径（N5）。

## 九、明确不做（一期）

作业模型三工具、门面与作业级 token（→ ACP-AGENTS.md 路线，后续可选）；`fs/*`、`terminal/*`
真实实现（桩返回 not-supported）；ACP server 方向；`session/resume`（免重放的新能力，等生态
成熟，一期只用 `session/load`）；`sandbox-home` 隔离档；把子 agent 成本并入 `cost.NewGuard`
（实测 `cost.amount` 恒为 0，配额在对面）；智能体页接入多 channel（仅网页端）。

## 十、风险

1. **resume 保真度未验证**（P1-A 实测 gate）——不可靠的 agent 走「上下文已重置」降级，体验明确降档但不诈；
2. **重放耗时**（N2）——长对话重接需数秒到更久，占位与进度反馈必须在 F 落地；
3. **agent 自更新行为漂**——opencode 更新积极，SDK pin `protocolVersion=1` + fake agent 回归先行；
4. **审批闸门的真实覆盖率**（N6）——只在 agent 肯问时有效，文档必须教用户调紧 agent 自身审批模式；
5. **真实目录写风险**（D2 的代价）——一期有人工审批卡（D7），但仅覆盖 agent 经 `request_permission`
   问询的部分；agent 自审批的部分照旧无闸门，thread 头部路径常驻是唯一的显性提醒；
6. **环境依赖**——`npx` 解析、无 TTY 环境、登录态都在 HOME（D9 inherit 的既有代价），LookPath
   预检 + 绝对路径支持 + 「测试」按钮诊断兜底。

## 十一、Definition of Done

- `go build ./... && go vet ./... && go test ./... -count=1` 全绿，`-race` 过（聚合器、thread 表、审批桥接均有并发访问）。
- fake agent 回归固化：request_permission 出卡与答复、session/load 重放补投影、崩溃转 failed、静默挂起 10s 握手报错、进程组击杀无孤儿。
- 真机至少两家（opencode + codex-acp）各完成一次完整对话：含一次审批卡放行、一次回收后重接；图片链路按 B2 结论收口。
- 时间线重构后 web 全量测试绿、eslint 干净；智能体页窄屏可用（沿用现有媒体查询惯例）。
- 文档同步：本文与 ACP-AGENTS.md 的衔接更新、ROADMAP 决策记录、USAGE.md 智能体一节（含"调紧 agent 审批模式"说明）、config.yaml.example 增 agents 示例段。
