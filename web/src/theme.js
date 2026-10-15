// 主题：浅色（Quiet SaaS）/ 深色（中性炭灰），html[data-theme] 驱动。
// 选择持久化在 localStorage；未选择时跟随系统。index.html 头部有同逻辑的
// 内联脚本，在挂载前预设 data-theme 防止首帧闪烁。
const KEY = 'friday-theme'

export function systemTheme() {
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

export function savedTheme() {
  try { return localStorage.getItem(KEY) } catch (e) { return null }
}

export function applyTheme(t) {
  document.documentElement.dataset.theme = t
  const meta = document.querySelector('meta[name="theme-color"]')
  if (meta) meta.setAttribute('content', t === 'dark' ? '#141414' : '#F6F6F8')
}

// 初始化：返回当前生效主题（saved → system → dark，与样例一致深色优先）。
export function initTheme() {
  const t = savedTheme() || systemTheme() || 'dark'
  applyTheme(t)
  return t
}

export function setTheme(t) {
  applyTheme(t)
  try { localStorage.setItem(KEY, t) } catch (e) {}
}
