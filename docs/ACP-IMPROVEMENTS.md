# ACP 接入改进清单

状态：**M-1/M-2/M-5/M-8 已实施（2026-10-21 上午）**；**M-3/M-4 已实施（2026-10-21 下午，
拍板：兼容官方 registry.json schema，直接同源）**；**M-9/M-10 已实施（2026-10-30，
工作区改动追踪与默认审批档位）**；M-6 已实施（2026-10-29）；M-11 已实施（2026-10-30，
上下文交接）；M-12 已实施（2026-10-30，工作区项目记忆）；M-13 已实施（2026-11-02，验证面板）；
M-14 已实施（2026-11-02，并发排队可见与槽位记账）；M-15 已实施（2026-11-02，超时预算分段与静默看门狗）；
M-7 按推荐缓做/不排期。
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

### M-9 工作区改动追踪与还原

- **动机**：agent 直接改盘，时间线只有文字和工具行；用户看不到“这一轮改了什么”，也没有退路。
  对标 ZCode 的编辑历史/撤销，但 Friday 的重点是**把改动落成可核对的文件清单与 diff**，
  而不是做一个完整 IDE。
- **方案**：每轮开始前做轮前快照——git 仓库用 `git stash create` 生成不碰工作区的 dangling
  commit（未跟踪文件另存副本），非 git 目录用带体积/数量上限的副本；轮末按 git diff /
  文件指纹生成改动清单与逐行 diff，落 `acp/snapshots/<thread>/<turn>/changes.json`。
  工作区抽屉展示文件树、预览、本轮 diff 与“还原本轮”；还原前整轮预检，本轮后被改过/删除/重建
  的文件拒绝还原，避免覆盖用户后续编辑。
- **边界**：单轮 2 万文件 / 100MB / 单文件 5MB，保留最近 30 轮；超出或快照失败时明说
  “只能看不能还原”，不假装成功；暂存区状态不承诺恢复；不提供 IDE 级文件编辑与终端。
- **实施记录（2026-10-30）**：`internal/acp/changes.go`（快照/扫描/逐行 diff/还原）、
  `manager.Send` 接入与 `Thread.Entry.changes` 投影、admin `files/changes/revert` 接口、
  `WorkspaceFilesDrawer.vue` 与时间线“本轮改动”摘要。回归：`changes_test.go` 覆盖 git/非 git
  的检测、diff、还原与“本轮后又被改过拒绝还原”；`TestSendRecordsWorkspaceChanges` 覆盖
  Send → 投影 → 详情 → 还原全链路；前端 `workspace-view.test.js` 覆盖文件树/预览/diff/还原。

### M-10 工作区默认审批档位

- **动机**：ACP 审批卡弹不弹取决于各 agent 自己的审批模式，Friday 没有统一的“只读/每次都问/自动”。
- **方案**：工作区保存 `policy`（默认 `ask`）；新会话/重连时按语义关键词映射到 agent 的
  `approval_mode` 能力，映射不到就在线程上写 `policyNote` 并在会话头部标“审批档位未下发”。
  它只是默认值：同一会话手动选过的审批模式由 `capChoices` 随后重放并覆盖。
- **边界**：没有沙箱，不能把不支持审批模式的 agent 强行变成只读；这类情况靠轮前快照兜底，
  文案明确说“只能事后还原、不能事前拦截”，不假装已经生效。
- **实施记录（2026-10-30）**：`Workspace.Policy` + `SetPolicy`、`applyWorkspacePolicy` 与
  `pickApprovalValue`、admin 更新接口与编辑工作区弹窗；回归 `TestPickApprovalValue` /
  `TestWorkspacePolicyDefaultsAndValidation` 与 admin 端点用例。
- **修订（2026-11-02，V-1/V-4）**：真机跑 codex 发现它只报 `read-only / workspace-write /
  agent / agent-full-access`，「每次都问」命中不到 → 悄悄留在 Auto review（等于自动放行）。
  映射改成**分层退让**：先找语义等价档，找不到只朝“同样能干活但更保守”的方向退一档
  （ask→可写受限，auto→自动但不免审批），并记下发给 agent 的实际取值
  （`policyValue` / `policyValueName` / `policyFallback`），会话头部直接写“审批档位实际是
  Workspace Write”。红线：ask 绝不命中自动档，也绝不退到只读——只读是另一个档位，
  用户想只读会显式选；挑不到可用档就明确不下发，并列出可选项、提醒它自己的默认档可能更宽松。
  回归 `TestPickApprovalValue`（codex/claude 两套取值）、`TestWorkspacePolicyLandedValue`、
  `TestWorkspacePolicyDeclinesAmbiguousAgent` 与前端头部落点用例。

### M-11 上下文交接（总结并新开）

- **动机**：ACP 没有上下文压缩/清除接口，水位数只能看；agent 会话真的满了只能重开，重开靠
  `session/load`，恢复不可靠的 agent 直接“上下文已重置”。
