import { describe, expect, it } from 'vitest'
import { estimateTokens, feedRate, fmtRate } from '../src/speed.js'

describe('estimateTokens', () => {
  it('中文按字算 1，英文按 4 字符算 1', () => {
    expect(estimateTokens('你好世界')).toBe(4)
    expect(estimateTokens('abcd')).toBe(1)
    expect(estimateTokens('你好ab')).toBe(2.5)
    expect(estimateTokens('')).toBe(0)
  })
})

describe('feedRate', () => {
  it('窗口不足 300ms 时先不报速率', () => {
    const m = {}
    expect(feedRate(m, '你好', 0)).toBe(0)
    expect(feedRate(m, '你好', 200)).toBe(0)
  })

  it('按最近窗口的增量算速率', () => {
    const m = {}
    feedRate(m, '你好', 0)
    expect(feedRate(m, '你好', 1000)).toBe(4) // 4 token / 1s
  })

  it('停顿超过 1.5s 后清零重算，不把停顿摊进速度', () => {
    const m = {}
    feedRate(m, '你好', 0)
    expect(feedRate(m, '你好', 1000)).toBe(4)
    expect(feedRate(m, '你好', 5000)).toBe(0) // 停顿后第一片：新窗口还没攒够
    expect(feedRate(m, '你好', 6000)).toBe(4)
  })

  it('窗口外的旧样本不参与计算', () => {
    const m = {}
    feedRate(m, '一二三四', 0)
    feedRate(m, '一二三四', 1000)
    feedRate(m, '一二三四', 2000)
    feedRate(m, '一二三四', 3000)
    // t=4000：0ms 的 4 个样本已滑出 3s 窗口，剩 16 token / 3s
    expect(feedRate(m, '一二三四', 4000)).toBeCloseTo(16 / 3, 1)
  })
})

describe('fmtRate', () => {
  it('100 以下留一位小数，以上取整', () => {
    expect(fmtRate(21.06)).toBe('21.1')
    expect(fmtRate(123.4)).toBe('123')
    expect(fmtRate(0)).toBe('0')
  })
})
