// push.js 是移动端与内核的通知长链接：一条 WebSocket 实时收新通知与投递状态
// 变化，替代桌面端的 5 秒轮询（手机网络下轮询更费电费流量）。
//
// 协议（与内核 notifyws.go 对齐）：hello 帧带当前未读数校准角标；notice 帧按
// id 并入本地列表（列表快照与事件流之间有竞态窗口，靠 id 去重兜住）；怀疑
// 失步就全量拉 GET /api/notifications 兜底。
//
// 断线重连：指数退避 1s→15s 封顶；回到前台立即补连一次（手机切后台被系统
// 断链是常态，回前台马上重连比等退避更符合直觉）。
import { reactive } from 'vue'
import { api } from '../api.js'
import { getToken } from '../auth.js'

// push.status: off（未连）| connecting（握手中）| on（hello 已到）
export const push = reactive({ status: 'off', unread: 0 })
export const notices = reactive([])

let ws = null
let retry = 0 // 连续失败次数，成功后归零
let retryTimer = null
let gen = 0 // stopPush 之后再连会拿到新代号；旧回调凭代号过期作废

function wsURL() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  return proto + '://' + location.host + '/api/notifications/ws?token=' + encodeURIComponent(getToken())
}

export function startPush() {
  stopPush()
  connect(gen)
}

export function stopPush() {
  gen++
  retry = 0
  if (retryTimer) { clearTimeout(retryTimer); retryTimer = null }
  if (ws) {
    const dead = ws
    ws = null
    try { dead.close() } catch { /* 已断就算了 */ }
  }
  push.status = 'off'
}

function connect(g) {
  if (g !== gen || ws) return
  push.status = 'connecting'
  let sock
  try {
    sock = new window.WebSocket(wsURL())
  } catch {
    return scheduleRetry(g) // 构造失败（无 token 等）：走退避，不崩页面
  }
  ws = sock
  sock.onopen = () => { if (ws === sock) retry = 0 } // 真正就绪看 hello 帧
  sock.onmessage = ev => {
    if (ws !== sock || g !== gen) return
    let f
    try { f = JSON.parse(ev.data) } catch { return }
    if (f.t === 'hello') {
      push.status = 'on'
      push.unread = f.unread || 0
      loadNotices().catch(() => {}) // 列表快照兜底；失败下次重连再拉
    } else if (f.t === 'notice' && f.notice) {
      mergeNotice(f.notice)
      recount()
    }
  }
  sock.onclose = () => {
    if (ws !== sock) return
    ws = null
    if (g !== gen) return
    push.status = 'off'
    scheduleRetry(g)
  }
  sock.onerror = () => { try { sock.close() } catch { /* onclose 会接着走 */ } }
}

function scheduleRetry(g) {
  if (g !== gen || retryTimer) return
  const delay = Math.min(1000 * 2 ** retry, 15000)
  retry++
  retryTimer = setTimeout(() => {
    retryTimer = null
    connect(gen)
  }, delay)
}

// 回前台立即补连：后台被系统断链后，等退避不如马上试一把。
if (typeof document !== 'undefined' && document.addEventListener) {
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && push.status !== 'on') {
      if (retryTimer) { clearTimeout(retryTimer); retryTimer = null }
      retry = 0
      connect(gen)
    }
  })
}

function mergeNotice(n) {
  const i = notices.findIndex(x => x.id === n.id)
  if (i >= 0) notices.splice(i, 1, n)
  else notices.unshift(n)
}

function recount() {
  push.unread = notices.filter(n => !n.readAt).length
}

export async function loadNotices() {
  const r = await api('GET', '/api/notifications')
  notices.splice(0, notices.length, ...(r.notifications || []))
  push.unread = r.unread || 0
}

export async function markRead(id) {
  await api('POST', `/api/notifications/${id}/read`, {})
  const n = notices.find(x => x.id === id)
  if (n && !n.readAt) n.readAt = new Date().toISOString()
  recount()
}

export async function retryNotice(id) {
  await api('POST', `/api/notifications/${id}/retry`, {})
  await loadNotices()
}

export async function snoozeNotice(id, minutes = 10) {
  await api('POST', `/api/notifications/${id}/snooze`, { minutes })
  await loadNotices()
}
