import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import TerminalView from '../src/components/TerminalView.vue'
import { api } from '../src/api.js'
import { store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

// xterm 与 FitAddon 用替身：这里要验的是我们的协议与状态机，不是 xterm 能不能在
// jsdom 里画字（它需要真实布局，量出来的尺寸恒为 0）。vi.mock 会被提到文件最前，
// 所以替身类必须在 vi.hoisted 里造好再交出去。
const xtermMocks = vi.hoisted(() => {
  const instances = []
  class FakeTerminal {
    constructor(options) {
      this.options = options
      this.cols = 100
      this.rows = 30
      this.written = []
      this.disposed = false
      this.addons = []
      instances.push(this)
    }

    loadAddon(a) { this.addons.push(a) }
    open() { this.opened = true }
    write(s) { this.written.push(s) }
    writeln(s) { this.written.push(s) }
    focus() { this.focused = (this.focused || 0) + 1 }
    dispose() { this.disposed = true }
    onData(cb) { this.onDataCb = cb }
    onResize(cb) { this.onResizeCb = cb }
  }
  return { FakeTerminal, instances }
})
const termInstances = xtermMocks.instances

vi.mock('@xterm/xterm', () => ({ Terminal: xtermMocks.FakeTerminal }))
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class { fit() { this.fitted = (this.fitted || 0) + 1 } },
}))

// WebSocket 替身：记录发出去的帧，测试可以主动推 ready/exit/输出。
class FakeSocket {
  static OPEN = 1

  constructor(url) {
    this.url = url
    this.frames = []
    this.readyState = 0
    FakeSocket.instances.push(this)
    // 微任务里再 open：构造方要先来得及挂 onopen
    Promise.resolve().then(() => {
      this.readyState = FakeSocket.OPEN
      this.onopen?.()
    })
  }

  send(text) { this.frames.push(JSON.parse(text)) }
  close() {
    if (this.readyState === 3) return
    this.readyState = 3
    this.onclose?.()
  }

  push(msg) { this.onmessage?.({ data: JSON.stringify(msg) }) }
  pushData(bytes) { this.onmessage?.({ data: new Uint8Array(bytes).buffer }) }
  last() { return this.frames[this.frames.length - 1] }
}
FakeSocket.instances = []

let wrapper

function terminalInfo(over = {}) {
  return { enabled: true, local: true, active: 0, maxSessions: 2, ...over }
}

async function show() {
  wrapper = mount(TerminalView)
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  termInstances.length = 0
  FakeSocket.instances = []
  store.view = 'terminal'
  api.mockReset().mockImplementation((method, url) => {
    if (url === '/api/terminal') return Promise.resolve(terminalInfo())
    return Promise.resolve({})
  })
  toast.mockClear()
  vi.stubGlobal('WebSocket', FakeSocket)
  vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
})

function socket() { return FakeSocket.instances[FakeSocket.instances.length - 1] }

