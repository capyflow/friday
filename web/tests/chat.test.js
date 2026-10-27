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

// holdStream 开一条不主动收尾的 SSE：测试自己 push/关闭，用来观察"流式进行中"的界面。
function holdStream() {
  let controller
  const enc = new TextEncoder()
  const response = new Response(new ReadableStream({
    start(c) { controller = c },
  }), { headers: { 'Content-Type': 'text/event-stream' } })
  return {
    response,
    push(event) { controller.enqueue(enc.encode(`data: ${JSON.stringify(event)}\n\n`)) },
    close() { controller.close() },
  }
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
  // 计划走协议字段（后端据此产清单并等确认）；目标模式暂仍传文字意图，待目标链路落地。
  it.each([
    [null, '任务', undefined],
    ['计划', '任务', 'plan'],
    ['目标', '任务', 'goal'],
  ])('发送 %s 模式', async (mode, expected, expectedMode) => {
    await mountChat()
    if (mode) await modeButton(mode).trigger('click')
    await send('任务')
    const body = JSON.parse(fetchMock.mock.calls[0][1].body)
    expect(body.message).toBe(expected)
    expect(body.mode).toBe(expectedMode)
    expect(wrapper.get('textarea').element.value).toBe('')
    expect(wrapper.get('[aria-label="发送"]').element.disabled).toBe(true)
    expect(wrapper.find('.mrow.user').text()).toBe(expected)
    expect(wrapper.find('.mrow.bot .turn-text').text()).toBe('回复')
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
    const retried = JSON.parse(fetchMock.mock.calls[2][1].body)
    expect(retried.message).toBe('制定计划')
    expect(retried.mode).toBe('plan')
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

  it('等待回复时主按钮变为暂停且不重复发送，卸载取消请求并释放图片', async () => {
    let controller
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ body: { id: 'photo.jpg' } })))
      .mockResolvedValueOnce(new Response(new ReadableStream({ start(value) { controller = value } })))
    await mountChat()
    await attach()
    await modeButton('计划').trigger('click')
    await send('计划任务')
    expect(wrapper.get('textarea').element.disabled).toBe(true)
    // 回复中主按钮变为"暂停回复"：点击走暂停接口，不会重发消息
    const halt = wrapper.get('[aria-label="暂停回复"]')
    expect(halt.element.disabled).toBe(false)
    await halt.trigger('click')
    expect(fetchMock).toHaveBeenCalledTimes(2)
    const signal = fetchMock.mock.calls[1][1].signal
    wrapper.unmount()
    wrapper = null
    expect(signal.aborted).toBe(true)
    expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:photo')
    controller.close()
    await flushPromises()
  })

  it('暂停后可继续，暂停期间输入新问题则开启新对话', async () => {
    fetchMock.mockResolvedValueOnce(stream([{ delta: '部分' }, { paused: true, partial: '部分' }, { done: true }]))
      .mockResolvedValueOnce(stream([{ delta: '续' }, { done: true }]))
    await mountChat()
    await send('写个故事')
    // 暂停现场：气泡带已暂停标记，主按钮变为"继续上次任务"
    expect(wrapper.find('.paused-chip').exists()).toBe(true)
    expect(wrapper.get('[aria-label="继续上次任务"]').element.disabled).toBe(false)
    // 空输入点击 = 继续：请求 resume 接口
    await wrapper.get('[aria-label="继续上次任务"]').trigger('click')
    await flushPromises()
    expect(fetchMock.mock.calls[1][0]).toBe('/api/chat/resume')
    const bots = wrapper.findAll('.mrow.bot')
    expect(bots.at(-1).text()).toContain('续')
    expect(wrapper.find('.paused-chip').exists()).toBe(false)
    // 再次暂停后输入新问题：点击发送新消息（不请求 resume）
    fetchMock.mockResolvedValueOnce(stream([{ delta: 'X' }, { paused: true, partial: 'X' }, { done: true }]))
    await send('换个话题')
    expect(wrapper.find('.paused-chip').exists()).toBe(true)
    await wrapper.get('textarea').setValue('新问题')
    fetchMock.mockResolvedValueOnce(stream([{ delta: '答' }, { done: true }]))
    await send()
    expect(JSON.parse(fetchMock.mock.calls.at(-1)[1].body).message).toBe('新问题')
    expect(fetchMock.mock.calls.filter(([url]) => url === '/api/chat/resume')).toHaveLength(1)
  })

  it('计划帧渲染计划卡；确认后打计划接口并刷新状态', async () => {
    const plan = {
      id: 'p1', title: '周末采购', state: 'awaiting_confirm',
      steps: [{ n: 1, title: '查清单', state: 'pending' }, { n: 2, title: '下单', state: 'pending' }],
    }
    fetchMock.mockResolvedValueOnce(stream([{ plan }, { delta: '拆好了' }, { done: true }]))
    await mountChat()
    await modeButton('计划').trigger('click')
    await send('帮我准备周末采购')
    await flushPromises()

    const card = wrapper.get('.plan-card')
    expect(card.attributes('data-plan')).toBe('p1')
    expect(card.text()).toContain('周末采购')
    expect(card.text()).toContain('待确认')
    expect(card.findAll('.pc-steps li')).toHaveLength(2)
    expect(card.text()).toContain('先不做')

    api.mockImplementation((method, url) => Promise.resolve(
      method === 'POST' && url.endsWith('/confirm') ? { message: '已确认' }
        : method === 'GET' && url === '/api/plans/p1' ? { plan: { ...plan, state: 'running' } } : {}))
    await card.findAll('.pc-acts button')[0].trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/plans/p1/confirm', { approve: true })
    expect(api).toHaveBeenCalledWith('GET', '/api/plans/p1')
    await flushPromises()
    expect(wrapper.get('.plan-card').text()).toContain('执行中')
  })

  it('失败步骤给出重试与跳过；已结束计划不反复刷新', async () => {
    const plan = {
      id: 'p2', title: '带失败步的计划', state: 'paused', error: '预算已用满',
      steps: [{ n: 1, title: '查清单', state: 'failed', error: '工具报错' }, { n: 2, title: '下单', state: 'pending' }],
    }
    fetchMock.mockResolvedValueOnce(stream([{ plan }, { done: true }]))
    await mountChat()
    await send('随便')
    await flushPromises()

    const card = wrapper.get('.plan-card')
    expect(card.text()).toContain('预算已用满')
    expect(card.findAll('.pc-acts button').map(b => b.text())).toEqual(['重试', '跳过', '继续', '取消'])
    api.mockClear()
    await card.findAll('.pc-acts button')[1].trigger('click')   // 跳过失败步
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/plans/p2/steps/1/skip', {})
  })

  it('时间线：思考一行实时预览、工具调用一行成块，各自点击展开', async () => {
    let controller
    fetchMock.mockResolvedValueOnce(new Response(new ReadableStream({ start(value) { controller = value } })))
    const enc = new TextEncoder()
    // 挂在 body 上：jsdom 仅对已挂载元素失效样式缓存，v-show 切换后 isVisible 才可信
    wrapper = mount(ChatView, { attachTo: document.body })
    await flushPromises()
    await send('查天气')
    const push = async ev => {
      controller.enqueue(enc.encode(`data: ${JSON.stringify(ev)}\n\n`))
      await flushPromises()
    }
    // 思考块默认收起：标题实时展示最新一行，正文不可见
    await push({ thinking: '先看用户想要什么' })
    expect(wrapper.get('.think-title').text()).toBe('先看用户想要什么')
    expect(wrapper.get('.think-body').isVisible()).toBe(false)
    // 前导是灯泡图标（不再是圆点），流式期间带 live 呼吸态
    expect(wrapper.get('.think .step-ico svg').exists()).toBe(true)
    expect(wrapper.get('.think .step-ico').classes()).toContain('live')
    // 新一行思考到来后，这一行随之更新
    await push({ thinking: '\n再查天气数据' })
    expect(wrapper.get('.think-title').text()).toBe('再查天气数据')
    // 单行展示取最新行尾部，超长时省略头部
    await push({ thinking: '\n长文' + '甲'.repeat(200) + '收尾' })
    expect(wrapper.get('.think-title').text().startsWith('…')).toBe(true)
    expect(wrapper.get('.think-title').text().endsWith('收尾')).toBe(true)
    // 点击展开：能看到全部思考内容；再点击收起
    await wrapper.get('.think-head').trigger('click')
    expect(wrapper.get('.think-body').isVisible()).toBe(true)
    expect(wrapper.get('.think-text').text()).toBe('先看用户想要什么\n再查天气数据\n长文' + '甲'.repeat(200) + '收尾')
    await wrapper.get('.think-head').trigger('click')
    expect(wrapper.get('.think-body').isVisible()).toBe(false)
    // 轮次结束：标题变为耗时摘要（token 不再摊在每一行思考上，见轮末汇总用例）
    await push({ step: { kind: 'round', detail: '耗时 1.2s' } })
    expect(wrapper.get('.think-title').text()).toBe('思考 · 耗时 1.2s')
    expect(wrapper.get('.think .step-ico').classes()).not.toContain('live')
    // 工具调用：收起只报"工具调用"，名称与参数不外露；展开才给详情与结果
    await push({ step: { kind: 'tool', name: 'weather', detail: '上海', status: 'run' } })
    expect(wrapper.get('.tool .step-ico .spin').exists()).toBe(true)
    await push({ step: { kind: 'tool', name: 'weather', detail: '晴 22℃', status: 'ok' } })
    const tool = wrapper.get('.tool')
    expect(tool.get('.tool-head .step-name').text()).toBe('工具调用')
    expect(tool.get('.tool-head').text()).not.toContain('weather')
    expect(tool.get('.tool-head').text()).not.toContain('上海')
    expect(tool.get('.tool-call').isVisible()).toBe(false)
    expect(tool.get('.tool-out').isVisible()).toBe(false)
    // 成功不再打勾：前导恒为扳手，状态只靠类名表达（✓/✕ 字符已退出模板）
    expect(tool.find('.step-ico').classes()).toContain('ok')
    expect(tool.get('.step-ico svg').exists()).toBe(true)
    expect(tool.get('.step-ico').text()).toBe('')
    expect(tool.find('.step-ico .spin').exists()).toBe(false)
    await tool.get('.tool-head').trigger('click')
    expect(tool.get('.tool-call .step-name').text()).toBe('weather')
    expect(tool.get('.tool-call .step-detail').text()).toBe('上海')
    expect(wrapper.get('.tool-out').text()).toBe('晴 22℃')
    // 正文另起一块，流式结束后光标消失
    await push({ delta: '今天晴' })
    await push({ done: true })
    expect(wrapper.get('.turn-text').text()).toBe('今天晴')
    expect(wrapper.find('.cursor').exists()).toBe(false)
  })

  it('结束帧兜底：结果事件丢失的工具块就地收掉，不再转圈', async () => {
    let controller
    fetchMock.mockResolvedValueOnce(new Response(new ReadableStream({ start(value) { controller = value } })))
    const enc = new TextEncoder()
    wrapper = mount(ChatView)
    await flushPromises()
    await send('查天气')
    const push = async ev => {
      controller.enqueue(enc.encode(`data: ${JSON.stringify(ev)}\n\n`))
      await flushPromises()
    }
    await push({ step: { kind: 'tool', name: 'weather', detail: '上海', status: 'run' } })
    expect(wrapper.get('.tool .step-ico .spin').exists()).toBe(true)
    // ok/fail 帧被丢（过程事件是非阻塞发布，可能半路丢弃）：done 一到，
    // 残留的运行中块要收掉，不能永远转圈
    await push({ delta: '今天晴' })
    await push({ done: true })
    expect(wrapper.find('.tool .step-ico .spin').exists()).toBe(false)
    expect(wrapper.get('.tool .step-ico').classes()).toContain('ok')
  })

  it('思考与正文分片交错到达时各并回一块，回答不碎成两字一行', async () => {
    // StepFun 这类网关会在正文已经吐了几个字之后补发思考分片：
    // 按"最后一块类型"分块会把回答切成「我是」「小五，…」这样的碎块。
    fetchMock.mockResolvedValueOnce(stream([
      { thinking: '先看用户想要什么' },
      { delta: '今天' },
      { thinking: '\n再确认一下' },
      { delta: '晴' },
      { step: { kind: 'round', detail: '耗时 1.2s' } },
      { done: true, turn: { in: 1200, out: 80, ms: 1200 } },
    ]))
    await mountChat()
    await send('查天气')
    expect(wrapper.findAll('.think')).toHaveLength(1)
    expect(wrapper.get('.think-text').text()).toBe('先看用户想要什么\n再确认一下')
    expect(wrapper.findAll('.turn-text')).toHaveLength(1)
    expect(wrapper.get('.turn-text').text()).toBe('今天晴')
    // 轮次收尾：耗时挂在思考行上，本轮 token 落在时间线末尾
    expect(wrapper.get('.think-title').text()).toBe('思考 · 耗时 1.2s')
    expect(wrapper.get('.turn-total').text()).toBe('本轮 · 1.2k+80 tokens · 耗时 1.2s')
  })

  it('思考分片晚于正文到达时仍排在回答前面', async () => {
    fetchMock.mockResolvedValueOnce(stream([
      { delta: '今天' },
      { thinking: '先想想用户要什么' },
      { delta: '晴' },
      { done: true },
    ]))
    await mountChat()
    await send('查天气')
    const rows = wrapper.findAll('.turn > div').map(row => row.classes()[0])
    expect(rows).toEqual(['think', 'turn-text', 'turn-foot'])
    expect(wrapper.get('.turn-text').text()).toBe('今天晴')
  })

  it('工具调用之后的新一轮思考与正文另起一块', async () => {
    fetchMock.mockResolvedValueOnce(stream([
      { thinking: '要查天气' },
      { step: { kind: 'round', detail: '耗时 1.1s' } },
      { step: { kind: 'tool', name: 'weather', detail: '上海', status: 'run' } },
      { step: { kind: 'tool', name: 'weather', detail: '晴', status: 'ok' } },
      { thinking: '拿到结果了' },
      { delta: '今天晴' },
      { done: true },
    ]))
    await mountChat()
    await send('查天气')
    const rows = wrapper.findAll('.turn > div').map(row => row.classes()[0])
    expect(rows).toEqual(['think', 'tool', 'think', 'turn-text', 'turn-foot'])
    expect(wrapper.findAll('.think-text').map(b => b.text())).toEqual(['要查天气', '拿到结果了'])
  })

  it('提问发出后立即可复制，回答要等本轮结束才给复制', async () => {
    const held = holdStream()
    fetchMock.mockResolvedValueOnce(held.response)
    await mountChat()
    await send('查天气')
    held.push({ delta: '今天' })
    await flushPromises()
    // 自己发出去的话不依赖回复：发出即有复制按钮
    expect(wrapper.findAll('.mrow.user .copy-btn')).toHaveLength(1)
    // 回复还在流式生成：正文已出一块，但轮末操作行还不存在（防止复制到半截答案）
    expect(wrapper.get('.turn-text').text()).toBe('今天')
    expect(wrapper.find('.turn-foot').exists()).toBe(false)

    held.push({ done: true })
    held.close()
    await flushPromises()
    expect(wrapper.find('.turn-foot .copy-btn').exists()).toBe(true)
  })

  it('刷新后从历史接口恢复思考块与工具调用块', async () => {
    api.mockResolvedValueOnce({
      messages: [
        { role: 'user', text: '现在几点' },
        {
          role: 'bot', text: '晚上 10 点。',
          blocks: [
            { kind: 'think', text: '先查时区' },
            { kind: 'tool', name: 'get_time', text: 'Asia/Shanghai', out: '22:00', status: 'ok' },
            { kind: 'text', text: '晚上 10 点。' },
          ],
        },
      ],
    })
    await mountChat()
    expect(wrapper.get('.think-title').text()).toBe('思考')
    expect(wrapper.get('.think-text').text()).toBe('先查时区')
    // 历史恢复的块同样走图标槽：思考=灯泡且不带 live，工具=扳手
    expect(wrapper.get('.think .step-ico').classes()).not.toContain('live')
    expect(wrapper.get('.tool .tool-head .step-name').text()).toBe('工具调用')
    expect(wrapper.get('.tool .step-ico svg').exists()).toBe(true)
    expect(wrapper.get('.tool .tool-call .step-name').text()).toBe('get_time')
    expect(wrapper.get('.tool .tool-call .step-detail').text()).toBe('Asia/Shanghai')
    expect(wrapper.get('.turn-text').text()).toBe('晚上 10 点。')
  })

  it('工具调用失败：图标换成告警圈并整行标红，错误摘要收进展开区', async () => {
    fetchMock.mockResolvedValueOnce(stream([
      { step: { kind: 'tool', name: 'exec_command', detail: 'rm -rf /', status: 'run' } },
      { step: { kind: 'tool', name: 'exec_command', detail: '权限被拒绝', status: 'fail' } },
      { delta: '这个操作被拦下了。' },
      { done: true },
    ]))
    await mountChat()
    await send('删掉文件')
    expect(wrapper.get('.tool .step-ico').classes()).toContain('fail')
    expect(wrapper.get('.tool').classes()).toContain('is-fail')
    expect(wrapper.get('.step-ico svg').exists()).toBe(true)
    expect(wrapper.get('.step-ico').text()).toBe('')       // 不再用 ✕ 字符
    expect(wrapper.get('.tool-head').attributes('title')).toBe('工具执行出错，展开详情')
    expect(wrapper.findAll('.tool')).toHaveLength(1)
    expect(wrapper.get('.tool-head').text()).not.toContain('权限被拒绝')
    expect(wrapper.get('.tool .tool-out').text()).toBe('权限被拒绝')
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

describe('composer 两行布局', () => {
  it('输入区独占第一行，第二行左为附件与模式 chip、右为快捷键提示与主键', async () => {
    await mountChat()
    expect(wrapper.get('.inputbar').element.firstElementChild).toBe(wrapper.get('textarea').element)
    const bar = wrapper.get('.composer-bar')
    expect(bar.find('.bar-left .plus-btn').exists()).toBe(true)
    expect(bar.find('.bar-right .send-btn').exists()).toBe(true)
    expect(bar.find('.bar-right .kbd-hint').text()).toContain('⇧⏎')
    expect(wrapper.get('.mode-chip').element.parentElement).toBe(bar.get('.bar-left').element)
  })

  it('选定模式后 chip 显示在工具条左组，退出后收起', async () => {
    await mountChat()
    await wrapper.get('.plus-btn').trigger('click')
    await modeButton('计划').trigger('click')
    expect(wrapper.get('.mode-chip').classes()).toContain('show')
    expect(wrapper.get('.bar-left .mode-chip').text()).toContain('计划')
    await wrapper.get('.mode-chip button').trigger('click')
    expect(wrapper.get('.mode-chip').classes()).not.toContain('show')
  })
})

describe('粘贴附件', () => {
  async function pasteFiles(files) {
    const ev = new window.Event('paste', { cancelable: true, bubbles: true })
    ev.clipboardData = { files }
    wrapper.get('textarea').element.dispatchEvent(ev)
    await flushPromises()
    return ev
  }

  it('剪贴板里的图片直接进附件预览', async () => {
    await mountChat()
    await pasteFiles([new File(['photo'], 'photo.png', { type: 'image/png' })])
    expect(wrapper.get('#attachPreviews img').attributes('src')).toBe('blob:photo')
  })

  it('非图片文件给出可读提示且不落附件', async () => {
    await mountChat()
    await pasteFiles([new File(['doc'], 'a.pdf', { type: 'application/pdf' })])
    // 1f28026 起粘贴只认图片：非图片按数量给出明确忽略提示，而不是笼统的“仅支持图片文件”。
    expect(toast).toHaveBeenCalledWith('剪贴板里有 1 个非图片文件，已忽略（目前只支持图片）', true)
    expect(wrapper.find('#attachPreviews img').exists()).toBe(false)
  })

  it('剪贴板没有文件时不拦截默认粘贴', async () => {
    await mountChat()
    const ev = await pasteFiles([])
    expect(ev.defaultPrevented).toBe(false)
  })
})

describe('输入框权限模式 chip', () => {
  const permApi = mode => vi.fn((method, url) => {
    if (method === 'GET' && url === '/api/permissions') return Promise.resolve({ mode, execMode: 'confirm', mcp: 'allow' })
    if (method === 'POST' && url === '/api/permissions') return Promise.resolve({ message: '已切换并热生效' })
    return Promise.resolve({ messages: [] })
  })

  async function openPermMenu() {
    await wrapper.get('[aria-label="权限模式"]').trigger('click')
    await flushPromises()
    return wrapper.findAll('#permPopover .pop-item')
  }

  it('chip 常驻显示当前档位，弹层选择后 POST 热切换', async () => {
    api.mockImplementation(permApi('acceptEdits'))
    await mountChat()
    const chip = wrapper.get('[aria-label="权限模式"]')
    expect(chip.classes()).toContain('pm-acceptEdits')
    expect(chip.text()).toContain('自动编辑')
    expect(wrapper.find('#permPopover').isVisible()).toBe(false)

    const items = await openPermMenu()
    expect(items).toHaveLength(3)
    await items.find(i => i.find('.pl').text() === '确认（默认）').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/permissions', { mode: 'confirm' })
    expect(wrapper.get('[aria-label="权限模式"]').text()).toContain('确认')
    expect(wrapper.find('#permPopover').isVisible()).toBe(false)
  })

  it('完全信任需二次确认：取消不发请求，确认才切换', async () => {
    api.mockImplementation(permApi('confirm'))
    await mountChat()
    vi.stubGlobal('confirm', vi.fn(() => false))
    let items = await openPermMenu()
    await items.find(i => i.find('.pl').text() === '完全信任').trigger('click')
    await flushPromises()
    expect(api).not.toHaveBeenCalledWith('POST', '/api/permissions', { mode: 'fullAccess' })

    vi.stubGlobal('confirm', vi.fn(() => true))
    items = await openPermMenu()
    await items.find(i => i.find('.pl').text() === '完全信任').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/permissions', { mode: 'fullAccess' })
    expect(wrapper.get('[aria-label="权限模式"]').classes()).toContain('pm-fullAccess')
  })
})

describe('轮末 token 汇总与上下文占用 chip', () => {
  const snap = (over = {}) => ({
    enabled: true, session: 'web', used: 43000, window: 128000, ratio: 0.34,
    softRatio: 0.7, blocks: 6, keepRecent: 12, messages: 22, canCompact: false, ...over,
  })
  const ctxApi = (snapshot, compact = {}) => vi.fn((method, url) => {
    if (method === 'GET' && url === '/api/context?session=web') return Promise.resolve(snapshot)
    if (method === 'POST' && url === '/api/context/compact') return Promise.resolve(compact)
    return Promise.resolve({ messages: [] })
  })
  const chip = () => wrapper.get('[aria-label="上下文使用率"]')
  async function openCtxMenu() {
    await chip().trigger('click')
    await flushPromises()
    return wrapper.findAll('#ctxPopover .pop-item')
  }
  const compactItem = items => items.find(i => i.find('.pl').text() === '压缩上下文')

  it('done 帧带 turn 时，时间线末尾落一行本轮汇总', async () => {
    fetchMock.mockResolvedValueOnce(stream([
      { delta: '今天晴' },
      { done: true, turn: { in: 1234, out: 300, ms: 3400 } },
    ]))
    await mountChat()
    await send('查天气')
    expect(wrapper.get('.turn-total').text()).toBe('本轮 · 1.2k+300 tokens · 耗时 3.4s')
  })

  it('后端没带 turn（这一请求没有轨迹）时不渲染汇总行', async () => {
    fetchMock.mockResolvedValueOnce(stream([{ delta: '今天晴' }, { done: true }]))
    await mountChat()
    await send('查天气')
    expect(wrapper.find('.turn-total').exists()).toBe(false)
  })

  it('被暂停的一轮同样汇总，但没有耗时（暂停现场没有结束时刻）', async () => {
    fetchMock.mockResolvedValueOnce(stream([
      { delta: '前半段' },
      { paused: true, partial: '前半段' },
      { done: true, turn: { in: 900, out: 120 } },
    ]))
    await mountChat()
    await send('写点东西')
    expect(wrapper.get('.turn-total').text()).toBe('本轮 · 900+120 tokens')
  })

  it('刷新后每轮各自的汇总留在自己那一轮下面，不只是最新一轮有数字', async () => {
    api.mockResolvedValueOnce({
      messages: [
        { role: 'user', text: '现在几点' },
        { role: 'bot', text: '晚上 10 点。', blocks: [{ kind: 'text', text: '晚上 10 点。' }],
          turn: { in: 1200, out: 80, ms: 2100 } },
        { role: 'user', text: '谢谢' },
        { role: 'bot', text: '不客气～', blocks: [{ kind: 'text', text: '不客气～' }],
          turn: { in: 1300, out: 12 } },
      ],
    })
    await mountChat()
    expect(wrapper.findAll('.turn-total').map(row => row.text())).toEqual([
      '本轮 · 1.2k+80 tokens · 耗时 2.1s',
      '本轮 · 1.3k+12 tokens',
    ])
  })

  it('chip 常驻显示占比，点开给出 token 数字与归档阈值刻度', async () => {
    api.mockImplementation(ctxApi(snap()))
    await mountChat()
    expect(chip().text()).toContain('34%')
    expect(chip().classes()).not.toContain('warm')
    const items = await openCtxMenu()
    expect(wrapper.get('#ctxPopover').text()).toContain('活跃区约 43k / 窗口 128k tokens')
    expect(wrapper.get('#ctxPopover').text()).toContain('6 个活跃块 · 22 条消息 · 压缩后保留最近 12 块')
    expect(wrapper.get('.ctx-mark').attributes('style')).toContain('left: 70%')
    expect(api).toHaveBeenCalledWith('GET', '/api/context?session=web')
    expect(compactItem(items).text()).toContain('压缩上下文')
  })

  it('达软阈值：chip 转 warm、压缩可点，执行后按返回快照刷新占比', async () => {
    const before = snap({ ratio: 0.78, used: 100000, blocks: 20, canCompact: true })
    const after = snap({ ratio: 0.21, used: 27000, blocks: 8, canCompact: false })
    api.mockImplementation(ctxApi(before, {
      message: '已归档 12 块，占用 100k → 27k tokens', before, after,
    }))
    await mountChat()
    expect(chip().classes()).toContain('warm')
    const items = await openCtxMenu()
    const compact = compactItem(items)
    expect(compact.attributes('disabled')).toBeUndefined()
    await compact.trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/context/compact', { session: 'web' })
    expect(toast).toHaveBeenCalledWith('已归档 12 块，占用 100k → 27k tokens')
    expect(chip().text()).toContain('21%')
    expect(chip().classes()).not.toContain('warm')
  })

  it('未达阈值：压缩置灰并说明为什么现在不用压', async () => {
    api.mockImplementation(ctxApi(snap()))
    await mountChat()
    const compact = compactItem(await openCtxMenu())
    expect(compact.attributes('disabled')).toBeDefined()
    expect(compact.text()).toContain('未达归档阈值（70%）')
  })

  it('本轮回复进行中：压缩置灰，提示等这一轮结束', async () => {
    let releaseStream
    fetchMock.mockResolvedValueOnce(new Response(new ReadableStream({
      start(c) { releaseStream = c },
    })))
    api.mockImplementation(ctxApi(snap({ ratio: 0.78, canCompact: true })))
    await mountChat()
    await send('长回复')
    const items = await openCtxMenu()
    expect(compactItem(items).attributes('disabled')).toBeDefined()
    expect(compactItem(items).text()).toContain('本轮回复进行中')
    releaseStream.close()
    await flushPromises()
  })

  it('上下文治理未启用时不出现 chip', async () => {
    api.mockImplementation(ctxApi({ enabled: false, session: 'web' }))
    await mountChat()
    expect(wrapper.find('[aria-label="上下文使用率"]').exists()).toBe(false)
  })
})

describe('模型快捷切换', () => {
  const modelsApi = (models, active) => (method, url) => {
    if (method === 'GET' && url === '/api/provider/models') {
      return Promise.resolve({ active, models })
    }
    if (method === 'POST' && url === '/api/provider/activate') return Promise.resolve({ message: '已切换' })
    return Promise.resolve({})
  }
  const two = [
    { name: 'step-5', model: 'step-5-preview', inputs: ['text', 'image'], contextWindow: 128000, keySet: true },
    { name: 'ds-chat', model: 'deepseek-chat', inputs: ['text'], contextWindow: 65536, keySet: true },
  ]

  it('chip 显示当前模型名，弹层列出库内条目与能力标注', async () => {
    api.mockImplementation(modelsApi(two, 'step-5'))
    await mountChat()
    const chip = wrapper.find('[aria-label="当前模型"]')
    expect(chip.exists()).toBe(true)
    expect(chip.text()).toContain('step-5')
    await chip.trigger('click')
    await flushPromises()
    const items = wrapper.findAll('#modelPopover .pop-item')
    expect(items.map(b => b.find('.pl').text())).toEqual(['step-5', 'ds-chat', '管理模型库…'])
    expect(items[1].text()).toContain('仅文本')
    expect(items[0].classes()).toContain('selected')
  })

  it('未配置模型：chip 明说未配置，弹层给提示，发送被拦下', async () => {
    api.mockImplementation(modelsApi([], ''))
    await mountChat()
    const chip = wrapper.find('[aria-label="当前模型"]')
    expect(chip.exists()).toBe(true)
    expect(chip.text()).toContain('未配置模型')
    await chip.trigger('click')
    await flushPromises()
    expect(wrapper.get('#modelPopover').text()).toContain('还没有配置模型')
    await send('你好')
    expect(toast).toHaveBeenCalledWith('还没有配置模型，请先在「设置 → 模型服务」添加', true)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('点选另一模型发 activate，切换后刷新库与上下文占比', async () => {
    api.mockImplementation(modelsApi(two, 'step-5'))
    await mountChat()
    await wrapper.find('[aria-label="当前模型"]').trigger('click')
    await flushPromises()
    await wrapper.findAll('#modelPopover .pop-item')[1].trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/provider/activate', { name: 'ds-chat' })
  })

  it('激活条目不支持图片时：添加图片置灰并说明，粘贴图片被拒', async () => {
    api.mockImplementation(modelsApi(two, 'ds-chat'))
    await mountChat()
    await wrapper.find('[aria-label="添加"]').trigger('click')
    await flushPromises()
    const imageItem = modeButton('添加图片')
    expect(imageItem.attributes('disabled')).toBeDefined()
    expect(imageItem.text()).toContain('当前模型不支持图片输入')

    const paste = new window.Event('paste', { bubbles: true, cancelable: true })
    paste.clipboardData = { files: [new File(['x'], 'x.png', { type: 'image/png' })] }
    wrapper.get('textarea').element.dispatchEvent(paste)
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('当前模型不支持图片输入', true)
  })
})
