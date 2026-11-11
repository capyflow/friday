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
