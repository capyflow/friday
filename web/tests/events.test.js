import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { subscribe } from '../src/events.js'

// events.js 是会话事件订阅客户端：一条 SSE 长连接收某个会话的所有 turn 事件。
// 这里验的是「连对了没有」与「缺口兜底」——这两件事错了，双端同步就会静默丢消息。

beforeEach(() => {
  globalThis.__esReset()
  localStorage.clear()
  localStorage.setItem('friday.authToken', 'tk-1')
})

afterEach(() => {
  localStorage.clear()
})

describe('事件订阅', () => {
  it('连的是会话事件流：session/since 走查询参数，token 也走查询参数', () => {
    const sub = subscribe('web', 0, {})
    const sock = globalThis.__esLast()
    expect(sock.url).toContain('/api/events/stream?')
    expect(sock.url).toContain('session=web')
    expect(sock.url).toContain('since=0')
    // EventSource 不能带自定义头，token 只能挂在 URL 上
    expect(sock.url).toContain('token=tk-1')
    sub.stop()
  })

  it('since 决定断点续传的位置', () => {
    const sub = subscribe('web', 42, {})
    expect(globalThis.__esLast().url).toContain('since=42')
    sub.stop()
  })

  it('收到事件帧就交给 onEvent（顶层平铺 JSON）', () => {
    const onEvent = vi.fn()
    const sub = subscribe('web', 0, { onEvent })
    globalThis.__esLast().push({ type: 'delta', turnId: 't1', delta: '你好' })
    expect(onEvent).toHaveBeenCalledTimes(1)
    expect(onEvent.mock.calls[0][0]).toMatchObject({ type: 'delta', turnId: 't1', delta: '你好' })
    sub.stop()
  })

  it('hello 帧只报连接状态，不当业务事件', () => {
    const onEvent = vi.fn()
    const onState = vi.fn()
    const sub = subscribe('web', 0, { onEvent, onState })
    globalThis.__esLast().push({ type: 'hello' })
    expect(onState).toHaveBeenCalledWith('on')
    expect(onEvent).not.toHaveBeenCalled()
    sub.stop()
  })

  it('resync 帧触发全量重拉', () => {
    const onResync = vi.fn()
    const sub = subscribe('web', 0, { onResync })
    globalThis.__esLast().push({ type: 'resync' })
    expect(onResync).toHaveBeenCalledTimes(1)
    sub.stop()
  })

  it('seq 跳号（中间漏了事件）也触发重拉，不假装消息是连续的', () => {
    const onResync = vi.fn()
    const sub = subscribe('web', 0, { onResync })
    const sock = globalThis.__esLast()
    sock.push({ seq: 5, type: 'delta', delta: 'a' })
    expect(onResync).not.toHaveBeenCalled()
    sock.push({ seq: 9, type: 'delta', delta: 'b' })   // 6/7/8 漏了
    expect(onResync).toHaveBeenCalledTimes(1)
    sub.stop()
  })

  it('连续 seq 不触发重拉', () => {
    const onResync = vi.fn()
    const sub = subscribe('web', 0, { onResync })
    const sock = globalThis.__esLast()
    sock.push({ seq: 5, type: 'delta', delta: 'a' })
    sock.push({ seq: 6, type: 'delta', delta: 'b' })
    expect(onResync).not.toHaveBeenCalled()
    sub.stop()
  })

  it('stop() 关掉连接，之后的帧不再送到回调', () => {
    const onEvent = vi.fn()
    const sub = subscribe('web', 0, { onEvent })
    const sock = globalThis.__esLast()
    sub.stop()
    expect(sock.closed).toBe(true)
    sock.push({ type: 'delta', delta: 'x' })
    expect(onEvent).not.toHaveBeenCalled()
  })

  it('记住最后收到的 seq，currentSeq 供断点续传用', () => {
    const sub = subscribe('web', 0, {})
    const sock = globalThis.__esLast()
    sock.push({ seq: 7, type: 'delta', delta: 'a' })
    expect(sub.currentSeq()).toBe(7)
    sub.stop()
  })
})

// 下面这组用例盯的是**连接生命周期**：EventSource 不会因为「被换下」就自己断开，
// 漏关的旧连接会一直占着浏览器的同源连接池（上限 6 条）。池子满了之后新连接
// 建不起来，页面就再也收不到事件——只能刷新。症状是「对话拿不到响应」，
// 根因却在连接管理上，所以这里逐条钉死。
describe('事件订阅的连接生命周期', () => {
  it('断线重连时旧连接会被关掉，不留僵尸连接', () => {
    vi.useFakeTimers()
    try {
      const sub = subscribe('web', 0, {})
      const first = globalThis.__esLast()
      first.onerror()                        // 当前连接出错
      vi.advanceTimersByTime(1000)           // 退避 1s 后重连
      const all = globalThis.__esAll()
      expect(all.length).toBe(2)             // 确实重连了
      expect(first.closed).toBe(true)        // 但旧的那条必须已经关掉
      expect(all.filter(s => !s.closed).length).toBe(1) // 只剩一条活连接
      sub.stop()
    } finally {
      vi.useRealTimers()
    }
  })

  it('僵尸连接（已被换下的旧连接）出错不会再触发重连', () => {
    vi.useFakeTimers()
    try {
      const sub = subscribe('web', 0, {})
      const first = globalThis.__esLast()
      first.onerror()
      vi.advanceTimersByTime(1000)
      const second = globalThis.__esLast()
      expect(second).not.toBe(first)
      // 僵尸再报一次错：不该再开新连接（否则每错一次就多泄漏一条）
      first.onerror()
      vi.advanceTimersByTime(1000)
      expect(globalThis.__esAll().length).toBe(2)
      expect(globalThis.__esLast()).toBe(second)
      sub.stop()
    } finally {
      vi.useRealTimers()
    }
  })

  it('回到前台时连接还活着就不重开（重开既占连接位又丢帧）', () => {
    const sub = subscribe('web', 0, {})
    expect(document.visibilityState).toBe('visible')
    document.dispatchEvent(new Event('visibilitychange'))
    expect(globalThis.__esAll().length).toBe(1)
    sub.stop()
  })

  it('连接断了（不是僵尸）时回前台会补连', () => {
    vi.useFakeTimers()
    try {
      const sub = subscribe('web', 0, {})
      const first = globalThis.__esLast()
      first.close()                         // 连接没了，但还没触发 onerror
      document.dispatchEvent(new Event('visibilitychange'))
      expect(globalThis.__esAll().length).toBe(2)
      sub.stop()
    } finally {
      vi.useRealTimers()
    }
  })

  it('stop() 关掉这次订阅建过的所有连接，不只是当前那条', () => {
    vi.useFakeTimers()
    try {
      const sub = subscribe('web', 0, {})
      const first = globalThis.__esLast()
      first.onerror()
      vi.advanceTimersByTime(1000)
      const second = globalThis.__esLast()
      sub.stop()
      expect(first.closed).toBe(true)
      expect(second.closed).toBe(true)
      expect(globalThis.__esAll().every(s => s.closed)).toBe(true)
    } finally {
      vi.useRealTimers()
    }
  })
})
