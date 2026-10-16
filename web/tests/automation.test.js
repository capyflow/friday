import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AutoView from '../src/components/AutoView.vue'
import SelectBox from '../src/components/SelectBox.vue'
import { api } from '../src/api.js'
import { toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))
const original = { id: 'task-1', text: '喝水', kind: 'repeat', interval: 3600000000000,
  times: 5, remaining: 3, next: '2026-10-11T10:00:00+08:00', status: 'active', channel: 'web' }
let wrapper
beforeEach(() => {
  vi.useFakeTimers()
  api.mockReset().mockImplementation(async (method, url) => url === '/api/tasks' && method === 'GET' ? { tasks: [original] } : { message: '已完成' })
  toast.mockClear()
  vi.stubGlobal('confirm', vi.fn(() => true))
})
afterEach(() => { wrapper?.unmount(); wrapper = null; vi.useRealTimers(); vi.unstubAllGlobals() })
async function show() { wrapper = mount(AutoView); await flushPromises() }
function button(text, root = wrapper) { return root.findAll('button').find(b => b.text() === text) }
function card() { return wrapper.get('[data-task-id="task-1"]') }

describe('自动化编辑与状态', () => {
  it('完整编辑周期与通道，保留失败输入并禁用重复提交', async () => {
    await show()
    await button('编辑', card()).trigger('click')
    expect(wrapper.get('[aria-label="间隔"]').element.value).toBe('3600s')
    expect(wrapper.get('[aria-label="总次数"]').element.value).toBe('5')
    expect(wrapper.text()).toContain('剩 3/5 次')
    expect(wrapper.text()).toContain('原计划下次')
    await wrapper.get('[aria-label="任务内容"]').setValue('改后的提醒')
    await wrapper.get('[aria-label="间隔"]').setValue('2小时')
    await wrapper.findAllComponents(SelectBox)[1].vm.$emit('update:modelValue', 'feishu')
    let reject
    api.mockImplementationOnce(() => new Promise((_, r) => { reject = r }))
    await wrapper.get('form').trigger('submit')
    expect(wrapper.get('form').attributes('aria-busy')).toBe('true')
    await wrapper.get('form').trigger('submit')
    expect(api.mock.calls.filter(([m]) => m === 'PUT')).toHaveLength(1)
    expect(api).toHaveBeenCalledWith('PUT', '/api/tasks/task-1', { text: '改后的提醒', agent: false, channel: 'feishu', in: '2小时', times: 5 })
    reject(new Error('磁盘写入失败'))
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('磁盘写入失败')
    expect(wrapper.get('[aria-label="任务内容"]').element.value).toBe('改后的提醒')
    expect(button('保存未来安排').element.disabled).toBe(false)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('新建任务')
    expect(toast).toHaveBeenCalledWith('已完成')
  })

  it('暂停失败保留启用任务，成功后恢复使用独立接口', async () => {
    await show()
    api.mockRejectedValueOnce(new Error('保存失败'))
    await button('暂停', card()).trigger('click')
    await flushPromises()
    expect(button('暂停', card()).element.disabled).toBe(false)
    expect(toast).toHaveBeenCalledWith('操作失败：保存失败', true)
    api.mockResolvedValueOnce({ task: { ...original, status: 'paused' } })
      .mockResolvedValueOnce({ tasks: [{ ...original, status: 'paused' }] })
    await button('暂停', card()).trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/tasks/task-1/pause', {})
    expect(card().text()).toContain('已暂停')
    expect(card().text()).toContain('原计划下次')
    await button('恢复', card()).trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/tasks/task-1/resume', {})
  })

  it('显示本任务执行历史且安全显示正文，关联待办只能查看历史', async () => {
    api.mockResolvedValueOnce({ tasks: [{ ...original, todoId: 'todo-1' }] })
    await show()
    expect(card().text()).toContain('请在待办页面管理')
    expect(button('编辑', card())).toBeUndefined()
    api.mockResolvedValueOnce({ notifications: [
      { id: 'own', taskId: 'task-1', text: '<img onerror="alert(1)">', status: 'processing', attempts: 2, createdAt: original.next },
      { id: 'other', taskId: 'task-2', text: '别的任务', status: 'failed' },
    ] })
    await button('执行历史', card()).trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('GET', '/api/notifications?taskId=task-1')
    expect(card().text()).toContain('处理中')
    expect(card().text()).toContain('已尝试 2 次')
    expect(card().text()).toContain('<img onerror="alert(1)">')
    expect(card().find('img').exists()).toBe(false)
    expect(card().text()).not.toContain('别的任务')
  })

  it('新建智能单次、循环和次数校验，卸载停止轮询', async () => {
    await show()
    await wrapper.get('[aria-label="任务内容"]').setValue('检查天气')
    await wrapper.get('[aria-label="触发时间"]').setValue('2026-10-11T10:00:00+08:00')
    await wrapper.get('input[type="checkbox"]').setValue(true)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/tasks', { text: '检查天气', agent: true, channel: '', when: '2026-10-11T10:00:00+08:00' })
    await wrapper.findAllComponents(SelectBox)[0].vm.$emit('update:modelValue', 'repeat')
    await wrapper.get('[aria-label="任务内容"]').setValue('提醒')
    await wrapper.get('[aria-label="间隔"]').setValue('1小时')
    await wrapper.get('[aria-label="总次数"]').setValue('2.5')
    const calls = api.mock.calls.length
    await wrapper.get('form').trigger('submit')
    expect(api.mock.calls.length).toBe(calls)
    expect(wrapper.get('[role="alert"]').text()).toContain('整数次数')
    await wrapper.findAllComponents(SelectBox)[0].vm.$emit('update:modelValue', 'cron')
    await wrapper.get('[aria-label="循环规则"]').setValue('0 8 * * *')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/tasks', { text: '提醒', agent: false, channel: '', every: '0 8 * * *' })
    wrapper.unmount(); wrapper = null
    const afterUnmount = api.mock.calls.length
    await vi.advanceTimersByTimeAsync(10000)
    expect(api.mock.calls.length).toBe(afterUnmount)
  })

  it('取消失败保留任务，文案不声称撤销已经入队执行', async () => {
    await show()
    api.mockRejectedValueOnce(new Error('目录不可写'))
    await button('取消', card()).trigger('click')
    await flushPromises()
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('已经入队的执行会保留'))
    expect(toast).toHaveBeenCalledWith('取消失败：目录不可写', true)
    expect(card().text()).toContain('喝水')
    expect(button('取消', card()).element.disabled).toBe(false)
  })
})

