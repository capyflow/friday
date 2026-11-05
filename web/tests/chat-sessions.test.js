import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ChatView from '../src/components/ChatView.vue'
import { api } from '../src/api.js'
import { setMode, store } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/anim.js', () => ({ gsap: {}, animate: false, bubbleIn: vi.fn(), popIn: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

let wrapper
let fetchMock
let workSessions

// sessionList 是后端 /api/sessions 的回包：主会话 + 工作会话 + 一条后台会话，
// 聊天页的会话栏只该看见中间那类。
function sessionList() {
  return {
    sessions: [
      { key: 'web', title: '主会话', kind: 'life', messages: 12, updatedAt: '2026-11-05T02:00:00Z' },
      ...workSessions,
      { key: 'task', kind: 'internal', messages: 3, updatedAt: '2026-11-05T01:00:00Z' },
    ],
  }
}

async function mountChat() {
  wrapper = mount(ChatView)
  await flushPromises()
  return wrapper
}

async function send(text) {
  if (text !== undefined) await wrapper.get('textarea').setValue(text)
  await wrapper.get('[aria-label="发送"]').trigger('click')
  await flushPromises()
}

function sentBody() {
  const call = fetchMock.mock.calls.find(([url]) => url === '/api/chat')
  return call ? JSON.parse(call[1].body) : null
}

function sessionRows() {
  return wrapper.findAll('#view-chat .ag-item')
}

beforeEach(() => {
  localStorage.clear()
  Object.assign(store, { loggedIn: true, view: 'chat', mode: 'life', chatBusy: false })
  workSessions = [
    { key: 'work-20261105-100000-aa', title: '写周报', kind: 'work', messages: 4, updatedAt: '2026-11-05T02:00:00Z', hasTitle: true },
    { key: 'work-20261105-090000-bb', title: '整理发票', kind: 'work', messages: 2, updatedAt: '2026-11-05T01:00:00Z', hasTitle: true },
  ]
  api.mockReset().mockImplementation((method, path, body) => {
    if (path.startsWith('/api/chat/history')) {
      return Promise.resolve({ session: path.split('=')[1] || 'web', messages: [] })
    }
    if (path === '/api/context') return Promise.resolve({ enabled: false })
    if (method === 'GET' && path === '/api/sessions') return Promise.resolve(sessionList())
    if (method === 'POST' && path === '/api/sessions') {
      // 后端建好就回带，列表里也立刻看得见（自动名还没内容时是「新会话」）
      const created = { key: 'work-20261105-110000-cc', title: '新会话', kind: 'work', messages: 0 }
      workSessions = [created, ...workSessions]
      return Promise.resolve({ session: created })
    }
    if (method === 'POST' && path.startsWith('/api/sessions/')) {
      const key = decodeURIComponent(path.slice('/api/sessions/'.length))
      const title = (body?.title || '').trim()
      return Promise.resolve({ session: { key, kind: 'work', title: title || '新会话',
        messages: 4, updatedAt: '2026-11-05T02:00:00Z', hasTitle: !!title } })
    }
    if (method === 'DELETE') return Promise.resolve({ message: '已删除' })
    return Promise.resolve({ message: '好' })
  })
  fetchMock = vi.fn().mockResolvedValue(new Response('data: {"done":true}\n\n', {
    headers: { 'Content-Type': 'text/event-stream' },
  }))
  vi.stubGlobal('fetch', fetchMock)
  vi.spyOn(window, 'confirm').mockReturnValue(true)
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  localStorage.clear()
  vi.restoreAllMocks()
})

describe('生活模式保持单会话', () => {
  it('没有会话栏，对话固定打在 web 主会话上', async () => {
    await mountChat()
    expect(wrapper.find('.ag-rail').exists()).toBe(false)
    expect(wrapper.get('#chatHead .title').text()).toBe('小五')
    await send('提醒我喝水')
    expect(sentBody().session).toBe('web')
  })

  it('拉历史与上下文都带 session=web', async () => {
    await mountChat()
    const paths = api.mock.calls.map(([, path]) => path)
    expect(paths).toContain('/api/chat/history?session=web')
    expect(paths).toContain('/api/context?session=web')
  })
})

