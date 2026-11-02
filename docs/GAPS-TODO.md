# 差距补齐 TODO（2026-10-30）

参照 ZCode / Qoder / WorkBuddy 的当前形态，按 Friday 的定位筛选：单管理员、本地优先的个人管家 + 跨 agent 宿主。目标不是做成 IDE，而是补齐“敢把电脑交给它”的信任层和“把活派出去还能收回来”的编排层。

开工顺序：**先修真机验收发现的 V-1…V-4**（都是“看得见/管得住”上的洞）；P0 已完，P0-4 等通道启用后再做；P1 从 9 起往后排，P2 按需。

## P0 先做（不做会不敢用）

- [x] **P0-1 工作区文件树 + 本轮变更清单 + diff**
  现状：ACP 时间线只有文字和工具行；文件预览抽屉只能点开单个路径，没有目录树，也没有“这一轮改了什么”。
  做到：工作区侧栏可浏览目录；每轮结束按 git diff/untracked 或文件指纹列出变更文件，逐文件看 diff；工具行能跳到对应文件。
  验收：agent 改 3 个文件，时间线出现“本轮 3 个文件”并可逐个看 diff；非 git 目录也能列变更（跳过 `.git`、`node_modules`），不静默漏文件。
  落点：`internal/acp`、`internal/admin/acp.go`、`web/src/components/WorkspaceView.vue`（可复用现有 files/preview）。
  **实施记录（2026-10-30）**：新增 `WorkspaceFilesDrawer`（文件树/预览、本轮 diff、还原入口）与时间线“本轮改动 N 个文件”摘要；后端 `changes.go` 生成变更与逐行 diff，接口 `GET /threads/:id/files`、`GET /threads/:id/changes`。

- [x] **P0-2 轮前快照 + 一键还原**
  现状：agent 直接改盘，Friday 不存快照；计划步骤也明确不做事务回滚。
  做到：每轮开始前做轻量快照（git 仓库用 `git stash create` 类无侵入快照 + 未跟踪文件备份；非 git 目录按文件指纹做带体积/数量上限的副本），轮末提供“还原本轮”；超限明确提示不可快照，不假装成功。
  验收：一轮改坏 2 个文件，点还原后内容恢复；快照失败时时间线有明确提示，且不影响本轮继续。
  依赖：P0-1 的变更清单。
  **实施记录（2026-10-30）**：`internal/acp/changes.go` 已落地 git/copy 双路径；快照每轮 20k 文件 / 100MB / 单文件 5MB 上限，保留最近 30 轮；还原前整轮预检，本轮后被改过/删除/重建的文件拒绝还原；暂存区状态不承诺恢复。

- [x] **P0-3 工作区级统一权限策略**
  现状：ACP 审批卡是否弹出取决于 agent 自己的审批模式；Friday 没有“只读 / 每次都问 / 自动”的统一档位，也没有沙箱，ACP 继承 HOME 登录态。
  做到：工作区设置里选三档；能映射到 agent `configOptions` 的就映射，映射不了就明说“该 agent 不支持，只能事后还原、不能事前拦截”，不假装有沙箱；放行/拒绝全部进审批轨迹。新工作区默认档位建议“每次都问”，需要你拍板。
  验收：同一个工作区换 codex/opencode，档位语义一致；agent 不支持时 UI 给出明确说明，而不是静默降级。
  落点：`internal/approval`、`internal/acp`、`web/src/components/WorkspaceView.vue`。
  **实施记录（2026-10-30）**：默认档位已拍板为 `ask`（每次都问）；工作区新增 `policy`，新建会话/重连时按关键词映射到 agent 的 approval_mode，映射不到写 `policyNote` 并在会话头部标“审批档位未下发”；手动 chip 选择优先。

- [ ] **P0-4 远程审批与运行状态（飞书/钉钉）**
  现状：审批卡、队列、工作区进度都只在网页；通道只是聊天入口。
  做到：通道内 `/status`（运行中/排队/待审批）、`/approve`、`/deny`、`/stop`；只读进度页用签名链接；操作限白名单用户。
  验收：手机飞书里能批准一条 ACP 审批，网页卡片同步消失；非白名单用户看不到也批不了。
  前提：对应通道已启用；没启用就明说“还没有配置该通道”。

