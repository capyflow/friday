# M6：ACP agent 接入 —— 作业模型路线（门面写类工具 + 审批 + 唤醒 + 记账）

状态：后续可选（一期方向已转向「智能体对话空间」，见 [ACP-AGENTS-P1.md](./ACP-AGENTS-P1.md)；本文的
门面/两拍审批/唤醒对应**作业模型**路线——Friday 的模型把自包含任务派给外部 agent、后台跑完收结果，
开工顺位在对话空间验证价值之后）｜ 分支：`explore/acp-agents`
依据：本文每条设计约束都对应 2026-10-15 的实测报文，报文归档在 worktree 的 `evidence/`，探测程序 `cmd/acp-probe/`。

## 〇、一期前置（本方案的输入接口）

一期的完整展开见 [ACP-AGENTS-P1.md](./ACP-AGENTS-P1.md)（2026-10-17 起为「智能体对话空间」）。
本文（作业模型）建立在一期地基与下列既有件之上；作业模型开工时以本文为实施方案、按需回补：

| 既有件 | 本文使用方式 |
|---|---|
| `internal/acp`：SDK 封装 + 进程管理（一期已按 thread 形状落地） | 连接与 session 句柄直接复用；作业维度在其上加 Job 记录 |
| `internal/acp/aggregate.go` 聚合器 | 新增 `tool_call`（来自门面的调用）事件类型 |
| 审批接入：`request_permission` → `approval.Queue`（一期 P1-D） | 门面写类工具的两拍审批复用同一 Queue，句柄语义一致 |
| thread 的 turn 串行（一 thread 一进程一连接，C3 结构性满足） | 作业与唤醒轮的排队语义建立在同一纪律上 |
| MCP 门面（`facade.go` + `token.go`） | **随作业模型自建**（一期无门面），token over Headers 实测（原 P1-E）在本文开工时首要验证 |

## 一、实测硬约束（设计必须满足，不可协商）

| # | 约束 | 实测依据 |
|---|---|---|
| C1 | 门面单次工具调用必须 **<15s** 返回，永不阻塞等人类 | opencode 的 MCP 客户端在 **60.1s** 放弃，发 `notifications/cancelled`，并把 `MCP error -32001: Request timed out` 当工具结果叙述给用户；codex-acp 等满 90s 无碍 → 不能按最宽容的 agent 设计 |
| C2 | 审批只能是**提交/查询两拍**，不能是同步挂起 | 同上；异步两拍下 opencode 自发轮询 6 次（7/11/15/18/22/28.5s）拿到 `APPROVED_BY_HUMAN` |
| C3 | 同一 session 的 `session/prompt` **必须串行** | 并发两轮：两请求在同一瞬间各回 `end_turn`，4 个 messageId 混入同一条 `sessionUpdate` 流，协议**无字段标明 update 属于哪次请求** → 实测归属错乱（第二轮指令出现在第一轮回答里，第二轮 0 字） |
| C4 | 唤醒轮可用，且能续上下文 | 第一轮只提交句柄即收尾；+40s 发"继续刚才那件事，把最终结果原样告诉我"（不提工具名、不重述任务）→ agent 自己调 check 拿到放行并回出同一句柄；`cachedReadTokens=43776` 证明复用同一条会话 |
| C5 | `notifications/cancelled` **不得**当作"用户放弃"的信号 | opencode 在**每一次** `tools/call` 之后都发一条（progress token 正常清理） |
| C6 | `SessionUpdate` handler 内禁止阻塞 | SDK 通知是单 goroutine 串行队列，且 `Prompt` 返回前会等到 notificationWatermark → handler 阻塞会冻住该连接上所有 session 的事件流并拖死 turn |
| C7 | 握手（`initialize`）独立短超时 | `gemini --acp` 静默挂起：3 分钟零字节回写，最后 `initialize 失败: context deadline exceeded` |
| C8 | 取消走显式 `session/cancel`，判定看 `stopReason` | opencode 37ms 内响应，`Prompt` 返回 **nil error + stopReason="cancelled"**；ctx 取消只发 Coder 私有 `$/cancel_request`，codex-acp 回 `-32601 Method not found` |
| C9 | 门面的返回值文本=给子 agent 的指令，须指令化 | `STILL_PENDING（已等 22s，请继续轮询）` 这句话直接驱动了 6 次轮询行为 |
| C10 | 不能靠解析 agent 回执记账 | codex 拿到了 `APPROVED_BY_HUMAN`，最终文本只有它自己的 `Automatic approval review approved (risk: low...)` 判词；opencode 还把内部错误串写进正常回答 |
| C11 | `toolCall.locations` 不可作为路径白名单，也不可原样展示 | opencode 上报 `/Users/dev/workspace-b9e91060/note.txt`，实际写入在沙箱目录 |
| C12 | MCP 协议版本必须宽容协商 | 同一门面：opencode 请求 `2025-11-25`，codex 请求 `2025-06-18`，Friday 现有客户端只声明 `2025-03-26` |
| C13 | 子 agent 继承用户 ambient config（默认行为，非缺陷） | 起 codex 加载用户全部 MCP server（canva/github OAuth 失败刷屏）、`~/.codex/config.toml` 的 `approval_mode="approve"`；起 opencode 把用户 skill 表吐成一条 **25003 B** 的 `available_commands_update` |

