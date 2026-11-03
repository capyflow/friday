import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import WorkspaceFilesDrawer from '../src/components/WorkspaceFilesDrawer.vue'
import SelectBox from '../src/components/SelectBox.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

// 抽屉里的「本轮改动」下拉：轮次文案由 turnLabel 生成，选中哪轮就该拉哪轮的 diff。
// 这条用例守的是 SelectBox 化之后的收口方式（@update:model-value 直接收到 value，
// 不是原生 select 的 change 事件），换错参数不会报错、只会永远停在最近一轮。
const thread = { id: 'th1', workspaceId: 'ws1', agent: 'codex', cwd: '/home/u/proj' }
const messages = [
  { role: 'bot', text: '第一轮', changes: { turnId: 't1', at: '2026-11-03T13:02:00Z',
    files: [{ path: 'a.go', status: 'M' }] } },
  { role: 'bot', text: '第二轮', changes: { turnId: 't2', at: '2026-11-03T14:20:00Z',
    files: [{ path: 'b.go', status: 'M' }, { path: 'c.go', status: 'A' }] } },
]

function show() {
  return mount(WorkspaceFilesDrawer, {
    props: { thread, messages, workspaceId: 'ws1' },
    global: { stubs: { teleport: true } },
  })
}

describe('文件抽屉 · 本轮改动', () => {
  beforeEach(() => {
    api.mockReset()
    api.mockImplementation(async (method, url) => {
      const turn = decodeURIComponent((url.match(/[?&]turn=(.*)$/) || [])[1] || '')
      const src = turn === 't1' ? messages[0].changes : messages[1].changes
      return { changes: { ...src, turnId: turn || src.turnId, snapshot: true, files: src.files } }
    })
  })

  it('两轮以上才给轮次下拉，选「第 2 轮」就按那一轮拉 diff', async () => {
    const w = show()
    w.vm.open('changes')
    await flushPromises()
    // 打开时默认落在最近一轮（t2）
    expect(api).toHaveBeenCalledWith('GET', '/api/acp/threads/th1/changes?turn=t2')

    const box = w.findAllComponents(SelectBox).find(s => s.props('ariaLabel') === '选择轮次')
    expect(box).toBeTruthy()
    expect(box.props('modelValue')).toBe('t2')

    await w.get('.selbox-trigger[aria-label="选择轮次"]').trigger('click')
    await flushPromises()
    const labels = w.findAll('.selbox-opt').map(o => o.text())
    expect(labels[0]).toContain('最近一轮 · 2 个文件')
    const older = w.findAll('.selbox-opt').find(o => o.text().startsWith('第 2 轮'))
    await older.trigger('click')
    await flushPromises()

    expect(box.props('modelValue')).toBe('t1')
    expect(api).toHaveBeenCalledWith('GET', '/api/acp/threads/th1/changes?turn=t1')
    expect(w.text()).toContain('a.go')
  })

  it('只有一轮时不铺下拉，写死那一轮的文本', async () => {
    api.mockImplementation(async () => ({ changes: { turnId: 't2', files: [], snapshot: true } }))
    const w = mount(WorkspaceFilesDrawer, {
      props: { thread, messages: [messages[1]], workspaceId: 'ws1' },
      global: { stubs: { teleport: true } },
    })
    w.vm.open('changes')
    await flushPromises()
    expect(w.find('.wfd-turn-select').exists()).toBe(false)
    expect(w.get('.wfd-turn-label').text()).toContain('最近一轮 · 2 个文件')
  })
})
