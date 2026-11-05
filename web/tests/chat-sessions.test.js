import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ChatView from '../src/components/ChatView.vue'
import { api } from '../src/api.js'
import { setMode, store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/anim.js', () => ({ gsap: {}, animate: false, bubbleIn: vi.fn(), popIn: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

let wrapper
let fetchMock

async function mountChat() {
  wrapper = mount(ChatView, { global: { stubs: { teleport: true } } })
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

// slowStream 回一段"已经出了字但还没结束"的 SSE：这一轮卡在流式里，
// 用来验切走再切回来时进度还在、也没被打断。
function slowStream(text) {
  return new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(`data: {"delta":"${text}"}\n\n`))
    },
  }), { headers: { 'Content-Type': 'text/event-stream' } })
}

beforeEach(() => {
  localStorage.clear()
  Object.assign(store, { loggedIn: true, view: 'chat', mode: 'life' })
  toast.mockClear()
  api.mockReset().mockImplementation(path => {
    if (path.startsWith('/api/chat/history')) return Promise.resolve({ session: 'web', messages: [] })
    if (path === '/api/context') return Promise.resolve({ enabled: false })
    return Promise.resolve({ message: '好' })
  })
  fetchMock = vi.fn().mockResolvedValue(new Response('data: {"done":true}\n\n', {
    headers: { 'Content-Type': 'text/event-stream' },
  }))
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  localStorage.clear()
  vi.restoreAllMocks()
})

// 工作模式不再有聊天入口（干活走工作区：新建会话时选内置 friday 或外部 agent），
// 这一页只剩生活模式那条主会话——没有会话栏、归属恒为 web、也不随模式切换变。
describe('聊天页只有主会话', () => {
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

  it('切到工作模式再切回来：归属没变、也不重拉历史，那一轮的半截回答还在', async () => {
    await mountChat()
    fetchMock.mockResolvedValue(slowStream('写到一半'))
    await send('慢慢说')
    expect(wrapper.text()).toContain('写到一半')
    api.mock.calls.length = 0
    setMode('work')
    await flushPromises()
    setMode('life')
    await flushPromises()
    // 聊天页不再监听模式：切模式只是把它藏起来（App 里 v-show），那一轮继续在后台收字，
    // 回来还是这段进度，也没有拿历史覆盖回去
    expect(wrapper.text()).toContain('写到一半')
    expect(api.mock.calls.map(([, path]) => path)).not.toContain('/api/chat/history?session=web')
  })

  it('这一页不再维护会话列表：不请求 /api/sessions', async () => {
    await mountChat()
    setMode('work')
    await flushPromises()
    expect(api.mock.calls.map(([, path]) => path)).not.toContain('/api/sessions')
  })
})
