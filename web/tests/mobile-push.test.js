import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { push, notices, startPush, stopPush, markRead, loadNotices } from '../src/mobile/push.js'
import { api } from '../src/api.js'

// push.js 是移动端通知长链接的管理器。这里用一只假 WebSocket 验证：握手
// hello 校准未读数、notice 帧按 id 并入去重、已读本地即时回写、断线退避重连
// 与 stopPush 的彻底回收。真实协议在内核 notifyws_test.go 里对拍过。
vi.mock('../src/api.js', () => ({ api: vi.fn() }))

class FakeWS {
  static latest = null
  constructor(url) {
    this.url = url
    this.closed = false
    this.onopen = null
    this.onmessage = null
    this.onclose = null
    this.onerror = null
    FakeWS.latest = this
  }
  close() {
    if (this.closed) return
    this.closed = true
    this.onclose && this.onclose()
  }
  emit(frame) {
    this.onmessage && this.onmessage({ data: JSON.stringify(frame) })
  }
}

beforeEach(() => {
  vi.useFakeTimers()
  api.mockReset().mockResolvedValue({ notifications: [], unread: 0 })
  window.WebSocket = FakeWS
  stopPush()
  notices.splice(0, notices.length)
})
afterEach(() => {
  stopPush()
  vi.useRealTimers()
})

describe('通知长链接 push.js', () => {
  it('连接带 token，hello 帧校准未读数并全量拉一次列表', async () => {
    api.mockResolvedValue({ notifications: [{ id: 'a', text: 'x', readAt: null }], unread: 3 })
    startPush()
    const sock = FakeWS.latest
    expect(sock.url).toContain('/api/notifications/ws?token=')
    expect(push.status).toBe('connecting')
    sock.emit({ t: 'hello', unread: 3 })
    expect(push.status).toBe('on')
    expect(push.unread).toBe(3)
    expect(api).toHaveBeenCalledWith('GET', '/api/notifications')
    await flushPromises() // hello 触发的全量拉取是异步的，等它落地再断言列表
    expect(notices.map(n => n.id)).toEqual(['a'])
  })

  it('notice 帧按 id 并入：新的前插，重复的替换不重复', async () => {
    startPush()
    const sock = FakeWS.latest
    sock.emit({ t: 'hello', unread: 0 })
    await flushPromises() // 先让 hello 的全量拉取落地，保持确定的事件顺序
    sock.emit({ t: 'notice', notice: { id: 'n1', text: '第一条', readAt: null, status: 'pending' } })
    sock.emit({ t: 'notice', notice: { id: 'n2', text: '第二条', readAt: null, status: 'pending' } })
    sock.emit({ t: 'notice', notice: { id: 'n1', text: '第一条', readAt: null, status: 'delivered' } })
    expect(notices.map(n => n.id)).toEqual(['n2', 'n1'])
    expect(notices[1].status).toBe('delivered')
    expect(push.unread).toBe(2)
  })

  it('已读写回本地并重算未读，不再数已读的', async () => {
    startPush()
    const sock = FakeWS.latest
    sock.emit({ t: 'hello', unread: 0 })
    await flushPromises()
    sock.emit({ t: 'notice', notice: { id: 'n1', text: 'x', readAt: null, status: 'delivered' } })
    api.mockResolvedValue({})
    await markRead('n1')
    expect(api).toHaveBeenCalledWith('POST', '/api/notifications/n1/read', {})
    expect(notices[0].readAt).toBeTruthy()
    expect(push.unread).toBe(0)
  })

  it('断线指数退避重连：先 1 秒，再 2 秒', () => {
    startPush()
    const first = FakeWS.latest
    first.close()
    expect(push.status).toBe('off')
    vi.advanceTimersByTime(999)
    expect(FakeWS.latest).toBe(first) // 还没到 1 秒，无新连接
    vi.advanceTimersByTime(1)
    expect(FakeWS.latest).not.toBe(first) // 第一次重连发生
    const second = FakeWS.latest
    second.close()
    vi.advanceTimersByTime(1999)
    expect(FakeWS.latest).toBe(second) // 第二轮退避 2 秒未到
    vi.advanceTimersByTime(1)
    expect(FakeWS.latest).not.toBe(second)
  })

  it('stopPush 关连接且不再重连', () => {
    startPush()
    stopPush()
    expect(FakeWS.latest.closed).toBe(true)
    vi.advanceTimersByTime(60000)
    // 唯一一条连接就是 stopPush 关掉的那条；没有新实例出现
    expect(push.status).toBe('off')
  })

  it('loadNotices 全量兜底', async () => {
    api.mockResolvedValue({ notifications: [{ id: 'b', text: 'y', readAt: null }], unread: 7 })
    await loadNotices()
    expect(notices.map(n => n.id)).toEqual(['b'])
    expect(push.unread).toBe(7)
  })
})
