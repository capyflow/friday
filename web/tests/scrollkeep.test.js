import { describe, expect, it, beforeEach, afterEach } from 'vitest'
import { createFootScrollKeep } from '../src/scrollkeep.js'

// jsdom 没有 ResizeObserver：塞一个假的，手动触发回调。
let fired = []
class FakeRO {
  constructor(cb) { fired.push(cb) }
  observe() {}
  disconnect() {}
}

beforeEach(() => { fired = []; globalThis.ResizeObserver = FakeRO })
afterEach(() => { delete globalThis.ResizeObserver })

// 假滚动容器：scrollTop 可写，scrollHeight/clientHeight 固定。
function scroller({ scrollHeight = 1000, clientHeight = 500, scrollTop = 500 } = {}) {
  return { scrollHeight, clientHeight, scrollTop }
}
// 假底栏：高度可变。
function foot(height) {
  return { getBoundingClientRect: () => ({ height }) }
}

describe('底栏变高时把滚动位置跟着挪', () => {
  it('第一次回调只记基线，不动滚动位置', () => {
    const s = scroller()
    const f = foot(100)
    createFootScrollKeep(s, f)
    fired[0]()
    expect(s.scrollTop).toBe(500)
  })

  it('输入框长高：内容底部重新贴到视口底部（对话跟着输入框平移）', () => {
    const s = scroller({ scrollHeight: 1000, clientHeight: 500, scrollTop: 500 })
    const f = foot(100)
    createFootScrollKeep(s, f)
    fired[0]() // 基线
    // 输入框长高 60px：视口矮了 60，底部被遮住 60（视口变矮不会夹 scrollTop，位置还在原处）
    s.clientHeight = 440
    f.getBoundingClientRect = () => ({ height: 160 })
    fired[0]()
    expect(s.scrollTop).toBe(1000) // 重新贴底：内容底部回到视口底部
  })

  it('用户正在翻历史时不把他拽回底部', () => {
    const s = scroller({ scrollHeight: 1000, clientHeight: 500, scrollTop: 200 })
    const f = foot(100)
    createFootScrollKeep(s, f)
    fired[0]()
    s.clientHeight = 440
    s.scrollTop = 200 // 隐藏量 360：用户在翻历史
    f.getBoundingClientRect = () => ({ height: 160 })
    fired[0]()
    expect(s.scrollTop).toBe(200)
  })

  it('输入框缩回：浏览器已把位置夹回，不重复动作', () => {
    const s = scroller({ scrollHeight: 1000, clientHeight: 440, scrollTop: 440 })
    const f = foot(160)
    createFootScrollKeep(s, f)
    fired[0]()
    s.clientHeight = 500
    s.scrollTop = 500 // 视口变高，浏览器已夹到新的底部
    f.getBoundingClientRect = () => ({ height: 100 })
    fired[0]()
    expect(s.scrollTop).toBe(500)
  })

  it('没有 ResizeObserver 时退化为不做任何事（老 WebView 不猜）', () => {
    delete globalThis.ResizeObserver
    const s = scroller()
    const stop = createFootScrollKeep(s, foot(100))
    expect(() => stop()).not.toThrow()
    expect(s.scrollTop).toBe(500)
  })
});
