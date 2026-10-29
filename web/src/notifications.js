import { reactive, watch } from 'vue'
import { api } from './api.js'
import { store } from './store.js'
import { desktopInvoke } from './open-url.js'
import { getPref, setPref } from './prefs.js'

export const notificationSettings = reactive({ status: '未开启', busy: false })
let timer = null
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
      const granted = await invoke('enable_notifications')
      notificationSettings.status = granted ? '系统通知已开启' : '系统未授权，请使用通知中心'
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
        native.onclick = () => { window.focus(); store.view = 'notifications'; native.close() }
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

export function stopNotifications() { generation++; lifecycle++; polling = false; if (timer) clearInterval(timer); timer = null }
