// 前端本地偏好（主题/通知开关/通知去重）的持久化：桌面壳内经 Tauri 写
// <数据目录>/desktop-prefs.json——跨 origin（端口回退随机）保留；浏览器直访
// 只写 localStorage，行为与以往一致。localStorage 始终是同步缓存（首屏内联
// 脚本和运行期读取都直接读它），原生文件是冷备，启动时补齐缺失项。
import { nativeInvoke, hasNative } from './tauri.js'

const PREFIX = 'friday-'
const TOKEN_KEY = 'friday.authToken' // token 走 auth.js 的独立严格校验通道
const VALUE_MAX = 8 * 1024          // 与壳侧校验一致

export function getPref(key) {
  try { return localStorage.getItem(key) } catch { return null }
}

export function setPref(key, value) {
  try { localStorage.setItem(key, value) } catch { /* 隐私模式等写不进去 */ }
  persist()
}

export function removePref(key) {
  try { localStorage.removeItem(key) } catch { /* 同上 */ }
  persist()
}

// 快照：所有 friday- 前缀的本地键（白名单由壳侧再校验一遍），排除 token。
function snapshot() {
  const out = {}
  try {
    for (let i = 0; i < localStorage.length; i++) {
      const key = localStorage.key(i)
      if (!key || !key.startsWith(PREFIX) || key === TOKEN_KEY) continue
      const value = localStorage.getItem(key)
      if (value != null && value.length <= VALUE_MAX) out[key] = value
    }
  } catch { /* 读不了就不备份 */ }
  return out
}

// 写穿：微任务合并同一轮事件循环内的多次写，只落一次快照。
let pending = null
function persist() {
  if (pending || !hasNative()) return
  pending = Promise.resolve().then(() => {
    pending = null
    return nativeInvoke('prefs_save', { prefs: snapshot() })
  })
}

// 启动回填：端口回退/WebView 数据被清后 localStorage 是空的，从原生冷备补齐；
// 本地已有的键保持不动（本地是活跃值）。返回是否实际补了内容。
let hydratePromise = null
export function hydratePrefs() {
  if (!hydratePromise) hydratePromise = doHydrate()
  return hydratePromise
}

async function doHydrate() {
  if (!hasNative()) return false
  let saved = null
  try {
    saved = await nativeInvoke('prefs_get')
  } catch { return false }
  if (!saved || typeof saved !== 'object') return false
  let restored = false
  for (const [key, value] of Object.entries(saved)) {
    if (typeof value !== 'string' || !key.startsWith(PREFIX) || key === TOKEN_KEY) continue
    if (value.length > VALUE_MAX) continue
    try {
      if (localStorage.getItem(key) == null) {
        localStorage.setItem(key, value)
        restored = true
      }
    } catch { /* 写不进去就跳过 */ }
  }
  return restored
}
