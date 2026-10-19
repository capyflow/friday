# 动态工作流（ZCode 式代码编排）

**状态**：2026-10-19 定案的设计方案；**T0 竖切已在 `explore/dynamic-workflow` 交付**
（宿主 `wfhost/` + Go 侧 `internal/workflow/`：check 预检、run 跨界协议与进程监督、Executor 与测试），
T1–T7 未开工。四条决策已同步记入 `docs/ROADMAP.md` 第七节。
本文是实施依据；实现若与本文不一致，改动要在 `changelog/` 里说明并同步回本文。

对标对象是智谱 ZCode 的「动态工作流」：主 agent 把用户意图翻译成一段 TypeScript 编排脚本，脚本先做
**编译期类型检查**，再进沙箱执行，由若干子代理协作完成复杂任务，跑完可存成文件复用。
本机可直接参照两份真实脚本：`~/.zcode/workflows/code-review-pipeline.dwf.ts`、
`~/.zcode/workflows/photos-core-release.dwf.ts`；完整 facade 契约在 ZCode 的
`dynamic-workflow` bundled skill 里（`/Applications/ZCode.app/Contents/Resources/glm/packages/bundled-skills/skills/dynamic-workflows/SKILL.md`）。

## 一、交付形态

用户说「用工作流做 X」→ 小五写出一段 TS 脚本 → node 侧 tsc + lint 预检（**有诊断就什么都没跑**）
→ 确认卡展示阶段图 / 命令全集 / 脚本原文 → 批准后后台运行，子代理与命令**都由 Go 履约**
→ `report` 即时落盘 → 完成通知带回 `WorkflowReport` → 可存成 `<name>.wf.ts` 复用。

一期不做：`hole()`、`AmendWorkflow` 缓存重放、artifact/dashboard、每步 journal 续跑、
子代理升级提问、`EvalWorkflowSnippet`。第 14 节是二期候选。

## 二、四条定案与被否方案

| 议题 | 定案 | 被否与原因 |
|---|---|---|
| 脚本运行时 | **引擎全在 Go，node 子进程只做脚本沙箱**（含 tsc 全量类型检查） | `goja` 进程内：没有编译期诊断、无自然栈/内存上限、`async function*` 不支持（见下方实测）；声明式 YAML/JSON：表达不出运行时决定的拓扑 |
| 一期范围 | 最小闭环（第一节那条链路） | 不含续跑 / artifact / hole |
| 编写契约 | 一份 friday 版精简 `SKILL.md`，authoring 入口在技能未加载时拒绝运行 | 完整三件套（patterns/examples）投入大且一期用不到；只塞工具描述则拓扑与复核质量全靠即兴 |
| 用户确认 | **只做初次确认**：阶段图 + 命令字面量全集 + 脚本原文一次整批批准，走 `plans` 那条独立通道，无超时 | 复用 `approval.Queue`（`acceptEdits/fullAccess` 档会静默放行且 5 分钟超时，见 `internal/admin/plans.go` 头部既有决定）；每条命令逐次审批（命令集本就冻结在确认时） |

### goja 实测记录（2026-10-19，goja v0.0.0-20261018 / esbuild v0.28.2 / node v23.11.0）

成立的：12 路 `Promise.all` 扇出（每路 400ms）墙钟 401ms，真并发；`for await` 消费 Go 侧喂的异步迭代器可用；
未监听的 reject 能被 `SetPromiseRejectionTracker` 捕获；`Interrupt` 可打断死循环；
`process/require/Buffer/Intl/structuredClone/setImmediate` 全部不存在（边界确实硬）。

否决它的：`SetMaxCallStackSize` 抛的是 **JS `try/catch` 抓不住的错误**，且尾递归（goja 做了 TCO）与深递归
默认都不自然终止——脚本跑飞会掀掉整个 run 而不是变成可处理异常；进程内无法设堆上限（1M 个对象让 RSS
从 7MB 涨到 63.5MB）；`async function*` 是 SyntaxError；裸 runtime 没有 `setTimeout/queueMicrotask` 需宿主注入垫片；
1.6MB/56k 行的文本处理 goja 3697ms vs node 17ms。

**关于类型检查的关键事实**：esbuild 的 Go API 转译很快（600 行 TS 2ms、能出 sourcemap），但它**只转译不检查**——
对含 `const y: number = "not a number"`、`xs[99].where` 越界、`fetch(...)`、`process.env` 的脚本 0 error 0 warning，
`enum/namespace/参数属性/top-level await` 照转。`microsoft/typescript-go`（tsgo）当时拉取超时，
**「Go 内做全量类型检查」至今未验证，不要当作已知可行**。