- [x] **P0-5 全局运行看板**
  现状：ACP 会话、计划、目标、定时任务分别在工作区/自动化/设置里看，没有一个“现在在跑什么”的入口。
  做到：一屏列出所有进行中/排队/待审批/失败项（ACP 轮次、计划步骤、目标、任务），可跳转、暂停/停止；空状态区分“没有任务”和“未配置”。
  验收：同时跑 2 个 ACP 会话 + 1 个目标 + 1 个计划，看板全部列出且状态实时；从看板能停掉其中一项。
  **实施记录（2026-10-30）**：新增侧栏「运行」页与只读聚合接口 `GET /api/runs`（5 秒轮询），汇总 ACP 运行/中断会话、活跃计划、目标、近期任务、待审批与失败通知；动作复用原有 stop/pause/cancel/decide/retry 接口；ACP 会话卡片可深链回工作区；各管理器未装配时回空数组而非假数据。

- [x] **P0-6 ACP 上下文交接（总结并新开）**
  现状：ACP 没有压缩接口，Friday 只显示水位；`session/load` 恢复保真度看 agent，差的重开即“上下文已重置”。
  做到：水位到阈值时提示；一键用 Friday 的模型把时间线总结成交接摘要 + 工作区说明，开新会话接着干；旧会话保留可查。摘要里标明“可能遗漏 agent 未上报的细节”。
  验收：水位 80% 时出现入口；新会话首轮带上摘要，agent 能接着上一轮的任务；旧会话历史不丢。
  落点：`internal/acp`、`internal/agent`（摘要）、`web/src/components/WorkspaceView.vue`。
  **实施记录（2026-10-30）**：上下文弹层新增「总结并新开」；`Manager.Handoff` 用可见时间线整理摘要（不调用模型，明确标注可能遗漏 agent 未上报细节），在同工作区/agent/主目录建「· 续」会话并复制手动能力选择；摘要存 `Thread.Handoff`，下一轮作为前置文本下发，送达成功才清空；旧会话保留。回归 `TestHandoffCreatesContinuationThread` / `TestHandoffRejectsEmptyThread` 与前端弹层用例。

## P1（能力与体验）

- [x] **P1-7 工作区项目记忆**
  现状：记忆是全局用户 wiki；工作区说明只是每轮注入的目录清单，不沉淀决策。
  做到：记忆页可按工作区归属；会话里“记住这条”写入当前工作区；首轮把项目记忆摘要注入（可选导出 `AGENTS.md`，默认不写用户仓库）；跨 agent 共用。
  验收：在工作区 A 记住“这个仓用 pnpm”，新开 codex/opencode 会话都能看到；工作区 B 看不到。
  落点：`internal/memory`、`internal/tools/memory.go`、`internal/acp` 工作区说明。
  **实施记录（2026-10-30）**：`memory.Page` 增加 `Scope`，全局记忆与项目记忆分仓读写（`SaveScoped` / `PagesScoped` / `SearchScoped` / `DigestScoped`）；工作区文件抽屉新增「项目记忆」标签，回答行有「记到项目记忆」按钮预填草稿；`Manager.SetMemory` 后工作区说明附带项目速览，并把摘要纳入注入指纹，更新后下一轮/重连重新交代。默认不写 `AGENTS.md`，导出留作后续小项。