## 二、范围

### 做

1. 门面写类工具：`remember` / `schedule_task` / `display_now`（后者需人工在场）。
2. 审批子系统：`friday_request_approval` + `friday_check_approval` 两拍，句柄复用 `approval.Queue`。
3. 批准后唤醒：作业转 `awaiting_wakeup`，队列串行发第二轮 prompt。
4. 成本与用量归账：`usage_update` + `PromptResponse.Usage` → `internal/usage` 维度 `acp:<agent>`。
5. 门面鉴权：每作业短时效 token。
6. 端到端回归用的 fake ACP agent（不烧真实配额）。

### 不做（本文范围外，写进 ROADMAP「明确不做」）

- ACP server 方向（Friday 对外被调）：需要框架支持 per-call 工具源，单独立项。
- `fs/read_text_file`、`fs/write_text_file`、`terminal/*`：桩实现返回 not-supported（SDK 的 `Client` 接口 9 个稳定方法硬必填，但都是可选能力，实测两家 agent 一次都没调）。
- Unstable 方法面：`session/fork`、model/config 切换、elicitation、mcp-over-ACP —— 不暴露给模型，不写代码。
- 把 `locations`/agent 自报元数据当授权依据。
- 门面经 stdio 对外暴露。

## 三、组件与落点

| 位置 | 职责 | 复用的既有形状 |
|---|---|---|
| `internal/acp/facade.go` | mcp-go streamable-HTTP server，挂 `Admin.Port` 前缀 `/mcp/agents` | 框架已依赖 `github.com/mark3labs/mcp-go@v0.44.1`（含 server 包），非从零 |
| `internal/acp/token.go` | 作业级 token：`{jobID, agent, 允许工具白名单, TTL, nonce}`，HMAC 签名，落盘可撤销 | `approval` 的 id 语义 |
| `internal/acp/approval.go` | 两拍审批：submit 立刻回句柄；check 查询状态 | `internal/approval/approval.go` 的 `Queue.Request`/`Pending`/`DecideWith` |
| `internal/acp/turnqueue.go` | 每 session 一个 FIFO：pending turn 串行执行，唤醒轮排队 | `internal/agent/pool.go` 的 `SessionPool.Lock` |
| `internal/acp/events.go` | `SessionUpdate` → `Step` 聚合器（唯一允许的非阻塞出口） | `internal/agent/events.go` 的 `PublishStep` |
| `internal/tools/acp_*.go` | 模型可见工具：`acp_dispatch`/`acp_status`/`acp_cancel`/`acp_approve_decide` | `internal/tools/mcp_edit.go` 的工具+审批形状 |
| `internal/config/config.go` | `AcpConfig` + `AcpAgentConfig` | 照 `McpServerConfig`（`config.go:465`）逐字段对齐 |
| `internal/admin/ext.go` | `handleAcpConfigGet/Set`、`handleAcpStatus`、`handleAcpJobs` | 照 `handleMCPConfigGet/Set`（`ext.go:288`）与 `handleListApprovals`（`ext.go:246`） |
| `internal/usage` | 新增维度 `acp:<agent>`，记 input/output/cached/contextUsed/cost | 现有按天+维度账本（默认 `token-usage.json`） |

