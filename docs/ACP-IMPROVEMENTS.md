# ACP 接入改进清单

状态：**M-1/M-2/M-5/M-8 已实施（2026-10-21 上午）**；**M-3/M-4 已实施（2026-10-21 下午，
拍板：兼容官方 registry.json schema，直接同源）**；M-6/M-7 按推荐缓做/不排期。
bug 类问题已于同日修复（附录 A）。
分支：`explore/acp-agents`｜参考系：Zed 的 ACP 客户端实现（`zed-industries/zed` 的
`agent_servers` / `acp_thread` crate，2026-10-20 逆向笔记）。

## 一、改进项

每项给出：动机（对齐 Zed 的什么做法 / 我们实测暴露的什么问题）、方案草案、工作量、
验收或决策点。编号 M-*，开工顺位见 §二。

### M-1 登录诊断与指引（in-band auth 的低成本替代）

- **Zed 做法**：`initialize` 响应里解析 `authMethods`，面板内直接引导 OAuth / 终端登录
  （`acp_thread/auth_methods.rs`，Gemini 的 terminal auth 是特判注入）。
- **Friday 现状**：全靠 ambient 登录态（C13 `inherit` 档）；`Test` 只报「initialize 失败 +
  stderr 尾部」，用户分不清"没登录"还是"命令写错"。
- **方案**：① 握手成功后把 `authMethods` 与内置的 per-agent 登录指引表（如 codex →
  `终端里跑 codex login`；gemini → `gemini` 首次交互登录）落到 agent 状态里；② `Test` 与
  thread 投影的失败 note 按错误类别给对应指引；③ `stderr` 关键词辅助（`not logged in`、
  `401`、`login`）作为兜底分类。
- **工作量**：小（半天～1 天，含指引表数据）。
- **验收**：对未登录的 codex 跑 `Test`，报错一句话指出「未登录，去服务器终端执行 xxx」。
- **实施记录**（2026-10-21）：`proc.go` 新增 `loginHints` 指引表（codex/claude/gemini/
  opencode/zcode，按配置名→命令基名两级匹配）+ `looksLikeAuthProblem` 关键词扫描；
  握手失败错误统一走 `classifyInitError` 给出可行动文案。回归 `TestClassifyInitError`。

### M-2 initialize 失败的错误分类

- **Zed 做法**：连接失败是 typed error（`UnsupportedVersion` 等），UI 按类别呈现；v1/v2
  协商失败能保留两次协商的错误链。
- **Friday 现状**：`spawnAgent` 把所有失败包成一个「initialize 失败: %w」，LookPath 失败、
  进程秒退、握手超时、协议版本不匹配混在一起。
- **方案**：spawn/握手路径错误分类——命令不可解析 / 进程秒退（附 stderr）/ 握手超时（C7）/
  版本不支持（响应 `protocolVersion != 1` 时明确说「该 agent 只支持更高协议版本，等待
  SDK 跟进」）；类别随错误文本进投影 note（与现有「上下文已重置」note 同机制）。
- **工作量**：小（与 M-1 同批做，共享分类结果）。
- **决策点**：无争议，建议直接排入。
- **实施记录**（2026-10-21）：握手失败分四类——进程秒退（附 stderr 定性：疑似未登录/
  命令问题）、握手超时（C7 形态）、疑似未登录（err+stderr 关键词）、协议层错误；
  另在 initialize 成功后校验应答 `protocolVersion`，非 v1 明确报「协议版本不支持，
  等待 ACP SDK 跟进」。

### M-3 目录远程化（对齐 Zed 的 registry.json 模式）

- **Zed 做法**：agent 目录托管在 `cdn.agentclientprotocol.com/registry/v1/latest/registry.json`，
  条目带 `distribution`——`npx`（package+args）或 `binary`（按 `darwin-aarch64` 等平台键给
  下载地址 + sha256）；客户端缓存 + 版本 ceiling 策略（他们踩过 min-release-age 导致
  「registry 比用户能装的更新」的坑）。用户点 Install 只是写一行 settings，真正的下载
  在首次连接时惰性发生。
