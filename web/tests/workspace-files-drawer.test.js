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

// 抽屉是挂在页面-level 的单例（WorkspaceView 只渲染一个 ref），切项目/切会话时
// 组件不重建，只换 props。以下三条守的是「本轮改动」面板跟着当前会话走：
// 关着抽屉换会话/跑新一轮时，缓存必须作废，不能把上一个项目的文件列表留在屏上。
const threadB = { id: 'thB', workspaceId: 'wsB', agent: 'codex', cwd: '/home/u/other' }
const messagesB = [
  { role: 'bot', text: 'B 项目的一轮', changes: { turnId: 'tB1', at: '2026-11-04T09:00:00Z',
    files: [{ path: 'other.go', status: 'M', diff: '@@ -1 +1 @@\n-old\n+new' }] } },
]

// changesFor：按「会话 id + 轮 id」回包，故意让 A/B 两个项目的文件不重名，
// 这样面板里出现哪个名字就说明它读的是哪个项目。
function changesFor(method, url) {
  if (url.includes('/files')) return { path: '', entries: [] }
  const m = url.match(/threads\/([^/]+)\/changes\?turn=(.*)$/)
  if (!m) return {}
  const [, th, turn] = m
  const src = th === 'thB' ? messagesB[0].changes
    : (turn === 't1' ? messages[0].changes : messages[1].changes)
  return { changes: { ...src, turnId: turn || src.turnId, snapshot: true, files: src.files } }
}

async function openChangesTab(w) {
  await w.findAll('.wfd-tabs button').find(b => b.text().includes('本轮改动')).trigger('click')
  await flushPromises()
}

describe('文件抽屉 · 切项目/切会话', () => {
  beforeEach(() => {
    api.mockReset()
    api.mockImplementation(async (method, url) => changesFor(method, url))
  })

  it('关着抽屉换到另一个项目的会话，重开后本轮改动是新项目的', async () => {
    const w = mount(WorkspaceFilesDrawer, {
      props: { thread, messages, workspaceId: 'ws1' },
      global: { stubs: { teleport: true } },
    })
    w.vm.open('changes')
    await flushPromises()
    expect(w.text()).toContain('b.go')
    w.vm.close()

    await w.setProps({ thread: threadB, messages: messagesB, workspaceId: 'wsB' })
    api.mockClear()
    w.vm.open('files') // 会话头部的「工作区文件」按钮：默认落在文件 tab
    await flushPromises()
    await openChangesTab(w)

    expect(api).toHaveBeenCalledWith('GET', '/api/acp/threads/thB/changes?turn=tB1')
    expect(w.text()).toContain('other.go')
    expect(w.text()).not.toContain('b.go')
  })

  it('同一项目里换会话，也不会沿用上一条会话那一轮的改动', async () => {
    const w = mount(WorkspaceFilesDrawer, {
      props: { thread, messages, workspaceId: 'ws1' },
      global: { stubs: { teleport: true } },
    })
    w.vm.open('changes')
    await flushPromises()
    w.vm.close()

    // 同 cwd、不同会话 id，且那条会话只跑过一轮（t1）
    const threadSame = { ...thread, id: 'thSame' }
    await w.setProps({ thread: threadSame, messages: [messages[0]] })
    api.mockClear()
    w.vm.open('files')
    await flushPromises()
    await openChangesTab(w)

    expect(api).toHaveBeenCalledWith('GET', '/api/acp/threads/thSame/changes?turn=t1')
    expect(w.text()).toContain('a.go')
    expect(w.text()).not.toContain('b.go')
  })

  it('关着抽屉又跑完一轮，重开后落在最近一轮而不是缓存那轮', async () => {
    const w = mount(WorkspaceFilesDrawer, {
      props: { thread, messages: [messages[0]], workspaceId: 'ws1' },
      global: { stubs: { teleport: true } },
    })
    w.vm.open('changes')
    await flushPromises()
    expect(w.text()).toContain('a.go')
    w.vm.close()

    await w.setProps({ messages }) // 本轮结束后投影里多了 t2
    api.mockClear()
    w.vm.open('files')
    await flushPromises()
    await openChangesTab(w)

    expect(api).toHaveBeenCalledWith('GET', '/api/acp/threads/th1/changes?turn=t2')
    expect(w.text()).toContain('c.go')
  })

  it('切会话时上一会话的改动回包晚到也不能盖掉当前面板', async () => {
    let release
    const gate = new Promise(resolve => { release = resolve })
    api.mockImplementation(async (method, url) => {
      if (url.includes('/threads/th1/changes')) {
        await gate
        return changesFor(method, url)
      }
      return changesFor(method, url)
    })
    const w = mount(WorkspaceFilesDrawer, {
      props: { thread, messages, workspaceId: 'ws1' },
      global: { stubs: { teleport: true } },
    })
    w.vm.open('changes') // A 会话的这一请求故意不返回
    await flushPromises()

    // 抽屉开着就切到 B 会话：watcher 立刻按 B 重拉，B 先回包
    await w.setProps({ thread: threadB, messages: messagesB, workspaceId: 'wsB' })
    await flushPromises()
    expect(w.text()).toContain('other.go')

    // 此时 A 的迟到回包才落地，不能把面板改回 A 项目的文件
    release()
    await flushPromises()
    expect(w.text()).toContain('other.go')
    expect(w.text()).not.toContain('b.go')
  })
})
