# Friday 工具注入给外部 agent 的可行性评估

调研日期：2026-11-06｜代码基线：`793e334`（fix(智能体): agent 报会话忙时自动取消其活跃轮并重发一次）

> **2026-11-06 晚更新：S0 + S1 + S2 已落地。** 门面已实装（`internal/acp/facade.go` +
> `tool_adapter.go`），`session/new` 与 `session/load` 的 `mcpServers` 不再是空数组；
> S2 的 Bearer token 鉴权与逐 agent 白名单（`allowedFacadeTools`）也已就位，
> header 转发已用 opencode 1.19.4 实测验证（探针 `鉴权拒绝 0 次`）。落地记录见 §六；
> 与原方案的偏差（独立回环监听、全局白名单兜底 + 逐 agent token）与两个拍板
> （写类工具不进门面、默认只读 + 按 agent opt-in）见 §五、§六。
> S5 的 admin API 已就位（`GET /api/acp/facade`：状态 + 调用记录 + 可勾选工具目录），
> 设置页 Vue 面板未接线；两拍审批 + 唤醒轮留给作业模型二期。
>
> **2026-11-10 变更（第一版口径，覆盖下方"默认只读 / 结构性拒绝审批类"的结论）：**
> 门面默认**暴露注册表里的全部工具**——`acp.facade.tools` 为空即不过滤，
> `facadeDeniedTools` 与 `FacadeToolDenied` 已删除；要收紧就在
> `acp.facade.tools` / `allowedFacadeTools` 里显式列名字。审批类工具（写文件、
> 执行命令、改记忆/待办）在 confirm 档下仍会弹 Friday 的审批卡，等不到人点就撞
> 门面的 12s 上限并按工具错误返回；审批请求跟着 ctx 一起取消，不会在 agent
> 放弃之后继续执行。

## 〇、一句话结论

**能，但当前没接。** Friday 已经有完整的两半：

- **工具供给侧**：Friday 自己的业务工具（`internal/tools`，约 33 个）已注册进 `vxagent.Registry`；
- **协议注入侧**：ACP 的 `session/new` 原生支持把 MCP server 挂给 agent，Friday 已经把这行代码写好了——但传的是**空数组**。

缺的只有中间那一层：**Friday 还没有把自己包装成一个 MCP server（"门面"）**。
补上之后，`session/new` 里的 `McpServers` 从 `[]` 变成一份清单，外部 agent（minimax / codex / zcode…）就能直接调用 Friday 的工具。

> 补一句边界：这里的"注入"是**单向**的——Friday 的工具给外部 agent 用。
> 反方向（外部 agent 的工具给 Friday 内置对话用）走的是另一条路：Friday 已经是 MCP **客户端**（`internal/mcp`，`mcp_<server>_<tool>` 前缀注册进 Registry，见 `internal/mcp/manager.go:421`），那条路今天已经通了。

## 一、现状：三条链路分别是什么

### 1. Friday 内置对话（`friday` agent）—— Friday 工具已在里面

内置 Friday 不 exec 子进程，而是用两条内存管道把自己接成一台"智能体"（`internal/acp/builtin.go`）。它跑的就是 Friday 自己的 `Brain`，工具来自 `internal/agent/robot.go:260` 的 `RegisterTools`：

| 分组 | 工具 |
|---|---|
| 文件 | `read_file` `list_files` `write_file` `edit_file` |
| 待办/笔记 | `add_todo` `update_todo` `list_todos` `complete_todo` `delete_todo` `add_note` `list_notes` `update_note` `delete_note` `pin_entry` |
| 记忆 | `remember` `recall` `forget` |
| 任务排程 | `schedule_task` `update_task` `pause_task` `resume_task` `list_tasks` `cancel_task` |
| 联网 | `web_search` `web_fetch` `get_weather` |
| 其它 | `get_time` `mcp_edit` `read_skill` `exec_command`（按配置注册） |

**这一路本来就是通的**，不需要任何改动。

### 2. 外部 agent（minimax / codex / zcode）—— 注入点存在，S1 起挂上门面清单

Friday 在这些 agent 的角色是 **ACP 客户端**（`internal/acp/proc.go` 拉起子进程、走 JSON-RPC）。建会话时的注入点（**S1 后已挂上门面清单**，此前是空数组）：

