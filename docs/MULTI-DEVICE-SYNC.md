# Friday 双端实时同步设计（桌面端 ⇄ 手机端）

> 状态：**已实现**（2026-11-11 设计 → 当日落地）。本文是设计与实现的依据。
> 配套接口契约见 [EVENTS-API.md](EVENTS-API.md)。
> 关联：[desktop/README.md](../desktop/README.md)（桌面端手册）、[docs/DESKTOP.md](DESKTOP.md)（桌面端架构）、
> [docs/ROADMAP.md](ROADMAP.md)、[internal/web/chat.go](../internal/web/chat.go)、[internal/admin/notifyws.go](../internal/admin/notifyws.go)

## 一、要解决什么

**目标（用户原话）**：*「我在移动端询问 agent 的时候，响应不会同步到桌面端，我想要两端同步」*——
即 **agent 的回复在两端同时可见**，不是「两端各看各的」。

拆成两个可验收的现象：

| 现象 | 今天的表现 |
|------|-----------|
| A. 回复双端可见 | 移动端问完，桌面端页面纹丝不动，必须手动刷新才看得到 |
| B. 两端同时使用 | 桌面端正在生成时移动端提问，移动端卡在「思考中」永不返回；且一端按「停止」可能停错对象 |

---

## 二、根因诊断（已定位到具体代码）

### 2.1 数据本来就是共享的，缺的是「推送」

这是最重要的一条认知修正：**桌面端和手机端连的是同一个内核进程**。

- 桌面端：Tauri 外壳拉起 Go sidecar 内核（或 macOS 上由 launchd 常驻，见 `docs/DESKTOP.md`「二·五」），
  数据目录 `~/Library/Application Support/Friday`。
- 手机端：扫码配对换 JWT（`internal/admin/pair.go`），走**同一个内核**的局域网 TLS 门
  （`admin.tls`，默认 45679 端口），连的是同一份 `session.json`、同一个 `SessionPool`。

所以移动端问完，**消息已经在内核里了**。桌面端看不到，纯粹是因为它不主动去读：

- `web/src/components/ChatView.vue:790` —— `loadHistory()` **只在 `bootstrap()` 里调一次**，
  即组件挂载时。之后没有任何刷新机制。
- 工作区 agent 同理：`web/src/components/WorkspaceView.vue:547` 的 `GET /api/acp/threads/:id`
  只在打开 / 切换线程时拉一次快照。

而**唯一存在的长连接是通知通道**：`internal/admin/notifyws.go` 的 `/api/notifications/ws` 只推
**通知中心**的事件（notice 帧），**不推会话事件**。桌面端甚至还没用它，仍在 5 秒轮询
（`web/src/notifications.js:136`）。

### 2.2 流式是「请求-响应」耦合的，没法直接拿去同步

`internal/web/chat.go:556`：

```go
if _, err := fmt.Fprintf(gin.Writer, "data: %s\n\n", b); err != nil {
```

SSE 帧直接写进**发起请求的那条 HTTP 连接的 writer**，发起端独占。
别的客户端没有任何通道能收到这些字节——**没有事件总线，只有点对点的连接**。

ACP 那条链路同理：`internal/admin/acp.go:739` 的 `handleAcpChat` 也是一次性 SSE，帧写给发起方。

### 2.3 现象 B 的两个独立缺陷

**缺陷 1：同会话并发提问会静默阻塞（不是排队，是卡死）**

`internal/agent/brain.go:208`：

```go
key := CurrentKey(SessionKeyFrom(ctx))
keyMu := b.pool.Lock(key)
keyMu.Lock()   // 整个 AskWith 全程持有
```

主会话的 key 恒为 `web`（`internal/web/sessions.go:43` 的 `resolveSessionKey` 只放行 `web`）。
所以桌面端在跑时移动端提问 → 移动端的 SSE 连接**已经建立**（`chat.go:538` 已 `WriteHeader(200)`），
但 `keyMu.Lock()` 一直阻塞 → **一帧都不发**。前端 `streaming` 状态永远为真，用户看到的是「一直思考中」，
既不知道在等什么，也等不到结果。ACP 侧同理（`internal/acp/manager.go:1694` 的 `m.threadLock(id)`）。