- **Friday 现状**：`internal/acp/catalog.go` 编译期固定 5 条，新增 agent 要发版。
- **方案分期**：
  1. 数据与代码分离：catalog 内嵌为 JSON 资源（`go:embed`），结构对齐官方 registry 条目
     的子集（id/name/description/check/installCmd/docUrl/template），新增 agent 改数据即可；
  2. 支持配置远端 URL（默认仍内嵌兜底）：拉取 + sha256 校验 + 本地缓存（对齐
     `agent_registry_store.rs` 的 fetch-cache-refresh 形状）；
  3. binary 分发：按平台下载 + 校验 + 解压进 `~/.vortex/acp/agents/<id>/<version>/`，
     复用 Install 的进程组击杀纪律。
- **决策点**：① 要不要**直接兼容官方 registry.json schema**——好处是白嫖上游条目更新，
  风险是官方目录面向编辑器场景（含大量无关条目），需要白名单/过滤规则；② 个人管家
  信任模型是单管理员，鉴权只做 sha256 不做签名是否可接受（Zed 也只做 sha256）。
- **工作量**：分期 ①半天、②1 天、③1～2 天。
- **实施记录**（2026-10-21，拍板：直接兼容官方 schema，全量展示不过滤——个人管家
  由管理员自己甄别）：新增 `internal/acp/registry.go`——wire schema 与 Zed
  RegistryIndex 逐字段对齐；`Catalog(ctx, refresh)` 合并内置精选（同 id 覆盖官方）
  与官方条目，拉取失败回退过期缓存（`~/.vortex/acp/registry-cache.json`，TTL 1h），
  无缓存降级仅内置（离线可用）；安装按 `distribution` 分发——npx → `npm install -g`
  后锚定 npx 绝对路径，binary → 按平台键（darwin-aarch64 形态）下载 + sha256 校验 +
  解压到 `~/.vortex/acp/agents/<id>/<version>/`（拒绝链接条目/路径越界，总量守卫）。
  设置页「刷新官方目录」按钮强制重拉。**线上 wire 兼容性已实测**：官方 42 个条目
  全部解析成功（npx=23 / binary=17 / 无分发=2）。
  **补（2026-10-28）**：registry 缓存与 binary 安装目录随配置统一迁入数据目录
  （`acp/`、临时会话 `acp-sessions/`），Friday 不再使用 `$HOME/.vortex`——上文
  路径按当时实现记录，以数据目录为准。

### M-4 安装的版本锚定与升级提示

- **Zed 做法**：npx 型安装到自有目录（`--save-exact` + 版本 ceiling 注释），带
  `new_version_available` 通知通道。
- **Friday 现状**：`InstallCmd` 直接全局安装，无版本记录、无升级提示。
- **方案**：安装/注册时探测版本号（`<check> --version`，失败则空）落 config；后台状态页
  显示当前版本；升级检查按需手动触发（不轮询），提示「重新安装可升级」。
- **工作量**：中（1 天内）。
- **实施记录**（2026-10-21）：`AcpAgentConfig` 新增 `Version` 版本戳（yaml/json 落盘，
  保存表单原样带回不丢失）；registry 条目安装/升级时把官方版本写入清单条目；目录卡片
  对比清单版本戳与官方版本，不一致显示「可升级」徽章 + 「一键升级」按钮（重新安装 =
  升级，npx 重装、binary 重下载到新版本目录）；已注册条目再装同步版本戳、启动命令
  与参数，保留自定义环境和工作目录，热更新回收旧进程，下一轮使用新版本。
  设置表单同步升级结果，避免随后保存时把旧版本和启动路径写回。

### M-5 用量记账补全（usage_update 口径）

- **现状**：只取 `PromptResponse.Usage`（单轮 in/out），`usage_update` 在聚合器里丢弃。
- **口径修正（实施时实测 SDK）**：`usage_update` 不是 in/out 累计，而是**上下文水位**——
  `size`=窗口总量、`used`=当前占用、可选 `cost`=会话累计花费。原方案「成本页分本轮/累计
  两栏」不成立，改为水位展示。
- **方案（已按修正后口径实施）**：聚合器记录最近一次水位，随投影落盘（Thread 新增
  `contextUsed/contextSize/contextCost`）；SSE done 帧带 `context`，前端轮末汇总显示
  「上下文 used/size」。单轮 in/out 记账仍以 `PromptResponse.Usage` 为准（`usage.Record`
  维度不变）；cost 是 agent 自报，仅展示、不进预算闸门（C10）。
- **工作量**：小～中。
- **实施记录**（2026-10-21）：`aggregate.go` 捕获 `UsageUpdate`；`manager.go` 落盘；
  `admin/acp.go` done 帧带水位；前端 `blocks.js`/`AgentView.vue` 展示。回归
  `TestUsageContextWatermark`（校验单轮记账与水位互不混淆）。