## 三、架构分层与三条不变量

```
Web / IM 入口 ──SSE(一帧) + 轮询── internal/workflow（引擎：生命周期·并发闸门·权限·记账·落盘）
                                       │ 换行帧 JSON-RPC（每个请求带 runID + seq）
                             node 子进程 wf-host.js（vm 沙箱 + tsc + lint + facade 注入）
                                       │ 反向调用：agent.ask / world.run / files.* / git.*
                                   Go：Brain.AskWith · Checker.Check · usage(byRun) · trace · PublishStep
```

1. **脚本碰不到机器**：node 里只有 facade 注入的全局，`fs/process/fetch` 不存在；每个跨界调用都回到 Go。
2. **每个跨界调用都被记账与把关**：带 `runID` 的 `ask` / `world.run` 一律过 `Checker.Check`
   （`internal/agent/permission.go:55`）+ `usage.Call{Run:"wf:<id>", Origin:OriginWorkflow}`
   （`internal/agent/pool.go:71`）+ `trace` + `PublishStep`（`internal/agent/events.go:53`）。
   **工作流路径内禁止 `Registry.Call` 裸调**——它会绕过权限层与 Hooks（现例见 `internal/admin/admin.go:636` 的用法，不可照搬）。
3. **node 进程可牺牲**：崩溃或超时 → run 记 `errored`，已落盘的 `report` 仍可读；不做进程内恢复。

## 四、facade 子集（一期）

| 提供 | 不提供 |
|---|---|
| `agent(name, persona)` / `.ask<T>()`、`phase()`、`world.run(cmd, args, {timeoutMs})`、`files.{glob,read,grep}`、`git.{changedFiles,diff,status,log}`、`args`、`log`、`report(item)`、`channel<T>()`、`future()` | `hole()`、`artifact.*`（含 dashboard 声明）、`model()` 与 per-agent 模型路由、升级提问 |

脚本禁用：`declare/export/import` 语句；`process/require/global/__dirname/fetch/WebSocket/Buffer`；
`Date.now`/`new Date`/`Math.random`（决定论与可复放的前提）。

保留的三条 ZCode 语义（它们是脚本质量的来源，不是风格偏好）：**阶段必须覆盖全脚本**、
**`world.run` 非零退出码是值不是异常**、**每条会被当事实用的结论要独立复核，复核不过保留并标 `unconfirmed`**。

## 五、预检：tsc + 自写 lint（都在 node 侧）

- **tsc 层**（`wfhost/lib/analyze.mjs`）：TypeScript 5.9 Compiler API，`strict` + `noUncheckedIndexedAccess: false`，
  `lib: es2022`（因此 `fetch/process/require` 之类"Cannot find name"天然成为诊断，无需单独的禁用标识符规则）。
  脚本被编译在 `(async () => { … })()` 体内，于是 `export/import/declare` 自动非法、顶层 `await`/`return` 合法；
  包装前缀写在同一行，**诊断行号与原脚本一致**（仅第 1 行列号回补前缀长度）。
- **lint 层**（同文件，走 TS AST；这是 node 依赖成为承重的原因）——已实现的规则：
  - R1 `world.run` 第一参数必须是字符串字面量，并收集**命令全集**供确认卡整批批准；
  - R2 每个 `phase` 至少含一个 `ask` 或 `world.run`；R3 `ask`/`world.run` 必须落在某个 phase 窗口内；
    阶段窗口 = 该标记到**下一个标记**为止（词法近似的 lexical stamping，见第十四节的并发阶段限制）；
  - R4 `agent()` 的字面量名不得重复（计算名在运行期才发现冲突，一期由 run 的失败兜底）；
  - R5 顶层必须有一个带值的 `return`（形状由 `WorkflowReport` 约定，一期不做结构断言）；
  - R7 `ask<T>` 的类型参数必须能合成 JSON Schema——拒绝泛型 interface、tuple、enum、Date、自引用；
    JSDoc 注释合成成字段的 `description`（`wfhost/lib/schema.mjs`）；
  - R8 决定论：禁 `Date.now`/`new Date`/`Math.random`/`eval`/`setTimeout`（运行期同样拦，
    沙箱里 `Date` 与 `Math.random` 是抛错替身，`codeGeneration.strings:false` 让 `eval`/`new Function` 不可能）；
  - R9 一期未提供的 facade 成员（`hole`/`artifact`/`model`）给出明确提示；
  - R11 `args.<name>` 必须在元数据里声明过；R12 元数据块本身合法且含 `description`/`whenToUse`。
