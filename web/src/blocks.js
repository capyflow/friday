// blocks.js 是聊天时间线内容块（zcode 风）的纯函数操作集，ChatView 与 AgentView
// 共用：消息对象上的 blocks 数组按到达顺序存放思考/工具调用/正文块，分片交错
// 到达也不会把回答切成碎块（轮次由 round 事件与工具调用推进）。
//
// 消息对象形状（bot）：
//   { role:'bot', text, streaming?, paused?, blocks:[], turn?, round }
// 块形状：
//   { kind:'think', text, meta?, open?, round }            思考（meta=轮次耗时摘要）
//   { kind:'tool', name, text, out?, status, open?, round } 工具调用
//   { kind:'text', text, round }                            回答正文（markdown）
// turn 是本轮问答的 token 汇总 { in, out, ms? }。
import { fmtTokens } from './format.js'

// blockIndex 找当前轮内最后一块指定类型的块；跨轮即停（上一轮的内容不再并入）。
export function blockIndex(m, kind) {
  for (let i = m.blocks.length - 1; i >= 0; i--) {
    const b = m.blocks[i]
    if (b.round !== m.round) break
    if (b.kind === kind) return i
  }
  return -1
}

// 思考预览行：收起时只显示一行，取思考文本最新非空行的尾部，随增量不断更新。
const THINK_LINE_MAX = 120
export function thinkPreview(text) {
  const lines = (text || '').split('\n')
  for (let i = lines.length - 1; i >= 0; i--) {
    const s = lines[i].trim()
    if (!s) continue
    return s.length > THINK_LINE_MAX ? '…' + s.slice(-THINK_LINE_MAX) : s
  }
  return ''
}

// 思考增量并入本轮的思考块；本轮还没有就新起一块，并插在正文块前面
// （网关把思考分片发在正文之后，时间线上仍按"先思考后回答"读）。
export function pushThinking(m, delta) {
  const ti = blockIndex(m, 'think')
  if (ti >= 0) { m.blocks[ti].text += delta; return }
  const block = { kind: 'think', text: delta, meta: '', open: false, round: m.round }
  const xi = blockIndex(m, 'text')
  if (xi >= 0) m.blocks.splice(xi, 0, block)
  else m.blocks.push(block)
}

// 轮次结束事件（耗时）：挂到本轮的思考块上，收起时即"思考 · 耗时…"，
// 然后推进轮次——之后的思考与正文另起一块。
export function finishRound(m, detail) {
  const ti = blockIndex(m, 'think')
  if (ti >= 0 && !m.blocks[ti].meta) m.blocks[ti].meta = detail || ''
  m.round++
}

// 工具调用事件：run 新起一行并推进轮次（工具调用是轮次边界）；
// ok/fail 回填最近的同名运行中块。
export function pushToolStep(m, s) {
  const name = s.name || ''
  if (s.status === 'run') {
    m.blocks.push({ kind: 'tool', name, text: s.detail || '', out: '',
      status: 'run', open: false, round: m.round })
    m.round++
    return
  }
  for (let i = m.blocks.length - 1; i >= 0; i--) {
    const b = m.blocks[i]
    if (b.kind === 'tool' && b.status === 'run' && b.name === name) {
      b.out = s.detail || ''
      b.status = s.status === 'fail' ? 'fail' : 'ok'
      return
    }
  }
}

// 轮次收尾兜底：把还没收到结束事件的运行中工具块标成完成。过程事件走 SSE
// 非阻塞发布，ok/fail 可能被丢在半路，缺了它对应块就永远转圈；Ask 循环是
// 同步收齐工具结果才会继续的，done 帧到了就说明这些调用实际都已执行完。
export function closeRunningTools(m) {
  for (const b of m.blocks || []) {
    if (b.kind === 'tool' && b.status === 'run') b.status = 'ok'
  }
}

// 回答增量并入本轮的正文块；工具调用之后的新一轮回答另起一块。
export function pushText(m, delta) {
  m.text += delta
  const xi = blockIndex(m, 'text')
  if (xi >= 0) m.blocks[xi].text += delta
  else m.blocks.push({ kind: 'text', text: delta, round: m.round })
}

// answerText 提取一条回答的正文：只取 text 块（思考、工具调用不进剪贴板），
// 多段正文之间空一行；供「复制」按钮使用。
export function answerText(blocks) {
  return (blocks || [])
    .filter(b => b && b.kind === 'text' && (b.text || '').trim())
    .map(b => b.text.trim())
    .join('\n\n')
}

// 轮末汇总文案：token 走 k/百万缩写；耗时缺失（被暂停的现场没有结束时刻）就不写。
// context 是 agent 自报的上下文水位（usage_update，M-5），有才显示。
export function turnTotalText(t) {
  if (!t) return ''
  let s = `本轮 · ${fmtTokens(t.in || 0)}+${fmtTokens(t.out || 0)} tokens`
  if (t.contextSize) s += ` · 上下文 ${fmtTokens(t.contextUsed || 0)}/${fmtTokens(t.contextSize)}`
  return t.ms ? `${s} · 耗时 ${(t.ms / 1000).toFixed(1)}s` : s
}
