// turnevents.js 把会话事件帧翻译成「时间线动作」：桌面端聊天页与移动端聊天页
// 共用这一份，两端渲染出的时间线因此必然一致（契约见 docs/EVENTS-API.md）。
//
// 为什么不再由各自那条 SSE 连接直接渲染：改造后提交与订阅分离，两端都从
// /api/events/stream 收事件。若渲染逻辑各写一份，很快就会漂移——同一轮问答
// 在两端显示成不同的样子，比不同步更难排查。
//
// 事件帧是顶层平铺的（与旧 SSE 帧键名一致）：{ seq, type, turnId, clientId,
// device, session, at, ...载荷 }。
import { closeRunningTools, finishRound, pushText, pushThinking, pushToolStep } from './blocks.js'

// botFor 找到某轮对应的 bot 气泡；找不到就按 makeBot 新建一条并归属该 turnId。
// 按 turnId 归属是双端一致的关键：同一轮的增量无论来自哪个连接，都落到同一条气泡上。
function botFor(ctx, turnId) {
  if (turnId) {
    const hit = ctx.messages.find(m => m.role === 'bot' && m.turnId === turnId)
    if (hit) return hit
  }
  const bot = ctx.makeBot()
  bot.turnId = turnId || ''
  ctx.messages.push(bot)
  return bot
}

// userFor 找到某条用户消息气泡：优先按 turnId 归属（继续对话的「问题」不带
// turnId 时退化为按文本匹配）。
function userFor(ctx, turnId, text) {
  if (turnId) {
    const hit = ctx.messages.find(m => m.role === 'user' && m.turnId === turnId)
    if (hit) return hit
  }
  if (text) {
    const hit = ctx.messages.find(m => m.role === 'user' && m.text === text)
    if (hit) return hit
  }
  return null
}

// applyEvent 处理一条事件帧。ctx 由各页面提供：
//   messages        消息数组（reactive）
//   makeBot()       新建一个 bot 气泡
//   mine            本端 clientId
//   onPlan(plan)    计划卡回调（ChatView 要 watchPlan，弹窗不需要）
//   onPaused(info)  暂停现场回调
//   onEnd(info)     本轮结束回调
//   onError(text)   错误回调
//   onStatus(text)  状态行回调
//   onRemoteUser(m) 收到「另一端发来的用户消息」回调（加「来自手机端」标识）
export function applyEvent(ev, ctx) {
  if (!ev || !ev.type) return
  const type = ev.type

  if (type === 'user.message') {
    // 自己发的消息已经有乐观气泡了，广播来的同一条不再显示一遍。
    if (ev.clientId && ev.clientId === ctx.mine) return
    // images 两种形态：ACP 侧给的是图片数量，主会话给的是附件 id 数组。
    // 这里统一成数组，makeUser 自己按需要取用。
    const imgs = Array.isArray(ev.images) ? ev.images : []
    const u = ctx.makeUser(ev.text || '', imgs, ev.images || 0)
    if (ev.turnId) u.turnId = ev.turnId
    u.remote = true
    u.device = ev.device || ''
    ctx.messages.push(u)
    if (ctx.onRemoteUser) ctx.onRemoteUser(u)
    return
  }

  if (type === 'turn.queued') {
    const u = userFor(ctx, ev.turnId, ev.text)
    if (u) {
      u.queued = true
      u.position = ev.position || 1
      // 别人排的队，本端标一句「手机端排队中」，免得以为没人理。
      if (!u.remote) u.device = ev.device || ''
    } else if (ev.clientId && ev.clientId !== ctx.mine) {
      // 另一端排队的问题在本端也留个痕（它还没进历史接口，刷新前只有事件知道）。
      const u2 = ctx.makeUser(ev.text || '', [])
      u2.turnId = ev.turnId || ''
      u2.remote = true
      u2.queued = true
      u2.position = ev.position || 1
      u2.device = ev.device || ''
      ctx.messages.push(u2)
      if (ctx.onRemoteUser) ctx.onRemoteUser(u2)
    }
    return
  }

  if (type === 'turn.start') {
    const bot = botFor(ctx, ev.turnId)
    bot.streaming = true
    if (ctx.onStart) ctx.onStart(bot, ev)
    return
  }

  if (type === 'delta') {
    const bot = botFor(ctx, ev.turnId)
    pushText(bot, ev.delta || '')
    if (ctx.onDelta) ctx.onDelta(bot, ev.delta || '')
    return
  }

  if (type === 'thinking') {
    const bot = botFor(ctx, ev.turnId)
    pushThinking(bot, ev.thinking || '')
    if (ctx.onThinking) ctx.onThinking(bot)
    return
  }

  if (type === 'step') {
    const bot = botFor(ctx, ev.turnId)
    const s = ev.step || {}
    if (s.kind === 'round') finishRound(bot, s.detail)
    else pushToolStep(bot, s)
    if (ctx.onStep) ctx.onStep(bot, s)
    return
  }

  if (type === 'plan') {
    const bot = botFor(ctx, ev.turnId)
    bot.plan = ev.plan
    if (ctx.onPlan) ctx.onPlan(ev.plan)
    return
  }

  if (type === 'paused') {
    const bot = botFor(ctx, ev.turnId)
    bot.paused = true
    bot.streaming = false
    bot.partial = ev.partial || bot.text
    closeRunningTools(bot)
    if (ctx.onPaused) ctx.onPaused({ partial: bot.partial, bot })
    return
  }

  if (type === 'turn.end') {
    const bot = botFor(ctx, ev.turnId)
    bot.streaming = false
    bot.paused = false
    if (ev.turn) bot.turn = ev.turn
    closeRunningTools(bot)
    if (ctx.onEnd) ctx.onEnd(bot, ev)
    return
  }

  if (type === 'turn.error') {
    const bot = botFor(ctx, ev.turnId)
    bot.streaming = false
    closeRunningTools(bot)
    if (ctx.onError) ctx.onError(ev.error || '这一轮回复失败了', bot)
    return
  }

  if (type === 'turn.canceled') {
    const u = userFor(ctx, ev.turnId, '')
    if (u) {
      u.queued = false
      u.canceled = true
    }
    if (ctx.onCanceled) ctx.onCanceled(ev.turnId)
    return
  }

  if (type === 'status') {
    if (ctx.onStatus) ctx.onStatus(ev.status || '')
    return
  }
}
