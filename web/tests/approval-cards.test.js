import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ApprovalCards from '../src/components/ApprovalCards.vue'
import { api } from '../src/api.js'
import { store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

let wrapper
beforeEach(() => {
  store.loggedIn = true
  store.view = 'chat'
  store.mode = 'life'
  store.acpCurrentThread = ''
  store.acpOpenThread = ''
  api.mockReset().mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/approvals') {
      return { approvals: [{ id: 7, tool: '智能体权限', summary: '执行 rm', detail: 'rm -rf tmp/' }] }
    }
    return { message: '已批准' }
  })
  toast.mockClear()
})
afterEach(() => { wrapper?.unmount(); wrapper = null })
async function show() {
  wrapper = mount(ApprovalCards)
  await flushPromises()
}
function approveButton() {
  return wrapper.findAll('button').find(b => b.text() === '批准')
}

describe('审批卡（含智能体权限卡被 turn 结束撤下的场景）', () => {
  it('轮询展示待审批卡，批准成功后移除并提示', async () => {
    await show()
    expect(wrapper.text()).toContain('执行 rm')
    await approveButton().trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/approvals/7', { approve: true, remember: false })
    expect(wrapper.text()).not.toContain('执行 rm')
    expect(toast).toHaveBeenCalledWith('已批准')
  })

  it('决定打在已撤下的卡上（对话已结束）时提示失效原因，而非无响应', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/approvals') {
        return { approvals: [{ id: 8, tool: '智能体权限', summary: '无人处理的授权' }] }
      }
      throw new Error('未找到待审批操作（可能已超时或已处理）')
    })
    await show()
    expect(wrapper.text()).toContain('无人处理的授权')
    await approveButton().trigger('click')
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('未找到待审批操作（可能已超时或已处理）', true)
    // 卡保留在本会话列表里（服务端 5s 轮询对齐后自然消失），用户不会误以为点成功
    expect(wrapper.text()).toContain('无人处理的授权')
  })
})

// 问答卡：agent 借 session/request_permission 问用户（AskUserQuestion 这类）。
// 守的是"别再复用授权那三个键"——按钮必须是 agent 自己给的选项，点哪条回哪条。
const questionItem = {
  id: 21, tool: '智能体提问', summary: '本轮修复的范围怎么定？', kind: 'question',
  detail: '智能体 zcode 在等你回答（1 个问题）\n1. 范围 · 本轮修复的范围怎么定？\n   a) 最小影响面 —— 只拦新上传',
  options: [
    { id: 'opt_min', name: '最小影响面', kind: 'allow_once' },
    { id: 'opt_full', name: '全量封堵', kind: 'allow_once' },
    { id: 'reject', name: '拒绝', kind: 'reject_once' },
  ],
  questions: [{ header: '范围', question: '本轮修复的范围怎么定？', multiSelect: false,
    options: [{ label: '最小影响面', description: '只拦新上传' },
      { label: '全量封堵', description: '含存量盘点与校验' }] }],
}

async function showQuestion(item = questionItem) {
  api.mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/approvals') return { approvals: [item] }
    return { message: '已回答，agent 继续' }
  })
  await show()
}

describe('审批卡归属（不是当前会话就收进右上角提示）', () => {
  const acpApproval = {
    id: 31, tool: '智能体权限', summary: '执行 pnpm test', detail: 'cd web && pnpm test',
    source: 'acp', threadId: 'th-1', threadTitle: '修构建', agent: 'codex',
  }
  async function showAcp() {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/approvals') return { approvals: [acpApproval] }
      return { message: '已批准' }
    })
    await show()
  }

  it('其他会话的审批不铺在当前页，右上角提示条数并可跳到对应会话', async () => {
    await showAcp()
    expect(wrapper.find('.approval-card').exists()).toBe(false)
    const hint = wrapper.get('#approvalHint')
    expect(hint.text()).toContain('1 个待处理')
    await hint.get('.ah-btn').trigger('click')
    expect(wrapper.get('.ah-item').text()).toContain('修构建')
    await wrapper.get('.ah-item').trigger('click')
    expect(store.mode).toBe('work')
    expect(store.view).toBe('agent')
    expect(store.acpOpenThread).toBe('th-1')
  })

  it('切到目标会话后卡片就地展开，不再占用右上角提示', async () => {
    store.view = 'agent'
    store.acpCurrentThread = 'th-1'
    await showAcp()
    expect(wrapper.get('.approval-card').text()).toContain('执行 pnpm test')
    expect(wrapper.find('#approvalHint').exists()).toBe(false)
  })
})