```go
// internal/acp/manager.go ensureSession（new 与 load 两处同款）
res, proc.conn.NewSession(nctx, acp.NewSessionRequest{
    Cwd: proc.cwd, McpServers: m.mcpServersFor(c),   // ← 门面开启时为 friday 一条
    AdditionalDirectories: extra,
})
```

`McpServers` 是 ACP 规范里的标准字段，SDK 支持四种内联传输（`vendor/github.com/coder/acp-go-sdk/types_gen.go:2702`）：

| 传输 | 结构 | 说明 |
|---|---|---|
| `Http` | `McpServerHttpInline{Name, Url, Headers}` | Streamable HTTP，**已有实测先例**（见下） |
| `Sse` | `McpServerSseInline` | 规范未稳定 |
| `Acp` | `McpServerAcpInline` | 未稳定 |
| `Stdio` | `McpServerStdio{Command, Args, Env}` | 规范要求所有 agent 必须支持 |

### 3. Friday 的 MCP 客户端 —— 方向相反，今天已经能用

`internal/mcp/manager.go` 管理外部 MCP server 进程，把它们的工具以 `mcp_<server>_<tool>` 前缀注册进 Registry，进程崩溃自动重启、配置变更热生效。设置页有可视化状态与重连按钮。

**所以"Friday 的工具"和"外部 MCP 工具"在 Friday 内部是同一个注册表。** 问题只在于这个注册表目前只喂给内置 Friday，没喂给外部 agent。

## 二、关键证据：这条路已经被实测走通过

`cmd/acp-probe/` 是 Friday 自己的 ACP 探针工具，其中 `cmd/acp-probe/mockmcp.go`（240 行）起了一个**最小 MCP streamable-HTTP 门面**，并把它挂进 `session/new`：

```go
// cmd/acp-probe/main.go:460-481
servers := []acp.McpServer{}
if *mockMCPFlag {
    mm, _ = startMockMCP()
    servers = append(servers, acp.McpServer{Http: &acp.McpServerHttpInline{
        Name: "friday-probe", Url: mm.url, Headers: []acp.HttpHeader{},
    }})
}
sess, _ := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: absSandbox, McpServers: servers})
```

它的存在目的写得很清楚：

> 「验证真实 ACP agent 会不会去调用客户端在 `session/new` 里挂给它的 MCP 工具，以及调用外部 MCP 工具时是否触发 `request_permission`。」
>
> —— `cmd/acp-probe/mockmcp.go:3-6`
>
> 「它不是 Friday 门面的实现参考——正式门面应当复用 `mark3labs/mcp-go` 的 server 包。」
>
> —— `cmd/acp-probe/mockmcp.go:7`
>
> 注意 mockmcp.go 里的 `friday_approval_gate` 阻塞开关，是专门用来测审批门联动的。

**也就是说：客户端挂 MCP → 真实 agent 去调 → 这条路已经有实测背书，缺的只是把 mock 换成生产门面。**

## 三、方案：建 `internal/acp/facade.go`（MCP 门面）

### 3.1 形态

在 Friday 的 Admin HTTP 服务上挂一个 Streamable HTTP 端点，作为 MCP server 暴露 Friday 工具：

```
external agent ──tools/call remember{...}──▶ Friday MCP 门面
                                             │
                                             ├─▶ vxagent.Registry.Get(name).Call(ctx, args)
                                             └─▶ 写类工具 → approval.Queue → 网页审批卡
```

落点复用既有件：

| 位置 | 职责 | 复用的既有形状 |
|---|---|---|
| `internal/acp/facade.go` | MCP server（streamable-HTTP），挂 `Admin.Port` 前缀 `/mcp/agents` | Admin 已默认只监听本机（`internal/admin/admin.go:476`） |
| `internal/acp/tool_adapter.go` | `vxagent.Tool` ↔ MCP tool schema 的双向适配 | `internal/mcp/manager.go:421` 的反方向 |
| `internal/acp/token.go` | 会话级短时效 token，绑工具白名单 | `internal/approval` 的 id 语义 |
| `internal/acp/manager.go` | `session/new` 的 `McpServers` 从 `[]` 换成实际清单 | 现有 `ensureSession` 流程 |
| `internal/config/acp.go` | `AcpAgentConfig` 加 `allowedFacadeTools` 白名单 | 照 `McpServerConfig` 逐字段对齐 |

