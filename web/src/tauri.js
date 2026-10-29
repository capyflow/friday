// Tauri 原生能力的统一入口：只在桌面壳的 WebView 里存在（tauri.conf 开了
// withGlobalTauri）；浏览器直访时全部降级为 no-op，调用方无需各自探测。
export function nativeInvoke(cmd, args) {
  try {
    const invoke = window.__TAURI__ && window.__TAURI__.core && window.__TAURI__.core.invoke
    if (!invoke) return null
    const p = invoke(cmd, args)
    if (p && typeof p.catch === 'function') p.catch(() => {})
    return p
  } catch { return null }
}

export function hasNative() {
  return !!(window.__TAURI__ && window.__TAURI__.core && window.__TAURI__.core.invoke)
}