describe('内置终端页', () => {
  it('可用时进来就开一个终端，并把量到的尺寸先告诉内核', async () => {
    await show()
    expect(FakeSocket.instances).toHaveLength(1)
    const start = socket().frames[0]
    expect(start).toMatchObject({ t: 'start', cols: 100, rows: 30 })
    // token 走查询参数：浏览器 WebSocket 加不了请求头
    expect(socket().url).toContain('/api/terminal/ws?token=')
    expect(termInstances[0].opened).toBe(true)
  })

  it('ready 之后头部说明用的是哪个 shell 与目录', async () => {
    await show()
    socket().push({ t: 'ready', id: 't1', shell: '/bin/zsh', cwd: '/home/u/proj' })
    await flushPromises()
    expect(wrapper.get('.term-hint').text()).toContain('/bin/zsh · /home/u/proj')
  })

  it('二进制帧原样写进 xterm，控制帧不写', async () => {
    await show()
    socket().pushData([104, 105]) // "hi"
    socket().push({ t: 'ready', shell: '/bin/sh', cwd: '/x' })
    await flushPromises()
    // 二进制帧按原样交给 xterm（不转字符串，控制字符不能被改写）；控制帧不进画布
    expect(termInstances[0].written).toHaveLength(1)
    expect(Array.from(termInstances[0].written[0])).toEqual([104, 105])
  })

  it('键盘与窗口尺寸都按协议发回内核', async () => {
    await show()
    termInstances[0].onDataCb('ls -l\n')
    termInstances[0].onResizeCb({ cols: 60, rows: 18 })
    await flushPromises()
    expect(socket().frames).toContainEqual({ t: 'input', d: 'ls -l\n' })
    expect(socket().frames).toContainEqual({ t: 'resize', cols: 60, rows: 18 })
  })

  it('进程退出在页面上说清结果，标签不自动消失', async () => {
    await show()
    socket().push({ t: 'exit', code: 3 })
    await flushPromises()
    expect(wrapper.get('.term-hint').text()).toContain('code 3')
    expect(wrapper.findAll('.term-tab')).toHaveLength(1)
  })

  it('出错帧把原因显示出来并 toast 一次', async () => {
    await show()
    socket().push({ t: 'error', message: '已经开了 2 个终端，上限 2：先关掉一个再开' })
    await flushPromises()
    expect(wrapper.get('.term-hint').text()).toContain('上限 2')
    expect(toast).toHaveBeenCalledWith(expect.stringContaining('上限 2'), true)
  })

  it('新建与关闭标签：关闭会发 close 并断开连接', async () => {
    await show()
    await wrapper.get('[aria-label="新建终端"]').trigger('click')
    await flushPromises()
    expect(FakeSocket.instances).toHaveLength(2)
    expect(wrapper.findAll('.term-tab')).toHaveLength(2)

    const second = socket()
    await wrapper.findAll('.term-tab [aria-label="关闭 终端 2"]')[0].trigger('click')
    await flushPromises()
    expect(second.frames).toContainEqual({ t: 'close' })
    expect(second.readyState).toBe(3)
    expect(wrapper.findAll('.term-tab')).toHaveLength(1)
    expect(termInstances[1].disposed).toBe(true)
  })

  it('到上限时不再新建，直接说明先关掉一个', async () => {
    api.mockImplementation((method, url) => Promise.resolve(
      url === '/api/terminal' ? terminalInfo({ maxSessions: 1 }) : {}))
    await show()
    await wrapper.get('[aria-label="新建终端"]').trigger('click')
    await flushPromises()
    expect(FakeSocket.instances).toHaveLength(1)
    expect(toast).toHaveBeenCalledWith(expect.stringContaining('上限 1'), true)
  })

  it('maxSessions 为 0 时不限数量，继续新建', async () => {
    api.mockImplementation((method, url) => Promise.resolve(
      url === '/api/terminal' ? terminalInfo({ maxSessions: 0 }) : {}))
    await show()
    await wrapper.get('[aria-label="新建终端"]').trigger('click')
    await flushPromises()
    expect(FakeSocket.instances).toHaveLength(2)
    expect(toast).not.toHaveBeenCalled()
  })

  it('不可用时不建任何连接，页面说清原因与开法', async () => {
    api.mockImplementation((method, url) => Promise.resolve(
      url === '/api/terminal'
        ? { enabled: false, local: false, reason: '内置终端只允许本机访问' }
        : {}))
    await show()
    expect(FakeSocket.instances).toHaveLength(0)
    expect(wrapper.text()).toContain('内置终端只允许本机访问')
    expect(wrapper.text()).toContain('terminal')
    await wrapper.get('[aria-label="新建终端"]').trigger('click')
    expect(toast).toHaveBeenCalledWith('内置终端只允许本机访问', true)
  })

  it('状态没读到之前不宣称"未开启"', async () => {
    let resolveInfo
    api.mockImplementation(() => new Promise(r => { resolveInfo = r }))
    wrapper = mount(TerminalView)
    await flushPromises()
    expect(wrapper.text()).toContain('正在确认终端是否可用')
    expect(wrapper.text()).not.toContain('未开启')
    resolveInfo(terminalInfo())
    await flushPromises()
    expect(FakeSocket.instances).toHaveLength(1)
  })

  it('切到别的导航不卸载组件，连接与终端都还在', async () => {
    await show()
    store.view = 'chat'
    await flushPromises()
    expect(wrapper.find('#view-terminal').exists()).toBe(true)
    expect(wrapper.find('#view-terminal').attributes('style')).toContain('display: none')
    expect(socket().readyState).toBe(FakeSocket.OPEN)
    store.view = 'terminal'
    await flushPromises()
    expect(wrapper.find('#view-terminal').attributes('style') || '').not.toContain('display: none')
  })

  it('卸载（登出等）把全部终端收掉', async () => {
    await show()
    await wrapper.get('[aria-label="新建终端"]').trigger('click')
    await flushPromises()
    const first = FakeSocket.instances[0]
    wrapper.unmount()
    wrapper = null
    await flushPromises()
    expect(first.readyState).toBe(3)
    expect(termInstances.every(t => t.disposed)).toBe(true)
  })
})
