// 模型输出和记忆正文均不可信：原始 HTML 显示为文本，生成的 HTML 再做白名单清洗。
// 链接与图片分别校验协议，并转义每个属性，避免 Markdown 标题/URL 注入事件属性。
// 流式中间态若 ``` 未闭合则临时补齐，避免半截代码块把整条消息渲染成代码。
import { Marked } from 'marked'
import DOMPurify from 'dompurify'

function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>"']/g, ch => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  })[ch])
}

function safeURL(value, image = false) {
  const href = String(value ?? '').trim()
  if (!href) return null
  try {
    // URL 会规范化协议大小写和浏览器忽略的制表/换行；相对路径使用固定安全基址。
    const url = new URL(href, 'https://friday.invalid/')
    if (!['https:', 'http:', ...(image ? [] : ['mailto:'])].includes(url.protocol)) return null
    return href
  } catch { return null }
}

const markdown = new Marked({
  breaks: true,
  gfm: true,
  renderer: {
    html(token) { return escapeHTML(token.text) },
    link(token) {
      const text = this.parser.parseInline(token.tokens ?? [])
      const href = safeURL(token.href)
      if (href === null) return text
      const title = token.title ? ` title="${escapeHTML(token.title)}"` : ''
      return `<a href="${escapeHTML(href)}" target="_blank" rel="noopener noreferrer"${title}>${text}</a>`
    },
    image(token) {
      const src = safeURL(token.href, true)
      if (src === null) return escapeHTML(token.text)
      const title = token.title ? ` title="${escapeHTML(token.title)}"` : ''
      return `<img src="${escapeHTML(src)}" alt="${escapeHTML(token.text)}"${title}>`
    },
  },
})

const SANITIZE_OPTIONS = {
  ALLOWED_TAGS: ['p', 'br', 'strong', 'em', 'del', 'a', 'img', 'blockquote', 'code', 'pre',
    'ul', 'ol', 'li', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'hr',
    'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'input'],
  ALLOWED_ATTR: ['href', 'src', 'alt', 'title', 'target', 'rel', 'class', 'start', 'align',
    'type', 'disabled', 'checked'],
  ALLOW_DATA_ATTR: false,
  ALLOW_ARIA_ATTR: false,
  SANITIZE_NAMED_PROPS: true,
}

export function renderMD(src) {
  let s = String(src ?? '')
  if ((s.match(/```/g) || []).length % 2 === 1) s += '\n```'
  let html
  try {
    html = markdown.parse(s, { async: false })
  } catch { html = '<p>' + escapeHTML(s).replace(/\n/g, '<br>') + '</p>' }
  return DOMPurify.sanitize(html, SANITIZE_OPTIONS)
}