### 3.2 工具来源与白名单

`vxagent.Registry` 是现成的工具目录，`Registry.Names()` 可枚举、`Registry.Get(name)` 可取实例（`vendor/github.com/capyflow/vortexagent/agent/tool.go:93-110`）。适配器直接复用它们，不重复实现工具逻辑。

但**不要把全部 33 个工具一股脑挂出去**，理由：

- `exec_command` 是任意命令执行，挂给外部 agent 等于开远程 shell；
- `write_file` / `edit_file` / `mcp_edit` 会改本机状态，需要走审批；
- 工具里有依赖 Friday 进程内单例的（`Memory` / `Todos` / `Engine` / `Approver`），跨 agent 共享同一份状态需要明确语义（共享还是隔离）。

建议：按 `AcpAgentConfig.allowedFacadeTools` 白名单逐 agent 授权，默认只给只读类（`read_file` `list_files` `recall` `list_todos` `list_notes` `list_tasks` `get_time` `web_search` `web_fetch`），写类按需单独开。

### 3.3 必须遵守的既有硬约束（实测得出，不能拍脑袋改）

`docs/ACP-AGENTS.md` 的 C1–C13 是当时逐条实测出来的，直接适用于门面：

| 约束 | 内容 | 对门面的要求 |
|---|---|---|
| **C1** | 门面单次工具调用必须 **<15s** 返回，永不阻塞等人类 | opencode 的 MCP 客户端 **60.1s** 就放弃并发 `MCP error -32001`；写类工具**不能**同步等审批 |
| **C2** | 审批只能是**提交/查询两拍**，不能同步挂起 | 复用 `approval.Queue` 的 `Request`/`Pending`/`DecideWith`（`internal/approval/approval.go:193-398`） |
| **C9** | 门面返回值文本 = 给子 agent 的指令，须指令化 | 返回要写明下一步动作（如 PENDING + 句柄 + 请轮询），别只回状态词 |
| **C10** | 不能靠解析 agent 回执记账 | 记账以门面自己的调用记录为准 |
| **C11** | `toolCall.locations` 不可作为路径白名单 | 路径合法性只认 Friday 自己算的 `file.allowDirs` |
| **C12** | MCP 协议版本必须宽容协商 | opencode 要 `2025-11-25`，codex 要 `2025-06-18`，Friday 客户端只声明 `2025-03-26` → 门面侧要接受多版本 |
| **C13** | 子 agent 继承用户 ambient config | 启动 agent 时它会带自己的 MCP server 与 skill 表进来，需预期 |

### 3.4 审批链路的特别说明

C1 逼着门面**不能同步等审批**，但 Friday 的 `approval.Queue` 现有形状是同步阻塞的（`Request` 等用户点）。两种做法：

1. **两拍（文档 M6 方案）**：门面 `tools/call` 立刻返回句柄 + "请轮询"，另开一个 `friday_check_approval` 工具查状态。合规，但多一次模型往返。
2. **绕开 MCP、复用 ACP 原生通道**：让外部 agent 通过 ACP 的 `request_permission` 走 Friday 已有的审批卡（`internal/acp/bridge.go` 的 `permissionHook` → `approval.Queue`）。这条路**今天已经通了**，不受 C1 的 15s 约束（agent 会等答复，可慢条斯理弹卡）。

**建议走 2**（复用现有 `request_permission` 通道），把门面只用于**只读工具**——读类不需审批，天然满足 C1；写类留在 ACP 原生审批通道上。这样能绕开整套两拍轮询机制，工作量小一个数量级。

> **✅ S3 已按此拍板落地（2026-11-06 晚），含一个重要修正**：读全 M6 原案后确认，
> `request_permission` 是 agent→Friday 方向的请求，**结构上服务不了门面的
> `tools/call`**（那是 Friday 侧函数调用，不产生 ACP 报文）——ACP-AGENTS.md
> §5.1 也写明「两拍形状只服务于门面 tools/call」。所以落地形态是：
> ① 门面**结构性拒绝**审批类工具（`write_file`/`edit_file`/`exec_command`/`mcp_edit`，
>   白名单点名也剔除——它们内部同步等审批，必撞 C1）；
> ② 写文件/执行命令类需求由外部 agent 用**自己的工具**走已有的 request_permission
>   审批卡（一期 P1-D，今天就在用）；
> ③ 低风险 Friday 原生写入（`remember`/`add_todo` 等，无审批、快，C1 天然满足）
>   开放为**按 agent opt-in**——写进 `allowedFacadeTools` 即生效（S2 机制，无新代码）。
> 两拍审批 + 唤醒轮（M6 §5.1 的完整形状）留给作业模型二期。