**缺陷 2：`runs` 映射被覆盖，「停止」会停错对象（真 bug）**

`internal/web/chat.go:55` 声明的是 `runs map[string]*chatRun`（会话 key → 单个 run），
`chat.go:572` 附近的登记逻辑：

```go
c.mu.Lock()
c.runs[key] = run         // 第二个请求直接覆盖第一个
c.mu.Unlock()
defer func() {
    c.mu.Lock()
    delete(c.runs, key)    // 第一个请求结束时，把第二个的登记也删了
    c.mu.Unlock()
}()
```

两个请求撞上时：后发的覆盖先发的登记；先发的结束时 `delete` 掉后发的登记。
于是 `POST /api/chat/stop`（`chat.go:418` 读 `c.runs[key]`）**可能停掉错误的那一轮**，
或者报「没有进行中的对话」。

---

## 三、目标架构

一句话：**把「一轮对话」从 HTTP 连接里解耦成内核里的一个 turn 实体，客户端从「发起者」变成「订阅者」。**

```
                     ┌──────────────────────────────────────┐
  桌面端 WebView  ──▶│                                      │
                     │        Go 内核（唯一真相源）           │
  手机端 PWA/App ──▶│  ┌────────────┐   ┌───────────────┐  │
                     │  │ eventbus   │◀──│ turn 执行器    │  │
                     │  │ (会话路由) │   │ (后台 goroutine)│ │
                     │  └─────┬──────┘   └───────┬───────┘  │
                     │        │                  │          │
                     │  ┌─────▼──────────────────▼───────┐  │
                     │  │  共享存储：SessionPool / ACP 投影 │  │
                     │  └────────────────────────────────┘  │
                     └───────┬──────────────────────┬───────┘
                             │ 订阅（增量）          │ 订阅（增量）
                        ┌────▼─────┐            ┌───▼──────┐
                        │ 桌面端    │            │ 手机端    │
                        └──────────┘            └──────────┘
```

### 3.1 会话事件总线（新增 `internal/eventbus`）

按**会话 key** 路由的事件通道。沿用仓库已有的两个成熟模式：

- `internal/admin/notifyws.go` 的 `noticeHub`：非阻塞广播 + 每连接缓冲队列 + 满则丢。
- `internal/agent/events.go` 的 `stepTaps`：按 ID 路由的订阅 tap 表。

事件类型：

| 事件 | 含义 |
|------|------|
| `turn.start` | 一轮开始（含 `turnId`、发起端 `clientId`） |
| `user.message` | 某端发来的用户消息 |
| `delta` | 回答文本增量 |
| `thinking` | 思考内容增量 |
| `step` | 过程事件（轮次 / 工具调用） |
| `turn.end` | 本轮结束（含 token 汇总、暂停 / 错误标记） |
| `turn.error` | 本轮失败 |

每条事件带：`seq`（会话内单调递增）、`turnId`、`clientId`（发起端标识）、`ts`。

**会话 key 映射**：主会话用 `web`；ACP 线程用 `acp:<threadID>`（与 `usage.Record` 里的记法一致）。

### 3.2 订阅端点（新增）

```
GET /api/events/stream?session=web&since=<seq>     （SSE，需登录）
```

- 桌面端与手机端**都连这一条**。连接建立后先收一帧 `hello`（含当前 `seq`），再持续收增量。
- **重放**：服务端按会话保留**环形缓冲**（建议每会话 200 条，或 5 分钟窗口），`since` 之前的补发。
- **缺口兜底**：缺口超过缓冲容量 → 发一帧 `resync`，前端改用现有 `/api/chat/history`（主会话）或
  `GET /api/acp/threads/:id`（ACP 线程）**全量重拉**。这两个接口已经存在且是只读的，直接复用。
- **断线重连**：SSE 自带 `Last-Event-ID`，配合 `seq` 天然幂等重放。移动端被系统冻结后回前台，
  按 `web/src/mobile/push.js:90` 现有的 `visibilitychange` 补连模式即可。

