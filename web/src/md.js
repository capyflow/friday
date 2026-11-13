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

// 行内代码里的本机绝对路径转成可点文件 chip（仅 renderMD 开了 fileChips 的调用方，
// 聊天/智能体时间线）。识别从严，点错顶多提示文件不存在：
// / 开头且非 //（排除协议形）、排除站点自身路由（/api 等）、无控制字符/反引号，
// 且最后一段带 1–8 位字母数字扩展名（目录与 /usr/local/go 这类无扩展名不认）。
export function looksLikeFilePath(text) {
  const s = String(text ?? '').trim()
  if (s.length < 4 || !s.startsWith('/') || s.startsWith('//')) return false
  if (/^\/(api|admin|palette|traces)(\/|$)/.test(s)) return false
  if (/[`<>|]/.test(s)) return false
  for (let i = 0; i < s.length; i++) {
    if (s.charCodeAt(i) < 0x20) return false
  }
  return /\.[A-Za-z0-9]{1,8}$/.test(s)
}

// localFileHref 判定一个 <a href> 是不是指向本机文件：是则返回去掉 ?# 后缀的
// 路径候选（相对路径原样返回，由调用方按基准目录拼绝对路径），否则 null。
// agent 的回答常带 [文案](docs/a.md) 这类相对链接——safeURL 的固定基址把它们
// 当 https 透传成 <a>，原生行为是跳到站点 404（桌面壳里干脆没反应）。识别从严：
// 带协议（http:/mailto:/…）与协议相对（//host/…）不算，纯锚点不算；绝对路径
// 与 file-chip 同口径（looksLikeFilePath），相对路径要求 1–8 位扩展名结尾——
// 点错顶多提示文件不存在，不误伤站内路由（/api/…）。
export function localFileHref(href) {
  const h = String(href ?? '').trim()
  if (!h || h.startsWith('#') || h.startsWith('//')) return null
  if (/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(h)) return null
  const clean = h.replace(/[?#].*$/, '')
  if (!clean || /[`<>|]/.test(clean)) return null
  for (let i = 0; i < clean.length; i++) {
    if (clean.charCodeAt(i) < 0x20) return null
  }
  if (clean.startsWith('/')) return looksLikeFilePath(clean) ? clean : null
  if (!/\.[A-Za-z0-9]{1,8}$/.test(clean)) return null
  try { return decodeURIComponent(clean) } catch { return clean }
}

// joinFilePath 把相对路径按基准目录拼成绝对路径（消化 ./ 与 ../）。base 非绝对
// 路径时返回空串——调用方据此回退默认行为，不猜位置。
export function joinFilePath(base, rel) {
  const b = String(base ?? '').trim()
  if (!b.startsWith('/')) return ''
  const out = []
  for (const part of (b.replace(/\/+$/, '') + '/' + rel).split('/')) {
    if (!part || part === '.') continue
    if (part === '..') out.pop()
    else out.push(part)
  }
  return '/' + out.join('/')
}

// 后处理已消毒 HTML：命中路径的行内 code（pre 里的不动）换成 button.file-chip，
// 全部经 DOM API 构造（textContent/dataset），不拼字符串，无注入面。
function linkifyFileChips(html) {
  const tpl = document.createElement('template')
  tpl.innerHTML = html
  let changed = false
  for (const code of [...tpl.content.querySelectorAll('code')]) {
    if (code.closest('pre')) continue
    const path = code.textContent.trim()
    if (!looksLikeFilePath(path)) continue
    const chip = document.createElement('button')
    chip.type = 'button'
    chip.className = 'file-chip'
    chip.dataset.path = path
    // 悬停显示完整路径（路径可能很长，但省略号吃掉的文件名最要紧）
    chip.title = path
    chip.textContent = path
    code.replaceWith(chip)
    changed = true
  }
  return changed ? tpl.innerHTML : html
}

// 对话正文开启 codeCopy：给每个块级代码块包一层，右上角放复制按钮。按钮不带
// 代码文本（大段代码不重复进 DOM），点击时从相邻 pre 取 textContent——复制
// 逻辑在 TimelineBlocks 的点击委托里，md.js 只管把结构摆出来。放在 sanitize
// 之后做，不往白名单里加 button。
function wrapCodeCopy(html) {
  return html
    .replace(/<pre>/g, '<div class="md-code"><pre>')
    .replace(/<\/pre>/g, '</pre><button type="button" class="md-copy">复制</button></div>')
}

export function renderMD(src, { fileChips = false, codeCopy = false } = {}) {
  let s = String(src ?? '')
  if ((s.match(/```/g) || []).length % 2 === 1) s += '\n```'
  let html
  try {
    html = markdown.parse(s, { async: false })
  } catch { html = '<p>' + escapeHTML(s).replace(/\n/g, '<br>') + '</p>' }
  html = DOMPurify.sanitize(html, SANITIZE_OPTIONS)
  if (codeCopy) html = wrapCodeCopy(html)
  return fileChips ? linkifyFileChips(html) : html
}
