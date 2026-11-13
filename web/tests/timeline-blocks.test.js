import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import TimelineBlocks from '../src/components/TimelineBlocks.vue'

// 时间线的工具行按来源分两种口径：ChatView（Friday 自己的 agent）传 own-tools 走展示目录，
// ACP 会话页不传、照直显示外部 agent 给的工具名。这几条守的是 prop 有没有一路接到
// 每一行（含叠里的）；目录本身对不对由 tool-catalog.test.js 管。
const tool = (id, name, status = 'ok') => ({ kind: 'tool', id, name, text: 'arg', out: 'result', status, open: false, round: 0 })
const think = (text, meta = '') => ({ kind: 'think', text, meta, open: false, round: 0 })

function mountRows(blocks, props = {}) {
  return mount(TimelineBlocks, { props: { blocks, ...props }, global: { stubs: { teleport: true } } })
}
function rowLabels(wrapper) {
  return wrapper.findAll('.tool .tool-head .step-name').map(n => n.text())
}

describe('时间线工具行的来源口径', () => {
  it('自己 agent 的调用给中文名与专属图标', () => {
    const w = mountRows([tool('c1', 'read_file')], { ownTools: true })
    expect(rowLabels(w)).toEqual(['读取文件'])
    // 文档图标是两条路径（轮廓 + 折角），扳手只有一条
    expect(w.get('.tool .step-ico svg').findAll('path')).toHaveLength(2)
  })

  it('ACP 会话的调用照直显示工具名，配默认扳手', () => {
    const w = mountRows([tool('c1', 'read_file')])
    expect(rowLabels(w)).toEqual(['read_file'])
    expect(w.get('.tool .step-ico svg').findAll('path')).toHaveLength(1)
  })

  // 名字撞上也不套：codex 自己也有 read_file / web_search，那不是我们的同名工具。
  it('一叠里的每条也各自按来源口径渲染', () => {
    const many = [tool('c1', 'read_file'), tool('c2', 'web_search')]
    expect(rowLabels(mountRows(many, { ownTools: true }))).toEqual(['读取文件', '联网搜索'])
    expect(rowLabels(mountRows(many))).toEqual(['read_file', 'web_search'])
    expect(mountRows(many).get('.tool-stack > .tool-head').text()).toBe('执行工具 2 次')
  })
})

describe('连续的思考与工具行并成一个过程行', () => {
  // 挂在 body 上：v-show 切换后 isVisible 才可信
  function mountAttached(blocks, props = {}) {
    return mount(TimelineBlocks, { props: { blocks, ...props }, attachTo: document.body })
  }

  it('收起只报次数（圆点分隔），失败数说在前面，成员行只是藏起不是摘除', () => {
    const w = mountAttached([
      think('先想一步', '耗时 1.0s'),
      tool('c1', 'read_file'),
      think('再想一步'),
      tool('c2', 'web_search'),
      tool('c3', 'edit_file', 'fail'),
    ])
    expect(w.get('.proc > .tool-head .step-name').text()).toBe('2 次思考 · 3 次工具调用 · 1 次出错')
    expect(w.get('.proc').classes()).toContain('is-fail')
    // 成员行仍挂载在摘要之后，收起时不可见
    expect(w.findAll('.think')).toHaveLength(2)
    expect(w.findAll('.tool')).toHaveLength(3)
    expect(w.get('.think-head').isVisible()).toBe(false)
    w.unmount()
  })

  it('点摘要展开全程：思考行可看，工具叠可再展开；再点一次收起', async () => {
    const w = mountAttached([think('先想一步', '耗时 1.0s'), tool('c1', 'read_file'),
      think('再想一步'), tool('c2', 'web_search'), tool('c3', 'web_search')])
    await w.get('.proc > .tool-head').trigger('click')
    expect(w.get('.think-head').isVisible()).toBe(true)
    // 叠的口径在过程行里照旧：连续工具仍是「执行工具 N 次」
    expect(w.get('.proc-body .tool-stack > .tool-head').text()).toContain('执行工具 2 次')
    await w.get('.proc > .tool-head').trigger('click')
    expect(w.get('.think-head').isVisible()).toBe(false)
    w.unmount()
  })

  it('只有一行的过程维持原样：单条思考、纯工具序列都不多套一层折叠', () => {
    expect(mountRows([think('只有一句')]).find('.proc').exists()).toBe(false)
    const pair = [tool('c1', 'read_file'), tool('c2', 'web_search')]
    expect(mountRows(pair).find('.proc').exists()).toBe(false)
    expect(mountRows(pair).get('.tool-stack > .tool-head').text()).toBe('执行工具 2 次')
  })

  it('正文打断后另起一个过程行：下一轮的思考不再并进上一段', () => {
    const w = mountAttached([think('第一轮'), tool('c1', 'read_file'),
      { kind: 'text', text: '先做一步' }, think('第二轮')])
    expect(w.findAll('.proc')).toHaveLength(1)
    // 正文之后的思考只剩一行，保持裸行可见
    expect(w.findAll('.think-head')[1].isVisible()).toBe(true)
    w.unmount()
  })

  it('纯思考的连续行也收（多轮思考无工具）', () => {
    const w = mountRows([think('一', '耗时 1.0s'), think('二', '耗时 2.0s')])
    expect(w.get('.proc > .tool-head .step-name').text()).toBe('2 次思考')
  })

  it('流式期间收着的摘要标题实时跟随思考预览，灯泡呼吸', () => {
    const w = mountRows([think('先想想用户要什么'), tool('c1', 'read_file')], { streaming: true, round: 0 })
    expect(w.get('.proc > .tool-head .step-name').text()).toBe('先想想用户要什么')
    expect(w.get('.proc .step-ico').classes()).toContain('live')
  })
})

