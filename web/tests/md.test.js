import { describe, expect, it } from 'vitest'
import { renderMD } from '../src/md.js'

function render(source) {
  const container = document.createElement('div')
  container.innerHTML = renderMD(source)
  return container
}

function expectSafe(container) {
  expect(container.querySelector('script,iframe,svg,math,style,object,form')).toBeNull()
  for (const element of container.querySelectorAll('*')) {
    for (const attribute of element.attributes) {
      expect(attribute.name).not.toMatch(/^on/i)
      if (['href', 'src'].includes(attribute.name)) {
        expect(new URL(attribute.value, 'https://example.com').protocol).toMatch(/^(https?:|mailto:)$/)
      }
    }
  }
}

describe('不可信 Markdown', () => {
  it.each([
    String.raw`[标题](https://example.com "\" onmouseover=\"alert(1)")`,
    '[URL](<https://example.com/"onmouseover="alert(1)>)',
    '[实体](https://example.com "&quot; onmouseover=&quot;alert(1)")',
    String.raw`![" onerror="alert(1)](https://example.com/p.png "\" onload=\"alert(1)")`,
    '<img src=x onerror="alert(1)"><script>alert(1)</script>',
    '<svg><a xlink:href="javascript:alert(1)">危险</a></svg>',
    '<form id="attributes"><input name="innerHTML"></form>',
  ])('阻止属性与原始 HTML 注入：%s', source => {
    const container = render(source)
    expectSafe(container)
    expect(container.textContent).not.toBe('')
  })

  it.each([
    'javascript:alert%281%29', 'JaVaScRiPt:alert%281%29', 'vbscript:msgbox%281%29',
    'data:text/html,boom', 'javascript&#58;alert%281%29', 'java&#x73;cript:alert%281%29',
    'java\tscript:alert%281%29',
  ])('危险协议无法执行：%s', href => {
    const container = render(`[危险链接](<${href}>)`)
    expectSafe(container)
    expect(container.textContent).toContain('危险链接')
  })

  it('安全链接保留查询参数、标题与新窗口隔离', () => {
    const container = render(String.raw`[正常](https://example.com/?a=1&b=2 "含有 \"引号\"")`)
    const link = container.querySelector('a')
    expect(link.getAttribute('href')).toBe('https://example.com/?a=1&b=2')
    expect(link.title).toBe('含有 "引号"')
    expect(link.target).toBe('_blank')
    expect(link.rel).toBe('noopener noreferrer')
    expectSafe(container)
  })

  it.each(['#section', '/docs/readme', './docs/readme', '../notes', 'mailto:hello@example.com'])('保留安全路径：%s', href => {
    expect(render(`[链接](${href})`).querySelector('a').getAttribute('href')).toBe(href)
  })

  it('保留引用、列表、表格和未结束的流式代码块', () => {
    const container = render('> 引用\n\n- **事项**\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n```js\n<img onerror="x">')
    expect(container.querySelector('blockquote').textContent).toContain('引用')
    expect(container.querySelector('li strong').textContent).toBe('事项')
    expect(container.querySelector('table')).not.toBeNull()
    expect(container.querySelector('pre code').textContent).toContain('<img onerror="x">')
    expectSafe(container)
  })
})