## 四、数据模型

```go
// Job 状态机（新增态在括号里标注二期）
queued → running → awaiting_approval ⇄ running → done | failed | canceled
                 → awaiting_wakeup → running      // 二期：批准后唤醒
                 → interrupted                    // 二期：进程重启/连接断开，上下文已丢
type Approval struct {
    Handle   string    // = approval.Queue 的 id，交给子 agent 当句柄
    JobID    string
    Agent    string    // Friday 侧配置名，非 agent 自报
    Tool     string    // 门面被请求的工具
    Summary  string    // 模板生成，不含 agent 文案（C10）
    Detail   string    // 工具名 + 参数摘要 + jobId
    State    string    // pending | approved | denied | expired | released
    Deadline time.Time // 默认 30m，见 §六
}
```

`AcpAgentConfig` 字段：`name / command / args / env / homePolicy / handshakeTimeout / turnTimeout / workspace / allowedFacadeTools`。其中 `homePolicy` 三档（见 §六隔离）。

## 五、关键流程

### 5.1 审批（二期主链路）

```
子 agent ──tools/call friday_request_approval{tool,args}──▶ 门面
门面   ：建 Approval(pending) → approval.Queue 入队 → 立即返回
         "PENDING: 句柄 H；请调用 friday_check_approval 轮询，勿做其他假设"   ← C1/C9
子 agent ──check H ×N──▶ 门面  →  "STILL_PENDING（已等 Xs，请继续轮询）"
用户在网页/后台点同意 ──▶ DecideWith → Approval(approved)
Friday    ：turn 空闲时（turnqueue）发唤醒轮
         "审批 H 已通过，允许执行 <tool>。继续原任务，完成后给出结果。"      ← C3/C4
```

关键：唤醒轮**不立即发出**，而是排进该 session 的 turn 队列；若第一轮仍在跑就等它结束（C3）。`DecideWith` 本身不发 prompt，只置状态 + 通知队列。

与一期的审批卡（`request_permission` → 审批 UI，见 ACP-AGENTS-P1.md §5.2）分工：一期走的是 ACP
原生异步请求通道，agent 会等答复，可慢条斯理弹卡；本节两拍形状只服务于**门面 `tools/call`**——
MCP 调用被 C1 的 15s 超时逼着立即返回，才需要 submit/check 轮询。两条通道共用 `approval.Queue`。

### 5.2 门面被调用时

1. token 校验 → 解出 `{jobID, agent}`（不信任 agent 自报的 `x-codex-turn-metadata`，仅作关联线索）。
2. 工具在白名单内？否则 `-32601`。
3. 只读类直接执行；写类一律走 §5.1 两拍（C1）。
4. 记账以门面自己的调用记录为准，不解析 agent 回执（C10）。

### 5.3 取消与超时

- `acp_cancel` → 显式 `Cancel(sessionId)` → 读 `stopReason`（C8）→ 关联审批卡置 `released`。
- 门面收到 `notifications/cancelled` **只记日志**，不撤卡（C5）。撤卡只在 turn 结束、作业取消或 `Deadline` 到期时发生。
- 三级超时：握手 10s（C7）／ 门面单调用 15s（C1）／ turn 默认 5min 可配。

## 六、隔离档位与鉴权

`homePolicy`：

