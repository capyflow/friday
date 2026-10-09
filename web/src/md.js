// renderMD 把助手消息渲染成 Markdown。先做 HTML 转义再交给 marked：
// 模型输出的原始 HTML 标签只会以文本显示，不产生可执行标签（防 XSS）。
// 链接走 renderer 白名单（http/https/mailto/锚点/相对路径），javascript: 等
// 危险协议降级为纯文本；统一新窗口打开。
// 流式中间态若 ``` 未闭合则临时补齐，避免半截代码块把整条消息渲染成代码。
import { marked } from 'marked'

let ready = false
function ensure() {
  if (ready) return
  marked.use({
    breaks: true,
    gfm: true,
    renderer: {
      link(token) {
        const href = token.href || ''
        const text = this.parser.parseInline(token.tokens ?? [])
        if (!/^(https?:|mailto:|#|\/)/i.test(href)) return text
        const title = token.title ? ` title="${token.title}"` : ''
        return `<a href="${href}" target="_blank" rel="noopener noreferrer"${title}>${text}</a>`
      },
    },
  })
  ready = true
}

export function renderMD(src) {
  ensure()
  // 只转义 < 和 &：标签必须以 < 开头，二者足以阻断 HTML 注入；
  // 保留 > 使 blockquote（> 引用）语法可用
  let s = String(src == null ? '' : src).replace(/&/g, '&amp;').replace(/</g, '&lt;')
  if ((s.match(/```/g) || []).length % 2 === 1) s += '\n```'
  try {
    return marked.parse(s, { async: false })
  } catch (e) { return '<p>' + s.replace(/\n/g, '<br>') + '</p>' }
}