### 3.3 提交端改造：把「发起」与「消费」拆开

当前 `/api/chat` 是「发起 + 消费流」一体。同步要求发起端和订阅端走**同一条路径**，否则会出现两份渲染。

改造为：

```
POST /api/chat  →  立即返回 { turnId, seq }   （不再持有 SSE）
                 →  内核起后台 goroutine 跑这一轮，事件全部进 eventbus
                 →  客户端从 /api/events/stream 收到 delta / turn.end 渲染
```

`/api/chat/resume`（继续被暂停的对话）同理。

**必须一起改的语义**：现在「客户端断开 = 中止生成」（`chat.go` 里 `writeFailed` 与 `rctx` 取消的联动，
ACP 侧 `manager.go:1897` 的 `aborted := ctx.Err() != nil` 同理）。拆开之后这条语义**必须废弃**，改为：

> **停止必须显式**：`POST /api/chat/stop`（ACP：`POST /api/acp/threads/:id/stop`）。页面关闭 / 断网
> **不**中止生成——这恰恰是移动端后台化、桌面端切窗口的前提。

这是本次改造**风险最高**的一处，需要同步改 UI 提示（「关闭页面不会中断本轮」），否则用户会以为关掉页面就停了。

### 3.4 ACP 侧：`live.sink` 从单值改 fan-out

`internal/acp/manager.go:279`：

```go
func (lv *live) setSink(sink *turnSink) { ... }   // 单值
```

`routeEvent`（`manager.go:1040`）把 agent 进程的事件喂给 `lv.takeSink()` 取出的**唯一**接收器。
所以外部 agent（opencode / codex 等）的增量**只有发起端能看到**。

改造：`live` 持有 `sinks map[*turnSink]struct{}`，`routeEvent` 向**全部** sink 扇出；
`takeSink` 语义改为「摘掉当前轮的全部 sink」。

好消息是 ACP 侧**轮次结束时的消息已经落进共享存储**了：`manager.go:2034` 的 `appendTurnLocked`
把 user / assistant 两条 Entry 追加进 `th.Messages` 并 `saveLocked()` 落盘。
所以 ACP 的「双端可见」比主会话**更容易**：

- **轮次结束后**：另一端只要刷新 `GET /api/acp/threads/:id` 就能看到完整问答 → **轮次结束通知即可**。
- **轮次进行中**：增量只在 sink 里 → 必须靠 fan-out 才能实时看到打字机效果。

### 3.5 并发语义：FIFO 排队（已确认）

> **决策（2026-11-11 确认）**：同一会话在另一端正在生成时，本端的提问**进入队列，先进先出**——
> 不拒绝、不丢弃。

主会话共享一份上下文，**同一时刻只能有一轮 active**——这是 `brain.go` 串行锁的既有语义，
本设计不改变它。改变的只是「等」的方式：从**静默阻塞**变成**显式排队**。

#### 3.5.1 队列模型

每个会话 key 一条 FIFO 队列：

| 项 | 设计 |
|----|------|
| 队列归属 | 主会话 `web` → `internal/web/chat.go` 的 `Chat`（它已按 key 管 `runs`/`paused`）；ACP 线程 → `internal/acp/manager.go` 的 `Manager`（已有 `threadLock`） |
| 执行者 | 每条会话 key 一个 dispatcher goroutine，串行取出队首执行；同一时刻只有一条在跑 |
| turn 状态 | `queued` → `running` → `done`/`failed`；`queued` 可转 `canceled` |
| 与 Brain 锁的关系 | dispatcher 单线程后 `pool.Lock(key)` 几乎不会争用；**保留该锁作兜底**——终端 REPL、钉钉、飞书等其它入口也走 Brain，不经过 dispatcher |
| 队列长度上限 | 每会话 20 条，超出返回 `429`，提示「排队太长了，先等前面的跑完」 |
| 排队超时 | 排队超过 10 分钟自动丢弃并通知本人（「等太久了，已取消」）——避免半小时前的问题突然开跑 |
| 发起端断线 | 排队中的 turn **照常执行**：执行发生在 dispatcher 里，与发起端已解绑；结果经事件总线广播给所有订阅者，另一端同样能看到 |

