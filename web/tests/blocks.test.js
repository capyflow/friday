import { describe, expect, it } from 'vitest'
import { answerText, closeRunningTools } from '../src/blocks.js'

describe('answerText', () => {
  it('只拼正文块，思考与工具调用不进入复制内容', () => {
    const blocks = [
      { kind: 'think', text: '先想一想' },
      { kind: 'text', text: '第一段回答' },
      { kind: 'tool', name: 'ls', text: '运行 ls', out: 'a.txt' },
      { kind: 'text', text: '  第二段回答  ' },
    ]
    expect(answerText(blocks)).toBe('第一段回答\n\n第二段回答')
  })

  it('没有正文时返回空串', () => {
    expect(answerText([{ kind: 'think', text: '只有思考' }, { kind: 'tool', name: 't' }])).toBe('')
    expect(answerText(undefined)).toBe('')
  })
})

describe('closeRunningTools', () => {
  it('只收运行中的工具块，已完成与出错的保持原样', () => {
    const m = { blocks: [
      { kind: 'tool', name: 'a', status: 'run' },
      { kind: 'tool', name: 'b', status: 'ok', out: 'x' },
      { kind: 'tool', name: 'c', status: 'fail', out: '炸了' },
      { kind: 'think', text: '思考' },
    ] }
    closeRunningTools(m)
    expect(m.blocks.map(b => b.status)).toEqual(['ok', 'ok', 'fail', undefined])
  })

  it('blocks 缺失时不抛错', () => {
    expect(() => closeRunningTools({})).not.toThrow()
  })
})
