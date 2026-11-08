import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import TimelineBlocks from '../src/components/TimelineBlocks.vue'

// 时间线的工具行按来源分两种口径：ChatView（Friday 自己的 agent）传 own-tools 走展示目录，
// ACP 会话页不传、照直显示外部 agent 给的工具名。这几条守的是 prop 有没有一路接到
// 每一行（含叠里的）；目录本身对不对由 tool-catalog.test.js 管。
const tool = (id, name) => ({ kind: 'tool', id, name, text: 'arg', out: 'result', status: 'ok', open: false, round: 0 })

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