#### 3.5.2 API 契约

| 接口 | 返回 |
|------|------|
| `POST /api/chat`（会话空闲） | `200 { turnId, status: "running", seq }` |
| `POST /api/chat`（会话忙） | `202 { turnId, status: "queued", position: N, seq }` |
| `POST /api/chat/turns/:turnId/cancel` | 取消**排队中**的那条（已在跑的不受影响，走 stop） |
| `POST /api/chat/stop { session, turnId }` | 停止**正在跑**的那一轮；`turnId` 必填，精确到轮 |
| `GET /api/chat/queue?session=web` | 队列快照（position、clientId、问题摘要），供客户端渲染排队列表 |

**`stop` 的 `turnId` 是必须的**：这同时修掉了 §2.3 缺陷 2 的 `runs` 覆盖 bug——
`runs` 改为 `map[key]map[turnId]*chatRun`，登记 / 注销 / 停止全部按 `turnId` 走。

#### 3.5.3 排队信息也要双端可见

排队不是「只有发起端知道」的事：本端排队了，**另一端也应该看到**「手机端还有 1 条在排队」，
否则用户会以为「电脑端问的怎么没人理」。

- P1 事件总线新增 `turn.queued` 帧（含 `position`、`clientId`、问题摘要）。
- 前端渲染：发起端显示「排队中，前面还有 N 个」；另一端显示「手机端排队中」。

#### 3.5.4 前端排队态

- 输入框在排队期间**保持可输入**（不锁死），发送按钮显示「排队中」。
- 排队中的那条消息在气泡上标「排队中 · 第 N 位」，可点「取消排队」。
- 轮到它执行时，气泡从「排队中」切到「正在回答」，打字机照常。
- 另一端正在生成时，本端输入框旁显示占用提示（「小五正在回答（来自手机端）」）+ 可用的跨端「停止」。

### 3.6 客户端身份（`clientId`）

每端生成一个持久 `clientId`（localStorage，与现有 `friday.authToken` 同机制）。

事件带 `originClientId`，前端据此决定渲染细节：

- **自己发的消息**：正常打字机动画。
- **别人发的**：同样实时渲染（这是「双端可见」的核心），但气泡旁标「来自手机端」，
  让用户知道对面有人在用。
- **输入框**：另一端正在生成时，显示占用提示 + 可用的「停止」按钮（跨端停止，见 §3.5）。

**打字机动画只能有一个来源**：改造后所有渲染都由 eventbus 驱动，双端各自独立渲染自己的那份，
不存在「同一段文字被两个连接各推一次」的重复问题。

---

## 四、分阶段落地

| 阶段 | 内容 | 预估 | 独立可验收 |
|------|------|------|-----------|
| **P0** | 修 `runs` 覆盖 bug；**FIFO 队列 + dispatcher**（同会话排队，不再静默卡死）；桌面端加 3s 轻量增量轮询 | 半天 | 立刻「看得到」，代价是几秒延迟 |
| **P1** | eventbus + `/api/events/stream` + `/api/chat` 提交式改造 + 前端事件驱动渲染 → **主会话双端实时可见** | 1-2 天 | 主会话达成核心目标 |
| **P2** | ACP `live.sink` fan-out + ACP 侧 FIFO 队列 + `/api/acp/threads/:id/stream` → **工作区 agent 双端实时可见** | 1-2 天 | agent 场景达成 |
| **P3** | 桌面端通知切 WS（替掉 5s 轮询）；跨端停止 / 暂停；重放与 resync 一致性测试 | 打磨 | 收尾 |

### P0 细节

- `runs` 改为两级：`map[string]map[string]*chatRun`（会话 key → turnId → run），登记 / 注销 / 停止都按 `turnId` 走。
- 忙时返回：`POST /api/chat` 遇到会话忙**不阻塞**，改为入队并返回 `202 { queued, position }`。
  实现：每个会话 key 一个 dispatcher goroutine + 一条 FIFO 队列（§3.5.1）。
  `Brain.AskWith` 的 `pool.Lock(key)` 保留作兜底（其它入口不走 dispatcher）。