- [x] **P1-8 验证面板（预览 + 测试）**
  现状：agent 说“做完了”，Friday 里只能看文字；文件预览是只读单文件，没有页面预览/测试结果。
  做到：会话里填本地 URL 一键在浏览器打开（内嵌受 X-Frame-Options 限制时给打开按钮）；常用测试命令一键跑，显示退出码与输出尾部；命令执行走现有审批/超时。
  验收：agent 改完前端，点预览能看到页面；跑 `npm test` 失败时面板显示失败用例，不用去终端。
  落点：`internal/acp`（运行命令）、`web/src/components/WorkspaceView.vue`。
  **实施记录（2026-11-02）**：抽屉新增「验证」标签。预览地址由时间线正则提取（`localhost/127.0.0.1/0.0.0.0/[::1]`），可内嵌 iframe（不带 `allow-same-origin`，预览页读不到 Friday 登录态）或新窗口打开，手填地址只收本机、外站直接拒。检查命令按标志文件给建议（package.json scripts + pnpm/yarn 判定、go.mod、Cargo.toml、pytest 标志、Makefile 兜底），也可手填；`internal/acp/verify.go` 走 `approval.Queue.RequestCancelable` 强制挂「验证命令」审批卡（运行页可见可批），批准后 `/bin/sh -c` 在会话主目录执行，独立进程组、超时/停止都杀整组，输出落临时文件只回读尾部（64 KB / 400 行 / 去 ANSI，超出标 truncated），失败行标红。毁灭性片段本地兜底拒绝、同会话单飞、记录留 10 条且不落盘。接口 `GET/POST /threads/:id/checks`、`GET /threads/:id/checks/:run`、`POST .../stop`。回归 `TestCheck*`/`TestSuggestChecksFromProjectMarkers`、`TestAcpCheckEndpoints` 与前端「验证标签」用例；45679 演示实例实跑 `node verify.js` 走完审批→失败→输出尾部，深浅色截图各一张。

- [ ] **P1-9 模型路由、失败回退与低峰调度**
  现状：单活模型手切，没有按任务路由/回退；定时任务只有普通 cron，没有低峰概念。
  做到：按来源（主对话/计划/目标/摘要/心跳）指定模型；主模型限流/超时按顺序回退；目标/心跳可设低峰窗口，窗口外只排队不烧额度。
  验收：主模型返回 429 时自动切备用并在时间线标注；把目标设到 02:00-06:00，白天创建只排队。
  落点：`internal/config`、`internal/agent`、`internal/task`。

- [ ] **P1-10 主对话派活到 ACP 会话并汇总**
  现状：ACP 会话要手动建、手动发；主对话和外部 agent 是两条线。
  做到：主对话里指定工作区/agent 派一个任务，Friday 建会话、跟踪进度、结束后把摘要/结果/改动清单带回主对话；可停止。不新造 agent runtime，复用现有 ACP 会话。
  验收：一句“让 codex 在 proj-a 里修这个测试”，主对话收到完成摘要和变更清单，不用切页。
  依赖：P0-1 / P0-5。

- [ ] **P1-11 动态工作流最小闭环**
  现状：T0 引擎与脚本宿主已有，没有产品入口（见 `docs/WORKFLOW.md`）。
  做到：写作技能 → 预检 → 初次确认（阶段图 + `world.run` 命令 + 脚本原文）→ 扇出 → report 落盘 → 通知带回；先不做续跑 journal/dashboard。
  验收：写一个两阶段工作流，确认后跑完，通知里有报告；预检失败时不执行。
  依赖：`internal/workflow`、`wfhost`；与 P1-10 共用“派活/汇总”UI 更好。

- [ ] **P1-12 通用附件（不止图片）**
  现状：聊天/ACP 只能发图（≤5MB×4）；PDF/日志/代码文件只能让 agent 自己去读路径。
  做到：支持文本/PDF/zip 等附件；保存到数据目录并把路径注入消息/工作区说明，限制大小与数量；发送前可移除。
  验收：上传一份日志，agent 能直接读到内容/路径；超限明确拒绝，不静默丢。
  落点：`web/src/components/ChatComposer.vue`、`internal/admin` 上传、`internal/acp`。

## P2（生态与运维）

- [ ] **P2-13 技能可执行化 + hooks**
  现状：技能只有提示词，不能带脚本/工具；没有 hooks。
  做到：技能包可带脚本与工具声明，`allowed-tools` 继续只能收窄；提供 before/after tool hooks；先做本地包，不做市场。
  验收：一个技能能跑脚本并把结果返回对话；越权工具被拒。
  落点：`internal/skill`、`internal/agent`。