- **方案**：上下文弹层提供「总结并新开」——Friday 从**可见时间线**整理交接摘要（不调用模型，
  明确标注 agent 未上报的细节可能遗漏），在同一工作区/agent/主目录新开「· 续」会话；
  摘要存新线程的 `handoff` 字段，下一轮作为前置文本发给 agent，送达成功才清空；旧会话原样保留，
  手动选过的模型/审批等能力选择一并复制。
- **边界**：不是原地压缩，旧会话不会变小；摘要只含用户原话、agent 最终回答与系统备注，
  工具过程不在投影里，可能遗漏；水位低时也可手动用。
- **实施记录（2026-10-30）**：`Manager.Handoff`/`handoffSummary`、`Thread.Handoff` 下发与清理、
  admin `POST /api/acp/threads/:id/handoff`、上下文弹层按钮与「· 续」会话切换。回归
  `TestHandoffCreatesContinuationThread` / `TestHandoffRejectsEmptyThread` 与前端弹层用例。

### M-12 工作区项目记忆

- **动机**：长期记忆原本是全局用户 wiki，工作区说明只告诉 agent 目录清单；项目约定、验收口径
  和上一次做出的决定没有地方沉淀，换 agent 还要重新交代。
- **方案**：`memory.Page` 增加 `Scope`（空=全局用户记忆，工作区 id=项目记忆），全局读写默认
  只看全局页；工作区抽屉新增「项目记忆」标签，回答行可一键把结论填成草稿。`Manager.SetMemory`
  后，工作区首轮说明附带该工作区的项目记忆速览，并把摘要纳入注入指纹——记忆变了下一轮/重连
  才重新交代，不每轮刷屏。
- **边界**：不写用户仓库（默认不生成 `AGENTS.md`），不混进全局记忆；项目记忆只注入速览，
  细节仍需要用户或 agent 主动确认。
- **实施记录（2026-10-30）**：`internal/memory` scoped API、admin `workspaces/:id/memory` CRUD、
  `WorkspaceFilesDrawer` 项目记忆页、`WorkspaceView` 回答行「记到项目记忆」。回归
  `TestScopedMemoryIsolatedAndPersisted`、`TestWorkspaceBriefIncludesProjectMemory`、
  `TestAcpWorkspaceMemoryEndpoints` 与前端抽屉用例。

### M-13 验证面板（本地预览 + 检查命令）

- **动机**：agent 说「做完了」不等于验证过。Friday 里只能看文字，既没有页面预览入口，也没有
  跑测试/lint 的地方，用户得回终端；而这些命令本来就该走同一套审批与超时。
- **方案**：`internal/acp/verify.go` 记录一次验证运行（awaiting→running→passed/failed/
  timeout/killed/denied/error），执行前用 `approval.Queue.RequestCancelable` 强制挂一张
  「验证命令」审批卡（与 agent 权限卡同源，超时 5 分钟），批准后在会话主目录 `/bin/sh -c` 起进程，
  独立进程组 + `CommandContext`，停止/超时都杀整组。输出落临时文件只回读尾部（64 KB、400 行、
  去 ANSI），列表接口不带输出、选中那条走详情接口。建议命令按标志文件推断（package.json scripts +
  pnpm/yarn 判定、go.mod、Cargo.toml、pytest 标志、Makefile 兜底）。抽屉「验证」标签承载 UI：
  预览地址从时间线正则提取，也能手填（只允许 localhost/127.0.0.1），内嵌 iframe 不带
  `allow-same-origin`，预览页读不到 Friday 登录态，同时始终给「新窗口」出口。
- **边界**：不承诺沙箱——命令是用户自己填的一行 shell，Friday 只做毁灭性片段兜底黑名单 + 人工审批；
  单条会话同时只跑一个验证命令，记录保留 10 条且不落盘（进程重启后「在跑」没有意义）。
- **实施记录（2026-11-02）**：admin `threads/:id/checks[/:run][/stop]` 四接口，`RunsView`
  的待审批区自动出现「验证命令」卡。回归 `TestCheckRun*`/`TestCheckStop*`/`TestCheckDenied*`/
  `TestCheckTrimsLongOutput`/`TestSuggestChecksFromProjectMarkers` 与
  `TestAcpCheckEndpoints`、前端「验证标签」用例。

### M-14 并发排队可见与槽位记账

- **动机**：261030 真机验收里，`maxConcurrent=2` 被两条空闲连接占住，第三个会话发消息后
  线程状态还是 idle、运行看板也是空的——用户只能猜“它到底有没有在干活”。
- **方案**：等槽位期间把会话标成 `queued`（`List` 会列出，异常收尾时自动回落 idle），时间线的
  status 帧带上占用比「2/2」；`Stop` 在没有 ACP 会话可取消时撤掉的就是这次等待，运行页据此给
  「取消排队」。有人排队时，刚跑完的空闲连接不再守满 `IdleKillSeconds`，5 秒就让位。
