// speed.js 估算流式回答的当前输出速度（tok/s）。
//
// 服务商只在流结束时才报 usage，生成途中前端拿不到真实 token 数，所以按字符
// 估：中文/日文/韩文 1 字≈1 token，其余 4 字符≈1 token——各家分词器的通用
// 近似。数字只用于"当前写得多快"的体感，轮末的准确量仍以 done 帧的 in/out 为准。
//
// 速率用最近 3 秒的滑动窗口算；隔了 1.5 秒才来的第一片视为停顿（思考、工具
// 调用），窗口清零重新起算，不把停顿摊进速度里；但**读数保留上一次的值**——
// 清零会在每次停顿后把头部的数字打回 0/会话平均，看起来像"不实时刷新"。

const WINDOW_MS = 3000
const PAUSE_MS = 1500
const MIN_SPAN_MS = 300

const CJK = /[\u2e80-\u9fff\uf900-\ufaff\u3040-\u30ff\uac00-\ud7af]/

// estimateTokens 按字符估 token：CJK 逐字算 1，其余按 4 字符 1 个。
export function estimateTokens(s) {
  let n = 0
  for (const ch of s || '') n += CJK.test(ch) ? 1 : 0.25
  return n
}

// fmtRate 速度显示：100 以上取整（数字太长反而读不出），以下留一位小数。
export function fmtRate(v) {
  const n = Number(v) || 0
  if (n <= 0) return '0'
  return n >= 100 ? String(Math.round(n)) : n.toFixed(1)
}

// 每条消息一个计量器。挂在 WeakMap 上而不是消息对象上：样本数组每片增量都变，
// 塞进 reactive 消息里会让整棵时间线跟着做无谓的依赖追踪。
const meters = new WeakMap()

// feedRate 吞一片输出增量，返回当前速率（tok/s）；窗口还没攒够时维持上次读数。
// now 只在测试里显式传入。
export function feedRate(target, text, now = Date.now()) {
  let m = meters.get(target)
  if (!m) { m = { samples: [], last: 0, rate: 0 }; meters.set(target, m) }
  const n = estimateTokens(text)
  if (n <= 0) return m.rate
  if (m.last && now - m.last > PAUSE_MS) m.samples = [] // 停顿之后重新起算（保留上次读数）
  m.last = now
  m.samples.push([now, n])
  const from = now - WINDOW_MS
  while (m.samples.length > 1 && m.samples[0][0] < from) m.samples.shift()
  const span = now - m.samples[0][0]
  if (span < MIN_SPAN_MS) return m.rate
  let tokens = 0
  for (const s of m.samples) tokens += s[1]
  m.rate = tokens / (span / 1000)
  return m.rate
}