- [ ] **P2-14 连接器预设**
  现状：MCP OAuth 已具备（发现/动态注册/PKCE/loopback/refresh + 设置页授权），但接一台常见服务仍要手填 URL/鉴权。
  做到：Notion、GitHub、浏览器等常见连接器给预设（名称/URL/鉴权方式/工具白名单），一键添加，配置仍可手改。
  验收：从预设添加 Notion，点授权后工具可用，不用手写配置。
  落点：`internal/mcp`、`web/src/components/settings/McpSection.vue`。

- [ ] **P2-15 文档产物与产物面板**
  现状：只能出 Markdown/图片，docx/xlsx/pptx 靠外部工具；没有“本次产物”列表。
  做到：文档生成走技能/MCP，产物统一落到会话产物区，可预览/下载；不把二进制塞进时间线。
  验收：让 Friday 做一份周报 docx，产物区出现文件并可下载；删会话时提示产物是否保留（默认保留）。
  落点：`internal/tools`、`web/src/components/WorkspaceView.vue`、`FilePreviewDrawer.vue`。

- [ ] **P2-16 桌面自动更新与备份/诊断**
  现状：桌面端手动装 `.app`；备份靠拷数据目录；没有诊断包。
  做到：签名更新清单 + 应用内检查/下载/安装；一键导出数据备份（配置脱敏可选）；一键生成诊断包（日志/版本/最近错误，密钥打码）。
  验收：旧版本能检测到新版本并完成更新；备份可在新机器恢复；诊断包不含明文密钥。
  落点：`desktop/src-tauri`、`internal/admin`。

- [ ] **P2-17 会话搜索与命令面板**
  现状：左栏只按工作区/最近 30 条；没有全文搜索。
  做到：跨会话消息搜索（标题+正文，结果跳转到消息）；Cmd/Ctrl+K 命令面板（切页/新建会话/停本轮）。
  验收：搜一个两周前的关键词能定位到消息；Cmd+K 能完成常用跳转。
  落点：`internal/agent/pool.go`、`internal/admin`、`web/src`。

## 已有地基（不要重复排）

- MCP OAuth：`internal/mcp/oauth.go` 已实现 OAuth 2.1 发现、动态注册、PKCE、loopback 回调与刷新，设置页有“授权”入口；只差连接器预设。
- ACP 注册表：安装/升级/版本戳/离线缓存已完成（`docs/ACP-IMPROVEMENTS.md` M-3/M-4）。
- 动态工作流：T0 引擎与 `wfhost` 预检已完成，缺产品入口（P1-11）。
- 记忆：wiki 页 + 目录优先召回 + 会话速览 + 工作区归属（P1-7）已落地；缺的是把项目记忆导出成
  `AGENTS.md` 的可选出口。
- 验证面板：本地预览与检查命令已落地（P1-8），命令走审批队列并出现在运行看板；缺沙箱（明确不做）。

## 明确不做（本轮）

团队协作/多租户/云同步、IDE 插件、原生移动端、语音、Docker 沙箱、把 Friday 改成自带 IDE。理由：单管理员、本地优先、跨 agent 宿主；这些会稀释定位。

## 横切收尾（每完成一批过一遍）

- [x] 真机验收（2026-10-30，包 `dev-05dda6b-dirty.2610301842`）：codex 0.163 全项通过；opencode 1.19.4 建会话超时未过，细节见下节
- [ ] `feat/workspace-trust` 整体验收后合回 main

## 真机验收记录（2026-10-30 18:45）

包：`build-release.sh --bundles app` → 替换 `/Applications/Friday.app`（旧包在 `/tmp/Friday.prev.app`）。工作区「P0 验收」含两个目录：`/tmp/p0-accept/repo`（git 仓）、`/tmp/p0-accept/plain`（非 git）。

**通过**