- `inherit`（默认）：继承 `HOME` 与用户 env。子 agent 能用日常登录态与配置，代价是 C13（它带自己的 MCP server、审批配置和 skill 表进来）。
- `sandbox-home`：给一次性 `HOME` + env 白名单（`PATH` 绝对路径、`LANG`、`XDG_*`）。彻底隔离，但**多半直接失去登录态**（codex 的 `auth.json`、opencode 的 provider 凭据都在 HOME 下），实际可用性需按 agent 逐个验证。
- 二期默认 `inherit`，但在作业记录与后台状态页显式标注该作业用了哪档，避免"以为隔离了其实没有"。（**2026-10-15 已拍板**：默认 `inherit` 与"一期不承诺人工拦截"一并认可，见 ROADMAP 决策记录。）

工作区：per-job 一次性目录，`session/new` 的 `cwd` 指向它。注意 agent 会在工作目录自落状态文件（实测 opencode 写 `.omo/run-continuation/<session>.json`），保留策略与清理时机要写进设计而非事后惊觉。

门面鉴权：`McpServer.Http.Headers` 带 `Authorization: Bearer <token>`，token 绑定 `jobID` + 工具白名单 + TTL。**这一条尚未实测**（探测用的 mock 无鉴权），实测点在一期 P1-E，是二期能否挂写类工具的地基。

## 七、事件与呈现

- 丢弃：`available_commands_update`（实测单条 25003 B，占一轮字节数 42%）、`config_option_update`、`session_info_update`。
- 聚合：`agent_message_chunk` 按 `messageId` 累积，flush 条件为「≥1200 字 或 ≥3s 或 turn 结束」；`agent_thought_chunk` 默认不投（隐私与刷屏），开关可配。
- 新增 `Step.Kind = "acp"`：只投影 `tool_call` 的 `kind` + `title`，并带后缀标记「文案来自子 agent」（C11）。
- 静默期占位：首条 update 实测落在 +7.3~15.3s，turn 前半程无输出 → 派发即发一条 `running` 占位步骤。

## 八、任务拆分（每步可独立验证）

| 步 | 内容 | 验收 |
|---|---|---|
| S1 | 门面写类工具接入（token 实测已前移至一期 P1-E） | 沿用一期已验证的 Bearer token 通路；写类工具一律走 S2 两拍，任何门面调用 <15s 返回（C1） |
| S2 | 审批两拍 + `approval.Queue` 句柄化 | agent 在 60s 内不因门面超时（C1/C2）；卡片字段全来自 Friday 模板（C10） |
| S3 | 唤醒轮 + pending 队列（串行锁已由一期 D2 建立，本步只加排队语义） | 用本次 `cmd/acp-probe -followup-during` 回归：并发场景不再出现归属错乱（C3），顺序唤醒能续上句柄（C4） |
| S4 | 写类工具接入 + 权限直通名单 | `acp_*` 四个工具进 `internal/agent/permission.go:38`，`permission_test.go:213` guard 绿 |
| S5 | 事件聚合 + 用量归账 | 一轮真实作业的 `Step` 条数 <30（当前 raw 153 条）；`acp:<agent>` 维度出现在成本页；金额缺失显示「配额在对面」而非 0 |
| S6 | 崩溃/重启归位 | 杀掉 agent 进程后作业转 `interrupted` 且不自动假装续上；重启后作业表可见、不残留 `running` |

## 九、测试策略

1. **fake ACP agent**：`coder/acp-go-sdk` 同时提供 agent 侧 API，用它在测试里起一个 stdio 假 agent（可控地发 `session/update`、`request_permission`、超时、崩溃）。这是关键工程投入——否则每条回归都要烧别人的配额、耗时 12~209s、结果不可复现。**脚手架在一期 P1-A 建立**，二期复用并追加审批相关注入场景。
2. 报文回放：本地 `evidence/*.jsonl`（含那两条 60s 超时与并发混流的坏案例）作为聚合器与状态机的开发参考；**报文不入版本库**（内含用户 skill 全表与本机路径），入库的固定输入改由 S9 的 fake agent 生成。
3. 超时纪律测试：门面工具 handler 内任何 `select` 都必须有 <15s 的兜底分支，用 `-race` + 超时断言守住。
4. 禁止阻塞：`SessionUpdate` handler 的测试断言其只入队（C6）。

