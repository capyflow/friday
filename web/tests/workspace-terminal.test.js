import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import WorkspaceTerminal from '../src/components/WorkspaceTerminal.vue'
import { api } from '../src/api.js'
import { toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

// xterm 与 FitAddon 用替身：这里要验的是我们的协议与面板开合语义，不是 xterm 能不能在
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
}
FakeSocket.instances = []

function terminalInfo(over = {}) {
  return { enabled: true, local: true, active: 0, maxSessions: 2, ...over }
}

let wrapper

async function show(props = { open: true, cwd: '/w/proj' }) {
  wrapper = mount(WorkspaceTerminal, { props })
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  termInstances.length = 0
  FakeSocket.instances = []
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

describe('工作区内嵌终端面板', () => {
  it('面板开着就绪一个终端，start 帧把会话主目录带给内核', async () => {
    await show()
    expect(FakeSocket.instances).toHaveLength(1)
    expect(socket().frames[0]).toMatchObject({ t: 'start', cwd: '/w/proj' })
    expect(termInstances[0].opened).toBe(true)
    // 回滚行数写死在代码里（不做成设置）
    expect(termInstances[0].options.scrollback).toBe(8000)
  })

  it('收起面板不卸载：连接照常活着，展开不新建终端', async () => {
    await show()
    const first = socket()
    await wrapper.setProps({ open: false })
    await flushPromises()
    expect(first.readyState).toBe(FakeSocket.OPEN) // 收起不打断终端
    await wrapper.setProps({ open: true })
    await flushPromises()
    expect(FakeSocket.instances).toHaveLength(1) // 展开不重开连接，终端与内容都在
    expect(termInstances[0].disposed).toBe(false)
    expect(wrapper.findAll('.term-tab')).toHaveLength(1)
  })

  it('收起期间内核的输出照常进 xterm 缓冲', async () => {
    await show()
    await wrapper.setProps({ open: false })
    await flushPromises()
    socket().pushData([104, 105]) // "hi"
    await flushPromises()
    expect(termInstances[0].written).toHaveLength(1)
    expect(Array.from(termInstances[0].written[0])).toEqual([104, 105])
  })

  it('开合切换（false→true）在状态已就绪时也会补开一个终端', async () => {
    await show({ open: false, cwd: '' })
    expect(FakeSocket.instances).toHaveLength(0)
    await wrapper.setProps({ open: true })
    await flushPromises()
    expect(FakeSocket.instances).toHaveLength(1)
    // cwd 为空回落内核工作目录，协议层允许
    expect(socket().frames[0]).toMatchObject({ t: 'start', cwd: '' })
  })

  it('新建标签上限按内核配置算，超了直接说明', async () => {
    api.mockImplementation((method, url) => Promise.resolve(
      url === '/api/terminal' ? terminalInfo({ maxSessions: 1 }) : {}))
    await show()
    await wrapper.get('[aria-label="新建终端"]').trigger('click')
    await flushPromises()
    expect(FakeSocket.instances).toHaveLength(1)
    expect(toast).toHaveBeenCalledWith(expect.stringContaining('上限 1'), true)
  })

  it('关闭标签发 close、断开连接并销毁终端', async () => {
    await show()
    await wrapper.get('[aria-label="新建终端"]').trigger('click')
    await flushPromises()
    const second = socket()
    await wrapper.findAll('.term-tab [aria-label="关闭 终端 2"]')[0].trigger('click')
    await flushPromises()
    expect(second.frames).toContainEqual({ t: 'close' })
    expect(second.readyState).toBe(3)
    expect(termInstances[1].disposed).toBe(true)
    expect(wrapper.findAll('.term-tab')).toHaveLength(1)
  })

  it('ready 之后头部说明用哪个 shell 与目录，退出在页面上说清结果', async () => {
    await show()
    socket().push({ t: 'ready', id: 't1', shell: '/bin/zsh', cwd: '/w/proj' })
    await flushPromises()
    expect(wrapper.get('.wt-hint').text()).toContain('/bin/zsh · /w/proj')
    socket().push({ t: 'exit', code: 2 })
    await flushPromises()
    expect(wrapper.get('.wt-hint').text()).toContain('code 2')
  })

  it('出错帧 toast 一次', async () => {
    await show()
    socket().push({ t: 'error', message: '内置终端只允许本机访问' })
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('内置终端只允许本机访问', true)
  })

  it('终端功能不可用：不开任何连接，面板说清原因', async () => {
    api.mockImplementation((method, url) => Promise.resolve(
      url === '/api/terminal'
        ? { enabled: false, local: false, reason: '内置终端只允许本机访问' }
        : {}))
    await show()
    expect(FakeSocket.instances).toHaveLength(0)
    expect(wrapper.text()).toContain('内置终端只允许本机访问')
    await wrapper.get('[aria-label="新建终端"]').trigger('click')
    expect(toast).toHaveBeenCalledWith('内置终端只允许本机访问', true)
  })

  it('卸载（登出/会话被删）把全部终端收掉', async () => {
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