- **有诊断即终止**：不进确认、不产生任何副作用；且此时**阶段图/命令集/schema 一律不给出**（`OK ⇒ 派生数据可信`）。
  两条都有测试覆盖（`internal/workflow/workflow_test.go`）。

## 六、wf-host 与 RPC 协议

宿主包在 `wfhost/`（private ESM 包，唯一依赖 `typescript`），入口 `wf-host.mjs`，模块在 `wfhost/lib/`：
`analyze.mjs`（Program + lint + 改写位置）、`schema.mjs`（类型→JSON Schema）、`meta.mjs`（元数据与入参校验）、
`check.mjs`、`run.mjs`（沙箱与 RPC）。两个子命令：

- `wf-host check <file>` → 一行 JSON `{ok, meta, phases[], commands[], agents[], askSites[], diagnostics[], typescript}`；
  退出码 0 = 无诊断，10 = 有诊断，其余 = 宿主自身失败。
- `wf-host run <file> [--args '<json>'] [--args-file <path>]` → `vm` 沙箱执行。
  执行前重新做一次静态分析，把 `obj.ask<T>(x)` 按 AST 位置改写成 `obj.ask(x, __s(i))`，
  这样运行期才拿得到被擦除的类型对应的 schema（类型参数挂在 CallExpression 上，
  不能用"找第一个左括号"的字符串办法——`agent(...).ask<T>(x)` 的第一个括号属于内层调用）。

帧是**扁平的单结构**（`internal/workflow/frame.go` 的 `Frame`），换行分隔，上限 32MB：

```
host→Go  {"id":N,"op":"ask|world.run|files.glob|files.read|files.grep|git.changedFiles|git.diff|git.status|git.log",…}
host→Go  {"op":"event","kind":"phase|log|report",…}
Go→host  {"id":N,"result":…} 或 {"id":N,"error":"…"}
Go→host  {"op":"cancel"}（宿主也把自己 stdin 关闭视为取消）
```

Go 侧 `Run` 对每个在途请求**单独起 goroutine** 应答——串行化会静默地把脚本里的 `Promise.all` 退化成队列。

进程与资源：`--max-old-space-size` 由 Go 传入（这正是选 node 而不是进程内解释器能拿到的硬兜底）；
`exec.CommandContext` + `RunOpts.Timeout`；ctx 结束时看门狗直接 kill，让 stdout 关闭、读取循环得以退出。
宿主目录解析顺序：显式配置 → cwd 逐级向上找 `wfhost/wf-host.mjs` → 可执行文件同级/上一级。
命令与输出的上限沿用框架 exec 约定（stdout/stderr 各 100KB、默认 5min、绝对上限 30min，超限即丢弃尾部并标注）。

## 七、子代理履约

- 每个 agent 名 → 独立会话 key `wf-<runID>-<slug>`：`SessionPool` 的 per-key 锁
  **天然给出**「同名 agent 多次 ask 串行（校准）、不同 agent 并行（独立）」这套语义，不需要自己实现。
- persona 用 `WithInlineSkill()` 注入，抄 `internal/plan/planner.go` 的 `__planning__` 范式，不进用户技能库。
- ctx 上加 `WithRun("wf:<runID>")` 与新常量 `OriginWorkflow = "workflow"`（与 `internal/agent/pool.go:54` 那组 Origin 同处）。
- `ask<T>()` 的 schema：一期用 host 侧**最小 interface 解析器**（interface/type alias、基本类型、字面量联合、
  可选属性、数组、嵌套 interface，**JSDoc → description**）。Go 侧执行 `WithJSONMode()` +
  `plan/parse.go` 的 `ExtractJSON` + 一次纠正重试 + 按 schema 校验必填与枚举，失败 throw 回脚本（可被 `catch`）。
  完整 TypeChecker 合成留到二期。
- 全局并发闸门 `sem chan struct{}`，默认 `maxConcurrent: 4`；同时打开 `vxagent.Options{ParallelTools: true}`
  （框架字段见 `vortexagent/agent/agent.go:35`，其 `examples/async-agent` 就是这么配的），
  否则扇出之后同轮多工具仍串行成瓶颈。