describe('工作模式的会话栏', () => {
  it('切到工作模式才出现会话列表，且只列工作会话', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    expect(sessionRows()).toHaveLength(2)
    expect(sessionRows()[0].text()).toContain('写周报')
    expect(wrapper.text()).not.toContain('主会话')
    expect(wrapper.text()).not.toContain('task')
  })

  it('默认选中最近活动的那条，并把它的名放到页头', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    expect(wrapper.get('#chatHead .title').text()).toBe('写周报')
    await send('先把本周结论写三行')
    expect(sentBody().session).toBe('work-20261105-100000-aa')
  })

  it('点另一条会话就换归属，历史按新 key 重拉', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    await wrapper.get('textarea').setValue('写给「写周报」的半截话')
    api.mock.calls.length = 0
    await sessionRows()[1].trigger('click')
    await flushPromises()
    expect(api.mock.calls.map(([, path]) => path)).toContain('/api/chat/history?session=work-20261105-090000-bb')
    // 草稿不跟着换会话跑：上面那句写给「写周报」的话不该冒到「整理发票」里
    expect(wrapper.get('textarea').element.value).toBe('')
    await send('把发票按月份归一下')
    expect(sentBody().session).toBe('work-20261105-090000-bb')
  })

  it('选中的会话记在本地，下次回到工作模式接着这条', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    await sessionRows()[1].trigger('click')
    await flushPromises()
    expect(localStorage.getItem('friday-chat-session')).toBe('work-20261105-090000-bb')

    // 切回生活再切回来：落回上次那条，而不是列表第一条
    setMode('life')
    await flushPromises()
    setMode('work')
    await flushPromises()
    expect(wrapper.get('#chatHead .title').text()).toBe('整理发票')
  })

  it('新建会话直接选中它，第一条消息就发进这条', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    await wrapper.get('[aria-label="新建会话"]').trigger('click')
    await flushPromises()
    expect(api.mock.calls).toContainEqual(['POST', '/api/sessions', {}])
    expect(wrapper.get('#chatHead .title').text()).toBe('新会话')
    // 刚建的会话一条都还没有：行上说"还没有内容"，不拿创建时刻算"多久之前"
    expect(sessionRows()[0].text()).toContain('还没有内容')
    expect(sessionRows()[1].text()).toContain('4 条')
    await send('这条是季度复盘')
    expect(sentBody().session).toBe('work-20261105-110000-cc')
  })

  it('删除当前会话后落到剩下的第一条', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    await sessionRows()[0].find('.ag-del').trigger('click')
    await flushPromises()
    expect(api.mock.calls).toContainEqual(['DELETE', '/api/sessions/work-20261105-100000-aa'])
    expect(sessionRows()).toHaveLength(1)
    expect(wrapper.get('#chatHead .title').text()).toBe('整理发票')
  })

  it('双击会话名可以改名，回车提交后端', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    await sessionRows()[0].find('.ag-title span').trigger('dblclick')
    const input = wrapper.get('.ag-title-edit')
    expect(input.element.value).toBe('写周报')
    await input.setValue('周报 v2')
    await input.trigger('keydown.enter')
    await flushPromises()
    expect(api.mock.calls).toContainEqual(['POST', '/api/sessions/work-20261105-100000-aa', { title: '周报 v2' }])
    expect(sessionRows()[0].text()).toContain('周报 v2')
  })
})

describe('工作模式下还没有会话', () => {
  beforeEach(() => { workSessions = [] })

  it('明说还没有会话，给一个新建入口，不把主会话顶上来', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    expect(wrapper.text()).toContain('工作模式还没有会话')
    expect(wrapper.text()).not.toContain('你好，我是小五')
    // 没有会话就没有对话归属：整条输入栏实底禁用，请求发不出去
    expect(wrapper.get('textarea').attributes('disabled')).toBeDefined()
    expect(wrapper.get('.inputbar').attributes('aria-busy')).toBe('true')
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('点空态里的按钮建第一条，之后就能正常发', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    await wrapper.get('.welcome .suggest').trigger('click')
    await flushPromises()
    await send('开工')
    expect(sentBody().session).toBe('work-20261105-110000-cc')
  })
})
