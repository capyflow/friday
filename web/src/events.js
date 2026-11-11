// events.js 是会话事件订阅客户端：一条 SSE 长连接收某个会话的所有 turn 事件。
//
// 为什么需要它：以前 /api/chat 既是「提交」又是「收流」，SSE 帧只写给发起请求
// 的那条连接——所以移动端问完，桌面端那边什么都没有。改造后提交与订阅分开，
// 两端都从这条连接收事件（契约见 docs/EVENTS-API.md）。
//
// 三件事：
//  1. 连接与重连：指数退避 1s→15s 封顶；回到前台立即补连（手机切后台被系统
//     断链是常态，回前台马上重连比等退避更符合直觉）。
//  2. 断点续传：记住最后收到的 seq，重连带 since 回来；服务端从环形缓冲补发。
//  3. 缺口兜底：收到 resync 帧或发现 seq 跳号时，回调 onResync 让上层全量重拉
//     历史——宁可重拉，也不假装消息是连续的。
import { getToken } from './auth.js'

// clientId 是本次页面实例的标识：每次加载随机生成，不跨标签页共享。
// 用途是让前端能跳过「自己发的」user.message（乐观气泡已经加过了），
// 避免同一条消息显示两遍。
export const clientId = 'c' + Math.random().toString(36).slice(2, 10) + Date.now().toString(36)

// device 是设备标签，只用于展示：另一端看到消息时标「来自手机端」。
// 存在 localStorage 里长期保留（同一台设备每次打开标签一致）。
export function deviceLabel() {
  const KEY = 'friday-device'
  let v = ''
  try { v = localStorage.getItem(KEY) || '' } catch (_) { /* 隐私模式 */ }
  if (!v) {
    v = window.__TAURI__ ? '桌面端' : '手机端'
    try { localStorage.setItem(KEY, v) } catch (_) { /* 存不下就用这次的 */ }
  }
  return v
}

// onEvent(ev) 收到一条事件帧。ev 是顶层平铺的 JSON（见契约 §2.4）。
// onResync() 收到 resync 帧或检测到 seq 缺口时调用：上层应全量重拉历史。
// onState(state) 连接状态变化：'connecting' | 'on' | 'off'。
export function subscribe(session, since, { onEvent, onResync, onState }) {
  let es = null
  let retry = 0
  let retryTimer = null
  let gen = 0
  let lastSeq = since || 0
  let disposed = false

  function url() {
    // EventSource 不支持自定义 Authorization 头，token 只能走查询参数
    // （后端用 wrapMedia 鉴权，与通知长链接 /api/notifications/ws 同一套）。
    const proto = location.protocol === 'https:' ? 'https' : 'http'
    return proto + '://' + location.host + '/api/events/stream?session=' +
      encodeURIComponent(session) + '&since=' + lastSeq + '&token=' + encodeURIComponent(getToken())
  }

  function connect(g) {
    if (disposed || g !== gen) return
    onState && onState('connecting')
    let sock
    try {
      sock = new EventSource(url())
    } catch (_) {
      scheduleRetry(g)
      return
    }
    es = sock
    sock.onopen = () => { if (es === sock) retry = 0 }
    sock.onmessage = ev => {
      if (es !== sock || disposed || g !== gen) return
      let f
      try { f = JSON.parse(ev.data) } catch (_) { return }
      handle(f)
    }
    sock.onerror = () => {
      // EventSource 出错时会自己重连，但我们的重连策略更可控（带退避与 since），
      // 所以自己关掉并按退避重连。
      try { sock.close() } catch (_) { /* 已关 */ }
      if (es === sock) es = null
      if (g === gen && !disposed) scheduleRetry(g)
    }
  }

  function handle(f) {
    if (!f || !f.type) return
    if (f.type === 'hello') {
      onState && onState('on')
      return
    }
    if (f.type === 'resync') {
      onResync && onResync()
      return
    }
    // seq 跳号 = 中间漏了事件（慢连接被丢过，或缓冲已过期）：不能假装连续。
    if (f.seq && lastSeq && f.seq > lastSeq + 1) {
      onResync && onResync()
    }
    if (f.seq) lastSeq = f.seq
    onEvent && onEvent(f)
  }

  function scheduleRetry(g) {
    if (disposed || g !== gen || retryTimer) return
    onState && onState('off')
    const delay = Math.min(1000 * 2 ** retry, 15000)
    retry++
    retryTimer = setTimeout(() => {
      retryTimer = null
      connect(g)
    }, delay)
  }

  function start() {
    gen++
    connect(gen)
  }

  function stop() {
    disposed = true
    gen++
    if (retryTimer) { clearTimeout(retryTimer); retryTimer = null }
    if (es) {
      const dead = es
      es = null
      try { dead.close() } catch (_) { /* 已断 */ }
    }
    onState && onState('off')
  }

  // 回前台立即补连：后台被系统断链后，等退避不如马上试一把。
  if (typeof document !== 'undefined' && document.addEventListener) {
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible' && !disposed) {
        if (retryTimer) { clearTimeout(retryTimer); retryTimer = null }
        retry = 0
        connect(gen)
      }
    })
  }

  start()
  return { stop, currentSeq: () => lastSeq }
}