- 框架已有的 `TaskHub`（`vortexagent/agent/taskhub.go:95`）与 `SubagentTool`（`agent/subagent.go:45`）一期**不接线**：
  扇出由脚本控制、Go 侧执行，比让模型用 `task_start` 工具更可控。二期若要「对话里直接委派」再引入。
- **Executor 是脚本副作用的唯一出口**（`internal/workflow/exec.go`）：`Local` 实现 `world.run`（固定 argv、不经 shell、
  `LookPath` 解析、非零退出码作为值返回）、`files.{glob,read,grep}`（glob 用 doublestar 支持 `**`，命中超限**拒绝而不是截断**；
  路径拒绝绝对路径、`..` 与符号链接）、`git.{changedFiles,diff,status,log}`（值以 `-` 开头一律拒绝，免得变成 git 选项）。
  两处与 ZCode 不同要写进编写契约：grep 是 Go 的 RE2（无后向引用与 lookbehind，ZCode 用 ripgrep），
  `files.read` 上限 1MB 而非按行截断。
  命令红线目前走 `GuardDenyPatterns`（复用 `approval.DenyPatterns()` 与框架 `vxexec.CommandDenyMatcher`，与权限层同一套语义）；
  **T4 要换成完整 Checker**（allow/confirm/记住），那时才谈"逐条命令审批"。
  子代理那一侧留 `Local.Asker` 注入点，T0 用它接假实现，T3 换成走 `Brain.AskWith` 的真实实现。

## 八、生命周期、落盘与重启行为

`compiling → awaiting_confirm → running → completed | errored | stopped | rejected`

- 确认卡数据：阶段节点序列、去重后的命令全集、脚本原文（默认折叠）、并发上限、子代理数、`args` 解析结果。
- 落盘：`<dataDir>/workflows/<name>.wf.ts`，头部元数据是**行注释块**（`// friday-workflow` 起，第一个非 `//` 行结束）：
  `description` / `whenToUse` / `args` 下每个参数的 `type`（string|number|boolean|json）/ `description` / `required` / `default`。
  没有沿用 `.dwf.ts` 的块注释，因为 `*/` 会提前终止注释——glob 默认值 `"**/*.go"` 里就有它，
  而 TS 层面救不回来（实测会把整段 YAML 变成语法错误）。从 `.dwf.ts` 迁移只需给每行加 `// `。
  run 状态与 `report` 项在 `<dataDir>/workflow-runs/<id>.json`。
  store 抄 `internal/plan/store.go:35` 的原子写 + 终态淘汰 + **损坏即拒绝写回**。
- **进程重启：`running → stopped`，绝不自动重跑**（对齐 `docs/STABILITY.md` 里「步骤不做事务回滚、重启不自动续跑」那条不变量）。
- `report` 每条即时 append——这是 ZCode「salvage by report」的保留部分，run 挂了前序 findings 仍可读。

## 九、入口、路由与前端

- 文本入口 `/workflow <名字|新需求> [参数]`，解析点放在 `internal/agent/brain.go` 的 `resolveSkill` **之前**
  （全仓唯一检查用户消息文本的地方）。IM 通道一期只给「已启动」与完成通知，不做卡片交互。
- Web：`req.Mode == "workflow"` 分支，形状对齐 `internal/web/chat.go:302`（goal）与 `:325`（plan）；
  SSE 增一帧 `{"workflow":{id,state,phases,...}}`；**进度沿用前端轮询**，不新建第二条推送通道
  （尊重 `changelog/261014.md` 里「多步运行走轮询而非第二通道」的既有决定）。
- `internal/admin/workflows.go`：list / get / confirm / cancel / runs，注册形状照 `internal/admin/plans.go`。
- 设置页新增「工作流」节：enabled、maxConcurrent、perRunTokens、node 探测状态（只读）。
- 新工具（`run_workflow` / `list_workflows` / `cancel_workflow`）是否进 `passThroughTools`
  要显式决定并补 guard 测试（`internal/agent/permission.go:38`，漏了会静默落进 confirm 审批、网页狂弹卡）。

## 十、配置

```yaml
workflow:
  enabled: true
  nodePath: ""            # 空=用 PATH 里的 node；桌面端后续换 sidecar 固定版本
  maxConcurrent: 4
  perRunTokens: 200000    # 与 perPlanTokens 同层（internal/config/config.go:384）
  runTimeoutSec: 1800
  keepRuns: 50
```

