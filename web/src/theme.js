// 主题：浅色（Quiet SaaS）/ 深色（中性炭灰），html[data-theme] 驱动。
// 偏好存 localStorage，桌面端写穿到壳的数据目录（prefs.js，跨 origin 保留）；
// 没有键 = 跟随系统。生效主题变化经 friday:theme 广播（账号菜单的图标文案与
// 设置页的选中态都听它），index.html 头部有同逻辑的内联脚本，在挂载前预设
// data-theme 防止首帧闪烁。
import { getPref, setPref, removePref } from './prefs.js'

const KEY = 'friday-theme'
const QUERY = '(prefers-color-scheme: dark)'

export function systemTheme() {
  return window.matchMedia?.(QUERY).matches ? 'dark' : 'light'
}

export function savedTheme() {
  return getPref(KEY)
}

// themePreference 返回用户的选择（'auto' | 'light' | 'dark'），不是当前生效主题。
export function themePreference() { return savedTheme() || 'auto' }

export function applyTheme(t) {
  document.documentElement.dataset.theme = t
  const meta = document.querySelector('meta[name="theme-color"]')
  if (meta) meta.setAttribute('content', t === 'dark' ? '#141414' : '#F6F6F8')
}

function announce(t) { window.dispatchEvent(new CustomEvent('friday:theme', { detail: t })) }

// 初始化：返回当前生效主题（saved → system → dark，与样例一致深色优先）。
// 顺带挂系统偏好监听：只在「跟随系统」时改界面，显式选过浅/深色的用户不受打扰。
export function initTheme() {
  const t = savedTheme() || systemTheme() || 'dark'
  applyTheme(t)
  window.matchMedia?.(QUERY)?.addEventListener?.('change', e => {
    if (savedTheme()) return
    const next = e.matches ? 'dark' : 'light'
    applyTheme(next)
    announce(next)
  })
  return t
}

export function setTheme(t) {
  applyTheme(t)
  setPref(KEY, t)
  announce(t)
}

// clearTheme 回到「跟随系统」：删掉显式选择，按当前系统偏好立即生效。
export function clearTheme() {
  removePref(KEY)
  const t = systemTheme()
  applyTheme(t)
  announce(t)
}
