import { describe, expect, it } from 'vitest'
import { joinFilePath, localFileHref, looksLikeFilePath, renderMD } from '../src/md.js'

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

describe('文件路径 chip', () => {
  it.each([
    '/Users/aaron/com.capyflow/friday/减脂计划.md',
    '/tmp/out.json',
    '/var/log/app.log',
    '/home/u/my file notes.txt',
  ])('识别文件路径：%s', path => {
    expect(looksLikeFilePath(path)).toBe(true)
  })

  it.each([
    '', '相对/路径.txt', '//protocol-like', '/api/chat', '/admin',
    '/usr/local/go', '/etc/hosts', 'https://x.com/a.png', '/has`tick.png', '/new\nline.png',
  ])('不误伤非文件路径：%s', path => {
    expect(looksLikeFilePath(path)).toBe(false)
  })

  it('fileChips 开启时行内代码路径变可点 chip，未开启时保持原样', () => {
    const src = '文件在 `/tmp/a.md`，代码里 `<div>/tmp/a.md</div>` 不算。'
    const withChip = render(src, true)
    const chip = withChip.querySelector('.file-chip')
    expect(chip).not.toBeNull()
    expect(chip.dataset.path).toBe('/tmp/a.md')
    expect(chip.textContent).toBe('/tmp/a.md')
    const plain = render(src, false)
    expect(plain.querySelector('.file-chip')).toBeNull()
    expect(plain.querySelector('code').textContent).toBe('/tmp/a.md')
  })

  it('代码块内的路径不转 chip；含引号的路径安全落进 dataset', () => {
    const block = render('```text\n/tmp/in-block.md\n```', true)
    expect(block.querySelector('pre .file-chip')).toBeNull()
    expect(block.querySelector('pre code').textContent).toContain('/tmp/in-block.md')
    const quoted = render('`/tmp/" onclick="x".md`', true)
    const chip = quoted.querySelector('.file-chip')
    expect(chip.dataset.path).toBe('/tmp/" onclick="x".md')
    expectSafe(quoted)
  })

  function render(source, fileChips) {
    const container = document.createElement('div')
    container.innerHTML = renderMD(source, { fileChips })
    return container
  }
})

describe('正文里的本机文件链接', () => {
  it.each([
    ['docs/spec.md', 'docs/spec.md'],
    ['./a/b.md#小结', './a/b.md'],
    ['../out/报告.pdf?download=1', '../out/报告.pdf'],
    ['/Users/aaron/x/说明.txt', '/Users/aaron/x/说明.txt'],
    ['a%20b.md', 'a b.md'],
  ])('识别文件链接：%s → %s', (href, want) => {
    expect(localFileHref(href)).toBe(want)
  })

  it.each([
    'https://example.com/a.md', 'mailto:a@b.c', 'file:///etc/hosts',
    '//cdn.example.com/a.css', '#section', '/api/sessions', '',
    '目录/', 'readme', 'a<b.md',
  ])('不误伤非文件链接：%s', href => {
    expect(localFileHref(href)).toBeNull()
  })
})

describe('相对链接按基准目录拼绝对路径', () => {
  it.each([
    ['/home/u/proj', 'docs/a.md', '/home/u/proj/docs/a.md'],
    ['/home/u/proj/', './a.md', '/home/u/proj/a.md'],
    ['/home/u/proj', 'docs/../b.md', '/home/u/proj/b.md'],
    ['/home/u/proj', '../shared/c.md', '/home/u/shared/c.md'],
  ])('%s + %s → %s', (base, rel, want) => {
    expect(joinFilePath(base, rel)).toBe(want)
  })

  it('基准不是绝对路径时返回空串（调用方回退默认行为）', () => {
    expect(joinFilePath('', 'a.md')).toBe('')
    expect(joinFilePath('relative/dir', 'a.md')).toBe('')
  })
})
