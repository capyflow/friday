// 登录凭据（JWT）的本地存取：内核登录/首装接口下发 token，存 localStorage
// （桌面主窗口与快捷键弹窗同源共享），之后每个请求带 Authorization: Bearer。
// 每次读取都走 localStorage，弹窗在登录前打开也能看到主窗口后来写入的 token。
//
// 桌面端另有一层原生冷备：外壳把 token 存数据目录（desktop-auth-token），
// 启动时 hydrateToken() 回填 localStorage。localStorage 按 origin 隔离，
// 端口被占回退随机端口（或 WebView 数据被清）时靠这层回填保住登录态。
// 浏览器直访没有 __TAURI__，所有原生调用自动降级为纯 localStorage。
// 浏览器直访没有 __TAURI__，所有原生调用自动降级为纯 localStorage。
import { nativeInvoke } from './tauri.js'

const TOKEN_KEY = 'friday.authToken'

export function getToken() {
  try { return localStorage.getItem(TOKEN_KEY) || '' } catch { return '' }
}

export function setToken(token) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token)
    else localStorage.removeItem(TOKEN_KEY)
  } catch { /* 隐私模式等写不进去：本次会话仍可用内存中的值 */ }
  // write-through：登录/退出/凭据失效清理都经过这里，同步原生冷备
  nativeInvoke(token ? 'auth_set_token' : 'auth_clear_token', token ? { token } : undefined)
}

// 启动回填：把原生冷备与 localStorage 对齐（native 为准；local 多出来的推给
// native，覆盖升级前的老登录态）。memoized：主窗口/弹窗共享同一次 IPC。
let hydratePromise = null
export function hydrateToken() {
  if (!hydratePromise) hydratePromise = doHydrate()
  return hydratePromise
}

async function doHydrate() {
  const invoke = window.__TAURI__ && window.__TAURI__.core && window.__TAURI__.core.invoke
  if (!invoke) return // 浏览器直访：没有原生层
  let native = ''
  try {
    native = (await invoke('auth_get_token')) || ''
  } catch { return } // 外壳拒绝/异常：保持现状，本次会话仍可用 localStorage
  if (native) {
    if (native !== getToken()) {
      try { localStorage.setItem(TOKEN_KEY, native) } catch { /* 写不进去就沿用旧值 */ }
    }
    return
  }
  const local = getToken()
  if (local) { // native 空但本地有（升级首启/上次落盘失败）：补推给原生层
    try { await invoke('auth_set_token', { token: local }) } catch { /* 忽略 */ }
  }
}

export function authHeaders(extra) {
  const headers = { ...(extra || {}) }
  const token = getToken()
  if (token) headers.Authorization = 'Bearer ' + token
  return headers
}

// <img> 等无法带自定义头的 GET：附件回读允许 ?token=，只在 /api/upload/ 上加。
export function mediaURL(url) {
  const token = getToken()
  if (!token || typeof url !== 'string' || !url.startsWith('/api/upload/')) return url
  return url + (url.includes('?') ? '&' : '?') + 'token=' + encodeURIComponent(token)
}
