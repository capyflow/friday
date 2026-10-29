// 打开外部链接：桌面外壳（Tauri）的 WebView 默认拒绝 window.open——wry 没装新窗口
// 处理器就直接返回 None，点了什么也不会发生。所以桌面端要走外壳命令 open_url 交给
// 系统默认浏览器；浏览器直连内核时仍用 window.open。两处入口（设置页「授权」、
// 正文里的外链）共用这里的判断，别再各自开窗。

// 外壳全局对象只在桌面端注入（tauri.conf.json 的 withGlobalTauri）。
export function desktopInvoke() { return window.__TAURI__?.core?.invoke }

// openExternal 返回是否真的把链接递出去了：false 时调用方给「复制链接」兜底。
export async function openExternal(url) {
  if (!url) return false
  const invoke = desktopInvoke()
  if (invoke) {
    try {
      await invoke('open_url', { url })
      return true
    } catch { return false }
  }
  try { return !!window.open(url, '_blank', 'noopener') } catch { return false }
}