describe('正文里的文件链接转预览', () => {
  it('相对链接按 baseDir 拼绝对路径交给预览，外链保持原生行为不拦', async () => {
    const w = mountRows([{
      kind: 'text',
      text: '已整理好：[客户端发现服务器接入说明](docs/client.md)。另见 [/tmp/a.md](/tmp/a.md) 与 [官网](https://example.com)。',
    }], { baseDir: '/home/u/proj' })
    await w.get('a[href="docs/client.md"]').trigger('click')
    await w.get('a[href="/tmp/a.md"]').trigger('click')
    const ext = w.get('a[href^="https"]')
    let extPrevented = null
    ext.element.addEventListener('click', e => { extPrevented = e.defaultPrevented })
    await ext.trigger('click')
    expect(w.emitted('preview')).toEqual([['/home/u/proj/docs/client.md'], ['/tmp/a.md']])
    // 外链没被拦（默认行为留给 App.vue 的桌面壳委托 / 浏览器原生导航）
    expect(extPrevented).toBe(false)
  })

  it('没有 baseDir 时相对链接不拦（回退默认行为），chip 照常可点', async () => {
    const w = mountRows([{
      kind: 'text',
      text: '相对 [a](docs/a.md)，chip `</tmp/b.md>` '.replace('</tmp/b.md>', '/tmp/b.md') + '都在。',
    }])
    await w.get('a[href="docs/a.md"]').trigger('click')
    expect(w.emitted('preview')).toBeUndefined()
    await w.get('.file-chip').trigger('click')
    expect(w.emitted('preview')).toEqual([['/tmp/b.md']])
  })
})

describe('代码块复制按钮', () => {
  const codeBlock = [{ kind: 'text', text: '```bash\nosascript -e "quit"\n```' }]

  function stubClipboard(writeText) {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } })
  }

  // flashCopy 有 isConnected 守卫（防流式重渲染换掉节点），必须挂在真实文档上才生效
  function mountAttached() {
    return mount(TimelineBlocks, { props: { blocks: codeBlock }, attachTo: document.body })
  }

  it('点右上角复制：写入剪贴板并提示已复制', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    stubClipboard(writeText)
    const w = mountAttached()
    expect(w.find('.md-copy').exists()).toBe(true)
    await w.get('.md-copy').trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('osascript -e "quit"\n')
    expect(w.get('.md-copy').text()).toBe('已复制')
    w.unmount()
    delete navigator.clipboard
  })

  it('写剪贴板失败：提示复制失败，不假装成功', async () => {
    stubClipboard(vi.fn().mockRejectedValue(new Error('denied')))
    const w = mountAttached()
    await w.get('.md-copy').trigger('click')
    await flushPromises()
    expect(w.get('.md-copy').text()).toBe('复制失败')
    w.unmount()
    delete navigator.clipboard
  })
})