预算沿用既有语义：**只在步边界判、只停主动型**（`internal/cost/guard.go:172`），
超线先把结论写进 `report` 再收尾，不硬取消用户发起的 run。

## 十一、任务拆分（≈14 人日，建议先竖切）

| | 内容 | 估 |
|---|---|---|
| **T0 竖切 ✅ 已交付** | facade `.d.ts` + `wf-host check/run` + Go 帧编解码与进程监督 + 端到端跑通（fixture 里含两路子代理扇出） | 1.5d |
| T1 | lint 七条规则 + 确认卡数据 | 1.5d |
| T2 | `internal/workflow`：def / store / compile / run + 重启与 node 缺失降级 | 2d |
| T3 | 子代理履约 + 最小 schema 合成与校验 + 记账 | 2d |
| T4 | 权限接线：`world.run` 过 Checker、命令集静态枚举、`passThroughTools` 决定 | 1d |
| T5 | `/workflow` 入口 + admin 路由 + SSE 帧 + 前端卡与轮询 | 2d |
| T6 | friday 版 `SKILL.md`（编写契约）+ 早间简报 / 月账单 / 备份校验三个样例 | 1.5d |
| T7 | 真模型端到端联调 + USAGE/changelog + 测试补齐 | 2d |

建议在 `explore/dynamic-workflow` 分支做完 T0–T4 再合回：T0 的作用是把跨界协议与进程监督这两处最容易返工的地方先钉死。

## 十二、验收判据

1. 故意写错类型 / 用 `fetch` / 把命令名拼进变量 → 只有诊断，**零副作用**。
2. 确认卡的阶段节点数 = `phase` 数；命令全集逐条去重列出；脚本可展开原文。
3. 6 路扇出的评审流程：墙钟 ≈ 最慢单路（对齐第二节实测的 12 路 401ms），token 全落 `wf:<id>` 桶。
4. 每条 finding 带 `verified|unconfirmed` 与非空 `evidence`；`notCovered` 能说明为何没查。
5. kill 掉 node → run 记 `errored`，之前的 `report` 仍读得到。
6. 重启 friday → `running` 变 `stopped` 且没有自动重跑。
7. 环境无 node → 功能关闭，`/workflow` 回一句人话，不 panic。
8. `permissions.mode=fullAccess` 时确认卡**仍然拦着**（第 2 节那条决策的关键安全点，必须有测试）。

## 十三、风险与对策

| 风险 | 对策 |
|---|---|
| node 版本漂移 / 缺失 | 启动即探测，缺失则 `enabled=false` + 人话提示；桌面端后续用 Tauri sidecar 固定版本 |
| **子代理假通过**（一期无升级提问） | SKILL.md 硬规定「门过不去就返回 `unconfirmed` 并说明受阻，禁止声称通过」+ `evidence` 必填 + 专门测一条「不可能的门」 |
| 成本失控 | `maxConcurrent: 4` + `perRunTokens` + 确认卡显示并发与阶段数 |
| 权限旁路 | 工作流路径内禁 `Registry.Call`；`world.run` 必过 Checker；`passThroughTools` 显式决定并补 guard 测试 |
| 脚本做大文本处理 | 契约写死「传路径不传内容」，解析交给子代理或 Go（实测大文本在解释器里贵一个量级） |
| schema 合成踩进类型系统深坑 | 一期最小解析器 + R7 显式拒绝不支持形态 |

## 十四、一期不做（二期候选）

`hole()` 尾巴洞（跑到一半才写后面的流程）、`EvalWorkflowSnippet` 预演固定逻辑、
`AmendWorkflow` 的「按子代理名 + ask 指令逐字节匹配」缓存重放、`artifact.*` 与 dashboard、
每步 journal 与 stopped 续跑、per-agent 模型路由、接 `TaskHub`/`SubagentTool` 做对话内直接委派。

实施中暴露的两处一期限制，二期要一并解决：

- **阶段归属是文档顺序的窗口切分**：并发阶段（各自 `future` 里的 `phase`）的步子会落进相邻阶段窗口，
  所以 R2「阶段里必须有步子」只在顺序流程上可靠；要按 `future` 的调用树归属才准。
- **`agent()` 的运行期重名**（模板串拼出来的同名子代理）静态查不住，只有 run 失败才暴露；
  编写契约要明确要求扇出时用 `agent(\`评审员·${file}\`)` 这种带 item 标识的名字。
