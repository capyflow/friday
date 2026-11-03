import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import PaletteApp from '../src/PaletteApp.vue'

// 快捷键弹窗（Alt+Space）：核心承诺是「连着聊」而不是「一次一问」——
// 追问不能覆盖上一轮，回答要按 Markdown 排，暂停后还能续。

let wrapper
let urls

function sse(events) {
  const enc = new TextEncoder()
  return new Response(new ReadableStream({
    start(controller) {
      for (const e of events) controller.enqueue(enc.encode(`data: ${JSON.stringify(e)}\n\n`))
      controller.close()
    },
  }), { headers: { 'Content-Type': 'text/event-stream' } })
}

function holdStream() {
  let controller
  const enc = new TextEncoder()
  const response = new Response(new ReadableStream({ start(c) { controller = c } }),
    { headers: { 'Content-Type': 'text/event-stream' } })
  return {
    response,
    push(event) { controller.enqueue(enc.encode(`data: ${JSON.stringify(event)}\n\n`)) },
    close() { controller.close() },
  }
}

function json(body, status = 200) {
  return new Response(JSON.stringify({ code: 0, body }), { status,
    headers: { 'Content-Type': 'application/json' } })
}

// stubFetch：history 默认空会话，chat 按 answers 依次给一轮回答。
function stubFetch({ answers = ['好的'], chatResponse } = {}) {
  let turn = 0
  vi.stubGlobal('fetch', vi.fn(async (url, opts) => {
    urls.push([url, opts && opts.method, opts && opts.body, opts && opts.headers])
    if (url.startsWith('/api/chat/history')) return json({ messages: [] })
    if (url === '/api/chat/stop') return json({ message: '已暂停' })
    if (chatResponse) return chatResponse(url)
    const reply = answers[Math.min(turn, answers.length - 1)]
    turn += 1
    return sse([{ delta: reply }, { done: true }])
  }))
}

function actionButton(label) {
  return wrapper.findAll('#actions button').find(b => b.text() === label)
}

async function ask(text) {
  await wrapper.get('#q').setValue(text)
  await wrapper.get('#q').trigger('keydown', { key: 'Enter' })
  await flushPromises()
}

beforeEach(() => {
  urls = []
  // rAF 在 jsdom 里按真实帧走，断言时正文还没渲染；这里同步执行即可观察流式中间态。
  vi.stubGlobal('requestAnimationFrame', cb => { cb(0); return 1 })
  localStorage.setItem('friday.authToken', 'tk-test')
})

afterEach(() => { wrapper?.unmount(); wrapper = null })

describe('快捷键弹窗对话面板', () => {
  it('追问追加在上一轮之后，不再覆盖答案', async () => {
    stubFetch({ answers: ['第一段', '第二段'] })
    wrapper = mount(PaletteApp)
    await flushPromises()
    await ask('第一问')
    await ask('第二问')
    expect(wrapper.findAll('.turn.user .ask').map(n => n.text())).toEqual(['第一问', '第二问'])
    expect(wrapper.findAll('.chat-md').map(n => n.text())).toEqual(['第一段', '第二段'])
  })

  it('回答按 Markdown 渲染，不再是裸文本', async () => {
    stubFetch({ answers: ['**重点** 与 `code`'] })
    wrapper = mount(PaletteApp)
    await flushPromises()
    await ask('解释一下')
    const html = wrapper.get('.chat-md').element.innerHTML
    expect(html).toContain('<strong>')
    expect(html).toContain('<code>')
  })

  it('暂停落到 paused 帧后给出「继续」，续写同一条回答', async () => {
    const held = holdStream()
    let first = true
    vi.stubGlobal('fetch', vi.fn(async (url, opts) => {
      urls.push([url, opts && opts.method])
      if (url.startsWith('/api/chat/history')) return json({ messages: [] })
      if (url === '/api/chat/stop') { held.push({ paused: true, partial: '写了一半' }); return json({}) }
      if (url === '/api/chat/resume') return sse([{ delta: '，接着写完' }, { done: true }])
      if (first) { first = false; return held.response }
      return sse([{ done: true }])
    }))
    wrapper = mount(PaletteApp)
    await flushPromises()
    await ask('写个长回答')
    held.push({ delta: '写了一半' })
    await flushPromises()
    expect(actionButton('暂停')).toBeTruthy()
    await actionButton('暂停').trigger('click')
    await flushPromises()
    held.close()
    await flushPromises()
    expect(wrapper.get('.chat-md').text()).toBe('写了一半')
    expect(actionButton('继续')).toBeTruthy()
    await actionButton('继续').trigger('click')
    await flushPromises()
    expect(wrapper.get('.chat-md').text()).toBe('写了一半，接着写完')
    expect(urls.map(u => u[0])).toContain('/api/chat/resume')
  })

  it('未登录时提示去主窗口登录，而不是抛连接异常', async () => {
    vi.stubGlobal('fetch', vi.fn(async url => {
      urls.push([url])
      if (url.startsWith('/api/chat/history')) return json({ messages: [] })
      return new Response('', { status: 403 })
    }))
    wrapper = mount(PaletteApp)
    await flushPromises()
    await ask('随便问一句')
    expect(wrapper.get('.chat-md').text()).toContain('还未登录')
    expect(wrapper.get('.turn.bot').classes()).toContain('err')
  })

  it('⌘L 只清本面板显示，不碰后端会话', async () => {
    stubFetch()
    wrapper = mount(PaletteApp)
    await flushPromises()
    await ask('记一下这件事')
    expect(wrapper.findAll('.turn')).toHaveLength(2)
    await wrapper.get('#q').trigger('keydown', { key: 'l', metaKey: true })
    await flushPromises()
    expect(wrapper.findAll('.turn')).toHaveLength(0)
    expect(urls.map(u => u[0])).not.toContain('/api/context/clear')
  })

  it('打开时回显主窗口的最近问答，接着聊不割裂', async () => {
    vi.stubGlobal('fetch', vi.fn(async url => {
      urls.push([url])
      if (url.startsWith('/api/chat/history')) {
        return json({ messages: [{ role: 'user', text: '明早叫我' },
          { role: 'assistant', text: '已经排好了' }] })
      }
      return sse([{ delta: '收到'}, { done: true }])
    }))
    wrapper = mount(PaletteApp)
    await flushPromises()
    expect(wrapper.findAll('.turn.user .ask').map(n => n.text())).toEqual(['明早叫我'])
    expect(wrapper.findAll('.chat-md').map(n => n.text())).toEqual(['已经排好了'])
  })

  it('发送带上共享会话的 Bearer 凭据', async () => {
    stubFetch()
    wrapper = mount(PaletteApp)
    await flushPromises()
    await ask('带着登录态问')
    const chat = urls.find(u => u[0] === '/api/chat')
    expect(chat[1]).toBe('POST')
    expect(chat[3].Authorization).toBe('Bearer tk-test')
    expect(JSON.parse(chat[2])).toEqual({ message: '带着登录态问' })
  })
})
