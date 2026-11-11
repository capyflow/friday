import { describe, expect, it, vi } from 'vitest'
import { applyEvent } from '../src/turnevents.js'

// turnevents.js 把事件帧翻译成时间线动作：桌面端与移动端共用这一份，
// 两端渲染出的时间线因此必然一致。这里验翻译规则本身。

// ctx 造一个最小的翻译上下文：消息数组 + 造气泡的工厂 + 本端 clientId。
function ctx(over = {}) {
  const messages = []
  return {
    messages,
    mine: 'me',
    makeBot: () => ({ role: 'bot', text: '', blocks: [], round: 0, streaming: true }),
    makeUser: (text, images) => ({ role: 'user', text, images: images || [], turnId: '' }),
    ...over,
  }
}

describe('事件翻译', () => {
  it('自己发的 user.message 不重复显示（乐观气泡已经加过了）', () => {
    const c = ctx()
    applyEvent({ type: 'user.message', clientId: 'me', text: '你好' }, c)
    expect(c.messages).toHaveLength(0)
  })

  it('别人的 user.message 追加进来，并标出来源设备', () => {
    const onRemoteUser = vi.fn()
    const c = ctx({ onRemoteUser })
    applyEvent({ type: 'user.message', clientId: 'other', text: '手机端问的', device: '手机端' }, c)
    expect(c.messages).toHaveLength(1)
    expect(c.messages[0]).toMatchObject({
      role: 'user', text: '手机端问的', remote: true, device: '手机端',
    })
    expect(onRemoteUser).toHaveBeenCalledTimes(1)
  })

  it('delta 按 turnId 归到同一条 bot 气泡', () => {
    const c = ctx()
    applyEvent({ type: 'turn.start', turnId: 't1' }, c)
    applyEvent({ type: 'delta', turnId: 't1', delta: '你好' }, c)
    applyEvent({ type: 'delta', turnId: 't1', delta: '，世界' }, c)
    expect(c.messages).toHaveLength(1)
    expect(c.messages[0].text).toBe('你好，世界')
    expect(c.messages[0].turnId).toBe('t1')
    expect(c.messages[0].streaming).toBe(true)
  })

  it('不同 turnId 是不同轮，不混进同一条气泡', () => {
    const c = ctx()
    applyEvent({ type: 'delta', turnId: 't1', delta: 'A' }, c)
    applyEvent({ type: 'delta', turnId: 't2', delta: 'B' }, c)
    expect(c.messages).toHaveLength(2)
    expect(c.messages.map(m => m.text)).toEqual(['A', 'B'])
  })

  it('turn.end 收尾：停流并挂上本轮汇总', () => {
    const onEnd = vi.fn()
    const c = ctx({ onEnd })
    applyEvent({ type: 'delta', turnId: 't1', delta: '答' }, c)
    applyEvent({ type: 'turn.end', turnId: 't1', turn: { in: 10, out: 2 } }, c)
    expect(c.messages[0].streaming).toBe(false)
    expect(c.messages[0].turn).toEqual({ in: 10, out: 2 })
    expect(onEnd).toHaveBeenCalledTimes(1)
  })

  it('turn.error 报错：这一轮停流，错误走回调', () => {
    const onError = vi.fn()
    const c = ctx({ onError })
    applyEvent({ type: 'delta', turnId: 't1', delta: '答到一半' }, c)
    applyEvent({ type: 'turn.error', turnId: 't1', error: '模型不可用' }, c)
    expect(c.messages[0].streaming).toBe(false)
    expect(onError).toHaveBeenCalledWith('模型不可用', c.messages[0])
  })

  it('paused 标暂停：保住已生成的半截', () => {
    const onPaused = vi.fn()
    const c = ctx({ onPaused })
    applyEvent({ type: 'delta', turnId: 't1', delta: '写了一半' }, c)
    applyEvent({ type: 'paused', turnId: 't1', partial: '写了一半' }, c)
    expect(c.messages[0].paused).toBe(true)
    expect(c.messages[0].streaming).toBe(false)
    expect(c.messages[0].partial).toBe('写了一半')
    expect(onPaused).toHaveBeenCalledTimes(1)
  })

  it('thinking 与正文分块：思考在前、正文在后', () => {
    const c = ctx()
    applyEvent({ type: 'thinking', turnId: 't1', thinking: '先想想' }, c)
    applyEvent({ type: 'delta', turnId: 't1', delta: '答' }, c)
    expect(c.messages[0].blocks.map(b => b.kind)).toEqual(['think', 'text'])
    expect(c.messages[0].blocks[0].text).toBe('先想想')
  })

  it('step 事件推进工具块', () => {
    const c = ctx()
    applyEvent({ type: 'step', turnId: 't1', step: { kind: 'tool', name: 'read', detail: 'a.go', status: 'run' } }, c)
    applyEvent({ type: 'step', turnId: 't1', step: { kind: 'tool', name: 'read', detail: '好了', status: 'ok' } }, c)
    const tools = c.messages[0].blocks.filter(b => b.kind === 'tool')
    expect(tools).toHaveLength(1)
    expect(tools[0].status).toBe('ok')
    expect(tools[0].out).toBe('好了')
  })

  it('plan 挂到这一轮的气泡上', () => {
    const onPlan = vi.fn()
    const c = ctx({ onPlan })
    applyEvent({ type: 'plan', turnId: 't1', plan: { id: 'p1', title: '周末采购' } }, c)
    expect(c.messages[0].plan).toEqual({ id: 'p1', title: '周末采购' })
    expect(onPlan).toHaveBeenCalledWith({ id: 'p1', title: '周末采购' })
  })

  it('turn.queued 给排队中的问题标位次', () => {
    const c = ctx()
    c.messages.push({ role: 'user', text: '排队的问题', turnId: 't1' })
    applyEvent({ type: 'turn.queued', turnId: 't1', text: '排队的问题', position: 2 }, c)
    expect(c.messages[0].queued).toBe(true)
    expect(c.messages[0].position).toBe(2)
  })

  it('turn.canceled 把排队中的问题标成已取消', () => {
    const onCanceled = vi.fn()
    const c = ctx({ onCanceled })
    c.messages.push({ role: 'user', text: '排队的问题', turnId: 't1', queued: true })
    applyEvent({ type: 'turn.canceled', turnId: 't1' }, c)
    expect(c.messages[0].queued).toBe(false)
    expect(c.messages[0].canceled).toBe(true)
    expect(onCanceled).toHaveBeenCalledWith('t1')
  })

  it('status 事件走状态回调', () => {
    const onStatus = vi.fn()
    const c = ctx({ onStatus })
    applyEvent({ type: 'status', status: '正在等另一端的问题跑完' }, c)
    expect(onStatus).toHaveBeenCalledWith('正在等另一端的问题跑完')
    expect(c.messages).toHaveLength(0)
  })
})