describe('问答卡（agent 在问用户，不是请求授权）', () => {
  it('按钮换成 agent 给的选项，授权那三个键不再出现', async () => {
    await showQuestion()
    const labels = wrapper.findAll('.approval-card button').map(b => b.text())
    expect(labels).toEqual(['最小影响面', '全量封堵', '拒绝'])
    expect(labels).not.toContain('批准并记住')
    expect(wrapper.get('.approval-card').classes()).toContain('is-question')
  })

  it('问题按 header/题干/选项说明排版，正文不是 JSON', async () => {
    await showQuestion()
    expect(wrapper.get('.aq-head').text()).toBe('范围')
    expect(wrapper.get('.aq-q').text()).toBe('本轮修复的范围怎么定？')
    const opts = wrapper.findAll('.aq-opts li').map(li => li.text())
    expect(opts[0]).toContain('最小影响面')
    expect(opts[0]).toContain('只拦新上传')
    expect(wrapper.find('.approval-card pre').exists()).toBe(false)
  })

  it('点「全量封堵」就按那条的 optionId 回答，卡片随即移除', async () => {
    await showQuestion()
    const btn = wrapper.findAll('.approval-card button').find(b => b.text() === '全量封堵')
    await btn.trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/approvals/21', { choose: 'opt_full' })
    expect(wrapper.text()).not.toContain('本轮修复的范围怎么定？')
    expect(toast).toHaveBeenCalledWith('已回答，agent 继续')
  })

  it('标了可多选时说明只能选一项，别让人以为点完还能补选', async () => {
    await showQuestion({ ...questionItem,
      questions: [{ ...questionItem.questions[0], multiSelect: true }] })
    expect(wrapper.get('.aq-note').text()).toContain('点一项就是答案')
  })

  it('多选卡：勾选带选中态，重复点取消，提交所选按 choices 数组回传', async () => {
    await showQuestion({ ...questionItem, multiSelect: true })
    const toggle = name => wrapper.findAll('.approval-card button').find(b => b.text() === name)
    await toggle('最小影响面').trigger('click')
    await toggle('全量封堵').trigger('click')
    await toggle('全量封堵').trigger('click') // 再点一次 = 取消勾选
    await toggle('拒绝').trigger('click')
    const on = wrapper.findAll('.approval-card button.is-on').map(b => b.text())
    expect(on).toEqual(['最小影响面', '拒绝'])
    const submit = wrapper.get('.aq-multi-submit')
    expect(submit.text()).toContain('提交所选（2）')
    await submit.trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/approvals/21', { choices: ['opt_min', 'reject'] })
    expect(wrapper.text()).not.toContain('本轮修复的范围怎么定？')
    expect(toast).toHaveBeenCalledWith('已回答，agent 继续')
  })

  it('多选卡没勾任何项时提交按钮置灰', async () => {
    await showQuestion({ ...questionItem, multiSelect: true })
    expect(wrapper.get('.aq-multi-submit').attributes('disabled')).toBeDefined()
    expect(wrapper.find('.aq-note').exists()).toBe(false)
  })

  it('没声明自由文本时不显示输入框', async () => {
    await showQuestion()
    expect(wrapper.find('.aq-text').exists()).toBe(false)
  })

  it('允许自由文本时显示输入框，提交走 text 而不是 choose', async () => {
    await showQuestion({ ...questionItem, allowText: true, textPlaceholder: '也可以自己写' })
    const input = wrapper.get('.aq-text textarea')
    expect(input.attributes('placeholder')).toBe('也可以自己写')
    await input.setValue('自定义方案')
    const btn = wrapper.findAll('.approval-card button').find(b => b.text() === '提交答案')
    await btn.trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/approvals/21', { text: '自定义方案' })
    expect(wrapper.text()).not.toContain('本轮修复的范围怎么定？')
    expect(toast).toHaveBeenCalledWith('已回答，agent 继续')
  })

  it('认不出问题正文时退回可读 detail 文本，不空着一张卡', async () => {
    await showQuestion({ ...questionItem, questions: undefined })
    expect(wrapper.find('.aq-list').exists()).toBe(false)
    expect(wrapper.get('.approval-card pre').text()).toContain('在等你回答')
  })

  it('回答失败时留着卡片并说清原因', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/approvals') return { approvals: [questionItem] }
      throw new Error('这张卡没有这个选项，或它不是问答卡（可能已超时或已处理）')
    })
    await show()
    await wrapper.findAll('.approval-card button')[0].trigger('click')
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('这张卡没有这个选项，或它不是问答卡（可能已超时或已处理）', true)
    expect(wrapper.text()).toContain('本轮修复的范围怎么定？')
  })
})