- 桌面端轻量轮询：`GET /api/chat/history?session=web` 已有，只加一个「比上次新」的判断
  （比较消息条数 / 最后一条消息 id），有变化才替换列表；3s 一次。

### P1 细节（改动面）

**后端**

| 文件 | 改动 |
|------|------|
| `internal/eventbus/`（新增） | 事件定义、按会话路由、环形缓冲重放、fan-out |
| `internal/web/chat.go` | `HandleSSE` 改提交式；增量改为投 eventbus；停止按 `turnId`；断开不再中止 |
| `internal/admin/ext.go` | 注册 `/api/events/stream` 路由（鉴权与 `/api/chat` 一致） |
| `internal/agent/brain.go` | 增量回调改为投 eventbus；保留 per-key 锁作兜底 |

**前端**

| 文件 | 改动 |
|------|------|
| `web/src/components/ChatView.vue` | 改为「订阅 + 渲染」，`streamTurn` 的直连消费改为订阅消费 |
| `web/src/PaletteApp.vue` | 同上（快捷面板） |
| `web/src/mobile/MobileChat.vue` | 同上（移动端） |
| `web/src/events.js`（新增） | SSE 订阅客户端：重连、`seq` 跟踪、`resync` 处理 |
| `web/src/blocks.js` | 渲染入口改为「按事件追加块」，而不是「按连接追加块」 |

**注意**：`web/src/blocks.js` 的 `pushText` / `pushThinking` / `pushToolStep` 是双端**共用**的块状态机
（`MobileChat.vue` 明确复用桌面端逻辑），改造后事件驱动的渲染入口也应放在这里，两端复用同一份。

---

## 五、风险与坑位

1. **「客户端断开 = 中止生成」语义废弃**（§3.3）——风险最高的一处。
   改造后关页面不中止生成，UI 必须明确提示，否则用户会以为停了其实还在跑。
   反过来，agent 长任务（可能跑几小时）也就不会因为切页面而丢了。
2. **`/api/chat` 语义变更影响三处前端**（`ChatView` / `PaletteApp` / `MobileChat`），
   漏改一处就表现为「那一端不显示」。
   排队态还额外要求这三处都渲染 `queued` 状态与「取消排队」按钮。
2.5 **队列与 dispatcher 的生命周期**：dispatcher 必须随会话闲置而退出（否则每个 key 泄漏一个 goroutine），
   且内核重启后队列丢失——排队中的问题**不会**自动恢复，这一点要在 UI 上说明。
3. **事件缓冲必须限长**：按会话做环形缓冲，不能无限增长（内存）。
4. **ACP 侧 `settleTurn` 的「结界」语义**（`manager.go:1048`）要重新审视：
   它依赖「只有一个 sink」来保证收尾文本不丢。改成 fan-out 后，多个 sink 必须**全部**收尾，
   否则某个客户端会漏掉最后一段文本。
5. **移动端后台被 iOS 冻结**：SSE 会被断开，靠 `visibilitychange` 补连 + `since=seq` 重放兜住。
   现有的 `web/src/mobile/push.js` 已经有这套模式的成熟实现，可直接照搬。
6. **本地开发端口冲突**：`8080` 会撞 Docker 容器，本机一律 `go run ./cmd/robot --port 45678`。
7. **macOS 打包不受影响**：本次改动全在内核与前端，不涉及 Tauri 外壳与签名流程。

---

## 六、不在本次范围内

- 多人协作 / 多租户 / 云同步（`docs/GAPS-TODO.md:132` 已明确排除）。
- 跨设备历史迁移（换机 / 换数据目录）——那是数据目录搬迁的事，与实时同步无关。
- 钉钉 / 飞书等消息通道与双端的关系：它们是**第三类客户端**。
  事件总线做出来后，它们天然也能受益（一条通道 = 一个订阅端），但**本设计不承诺**——
  先把桌面 ⇄ 手机这两端做扎实。