### 3.5 鉴权

Admin 服务默认只监听本机（`internal/admin/admin.go:476` 的 `net.Listen` + 默认 host），门面上在本机暴露风险可控。**S2 已实装**：`session/new` 传 `Headers: []acp.HttpHeader{{Name:"Authorization", Value:"Bearer "+tok}}`，tok 绑 `{工具白名单, TTL=24h}`（`facade.go` 的 grant 表），tools/list 与 tools/call 都按 token 白名单执行；每次建会话/重接换新 token。

✅ **已实测（2026-11-06，opencode 1.19.4）**：`acp-probe -mock-mcp -mock-auth` 实测 agent 会转发 `session/new` 里声明的自定义 header——mock 要求 Bearer，opencode 带着原值调用，`鉴权拒绝 0 次`、`tools/list` 正常到达。逃生口：`acp.facade.disableAuth: true` 可关鉴权（某家 agent 不转发 header 时用，门面只听回环，风险同级于 Admin 本机暴露）。

## 四、落地顺序建议

| 步 | 内容 | 验收 |
|---|---|---|
| S0 | 用 `cmd/acp-probe -mock-mcp` 复跑，确认今天的 agent 仍会调挂上去的 MCP 工具 | 探针输出里出现 `tools/call` |
| S1 | `facade.go`：MCP server 只挂**只读**工具，`McpServers` 从 `[]` 换成实际清单 | 外部 agent 能列出并调用 Friday 只读工具 |
| S2 | token 鉴权 + `allowedFacadeTools` 白名单 | 非白名单工具返回 `-32601`；无 token 被拒 |
| S3 | 写类工具接入（走 ACP 原生 `request_permission`，非 MCP） | 审批卡弹出、批准后工具真实执行 |
| S4 | 协议版本宽容协商（C12）+ 15s 超时兜底（C1） | 多版本 agent 都能连；慢调用按期返回 |
| S5 | 设置页：按 agent 勾选可注入的工具、查看调用记录 | 配置热生效，不用重启 agent |

## 五、两个拍板（均已定，2026-11-06 晚）

1. **写类工具走哪条路** → **门面不接审批类工具**。读全 M6 原案后确认：`request_permission` 是 agent→Friday 方向的请求，结构上服务不了门面的 `tools/call`（ACP-AGENTS.md §5.1 也写明两拍形状只服务于门面 tools/call）；两拍 + 唤醒轮留给作业模型二期。写文件/执行命令类需求由外部 agent 用自己的工具走已有审批卡（一期 P1-D）。护栏代码：`facadeDeniedTools`（`internal/acp/facade.go`）。
2. **工具白名单的默认值** → **默认只读**。`remember` / `add_todo` / `schedule_task` 等低风险状态写入不默认开放——需要哪个 agent 能写，就在它的 `allowedFacadeTools` 里点名（按 agent opt-in，机制 S2 已就位，无新代码）。

## 六、落地记录（S0–S3，2026-11-06 晚）

### 6.1 S0 + S1 已完成

| 步 | 结果 |
|---|---|
| **S0** | `acp-probe -mock-mcp` 对 opencode 1.19.4 复跑两次：注入链路均通（agent 连上门面 → `initialize`（要 2025-11-25，再次印证 C12）→ `tools/list`）。`tools/call` 未在本次出现——opencode 自身模型调用挂死 4 分钟零输出后超时，属 agent 侧模型问题；`tools/call` 语义沿用探针此前的实测背书 |
| **S1** | 门面实装并接线，测试全绿（`internal/acp`、`internal/config` 全量回归 + build + vet） |

新增/改动文件：

