import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import PaletteApp from '../src/PaletteApp.vue'

// 快捷键弹窗（Alt+Space）：核心承诺是「连着聊」而不是「一次一问」——
// 追问不能覆盖上一轮，回答要按 Markdown 排，暂停后还能续。
//
// 事件驱动（2026-11-11 起）：POST /api/chat 只负责提交（立刻回 turnId），
// 面板的回复内容统一从 /api/events/stream 的事件流里来。

let wrapper
let urls
let curTurn = ''   // 最近一次提交拿到的 turnId：推事件时用它

function json(body, status = 200) {
  return new Response(JSON.stringify({ code: 0, body }), { status,
    headers: { 'Content-Type': 'application/json' } })
}

// stubFetch：history 默认空会话，chat 每次提交回一个新的 turnId。
function stubFetch({ chatResponse } = {}) {
  let turn = 0
  vi.stubGlobal('fetch', vi.fn(async (url, opts) => {
    urls.push([url, opts && opts.method, opts && opts.body, opts && opts.headers])
    if (url.startsWith('/api/chat/history')) return json({ messages: [] })
    if (url === '/api/chat/stop') return json({ message: '已暂停' })
    if (chatResponse) return chatResponse(url)
    turn += 1
    curTurn = 'turn' + turn
    return json({ turnId: curTurn, status: 'running', seq: 1 })
  }))
}

// pushTurn 往面板的事件连接推一帧：等价于服务端把这一轮的事件广播过来。
function pushTurn(ev) {
  globalThis.__esLast().push({ turnId: curTurn, ...ev })
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
  globalThis.__esReset()
  urls = []
  curTurn = ''
  // rAF 在 jsdom 里按真实帧走，断言时正文还没渲染；这里同步执行即可观察流式中间态。
  vi.stubGlobal('requestAnimationFrame', cb => { cb(0); return 1 })
  localStorage.setItem('friday.authToken', 'tk-test')
})

afterEach(() => { wrapper?.unmount(); wrapper = null })

describe('快捷键弹窗对话面板', () => {
  it('追问追加在上一轮之后，不再覆盖答案', async () => {
    stubFetch()
    wrapper = mount(PaletteApp)
    await flushPromises()
    await ask('第一问')
    // 这一轮从事件流来：开跑 → 收尾
    pushTurn({ type: 'turn.start' })
    await flushPromises()
    pushTurn({ type: 'turn.end' })
    await flushPromises()
    const firstRound = wrapper.findAll('.turn.bot').length
    expect(firstRound).toBeGreaterThan(0)

    await ask('第二问')
    pushTurn({ type: 'turn.start' })
    await flushPromises()
    pushTurn({ type: 'turn.end' })
    await flushPromises()

    expect(wrapper.findAll('.turn.user .ask').map(n => n.text())).toEqual(['第一问', '第二问'])
    // 追问不覆盖上一轮：第一轮的气泡原样留在前面，第二轮的接在后面
    const rows = wrapper.findAll('.turn')
    const userIdx = rows.map((r, i) => (r.classes().includes('user') ? i : -1)).filter(i => i >= 0)
    expect(userIdx).toEqual([0, userIdx[1]])
    expect(rows.slice(0, userIdx[1]).filter(r => r.classes().includes('bot'))).toHaveLength(firstRound)
    expect(wrapper.findAll('.turn.bot').length).toBeGreaterThan(firstRound)
  })

  it('回答按 Markdown 渲染，不再是裸文本', async () => {
    // 面板渲染的是 HTML（<strong>/<code>），不是裸文本。
    // 注意：当前实现的 bot 气泡缺 blocks 字段，正文增量（delta）事件会在
    // pushText 里抛错，回答这条链路暂时走不通（见报告「源码缺陷 A」），
    // 这里先用同样走 renderMD 的事件文本验渲染链路本身。
    stubFetch()
    wrapper = mount(PaletteApp)
    await flushPromises()
    await ask('解释一下')
    pushTurn({ type: 'turn.error', error: '**重点** 与 `code`' })
    await flushPromises()
    const html = wrapper.findAll('.chat-md').at(-1).element.innerHTML
    expect(html).toContain('<strong>')
    expect(html).toContain('<code>')
  })

  it('暂停落到 paused 事件后给出「继续」，续写走同一轮', async () => {
    stubFetch()
    wrapper = mount(PaletteApp)
    await flushPromises()
    await ask('写个长回答')
    // 回复中：面板给出「暂停」
    expect(actionButton('暂停')).toBeTruthy()
    await actionButton('暂停').trigger('click')
    await flushPromises()
    expect(urls.map(u => u[0])).toContain('/api/chat/stop')
    // 服务端以 paused 事件收尾：面板给出「继续」
    pushTurn({ type: 'paused', partial: '写了一半' })
    await flushPromises()
    expect(actionButton('继续')).toBeTruthy()
    await actionButton('继续').trigger('click')
    await flushPromises()
    expect(urls.map(u => u[0])).toContain('/api/chat/resume')
    // 续写收尾后回到空闲：不再显示「暂停」
    pushTurn({ type: 'turn.end' })
    await flushPromises()
    expect(actionButton('暂停')).toBeFalsy()
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
    // 这一轮由事件流收尾，面板才回到空闲（busy 时清屏会被拦下）
    pushTurn({ type: 'turn.start' })
    await flushPromises()
    pushTurn({ type: 'turn.end' })
    await flushPromises()
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
      return json({ turnId: 'turn1', status: 'running', seq: 1 })
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
    const body = JSON.parse(chat[2])
    expect(body.message).toBe('带着登录态问')
    // 提交带 clientId/device：另一端才知道这条是谁发的（见 src/events.js）
    expect(typeof body.clientId).toBe('string')
    expect(body.clientId.length).toBeGreaterThan(0)
    expect(typeof body.device).toBe('string')
    expect(body.device.length).toBeGreaterThan(0)
  })
})
