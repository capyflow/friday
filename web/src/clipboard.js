// 写剪贴板：优先 Clipboard API（安全上下文才有），失败退回临时 textarea 选中复制。
// 桌面 WebView 与非 https 的本地页面常走第二条路，别只信 navigator.clipboard。

export async function copyText(v) {
  const text = String(v ?? '')
  if (!text) return
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return
    } catch {
      // 落到 execCommand：部分 WebView / 非安全上下文不允许 Clipboard API
    }
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.top = '-1000px'
  document.body.appendChild(ta)
  ta.select()
  const ok = document.execCommand && document.execCommand('copy')
  document.body.removeChild(ta)
  if (!ok) throw new Error('浏览器不允许写入剪贴板')
}