| 文件 | 内容 |
|---|---|
| `internal/acp/facade.go` | MCP streamable-HTTP server：initialize（回显版本，C12）/ tools/list（白名单过滤）/ tools/call（12s 超时 + 指令化错误文本，C1/C9）/ ping；工具执行错误走 `isError:true` 的 result，未知工具走 `-32602` |
| `internal/acp/tool_adapter.go` | `vxagent.Tool` → MCP 条目（schema 空兜底 object）与白名单解析；不重复实现工具逻辑 |
| `internal/acp/manager.go` | `SetRegistry`、`mcpServersFor`、`ensureFacade`（白名单变化自动重建）；`session/new` 与 `session/load` 挂清单；`Close` 关门面 |
| `internal/config/acp.go` | `acp.facade` 段（`enabled` 缺省开启、`tools` 缺省 `FacadeDefaultTools`）；`Normalized`/`Validate`/`UpdateAcp` 落盘（默认形态不写进 config.yaml） |
| `cmd/robot/main.go` | `acpMgr.SetRegistry(regFn)`——与设置页共用同一份注册表，MCP 工具清单 rebuild 后门面下次 `tools/list` 自动拿到新清单 |
| `internal/acp/facade_test.go`、`internal/config/acp_test.go` | 门面 HTTP 行为 ×7、会话注入/关断 ×3、facade 配置 ×3 |

### 6.2 S1 与原方案的偏差

1. **监听形态**：没用「挂 Admin 端口 `/mcp/agents`」，而是门面自己起 `127.0.0.1:0` 随机端口监听（惰性，第一个外部 agent 建会话时才占端口）。理由：外部可见面与 Admin 同级（仅回环）、不依赖 Admin 是否装配、无需在 Admin 鉴权层给 MCP 开旁路（JWT 之外的裸路由）。
2. **白名单模型**：S2 落地为「全局 `acp.facade.tools` 兜底 + 逐 agent `allowedFacadeTools` 覆盖 + token 绑白名单」——一台门面同时服务不同权限的多个 agent，`tools/list`/`tools/call` 都按 token 执行。
3. **写类工具**：S3 未动，默认清单只含只读九件。

### 6.3 S2 落地记录

| 件 | 内容 |
|---|---|
| `internal/config/acp.go` | `AcpAgentConfig.allowedFacadeTools`（空 = 全局兜底）、`AcpFacadeConfig.disableAuth`（缺省鉴权开启）；Validate 拒空工具名；UpdateAcp 只在偏离默认时落盘 |
| `internal/acp/facade.go` | grant 表（token → 白名单 + 24h TTL，签发时顺手清过期）；`ServerFor(allowed)` 签发带 Authorization header 的条目；无/错/过期 token 一律 401；tools/list 与 tools/call 按 grant 白名单执行；`disableAuth` 时回裸条目 |
| `internal/acp/manager.go` | `facadeWhitelistFor`（agent 覆盖 > 全局兜底）；`ensureFacade` 只随兜底白名单/鉴权开关重建，逐会话 token 不触发重建；session/new 与 session/load 都带 token |
| `cmd/acp-probe` | `-mock-auth` 开关：mock 要求 Bearer、探针在 server 条目里带 header——实测 opencode 1.19.4 会转发（鉴权拒绝 0 次） |
| 测试 | facade 鉴权 4 件（401/逐 token 白名单/过期/关鉴权）+ 真监听冒烟 1 件 + manager 逐 agent e2e 1 件 + config 落盘回读 1 件 |

**过程中抓到的重大 bug**：S2b 重写 facade.go 时把 `ensureListen` 里的 `go f.server.Serve(ln)` 弄丢——listener fd 活着（TCP 能连上）但没有 goroutine accept，所有请求堆 backlog 直到客户端超时。S1 的测试全是 httptest 回放式，探不到这种坏法；靠新增的「真监听冒烟用例」（`TestFacadeServeAcceptsRealRequests`，对真实端口打一发）当场抓住。教训：**凡是起监听的代码，测试里必须有一次真实回环 HTTP 请求。**

### 6.4 S3 落地记录

| 件 | 内容 |
|---|---|
| `internal/acp/facade.go` | `facadeDeniedTools` 拒绝清单（write_file/edit_file/exec_command/mcp_edit）+ `facadeFilterDenied`：构造期滤兜底白名单、发 grant 时滤逐 agent 白名单——tools/list 不出现、tools/call 到不了 Registry，双鉴权模式全覆盖 |
| `internal/acp/tool_adapter.go` | 清单生成与解析同步剔除拒绝名单 |
| 测试 | `TestFacadeDeniesApprovalTools`：白名单显式点名 write_file 仍被拒（tools/list 无、call -32602、关鉴权模式同拒） |