- **边界**：排队不是队列保证——谁先抢到槽位看调度，只保证“看得见、能取消”；重启后遗留的
  queued 与 running 一样转成 interrupted，不假装还在等。
- **顺带修的坑**：槽位改成“按取到时的 channel 记账”（`Reload` 会换 `m.sem`），并把 spawn 失败
  路径上的阻塞式 `<-m.sem` 换成非阻塞归还——原来一旦计数被换表或重复回收打乱，就会死在握着
  `spawnMu` 的 goroutine 里，之后所有会话都起不了进程、只能重启，正是验收看到的“槽位没恢复”。
- **实施记录（2026-11-02）**：`StateQueued`、`acquireSlot`/`releaseSlot`、`armIdle` 让位、
  `Reload` 重新记账；回归 `TestSendQueuesWhenSlotsExhausted`、`TestStopCancelsQueuedTurn`、
  `TestReloadReaccountsConcurrencySlots`、`TestReleaseSlotNeverBlocks` 与运行页「排队中」卡片用例。

### M-15 超时预算分段与静默看门狗

- **动机**：261030 验收记的「opencode 建会话超时」，2026-11-02 用 `cmd/acp-probe` 实测拆开看是两件事：
  ① `session/new` 与 initialize 共用 10 秒握手预算，而 opencode 1.18 光回 `available_commands_update`
  （列全部 skill 表）就要 9.3 秒 —— 首启确实会撞线，表现成"建会话失败: context deadline exceeded"；
  ② 真正让那一轮 3 分钟没下文的，是 opencode 自己的模型调用挂住（它最终回 `-32603 Internal error:
  context deadline exceeded`，stderr 全空）—— 不是 Friday 的超时。另外 npx 型命令首次拉起要装包，
  10 秒握手基本必踩。
- **方案**：三段预算各自独立且都能按 agent 覆盖 —— 握手（默认 10s，`npx/pnpm/bunx/uvx` 这类
  启动器首次给 90s，成功起过一次后回落）、建会话（默认 60s）、单轮（300s）。撞线的文案点名
  "是哪一段、多久、去设置哪里调"。再加一条静默看门狗：本轮 30 秒内没有任何 update（文本/工具/
  用量都算）就投一条状态帧说清"谁、等了多久、可以做什么"；这一轮最终空手被中止时，把 agent 的
  stderr 尾巴附进中断说明。
- **边界**：看门狗只负责"别让人对着安静干等"，不改判定：静默不等于失败，仍按单轮超时/用户停止
  收尾；stderr 尾巴只是线索，agent 不写 stderr 时就没有（opencode 正是这种），所以还要靠
  「到终端跑一次」这句下一步。
- **实施记录（2026-11-02）**：`config.AcpConfig.SessionTimeoutSeconds` + 条目级
  `Handshake/SessionTimeoutSeconds` 覆盖、`handshakeBudget`/`dialBudget`、`turnSilenceNotice`
  与 `turnSink.quiet`（atomic，读方在另一条 goroutine）；`cmd/acp-probe` 顺手修了自己的报文形状
  （`mcpServers` 从 null 改成 `[]`，否则探到的不是 Friday 的请求）。回归
  `TestHandshakeBudgetCoversColdStart`、`TestSessionNewTimeoutNamesTheKnob`、
  `TestSilenceWatchdogNoticesQuietAgent`（+ 反例）、config 侧默认值/覆盖/写回用例。
- **后续调整（2026-11-03）**：**单轮 300s 上限整体移除**（用户拍板：一轮对话跑几个小时很正常，
  总时限反而在长任务中途切断；agent 侧自己的请求超时才该兜底）。`TurnTimeoutSeconds`/
  `AcpDefaultTurnSeconds`/`TurnSeconds` 删除，manager 的 turnCtx/warnTimer/脏连接宽限回收
  机制一并拆除；超时预算只剩握手 + 建会话两段，静默看门狗保留（仅提示不判定），
  中止只剩用户停止与客户端断开两种。

## 二、开工顺位

| 顺位 | 项 | 状态 |
|---|---|---|
| 1 | M-1 + M-2 | ✅ 已实施（2026-10-21） |
| 2 | M-8 | ✅ 已实施（2026-10-21） |
| 3 | M-5 | ✅ 已实施（2026-10-21，口径修正后） |
| 4 | M-3（①②③ 全量）+ M-4 | ✅ 已实施（2026-10-21，拍板：直接兼容官方 schema） |
| 5 | M-6 | ✅ 已实施（2026-10-29，同一 agent+目录复用连接） |
| 6 | M-9 + M-10 | ✅ 已实施（2026-10-30） |
| 7 | M-11 | ✅ 已实施（2026-10-30） |
| 8 | M-12 | ✅ 已实施（2026-10-30） |
| 9 | M-13 | ✅ 已实施（2026-11-02，验证面板） |
| 10 | M-14 | ✅ 已实施（2026-11-02，并发排队可见与槽位记账） |
| 11 | M-15 | ✅ 已实施（2026-11-02，超时预算分段与静默看门狗） |
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
