# 会话事件接口契约（双端同步）

> 本文件是**后端与前端并行开发的唯一依据**。改动任一侧都必须同步本文件。
> 配套设计见 [MULTI-DEVICE-SYNC.md](MULTI-DEVICE-SYNC.md)。

## 一、总览

一次对话 = 一个 **turn**。turn 的产生与消费被解耦：

- **提交**（写）：`POST /api/chat` 立刻返回，不等生成结束。
- **订阅**（读）：`GET /api/events/stream` 一条长连接，收该会话的所有 turn 事件。

发起端与另一端**走完全相同的读取路径**（都从 stream 读），因此不存在「两份渲染」。

## 二、HTTP 接口

### 2.1 提交一轮

`POST /api/chat`
`POST /api/chat/resume`（继续被暂停的对话）

请求体（JSON）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `message` | string | 必填（附件可单独构成一轮） |
| `attachments` | string[] | 可选，上传返回的 id |
| `session` | string | 可选，缺省 `web`；ACP 线程用 `acp:<threadID>` |
| `skill` | string | 可选，显式点名技能 |
| `mode` | string | 可选，`plan` / `goal` |
| `clientId` | string | **新增**，客户端实例标识（见 §三） |
| `device` | string | **新增**，设备标签（`桌面端` / `手机端`…） |

响应（202 表示排队）：

```json
// 200 —— 立刻开跑
{ "turnId": "t1731...", "status": "running", "seq": 42, "session": "web" }

// 202 —— 会话忙，已排队
{ "turnId": "t1731...", "status": "queued", "position": 1, "seq": 42, "session": "web" }
```

错误：`{ "body": { "error": "..." } }`（沿用现有信封格式）。

> **语义变更**：本接口**不再返回 SSE 流**。旧前端按流解析会拿到非流式 JSON——
> 这是有意的破坏性变更，两侧必须同时上线。

### 2.2 停止与取消

| 接口 | 请求体 | 行为 |
|------|--------|------|
| `POST /api/chat/stop` | `{ session?, turnId }` | 停止**正在跑**的那一轮。`turnId` **必填** |
| `POST /api/acp/threads/:id/stop` | `{ turnId }` | 同上，ACP 线程 |
| `POST /api/chat/turns/:turnId/cancel` | 无 | 取消**排队中**的那一轮（已在跑的不受影响） |
| `POST /api/acp/threads/:id/turns/:turnId/cancel` | 无 | 同上，ACP 线程 |

`turnId` 必填是硬要求：它同时修掉了 `runs` 映射被覆盖的 bug
（见 MULTI-DEVICE-SYNC.md §2.3 缺陷 2）。

### 2.3 队列快照

`GET /api/chat/queue?session=web`（ACP：`GET /api/acp/threads/:id/queue`）

```json
{ "items": [ { "turnId": "t1", "position": 1, "clientId": "c1", "device": "手机端", "text": "问题摘要", "createdAt": "2026-11-11T10:00:00Z" } ] }
```

### 2.4 订阅事件流

`GET /api/events/stream?session=web&since=42`

- `session`：`web` 或 `acp:<threadID>`。
- `since`：客户端已收到的最后一条 seq；缺省 0（表示只要新事件）。
- 鉴权：与 `/api/chat` 一致（登录 token）。

**响应**：`text/event-stream`，每帧 `data: {JSON}\n\n`。

帧格式（**顶层平铺**，与旧 SSE 帧的键名保持一致，降低前端改动量）：

```json
{
  "seq": 43,
  "type": "delta",
  "turnId": "t1731...",
  "clientId": "c-abc",
  "device": "手机端",
  "session": "web",
  "at": "2026-11-11T10:00:00.123Z",
  "delta": "回答文本增量"
}
```

**事件类型与载荷**：

| `type` | 额外字段 | 含义 |
|---------|----------|------|
| `hello` | — | 连接建立的第一帧，`seq` 为当前最新序号 |
| `resync` | — | 缺口过大无法重放，客户端应**全量重拉历史** |
| `user.message` | `text`, `images?` | 某端发来的用户消息 |
| `turn.queued` | `position`, `text` | 提问进入队列 |
| `turn.start` | — | 一轮开始 |
| `delta` | `delta` | 回答文本增量 |
| `thinking` | `thinking` | 思考内容增量 |
| `step` | `step` | 过程事件（轮次 / 工具调用） |
| `plan` | `plan` | 计划模式产出的清单 |
| `paused` | `partial` | 本轮被暂停 |
| `turn.end` | `turn?` | 本轮结束（`turn` 是 token 汇总） |
| `turn.error` | `error` | 本轮失败 |
| `turn.canceled` | — | 排队中的那轮被取消 |
| `status` | `status` | 说明性状态（如「正在等另一端的问题跑完」） |

**重放与重连**：

- 服务端每会话保留 200 条环形缓冲。
- `since` 落后在缓冲内 → 补发该区间事件，再转实时。
- `since` 落后到缓冲外 → 先发一帧 `resync`，客户端全量重拉
  （主会话 `GET /api/chat/history?session=web`；ACP `GET /api/acp/threads/:id`）。
- 客户端按 `seq` 去重；发现 `seq` 跳号（>1）即视为缺口，触发全量重拉。

## 三、客户端身份

| 标识 | 生成方式 | 生命周期 | 用途 |
|------|----------|----------|------|
| `clientId` | 页面加载时随机生成 | **每次加载不同**（不跨标签页共享） | 区分「这条是哪个客户端发的」；前端据此跳过自己发的 `user.message`，避免乐观气泡与广播气泡重复 |
| `device` | `localStorage` 持久化 | 长期 | 仅展示：另一端看到时标「来自手机端」 |

## 四、FIFO 队列语义

- 每个会话 key 一条 FIFO 队列，一个 dispatcher goroutine 串行执行。
- 队列上限 20 条，超出返回 `429`。
- 排队超过 10 分钟自动丢弃并发 `turn.canceled`。
- **发起端断线不影响排队项**：执行发生在 dispatcher 里，与客户端已解绑，
  结果照常广播给所有订阅者。
- 「停止」必须显式（`POST .../stop` 带 `turnId`）；**页面关闭 / 断网不中止生成**。

## 五、渲染约定（两端一致）

前端收到事件后按 `type` 分派，**打字机动画只有一个来源**（事件流）：

| 事件 | 前端动作 |
|------|----------|
| `user.message` | 若 `clientId` 是自己 → 忽略（已有乐观气泡）；否则**追加一条用户气泡**并标「来自手机端」 |
| `turn.queued` | 自己发的那条标「排队中 · 第 N 位」；另一端发的标「手机端排队中」 |
| `turn.start` | 若 `clientId` 是自己 → 该气泡切到「正在回答」 |
| `delta` / `thinking` / `step` | 追加到**当前 turn 的 bot 气泡**（按 `turnId` 归属） |
| `turn.end` | 收尾：`bot.streaming=false`，挂上 `turn` token 汇总，`closeRunningTools` |
| `turn.error` | 追加错误气泡 |
| `paused` | 该 bot 气泡标「已暂停」，可继续 |
| `resync` | 全量重拉历史并替换本地列表 |

**bot 气泡的归属**：按 `turnId` 找到对应的那条（自己发起的或另一端发起的），
找不到就新建一条并归属该 `turnId`。这样两端渲染出的时间线完全一致。