const goalTask = { id: 'g1', text: '把年度报销办完', kind: 'agent', goal: true, maxSteps: 6,
  maxTokens: 120000, skill: '单据整理', when: '2026-10-14T18:00:00+08:00', next: '2026-10-14T18:00:00+08:00',
  status: 'active', channel: '' }

describe('目标模式（自动化页）', () => {
  beforeEach(() => {
    api.mockImplementation(async (method, url) => {
      if (url === '/api/tasks') return { tasks: [goalTask] }
      if (url === '/api/goals') return { goals: [{ ...goalTask, running: true, usedTokens: 4200, stepsLimit: 6, tokensLimit: 120000 }] }
      return { message: '已完成' }
    })
  })

  it('卡片显示目标徽标、已用 token 与运行状态', async () => {
    wrapper = mount(AutoView)
    await flushPromises()
    const cardText = wrapper.get('[data-task-id="g1"]').text()
    expect(cardText).toContain('目标')
    expect(cardText).toContain('4,200')
    expect(cardText).toContain('tokens')
    expect(cardText).toContain('推进中')
    expect(cardText).toContain('技能 单据整理')
    expect(api).toHaveBeenCalledWith('GET', '/api/goals')
  })

  it('勾选目标才出现步数/预算/技能，提交带 goal 字段', async () => {
    wrapper = mount(AutoView)
    await flushPromises()
    expect(wrapper.find('[aria-label="目标步数上限"]').exists()).toBe(false)

    await wrapper.get('input[type="checkbox"]').setValue(true)          // 智能任务
    await wrapper.findAll('input[type="checkbox"]')[1].setValue(true)   // 目标
    expect(wrapper.get('[aria-label="目标步数上限"]').exists()).toBe(true)

    await wrapper.get('[aria-label="任务内容"]').setValue('把花养好')
    await wrapper.get('[aria-label="触发时间"]').setValue('2小时')
    await wrapper.get('[aria-label="目标步数上限"]').setValue('4')
    await wrapper.get('[aria-label="目标 token 预算"]').setValue('50000')
    await wrapper.get('[aria-label="目标钉住的技能"]').setValue('园艺')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    const call = api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/tasks')
    expect(call[2]).toMatchObject({ text: '把花养好', agent: true, goal: true, in: '2小时',
      maxSteps: 4, maxTokens: 50000, skill: '园艺' })
  })

  it('步数填 0 或非整数被挡下，不发请求', async () => {
    wrapper = mount(AutoView)
    await flushPromises()
    await wrapper.findAll('input[type="checkbox"]')[0].setValue(true)
    await wrapper.findAll('input[type="checkbox"]')[1].setValue(true)
    await wrapper.get('[aria-label="任务内容"]').setValue('目标')
    await wrapper.get('[aria-label="触发时间"]').setValue('2小时')
    await wrapper.get('[aria-label="目标步数上限"]').setValue('0')
    const before = api.mock.calls.length
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(api.mock.calls.length).toBe(before)
    expect(wrapper.get('[role="alert"]').text()).toContain('至少为 1')
  })

  it('取消智能任务时目标与其预算一并关闭', async () => {
    wrapper = mount(AutoView)
    await flushPromises()
    await wrapper.findAll('input[type="checkbox"]')[0].setValue(true)
    await wrapper.findAll('input[type="checkbox"]')[1].setValue(true)
    await wrapper.get('[aria-label="目标步数上限"]').setValue('3')
    await wrapper.findAll('input[type="checkbox"]')[0].setValue(false)
    await wrapper.get('input[type="checkbox"]').trigger('change')
    await flushPromises()
    // 取消智能任务后：目标勾选与其预算输入一起消失（目标必须是智能任务）
    expect(wrapper.findAll('input[type="checkbox"]').length).toBe(1)
    expect(wrapper.find('[aria-label="目标步数上限"]').exists()).toBe(false)
    const call = api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/tasks')
    expect(call?.[2]?.goal).not.toBe(true)
  })

  it('查看执行轮次改为打开设置定位调用轨迹，并带上该目标的会话过滤', async () => {
    const { store } = await import('../src/store.js')
    wrapper = mount(AutoView)
    await flushPromises()
    await wrapper.get('[data-task-id="g1"]').findAll('button')
      .find(b => b.text() === '查看执行轮次').trigger('click')
    expect(store.traceFilter).toBe('goal-g1')
    expect(store.settingsSection).toBe('trace')
    expect(store.settingsOpen).toBe(true)
  })

  it('停止这次推进走目标接口', async () => {
    wrapper = mount(AutoView)
    await flushPromises()
    const btn = wrapper.get('[data-task-id="g1"]').findAll('button')
      .find(b => b.text() === '停止这次推进')
    await btn.trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/goals/g1/stop', {})
    expect(toast).toHaveBeenCalledWith('已完成')
  })
})