- P0-1：codex 一轮建 3 个文件，done 帧带 `changes`（3 added + 逐文件 unified diff 正确）；`/files` 目录树不含 `.git`；非 git 目录走 `vcs=copy`，added/modified 与 diff 同样正确。
- P0-2：`changes/revert` 把本轮改的 2 个文件恢复原样，且不误伤上一轮的未跟踪文件；copy 路径还原正确（删新增 + 恢复被改）；人工在本轮后动过 README 时拒绝还原并点名该文件；`git stash create` 失败的轮次自动退到目录副本、写进 `changes.note`，本轮照常结束。
- P0-3：新建工作区默认 `ask`；`readonly` → codex `approval_mode=read-only` 且写入真被挡住；`auto` → 落到 `agent`（Auto review）。
- P0-5：两个会话同跑时 `/api/runs` 列出 2 条（标题/agent/工作区/state 齐全），停止后 6 秒内清零。
- P0-6：新开「… · 续」，摘要含三轮问答并标「可能遗漏」；首轮下发后 agent 能准确复述此前改过的文件；送达后 `handoff` 清空，旧会话保留。

**未通过 / 待修**

- P0-3 codex 的「每次都问」下发不了：codex 只报 read-only / workspace-write / agent / agent-full-access，`pickApprovalValue` 命中不到 ask 关键词 → 有 policyNote 明说，但实际跑在 Auto review（等于自动放行）。「不支持就明说」达标，「换 agent 语义一致」在 codex 上不成立。
- opencode 建会话超时：`session/new` 返回 `context deadline exceeded`，重启后单跑仍复现 → 跨 agent 一致性这一条没法验。同类：codex 首次走 npx 冷启动时握手也超时（装包 > 10s），缓存热了才正常，第一次用 npx 型 agent 基本必踩。
- 并发槽位被空闲进程占住：`maxConcurrent=2` 时空闲进程仍占槽，新会话排队；排队期间线程 state 还是 idle、看板 acp 为空 —— 用户看到的是「发了消息没反应」。opencode 失败那轮之后槽位疑似未回收，重启才恢复。
- 自动档落到 Auto review 而非 Full access，与「全自动」的直觉有落差，UI 最好写清实际落到哪一档。

**未覆盖**：快照超限（20k 文件 / 100MB）提示、目标与计划在看板的条目、P0-4、P1-7 单独复验（前端产物已含「项目记忆」「验证」入口文案，P1-8 的「验证」标签仍缺）。

**环境侧改动（可回滚）**：`~/.npm/_npx/4877722a062902ce` 因并发装包损坏，已改名隔离后重装；工作区「P0 验收」与其会话留在软件里供复核，验完可删。

## 真机验收发现的缺陷（待修，优先级高于新特性）

- [x] **V-1 codex 上「每次都问」落不下去**
  现象：codex 只报 `read-only / workspace-write / agent / agent-full-access`，`pickApprovalValue`
  命中不到 ask 关键词 → 有 `policyNote` 明说，但进程实际跑在 Auto review（等于自动放行）。
  影响：用户拍板的默认档位在 codex 上名不副实，「不敢放手」这条没达成。
  **实施记录（2026-11-02）**：映射改成分层退让（`approvalTiers`/`pickApproval`）——先找语义等价档，
  找不到只朝“同样能干活但更保守”的方向退一档（ask→可写受限，auto→自动但不免审批），并记下发给
  agent 的实际取值。红线：ask 绝不命中自动档、也绝不悄悄退成只读；挑不到可用档就明确不下发，
  列出可选项并提醒它自己的默认档可能更宽松。回归 `TestPickApprovalValue`（codex/claude 两套取值）、
  `TestWorkspacePolicyLandedValue`、`TestWorkspacePolicyDeclinesAmbiguousAgent`。