- **补充**（2026-10-27）：智能体页 composer 左组新增常驻只读水位 chip（百分比 +
  used/size 弹层，agent 不上报则缺席）；降级重开会话时清空旧水位，避免展示已失效
  的数字。ACP 协议无压缩/清除接口，故不提供对应操作。回归
  `TestDegradedSessionClearsContextWatermark` 与 agent-view 前端用例。

### M-6 进程复用模型（一 thread 一进程 → 一连接多会话）

- **对比**：Zed 是一条连接（进程）承载多 session（`HashMap<SessionId, Session>`），内存
  省、登录态进程只有一份；Friday 一 thread 一进程，隔离最彻底，但 `MaxConcurrent=2`
  很快见顶，且同一 agent 开多个 thread 会并发跑多个登录态进程（codex 多进程写
  `~/.codex` 会话库的并发风险未验证）。
- **若做**：Manager 按 agent 名复用进程、thread 绑定 session——SDK 连接层并发安全
  （inbound 每请求一 goroutine，`bridge.go` 注释已确认），C3 串行本来就是 session 级的；
  主要是 `events` 通道要按 `sessionId` 过滤分发（update 事件自带 sessionId），以及连接
  生命周期从 thread 迁到 agent（一个 thread 删除不再杀进程）。
- **决策点**：个人管家场景 thread 并发有限，**建议缓做**；触发条件是 MaxConcurrent 实际
  成为瓶颈，或同 agent 多 thread 出现登录态互踩。M-3 的目录远程化若带来"装很多 agent"
  的场景，优先级上调。
- **工作量**：大（连接生命周期重构 + 事件分发改造 + 全量回归）。
- **实施记录**（2026-10-29，已落地）：连接按 **agent 名 + 主目录** 复用（`procState`）——
  进程的 cwd、MCP/skill 表与登录态都是进程级的，会话的 cwd 又必须与进程一致，所以
  不同目录的会话不能共用一条连接；同一工作区的会话因此天然共享。改动点：
  ① 会话能力（configOptions/models/modes）从进程缓存搬到会话记录 `live`，切换与推送
  都按会话刷新；② 每个进程一条**常驻事件泵**，按 `SessionId` 分发给进行中的那一轮，
  没有轮次的事件（session/load 重放、迟到通知）直接丢弃；③ 轮次收尾加"结界"
  `settleTurn`：先让泵暂停、把已入队的事件排空再摘接收器——`Prompt` 返回只保证
  通知已入队（watermark），少这一步会随机丢结尾文本与用量（首轮实现踩到，三连跑
  全绿后加 `-race` 复核）；④ 权限请求按 `SessionId` 路由回所属会话的审批卡；
  ⑤ 并发上限语义从"会话数"变成"进程数"；⑥ 删会话只摘 session，删到该连接最后一个
  才回收；配置热更新只回收 agent 配置真变了的连接。
  **回归**：新增「同目录两会话共用一进程 / 不同目录各起进程 / 上限 1 仍可跑同连接
  第二个会话 / 删一个会话不回收进程 / 权限按会话路由」五条用例；`go test -race`
  acp 与 admin 全绿。
  **剩余风险**：同一 agent 的多个会话并发写它自己的登录态/会话库（如 codex 的
  `~/.codex`）仍未验证真实 agent 的表现——目前只在 fake 上过。


### M-7 协议演进观察（v2 / elicitation / config options）

- **背景**：Zed 主线的 v2（`session/set_config_option`、elicitation、resume、session 列表）
  尚在 beta flag 后；coder SDK pin 在 v1 的九个稳定方法面。ACP-AGENTS.md §十已记录
  「协议仍在漂」。
- **方案**：不做预研投入，保持 SDK pin + `cmd/acp-probe` 实测纪律。触发条件：目标 agent
  宣布只支持 v2、或需要 elicitation（表单式交互）时，再评估双栈（Zed 自己也是 v2 失败
  回退 v1 的双适配器形状，`acp.rs` 的 connect 可参考）。
- **工作量**：0（观察项）。M-2 的错误分类先留「版本不支持」识别，作为触发信号。

### M-8 审批卡前端联动细节（本期修复的延伸）

- **已修**（后端）：turn 结束自动撤卡（`approval.Pending.Cancel` + `manager.handlePermission`
  watchdog）。