---

## 七、决策记录

1. ✅ **并发语义**：FIFO 排队，先进先出（§3.5）。
2. ✅ **阶段范围**：P0→P2 一次做全（用户 2026-11-11 决定）。
3. ✅ **「关闭页面不中止生成」**：接受。这是移动端后台化与跨端停止的前提。

### 实现与设计的两处偏差

**其一：ACP 侧不叠后端队列。**
设计 §3.5 原打算主会话与 ACP 两侧各建一条后端 FIFO 队列。落地时发现
`web/src/components/WorkspaceView.vue` 已有一套更成熟的前端队列
（`enqueue` / `pumpQueue` / `editQueueItem` / `dropQueueItem` / `toggleQueuePause`）——
支持编辑、删除、整队清空与暂停，这些交互后端队列做不了。
于是 ACP 侧改为：**后端不排队，撞上忙时立刻返回 `202 {status:"queued"}`**，
由前端把消息压进自己既有的队列，本轮结束后 `pumpQueue` 再发一次。
主会话没有既有前端队列，仍走后端 FIFO。

**其二：ACP 的订阅端点复用通用端点。**
设计里提过 `GET /api/acp/threads/:id/stream`，实现改为复用通用的
`GET /api/events/stream?session=acp:<threadID>`——前端一套代码读两种会话，
不必为 ACP 单开一路订阅协议。

---

## 八、落地实现要点（2026-11-11 实际落地）

设计到实现有几处调整，记在这里免得下次对着设计文档找不着代码：

| 设计的说法 | 实际落地 | 原因 |
|---|---|---|
| 主会话与 ACP 各建一条后端 FIFO 队列 | 只有主会话建了（`internal/web/queue.go`）；ACP 侧后端不排队，撞忙只回 `202` | 工作区已有一套更成熟的前端队列（`enqueue`/`pumpQueue`/`editQueueItem`/`dropQueueItem`/`toggleQueuePause`），后端那套做不了编辑与暂停 |
| ACP 订阅端点 `/api/acp/threads/:id/stream` | 复用通用的 `/api/events/stream?session=acp:<threadID>` | 前端一套代码读两种会话，不必单开一路协议 |
| `POST /api/chat/stop` 可选 turnId | turnId **必填**，不带就 400 并说明为什么 | 不带就可能停到另一端正在跑的那轮 |
| 事件载荷放在 `data` 字段里 | 顶层平铺（`eventFrame` 把 data 的键并到顶层） | 前端 `turnevents.js` 一套解析覆盖两种会话，不用分支 |

### 关键实现位置

- 事件总线：`internal/eventbus/eventbus.go`（`Publish` / `Subscribe` / `Events` / `CurrentSeq`）
- 主会话提交式与队列：`internal/web/chat.go`（`HandleSSE` / `enqueue` / `dispatch` / `runTurn` / `HandleStop`）
- 主会话队列：`internal/web/queue.go`（`turnQueue`）
- ACP 提交式与扇出：`internal/acp/manager.go`（`SubmitTurn` / `publish` / `live.sinks`）
- 订阅端点与装配：`internal/admin/acp.go`（`handleEventStream`）与 `internal/admin/admin.go`（路由注册）
- 前端订阅客户端：`web/src/events.js`
- 前端共享渲染层：`web/src/turnevents.js`

### 实现期间发现并修掉的额外问题

- **`Subscriber.Events()` 每次调用都新建通道**：调用方若写成 `for { ev := <-sub.Events() }`，
  每调一次就丢掉上一条通道里的事件。现在通道建一次并缓存（`sync.Once`）。
- **turnId 并发撞号**：`time.Now().UnixNano()` 在并发下可能相同，而 turnId 是「停止」
  精确指认那一轮的唯一依据。改为时间戳加进程内自增号。
- **`chatErrorMessage` 不认 `context.DeadlineExceeded`**：超时文案落到了兜底分支，
  用户看到的是「这轮回复失败了：context deadline exceeded」而不是「连不上模型服务」。
