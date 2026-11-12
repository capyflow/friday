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
  let visHandler = null
  // sockets 记下这次订阅建过的**所有**连接。
  //
  // 为什么不能只留当前那一条：重连会换连接，而 EventSource 不会因为「被换下」
  // 就自己断开。旧连接留在那儿就变成僵尸——服务端照样往它写帧、浏览器照样
  // 收帧（但被 es !== sock 丢掉），连接却一直占着。浏览器同源连接池只有 6 条
  // （通知 WS、终端 WS、主会话流、当前会话流各占一条），几个僵尸就把池子占满，
  // 新连接再也建不起来——表现正是「对话收不到响应，刷新一下页面又好了」。
  const sockets = new Set()

  function url() {
    // EventSource 不支持自定义 Authorization 头，token 只能走查询参数
    // （后端用 wrapMedia 鉴权，与通知长链接 /api/notifications/ws 同一套）。
    const proto = location.protocol === 'https:' ? 'https' : 'http'
    return proto + '://' + location.host + '/api/events/stream?session=' +
      encodeURIComponent(session) + '&since=' + lastSeq + '&token=' + encodeURIComponent(getToken())
  }

  // closeSock 关掉一条连接，并把它从 sockets 里摘掉。
  function closeSock(sock) {
    if (!sock) return
    sockets.delete(sock)
    try { sock.close() } catch (_) { /* 已关 */ }
  }

  function connect(g) {
    if (disposed || g !== gen) return
    onState && onState('connecting')
    // 重连前先关掉上一条：换下来的旧连接不会自己断，不关就等于每次重连都
    // 多留一条僵尸连接（见上面 sockets 的注释）。
    if (es) {
      const old = es
      es = null
      closeSock(old)
    }
    let sock
    try {
      sock = new EventSource(url())
    } catch (_) {
      scheduleRetry(g)
      return
    }
    es = sock
    sockets.add(sock)
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
      const wasCurrent = es === sock
      closeSock(sock)
      if (wasCurrent) es = null
      // 僵尸连接（已被换下的旧连接）出错不再触发重连：它每错一次就会多开一条
      // 新连接，等于自己给自己放大泄漏。
      if (!wasCurrent) return
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
    // 这次订阅建过的连接全关掉，不只是当前那条：漏关的那些会一直占着浏览器的
    // 同源连接池，直到页面被刷新为止。
    for (const sock of Array.from(sockets)) closeSock(sock)
    es = null
    // 监听器也要摘掉：每次 subscribe 都往 document 上挂一个，攒多了纯属泄漏。
    if (visHandler && typeof document !== 'undefined' && document.removeEventListener) {
      document.removeEventListener('visibilitychange', visHandler)
      visHandler = null
    }
    onState && onState('off')
  }

  // 回前台立即补连：后台被系统断链后，等退避不如马上试一把。
  //
  // 只在连接真的不在了时才补：连接还活着时再开一条，既白占一个连接位，
  // 又把旧连接的帧全丢掉（es !== sock 的帧会被忽略）。
  if (typeof document !== 'undefined' && document.addEventListener) {
    visHandler = () => {
      if (disposed) return
      if (document.visibilityState !== 'visible') return
      if (es && es.readyState === 1) return // CONNECTING=0 OPEN=1 CLOSED=2
      if (retryTimer) { clearTimeout(retryTimer); retryTimer = null }
      retry = 0
      connect(gen)
    }
    document.addEventListener('visibilitychange', visHandler)
  }

  start()
  return { stop, currentSeq: () => lastSeq }
}