- **剩余**：① 前端审批卡轮询 `pending` 列表时，被撤下的卡的呈现（直接消失 vs 灰置
  「已随对话结束关闭」）；② `Decide` 打在已撤卡上时后端返回 false，前端应有
  「该审批已失效」提示而不是无响应。
- **工作量**：小（纯前端）。
- **实施记录**（2026-10-21）：核查发现前端 `ApprovalCards.vue` 的 `decide()` 已会把
  后端「未找到待审批操作（可能已超时或已处理）」toast 出来、且卡保留到下一次轮询
  对齐——剩余缺口只是回归保护。新增 `web/tests/approval-cards.test.js`：成功路径
  （移除+提示）与失效路径（toast 失效原因、卡保留）两用例。

## 二、开工顺位

| 顺位 | 项 | 状态 |
|---|---|---|
| 1 | M-1 + M-2 | ✅ 已实施（2026-10-21） |
| 2 | M-8 | ✅ 已实施（2026-10-21） |
| 3 | M-5 | ✅ 已实施（2026-10-21，口径修正后） |
| 4 | M-3（①②③ 全量）+ M-4 | ✅ 已实施（2026-10-21，拍板：直接兼容官方 schema） |
| 5 | M-6 | ⏸ 缓做，等 MaxConcurrent 真实瓶颈信号 |
| — | M-7 | 观察，不排期 |

## 附录 A：本次已修复的 bug（2026-10-21，供对照）

| # | 问题 | 修复 |
|---|---|---|
| B1 | `proc.go` env 用 append 叠加，重复 key 时 libc 子进程读到旧值（覆盖静默失效） | `buildEnv`：map 合并、同名 key 原地替换、继承环境自身重复保留首条 |
| B2 | 权限审批阻塞 30 分钟不受 turn 生命周期约束；turn 超时/停止/进程崩溃后卡仍挂着 | approval 新增 `RequestCancelable/Pending.Wait/Cancel/Done`；`handlePermission` 挂 watchdog，turn 结束即撤卡并按拒绝结算；无进行中对话时的权限请求直接拒绝不入队 |
| B3 | `live.turnCancel` 只写不读（死代码） | 删除，统一走 `session/cancel`（C8）路径；`live.turnDone` 承担结束信号 |
| B4 | `catalog.Install` 超时只杀 sh，npm/curl 孙进程残留 | `Setpgid` + `cmd.Cancel` 进程组击杀 + `WaitDelay`（与 N1 同纪律） |
| B5 | init 失败路径 `cmd.Process.Wait()` 与收尸 goroutine 双重 Wait | 删除 Process.Wait，收尸统一由 goroutine 负责 |
| B6 | stderr 临时文件永不清理，长期运行累积 | kill 时缓存 64KB 尾部进内存（`stderrMemo`）再删文件；报错路径引用不受影响 |
| B7 | `threadLock` map 只增不删 | `Delete` 时移除 |
| B8 | 注册模板保留裸 `npx`，daemon PATH 缺失时 spawn 失败 | `templateWithPath` 把启动器锚定为绝对路径（解析失败保留原名）；CLI 形态行为不变 |

新增回归：`TestBuildEnvOverride` / `TestBuildEnvDuplicateInheritKeepsFirst` /
`TestStderrTailAfterKill` / `TestPermissionReleasedOnTurnEnd` /
`TestRequestCancelableCancel` / `TestRequestCancelableApprove`；
`TestTemplateWithPath` 更新（启动器锚定分支）。`-race` 全绿。

## 附录 B：M-1/2/5/8 与 M-3/4 实施的新增回归（2026-10-21 下午）

`TestClassifyInitError`（握手错误四分类 + 登录指引）、`TestUsageContextWatermark`
（水位落盘与单轮记账互不混淆）、`TestParseAndConvertRegistry`（官方 wire schema
解析与卡片转换）、`TestRegistryCacheFallbackAndBuiltinOverride`（缓存回退/内置覆盖/
离线降级）、`TestInstallRegistryNpx`、`TestInstallRegistryBinary`（下载+sha256+解包，
含篡改拒绝）、`TestExtractArchiveRejectsTraversal`（路径越界）、
`TestUpdateAcpVersionStamp`（版本戳落盘）、`approval-cards.test.js`（审批卡失效提示）。
另做一次性线上验证：官方 registry.json（42 条）全量解析通过。