## 十、风险与未决

- **token over `Headers` 未实测**（实测点在一期 P1-E，若某家 agent 不带自定义 header，门面鉴权要改方案）。
- **唤醒轮的语义依赖 agent 记性好**：C4 只在 opencode 上验证过一次；换 agent（尤其 auto_review 类会自说自话的 codex）可能需要在唤醒文本里带上任务重述。缓解：唤醒文本模板化，含「原任务摘要 + 句柄 + 下一步动作」。
- **session 恢复未验证**：`agentCapabilities.loadSession` 在两家都为 true，但重启后能否真把上下文捞回来没测过；本期按"恢复不了就 `interrupted`"设计，不赌。
- **配额与成本盲区**：子 agent 花它自己的订阅，Friday 只拿得到 token 数和（可选的）`cost` 字段，两家 `cost.amount` 实测都是 0 USD。预算护栏（`cost.NewGuard`）不能把它算进自家支出。
- **协议仍在漂**：SDK v0.13.5（2026-06 发版，主干 2026-10-13 仍动），稳定面只有那 9 个 client 方法 + 核心 turn；版本必须 pin 并锁 `protocolVersion=1`。

## 十一、资产去向

`cmd/acp-probe/` 二期不删：转正为 S6/S9 的联调工具与 fake agent 脚手架蓝本，其 `-fs/-terminal/-mock-mcp/-approve-mode/-followup-*` 全部保留为回归开关。

原始 ACP/MCP 报文归档在 worktree 本地 `evidence/`（约 560K），**已加入 `.gitignore` 不入库**：其中 `available_commands_update` 完整包含用户 skill 全表，且含本机绝对路径。入库的固定测试输入由 §九 的 fake agent 生成；需要复现坏案例时用 `cmd/acp-probe` 的对应 flag 重跑即可。

## 十二、会话能力归一化（统一能力清单）

各 agent 暴露可选配置的机制不一致，WebUI 只对接归一化后的 Capability 清单（`internal/acp/options.go`）：

- **来源三路**：configOptions（session/new、load/resume 响应与 `config_options_update` 推送）、
  协议原生 models（`session/set_model`）、协议原生 modes（`session/set_mode`）。modes 全套已在
  SDK v0.13.5；models 为 vendor 补丁（代码带「friday vendor patch」标记，上游收录后整体移除）。
- **kind 词汇表**：model / thought_level / approval_mode / other。去重规则：同一 kind 先到先得，
  config-option 优先于协议原生（zcode 模式双轨取热切路径）；thought_level 仅 config-option 来源。
  **agent 不支持的能力直接缺席**（而非占位），前端 chip 不渲染。
- **切换按 source 路由**：set_config_option / set_model / set_mode，均为会话内热切；
  set_model/set_mode 响应无状态回执，本地兜底改当前值、真值靠 current_mode_update 等推送刷新。
- **实测形态**（2026-10-26）：codex=configOptions(模型/强度/审批/协作)+models；claude-code=仅
  models+modes（模型热切可用，思考强度未暴露）；zcode=configOptions(模型/思考/模式)+modes 双轨。
  cc 思考强度（`_meta.claudeCode.options.maxThinkingTokens`，会话重建级切换）未接入，属可选后续。
- 管理接口：`GET/POST /api/acp/threads/:id/options`（响应键 `capabilities`），会话未建时 GET 会
  拉起 agent 建会话（清单是静态配置，进程随后照常空闲回收）。
- **前端加载时机（Zed 同款）**：选中线程即拉清单——不发消息也能切模型/思考强度/审批模式。
  零消息新线程因此在「选中/创建」时即启动 agent（冷启动期间 composer 显示加载 chip）；
  加载失败（并发槽位满、agent 起不来）toast 报错、按钮不卡死，下次选中重试。协议没有
  「不建会话查清单」的方法，这是「随时可切」的必要代价；进程照常 600s 空闲回收。
