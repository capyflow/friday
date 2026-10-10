import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ChatView from '../src/components/ChatView.vue'
import { api } from '../src/api.js'
import { store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/anim.js', () => ({ gsap: {}, animate: false, bubbleIn: vi.fn(), popIn: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

let wrapper
let fetchMock
let revoke

function stream(events) {
  return new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(events.map(event => `data: ${JSON.stringify(event)}\n\n`).join('')))
      controller.close()
    },
  }), { headers: { 'Content-Type': 'text/event-stream' } })
}

async function mountChat() {
  wrapper = mount(ChatView)
  await flushPromises()
  return wrapper
}

function modeButton(mode) {
  return wrapper.findAll('.pop-item').find(button => button.find('.pl').text() === mode)
}

async function attach() {
  const file = new File(['photo'], 'photo.png', { type: 'image/png' })
  Object.defineProperty(wrapper.get('input[type=file]').element, 'files', { configurable: true, value: [file] })
  await wrapper.get('input[type=file]').trigger('change')
  await flushPromises()
}

async function send(text) {
  if (text !== undefined) await wrapper.get('textarea').setValue(text)
  await wrapper.get('[aria-label="发送"]').trigger('click')
  await flushPromises()
}

beforeEach(() => {
  store.loggedIn = true
  store.view = 'chat'
  api.mockReset().mockResolvedValue({ messages: [], message: '已清除' })
  toast.mockClear()
  fetchMock = vi.fn().mockResolvedValue(stream([{ delta: '回复' }, { done: true }]))
  vi.stubGlobal('fetch', fetchMock)
  revoke = vi.fn()
  const NativeURL = URL
  vi.stubGlobal('URL', class extends NativeURL {
    static createObjectURL = vi.fn(() => 'blob:photo')
    static revokeObjectURL = revoke
  })
  vi.stubGlobal('createImageBitmap', vi.fn().mockResolvedValue({ width: 20, height: 20, close: vi.fn() }))
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({ fillRect: vi.fn(), drawImage: vi.fn() })
  vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation(function (callback) {
    callback(new Blob(['jpeg'], { type: 'image/jpeg' }))
  })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
})

describe('聊天发送和失败恢复', () => {
  it.each([[null, '任务'], ['计划', '【计划】任务'], ['目标', '【目标】任务']])('发送 %s 模式', async (mode, expected) => {
    await mountChat()
    if (mode) await modeButton(mode).trigger('click')
    await send('任务')
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).message).toBe(expected)
    expect(wrapper.get('textarea').element.value).toBe('')
    expect(wrapper.get('[aria-label="发送"]').element.disabled).toBe(true)
    expect(wrapper.find('.mrow.user').text()).toBe(expected)
    expect(wrapper.find('.mrow.bot').text()).toBe('回复')
  })

  it('纯图片发送保留消息 URL，清除消息时才释放', async () => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ body: { id: 'photo.jpg' } })))
    await mountChat()
    await attach()
    expect(wrapper.get('#attachPreviews img').attributes('src')).toBe('blob:photo')
    await send()
    expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toMatchObject({ message: '', attachments: ['photo.jpg'] })
    expect(wrapper.get('.chat-img').attributes('src')).toBe('blob:photo')
    expect(wrapper.find('#attachPreviews img').exists()).toBe(false)
    expect(revoke).not.toHaveBeenCalled()
    await wrapper.get('[aria-label="更多操作"]').trigger('click')
    await wrapper.findAll('[role=menuitem]').find(button => button.text().includes('清除上下文')).trigger('click')
    await flushPromises()
    expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:photo')
  })

  it('上传失败恢复文本、模式与附件，可重试成功', async () => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ body: { error: '磁盘已满' } }), { status: 500 }))
    await mountChat()
    await attach()
    await modeButton('计划').trigger('click')
    await send('  制定计划  ')
    expect(wrapper.get('textarea').element.value).toBe('  制定计划  ')
    expect(wrapper.get('.mode-name').text()).toBe('计划模式')
    expect(wrapper.get('#attachPreviews img').attributes('src')).toBe('blob:photo')
    expect(wrapper.get('[aria-label="发送"]').element.disabled).toBe(false)
    expect(toast).toHaveBeenCalledWith(expect.stringContaining('磁盘已满'), true)
    expect(revoke).not.toHaveBeenCalled()
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ body: { id: 'photo.jpg' } })))
    await send()
    expect(JSON.parse(fetchMock.mock.calls[2][1].body).message).toBe('【计划】制定计划')
    expect(wrapper.find('#attachPreviews img').exists()).toBe(false)
    expect(revoke).not.toHaveBeenCalled()
  })

  it.each([
    () => Promise.reject(new Error('断网')),
    () => Promise.resolve(new Response('', { status: 500 })),
    () => Promise.resolve(stream([{ done: true, error: '模型不可用' }])),
    () => Promise.resolve(stream([{ delta: '部分回复' }])),
  ])('请求或流式失败保留输入，退出忙碌状态', async response => {
    fetchMock.mockImplementationOnce(response)
    await mountChat()
    await modeButton('目标').trigger('click')
    await send('完成目标')
    expect(wrapper.get('textarea').element.value).toBe('完成目标')
    expect(wrapper.get('.mode-name').text()).toBe('目标模式')
    expect(wrapper.get('textarea').element.disabled).toBe(false)
    expect(wrapper.get('[aria-label="发送"]').element.disabled).toBe(false)
    expect(wrapper.find('.typing').exists()).toBe(false)
    expect(wrapper.find('.cursor').exists()).toBe(false)
    expect(wrapper.find('.mrow.err').text()).toContain('可以重试')
  })

  it('请求失败时恢复已上传的图片，不提前释放预览', async () => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ body: { id: 'photo.jpg' } })))
      .mockRejectedValueOnce(new Error('断网'))
    await mountChat()
    await attach()
    await send('看图')
    expect(wrapper.get('#attachPreviews img').attributes('src')).toBe('blob:photo')
    expect(revoke).not.toHaveBeenCalled()
    await wrapper.get('[aria-label="移除图片"]').trigger('click')
    expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:photo')
  })

  it('等待回复时阻止重复发送，卸载取消请求并释放图片', async () => {
    let controller
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ body: { id: 'photo.jpg' } })))
      .mockResolvedValueOnce(new Response(new ReadableStream({ start(value) { controller = value } })))
    await mountChat()
    await attach()
    await modeButton('计划').trigger('click')
    await send('计划任务')
    expect(wrapper.get('textarea').element.disabled).toBe(true)
    await wrapper.get('[aria-label="发送"]').trigger('click')
    expect(fetchMock).toHaveBeenCalledTimes(2)
    const signal = fetchMock.mock.calls[1][1].signal
    wrapper.unmount()
    wrapper = null
    expect(signal.aborted).toBe(true)
    expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:photo')
    controller.close()
    await flushPromises()
  })

  it('晚到的历史响应不能覆盖刚发送的消息', async () => {
    let finishHistory
    api.mockReturnValueOnce(new Promise(resolve => { finishHistory = resolve }))
    await mountChat()
    await send('新消息')
    finishHistory({ messages: [{ role: 'user', text: '旧消息' }] })
    await flushPromises()
    expect(wrapper.find('.mrow.user').text()).toBe('新消息')
    expect(wrapper.text()).not.toContain('旧消息')
  })
})
