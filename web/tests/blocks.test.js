import { describe, expect, it } from 'vitest'
import { answerText, closeRunningTools, pushToolStep } from '../src/blocks.js'

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

describe('pushToolStep', () => {
  it('同 id 的进度更新只保留一行工具调用', () => {
    const m = { blocks: [], round: 0 }
    pushToolStep(m, { id: 't1', name: 'Bash', detail: 'other', status: 'run' })
    pushToolStep(m, { id: 't1', name: 'Bash', detail: 'other', status: 'run' })
    expect(m.blocks).toHaveLength(1)
    expect(m.round).toBe(1)
  })

  it('ok/fail 按 id 回填原行，同名并行的两次调用互不串行', () => {
    const m = { blocks: [], round: 0 }
    pushToolStep(m, { id: 't1', name: 'Bash', status: 'run' })
    pushToolStep(m, { id: 't2', name: 'Bash', status: 'run' })
    pushToolStep(m, { id: 't1', name: 'Bash', detail: 'done', status: 'ok' })
    expect(m.blocks[0].status).toBe('ok')
    expect(m.blocks[0].out).toBe('done')
    expect(m.blocks[1].status).toBe('run')
    pushToolStep(m, { id: 't2', name: 'Bash', status: 'fail' })
    expect(m.blocks[1].status).toBe('fail')
  })

  it('主会话没有 id 时仍按同名运行中块收尾', () => {
    const m = { blocks: [], round: 0 }
    pushToolStep(m, { name: 'exec_command', status: 'run' })
    pushToolStep(m, { name: 'exec_command', detail: 'ok', status: 'ok' })
    expect(m.blocks).toHaveLength(1)
    expect(m.blocks[0].status).toBe('ok')
    expect(m.blocks[0].out).toBe('ok')
  })

  it('参数晚到只刷新原行；完成事件没带结果也不清掉已有内容', () => {
    const m = { blocks: [], round: 0 }
    pushToolStep(m, { id: 't1', name: 'Bash', status: 'run' })
    pushToolStep(m, { id: 't1', name: 'Bash', detail: '{"command":"ls"}', status: 'run' })
    expect(m.blocks).toHaveLength(1)
    expect(m.blocks[0].text).toContain('ls')
    pushToolStep(m, { id: 't1', name: 'Bash', status: 'ok' })
    expect(m.blocks[0].status).toBe('ok')
    expect(m.blocks[0].text).toContain('ls')
    expect(m.blocks[0].out).toBe('')
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
