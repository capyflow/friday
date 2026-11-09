import { reactive, watch } from 'vue'
import { api } from './api.js'
import { setMode, store } from './store.js'
import { desktopInvoke } from './open-url.js'
import { getPref, setPref } from './prefs.js'

export const notificationSettings = reactive({ status: '未开启', busy: false })
let timer = null
let syncTimer = null
let polling = false
let seen = new Set()
let currentUser = ''
let enabled = false
let generation = 0
let lifecycle = 0

watch(() => [store.loggedIn, store.username], () => {
  generation++
  store.unread = 0
  currentUser = ''
  seen = new Set()
  polling = false
}, { flush:'sync' })

export async function enableSystemNotifications() {
  if (notificationSettings.busy) return
  notificationSettings.busy = true
  try {
    const invoke = desktopInvoke()
    if (invoke) {
      notificationSettings.status = '正在请求系统授权…'
      const granted = await invoke('enable_notifications')
      if (granted) { notificationSettings.status = '系统通知已开启'; return }
      // 未授权说明系统此前已经拒过（或授权框被抑制），再点也不会弹——
      // 只能把用户送到系统设置里改，见 openSystemNotificationSettings。
      await openSystemNotificationSettings(invoke)
      return
    }
    if (!('Notification' in window)) { notificationSettings.status = '当前浏览器不支持，请使用通知中心'; return }
    const permission = await window.Notification.requestPermission()
    enabled = permission === 'granted'
    notificationSettings.status = enabled ? '系统通知已开启' : '系统未授权，请使用通知中心'
    setPref('friday-notifications-enabled', enabled ? '1' : '0')
  } catch { notificationSettings.status = '系统通知暂不可用，请使用通知中心' }
  finally { notificationSettings.busy = false }
}

// 打开系统通知设置面板（外壳里写死的深链，前端不传 URL）。打不开就把手动
// 路径说清楚，别让用户对着一句「系统未授权」猜下一步该做什么。
async function openSystemNotificationSettings(invoke) {
  try {
    await invoke('open_notification_settings')
    notificationSettings.status = '已打开系统通知设置，找到 Friday 允许通知'
    watchSystemPermission(invoke)
  } catch {
    notificationSettings.status = '系统未授权，请在系统设置的通知里允许 Friday'
  }
}

// 用户在系统设置里拨开关时，应用是被动方，只能回来看：每 2 秒查一次系统权限
// （sync_notifications 只读权限、不请求授权，不会弹系统框），最多 90 秒；
// 查到已授权就由外壳落开启位并把状态收尾。退出登录/切页会 bump lifecycle，
// 这里靠它停掉遗留轮询。
function watchSystemPermission(invoke) {
  stopSync()
  const epoch = lifecycle
  let ticks = 0
  syncTimer = setInterval(async () => {
    if (lifecycle !== epoch || ++ticks > 45) { stopSync(); return }
    let granted = false
    try { granted = await invoke('sync_notifications') } catch { stopSync(); return }
    if (lifecycle !== epoch) return
    if (!granted) return
    stopSync()
    notificationSettings.status = '系统通知已开启'
  }, 2000)
}

function stopSync() { if (syncTimer) clearInterval(syncTimer); syncTimer = null }

export async function pollNotifications() {
  if (!store.loggedIn) { store.unread = 0; currentUser = ''; seen = new Set(); return }
  if (polling) return
  polling = true
  const user = store.username
  const epoch = generation
  try {
    const result = await api('GET', '/api/notifications')
    if (!store.loggedIn || store.username !== user || generation !== epoch) return
    store.unread = result.unread || 0
    if (currentUser !== user) {
      currentUser = user
      try { seen = new Set(JSON.parse(getPref('friday-notices-' + user) || '[]')) } catch { seen = new Set() }
    }
    const notifications = (result.notifications || []).filter(n => n.prepared && n.push && !n.readAt)
    const invoke = desktopInvoke()
    for (const notice of notifications) {
      if (seen.has(notice.id) || !enabled || invoke) continue
      if (window.Notification?.permission !== 'granted') { notificationSettings.status = '系统未授权，请使用通知中心'; continue }
      try {
        const native = new window.Notification('小五提醒', { body: notice.text, tag: notice.id })
        native.onclick = () => {
          window.focus()
          // 通知归属某条会话（工作区里 agent 的一轮结束、等你批准等）时直接跳到那条
          // 会话：点横幅的意图是「回到那儿接着看」，不是「去看通知列表」。
          // 没有具体会话的（生活模式提醒等）仍然落到通知页。
          if (notice.threadId) {
            setMode('work')
            store.acpOpenThread = notice.threadId
            store.view = 'agent'
          } else {
            store.view = 'notifications'
          }
          native.close()
        }
        seen.add(notice.id)
      } catch { notificationSettings.status = '系统通知暂不可用，请使用通知中心' }
    }
    setPref('friday-notices-' + user, JSON.stringify([...seen].slice(-500)))
  } catch { /* 通知页会显示加载失败；全局轮询避免重复弹出错误 */ }
  finally { if (generation === epoch) polling = false }
}

export function startNotifications() {
  stopNotifications()
  enabled = getPref('friday-notifications-enabled') === '1'
  if (enabled) notificationSettings.status = '系统通知已开启'
  const invoke = desktopInvoke()
  const epoch = lifecycle
  if (invoke) {
    invoke('notifications_enabled').then(granted => {
      if (lifecycle === epoch) notificationSettings.status = granted ? '系统通知已开启' : '未开启，请点击开启系统通知'
    }).catch(() => { if (lifecycle === epoch) notificationSettings.status = '系统通知暂不可用，请使用通知中心' })
  }
  pollNotifications()
  timer = setInterval(pollNotifications, 5000)
}

export function stopNotifications() { generation++; lifecycle++; polling = false; stopSync(); if (timer) clearInterval(timer); timer = null }