- [x] **V-2 opencode 建会话超时**
  现象：`session/new` 返回 `context deadline exceeded`，重启后单跑仍复现；同类：npx 型 agent 首次
  冷启动（装包 > 10s）握手也超时，缓存热了才正常。影响：跨 agent 一致性没法验，第一次用必踩。
  **定位与实施记录（2026-11-02，用 `cmd/acp-probe` 实测拆开）**：两件事被记成了一条。
  ① 建会话与 initialize 共用 10 秒握手预算，而 opencode 1.18 光回 `available_commands_update`
  （列全部 skill 表）就要 9.3 秒，首启确实撞线 → 已把建会话独立成默认 60 秒、可按 agent 覆盖，
  npx 这类启动器首次握手自动放宽到 90 秒、起过一次后回落；② 那一轮 3 分钟没下文的真凶是
  opencode 自己的模型调用挂住（它最终回 `-32603 Internal error: context deadline exceeded`，
  stderr 全空），不是 Friday 超时 → 加静默看门狗：30 秒没有任何 update 就投一条状态帧说清
  「谁、等了多久、可以做什么」，空手被中止时把 agent stderr 尾巴带进中断说明。
  顺手修了探针自己的报文形状（`mcpServers` null → `[]`，否则探到的不是 Friday 的请求）。
  真机复跑：opencode 在 30 秒处弹出回音提示（原来是一片安静）。回归
  `TestHandshakeBudgetCoversColdStart`、`TestSessionNewTimeoutNamesTheKnob`、
  `TestSilenceWatchdog*` 与 config 默认值/覆盖/写回用例。
  **仍未解决（不属 Friday）**：这台机器上的 opencode 模型调用本身不可用，要它干活得先在终端
  把它跑通（`opencode` 已登录 opencode-go，但请求超时）。
- [x] **V-3 空闲进程占住并发槽 → 排队不可见**
  现象：`maxConcurrent=2` 时空闲进程仍占槽，新会话排队；排队期间 thread state 还是 idle、运行看板
  的 acp 为空，用户看到「发了消息没反应」；opencode 失败那轮之后槽位疑似未回收，重启才恢复。
  **实施记录（2026-11-02）**：等槽位期间会话标 `queued`（头部/列表/运行页都列，异常收尾自动回落
  idle），时间线 status 帧带占用比「2/2」；`Stop` 在无会话可取消时撤掉的就是这次等待，运行页给
  「取消排队」；有人排队时空闲连接 5 秒让位而不是守满 `IdleKillSeconds`。另修了真正的“槽位没恢复”
  根因：spawn 失败路径上的阻塞式 `<-m.sem` 在计数被 `Reload` 换表/重复回收打乱后会死在握着
  `spawnMu` 的 goroutine 里（之后所有会话都起不了进程）——改成按取到时的 channel 记账 + 非阻塞归还。
  回归 `TestSendQueuesWhenSlotsExhausted`、`TestStopCancelsQueuedTurn`、
  `TestReloadReaccountsConcurrencySlots`、`TestReleaseSlotNeverBlocks` 与运行页「排队中」用例。
- [x] **V-4 自动档的真实落点要说清**
  现象：`auto` 落到 Auto review 而不是 Full access，与「全自动」的直觉有落差。
  **实施记录（2026-11-02）**：与 V-1 同一处改动解决——`policyValue` / `policyValueName` /
  `policyFallback` 记实际落点，会话头部标「审批档位实际是 <显示名>」，`policyNote` 解释差别
  （“仍可能向你请求审批，不是完全放行”）；编辑工作区弹窗的档位说明同步。
- [ ] **V-5 验收未覆盖项**：快照超限（2 万文件 / 100MB）的降级提示、看板里的目标与计划条目、
  P1-7 项目记忆真机复验、P1-8 验证面板真机复验（当时包里还没有）。
- [ ] **V-6 环境侧遗留清理**：软件里的工作区「P0 验收」及其会话、`/tmp/p0-accept`、`/tmp/Friday.prev.app`。

## 备注

- P0 都不依赖新模型能力，先把“看得见、退得回、管得住”补齐。
- 每项落地时同步 `USAGE.md` 与对应 `docs/`，changelog 按提交日期记录。
- 竞品事实来源与时效见本轮分析；产品迭代快，排期前再核对一次对手现状。