### 6.5 S5 admin API 落地记录（2026-11-06 晚）

| 件 | 内容 |
|---|---|
| `internal/acp/facade.go` | grant 绑定 agent 配置名（调用记录归因，C10 语义）；`facadeRecord` 环形缓冲（至多 50 条：at/agent/tool/outcome ok·error·timeout·denied/detail 截 200 字）；`FacadeStatus`/`FacadeGrantInfo` 对外快照（活授权按 agent 排序，不含 token 值）；`FacadeToolDenied()` 供渲染勾选框 |
| `internal/acp/manager.go` | `FacadeStatus()`：配置层开关 + 运行态归并；**不为看状态而起监听**（惰性语义保持） |
| `internal/admin/acp.go` | `GET /api/acp/facade`（需登录）：`status`（状态+记录）+ `tools`（注册表全目录，带 `denied` 审批类护栏标记与 `inDefault` 只读默认清单标记，前端据此渲染按 agent 勾选框） |
| 白名单读写 | 复用既有 `GET/POST /api/acp/config`——`acp.facade`（enabled/tools/disableAuth）与 `agents[].allowedFacadeTools` 已随结构体往返并落盘（S2 测试覆盖） |
| 测试 | facade 记录与快照 2 件（含 agent 归因、最新在前、denied 记录）；admin 端点 1 件（enabled、工具标记、未登录拦截） |

**前端 Vue 面板已接线（同日晚间）**：设置 → 智能体新增「Friday 工具门面」卡片——
启用/关鉴权开关、门面地址与计数（tools/list · 调用 · 拒绝 · 活授权）、全局工具
勾选网格（denied 禁选 + 「审批类」标记）、最近调用表（时间/agent/工具/结果/详情，
结果中文化）；每个 agent 卡片内附逐 agent 白名单勾选（留空 = 用全局清单），折叠态
以「Friday 工具 ×N」徽标提示自定义覆盖。白名单读写随既有保存按钮走
`POST /api/acp/config`。「全部取消勾选 = 恢复默认只读清单」。

**顺带修复一个竞态**：`ensureListen` 的 Serve goroutine 无锁回读 `f.server`，与
`Close` 置 nil 竞争时 nil 接收者 SIGSEGV（子集测试必现、全量跑时序不同则绿）。
改为局部变量持有 `*http.Server`；`-race` 全量回归三包全绿。

### 6.6 已知边界（待后续步骤处理）

- **白名单变更会换端口**：设置页改 `facade.tools` 后下一次建会话起新监听，旧 agent 会话里记的还是旧 URL（重建会话即恢复）。
- `read_file`/`list_files` 的路径边界是 Friday 全局 `file.allowDirs`（C11 的既定语义），不是各 thread 的工作区根——跨工作区可读，若要收紧需按 thread 收窄。
- token TTL 24h：一轮跑超过 24h 会撞线（下次建会话/重接自动换新），实际可忽略。

## 七、参考位置速查

| 事实 | 位置 |
|---|---|
| Friday 工具清单与注册 | `internal/agent/robot.go:260`（`RegisterTools`） |
| 工具实现 | `internal/tools/*.go`（33 个 `Name()`） |
| MCP 客户端（反向：外部工具给 Friday 用） | `internal/mcp/manager.go:421` |
| ACP `session/new` 注入点 | `internal/acp/manager.go:1209`（load）、`:1233`（new） |
| SDK 的 MCP 内联传输结构 | `vendor/github.com/coder/acp-go-sdk/types_gen.go:2702` |
| 实测探针（挂 MCP → agent 调用的证据） | `cmd/acp-probe/mockmcp.go`、`cmd/acp-probe/main.go:460-481` |
| 硬约束 C1–C13 | `docs/ACP-AGENTS.md:25-36` |
| 门面的既有设计稿（M6 作业模型路线） | `docs/ACP-AGENTS.md:40-110` |
| 审批队列 API | `internal/approval/approval.go:193-398` |
| ACP 原生审批通道 | `internal/acp/bridge.go`（`permissionHook`） |
| Admin 服务只听本机 | `internal/admin/admin.go:476` |
