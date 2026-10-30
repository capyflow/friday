import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import RunsView from '../src/components/RunsView.vue'
import { api } from '../src/api.js'
import { store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

const now = new Date().toISOString()
const runs = () => ({
  generatedAt: now,
  acp: [{ threadId: 'th1', title: '修登录跳转', workspace: 'proj-a', agent: 'codex',
    cwd: '/home/u/proj', state: 'running', startedAt: now, contextUsed: 40, contextSize: 100 }],
  plans: [{ id: 'p1', title: '补全文档', state: 'awaiting_confirm', totalSteps: 2,
    currentStep: 1, currentTitle: '梳理缺口', updatedAt: now }],
  goals: [{ id: 'g1', text: '整理书架', status: 'active', running: true,
    usedTokens: 1200, tokensLimit: 10000, stepsLimit: 8 }],
  tasks: [{ id: 'k1', text: '提醒喝水', kind: 'once', status: 'active', next: now },
    { id: 'k2', text: '周报', kind: 'cron', status: 'paused', next: now }],
  approvals: [{ id: 1, tool: 'exec', summary: '执行命令 rm -rf build', detail: 'rm -rf build', createdAt: now }],
  failures: [{ kind: 'notification', id: 'n1', title: '推送失败', detail: 'dingtalk 超时', at: now }],
  counts: { acp: 1, plans: 1, goals: 1, tasks: 2, approvals: 1, failures: 1 },
})

let wrapper
beforeEach(() => {
  api.mockReset().mockResolvedValue(runs())
  toast.mockClear()
  store.view = 'runs'
  store.acpOpenThread = ''
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  store.view = 'chat'
  store.acpOpenThread = ''
})

async function show() {
  wrapper = mount(RunsView)
  await flushPromises()
}

describe('运行看板', () => {
  it('汇总各来源，并给出待审批/计划/目标/任务/失败的分区', async () => {
    await show()
    expect(api).toHaveBeenCalledWith('GET', '/api/runs')
    expect(wrapper.text()).toContain('待审批')
    expect(wrapper.text()).toContain('智能体运行')
    expect(wrapper.text()).toContain('计划')
    expect(wrapper.text()).toContain('目标')
    expect(wrapper.text()).toContain('定时任务')
    expect(wrapper.text()).toContain('需要处理')
    expect(wrapper.text()).toContain('修登录跳转')
    expect(wrapper.text()).toContain('补全文档')
    expect(wrapper.text()).toContain('整理书架')
    expect(wrapper.text()).toContain('提醒喝水')
    expect(wrapper.findAll('.run-card').length).toBe(7)
  })

  it('批准/拒绝走同一条审批接口，并刷新看板', async () => {
    api.mockResolvedValueOnce(runs())
      .mockResolvedValueOnce({ ...runs(), approvals: [], counts: { ...runs().counts, approvals: 0 } })
      .mockResolvedValueOnce({ ...runs(), approvals: [], counts: { ...runs().counts, approvals: 0 } })
    await show()
    const approve = wrapper.findAll('button').find(b => b.text() === '批准')
    await approve.trigger('click')
    await flushPromises()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/approvals/1', { approve: true })
    expect(toast).toHaveBeenCalledWith('已批准')
    expect(wrapper.text()).not.toContain('执行命令 rm -rf build')
  })

  it('ACP 打开会跳到工作区并带上目标会话；停止需要二次确认', async () => {
    await show()
    const open = wrapper.findAll('button').find(b => b.text() === '打开')
    await open.trigger('click')
    expect(store.view).toBe('agent')
    expect(store.acpOpenThread).toBe('th1')

    store.view = 'runs'
    const stop = wrapper.findAll('button').find(b => b.text() === '停止本轮')
    await stop.trigger('click')
    expect(stop.text()).toContain('确认停止')
    await stop.trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/stop', {})
  })

  it('计划、任务和失败项的动作走各自原有接口', async () => {
    await show()
    const pause = wrapper.findAll('button').find(b => b.text() === '暂停')
    await pause.trigger('click')
    await flushPromises()
    // 第一条“暂停”是任务；计划当前是待确认，没有暂停按钮
    expect(api).toHaveBeenCalledWith('POST', '/api/tasks/k1/pause', {})

    const retry = wrapper.findAll('button').find(b => b.text() === '重新排队')
    await retry.trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/notifications/n1/retry', {})

    const stopGoal = wrapper.findAll('button').find(b => b.text() === '停止这次推进')
    await stopGoal.trigger('click')
    expect(stopGoal.text()).toContain('确认停止')
  })

  it('全部为空时显示空状态，而不是一张空表', async () => {
    api.mockResolvedValue({ generatedAt: now, acp: [], plans: [], goals: [], tasks: [],
      approvals: [], failures: [], counts: {} })
    await show()
    expect(wrapper.text()).toContain('现在没有在跑的任务')
    expect(wrapper.find('.empty-art').exists()).toBe(true)
  })
})
