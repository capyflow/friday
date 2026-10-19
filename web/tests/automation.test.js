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

// 表单弹窗 Teleport 到 body（与设置弹窗一致的查法），故用 document 查询
const dialog = () => document.querySelector('.dlg')
const field = label => dialog()?.querySelector(`[aria-label="${label}"]`)
const boxes = () => Array.from(document.querySelectorAll('.dlg input[type="checkbox"]'))
const dlgButton = text => Array.from(document.querySelectorAll('.dlg button')).find(b => b.textContent === text)
const alertText = () => document.querySelector('.dlg [role="alert"]')?.textContent
async function click(el) {
  el.dispatchEvent(new window.MouseEvent('click', { bubbles: true, cancelable: true }))
  await flushPromises()
}
async function setValue(label, value) {
  const el = field(label)
  el.value = value
  el.dispatchEvent(new window.Event('input', { bubbles: true }))
  await flushPromises()
}
// v-model 的写回排在 @change 之后，需再派发一次才能让 onAgentToggle 看到新勾选值
async function setChecked(el, value) {
  el.checked = value
  el.dispatchEvent(new window.Event('change', { bubbles: true }))
  el.dispatchEvent(new window.Event('change', { bubbles: true }))
  await flushPromises()
}
async function submitForm() {
  dialog().querySelector('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }))
  await flushPromises()
}
async function openNew() { await button('添加任务').trigger('click'); await flushPromises() }
async function pick(idx, value) { wrapper.findAllComponents(SelectBox)[idx].vm.$emit('update:modelValue', value); await flushPromises() }

describe('自动化新建弹窗', () => {
  it('表单只在弹窗里：添加任务打开，取消清空草稿', async () => {
    await show()
    expect(dialog()).toBeNull()
    expect(wrapper.text()).toContain('新建任务')
    await openNew()
    expect(dialog().getAttribute('aria-modal')).toBe('true')
    expect(document.getElementById('taskDialogTitle').textContent).toBe('新建任务')
    await setValue('任务内容', '没提交的草稿')
    await click(dlgButton('取消'))
    expect(dialog()).toBeNull()
    await openNew()
    expect(field('任务内容').value).toBe('')
  })

  it('确认创建任务，成功后收起弹窗', async () => {
    await show()
    await openNew()
    await setValue('任务内容', '提醒喝水')
    await setValue('触发时间', '10分钟')
    await click(dlgButton('确认'))
    expect(api).toHaveBeenCalledWith('POST', '/api/tasks', { text: '提醒喝水', agent: false, channel: '', in: '10分钟' })
    expect(dialog()).toBeNull()
    expect(toast).toHaveBeenCalledWith('已完成')
  })

  it('校验失败留在弹窗内并保留已填内容', async () => {
    await show()
    await openNew()
    await setValue('任务内容', '提醒喝水')
    await submitForm()
    expect(alertText()).toContain('请填写触发时间')
    expect(field('任务内容').value).toBe('提醒喝水')
  })
})

describe('自动化编辑与状态', () => {
  it('完整编辑周期与通道，保留失败输入并禁用重复提交', async () => {
    await show()
    await button('编辑', card()).trigger('click')
    await flushPromises()
    expect(document.getElementById('taskDialogTitle').textContent).toBe('编辑任务 #task-1')
    expect(field('间隔').value).toBe('3600s')
    expect(field('总次数').value).toBe('5')
    expect(wrapper.text()).toContain('剩 3/5 次')
    expect(dialog().textContent).toContain('原计划下次')
    await setValue('任务内容', '改后的提醒')
    await setValue('间隔', '2小时')
    await pick(1, 'feishu')
    let reject
    api.mockImplementationOnce(() => new Promise((_, r) => { reject = r }))
    await submitForm()
    expect(dialog().querySelector('form').getAttribute('aria-busy')).toBe('true')
    await submitForm()
    expect(api.mock.calls.filter(([m]) => m === 'PUT')).toHaveLength(1)
    expect(api).toHaveBeenCalledWith('PUT', '/api/tasks/task-1', { text: '改后的提醒', agent: false, channel: 'feishu', in: '2小时', times: 5 })
    reject(new Error('磁盘写入失败'))
    await flushPromises()
    expect(alertText()).toBe('磁盘写入失败')
    expect(field('任务内容').value).toBe('改后的提醒')
    expect(dlgButton('确认').disabled).toBe(false)
    await submitForm()
    expect(wrapper.text()).toContain('新建任务')
    expect(dialog()).toBeNull()
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
    await openNew()
    await setValue('任务内容', '检查天气')
    await setValue('触发时间', '2026-10-11T10:00:00+08:00')
    await setChecked(boxes()[0], true)
    await submitForm()
    expect(api).toHaveBeenCalledWith('POST', '/api/tasks', { text: '检查天气', agent: true, channel: '', when: '2026-10-11T10:00:00+08:00' })
    await openNew()
    await pick(0, 'repeat')
    await setValue('任务内容', '提醒')
    await setValue('间隔', '1小时')
    await setValue('总次数', '2.5')
    const calls = api.mock.calls.length
    await submitForm()
    expect(api.mock.calls.length).toBe(calls)
    expect(alertText()).toContain('整数次数')
    await pick(0, 'cron')
    await setValue('循环规则', '0 8 * * *')
    await submitForm()
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
  async function showGoal() { wrapper = mount(AutoView); await flushPromises() }

  it('卡片显示目标徽标、已用 token 与运行状态', async () => {
    await showGoal()
    const cardText = wrapper.get('[data-task-id="g1"]').text()
    expect(cardText).toContain('目标')
    expect(cardText).toContain('4,200')
    expect(cardText).toContain('tokens')
    expect(cardText).toContain('推进中')
    expect(cardText).toContain('技能 单据整理')
    expect(api).toHaveBeenCalledWith('GET', '/api/goals')
  })

  it('勾选目标才出现步数/预算/技能，提交带 goal 字段', async () => {
    await showGoal()
    await openNew()
    expect(field('目标步数上限')).toBeNull()

    await setChecked(boxes()[0], true)                        // 智能任务
    await setChecked(boxes()[1], true)                        // 目标
    expect(field('目标步数上限')).not.toBeNull()

    await setValue('任务内容', '把花养好')
    await setValue('触发时间', '2小时')
    await setValue('目标步数上限', '4')
    await setValue('目标 token 预算', '50000')
    await setValue('目标钉住的技能', '园艺')
    await submitForm()

    const call = api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/tasks')
    expect(call[2]).toMatchObject({ text: '把花养好', agent: true, goal: true, in: '2小时',
      maxSteps: 4, maxTokens: 50000, skill: '园艺' })
  })

  it('步数填 0 或非整数被挡下，不发请求', async () => {
    await showGoal()
    await openNew()
    await setChecked(boxes()[0], true)
    await setChecked(boxes()[1], true)
    await setValue('任务内容', '目标')
    await setValue('触发时间', '2小时')
    await setValue('目标步数上限', '0')
    const before = api.mock.calls.length
    await submitForm()
    expect(api.mock.calls.length).toBe(before)
    expect(alertText()).toContain('至少为 1')
  })

  it('取消智能任务时目标与其预算一并关闭', async () => {
    await showGoal()
    await openNew()
    await setChecked(boxes()[0], true)
    await setChecked(boxes()[1], true)
    await setValue('目标步数上限', '3')
    await setChecked(boxes()[0], false)
    // 取消智能任务后：目标勾选与其预算输入一起消失（目标必须是智能任务）
    expect(boxes().length).toBe(1)
    expect(field('目标步数上限')).toBeNull()
    await submitForm()
    const call = api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/tasks')
    expect(call?.[2]?.goal).not.toBe(true)
  })

  it('查看执行轮次改为打开设置定位调用轨迹，并带上该目标的会话过滤', async () => {
    const { store } = await import('../src/store.js')
    await showGoal()
    await wrapper.get('[data-task-id="g1"]').findAll('button')
      .find(b => b.text() === '查看执行轮次').trigger('click')
    expect(store.traceFilter).toBe('goal-g1')
    expect(store.settingsSection).toBe('trace')
    expect(store.settingsOpen).toBe(true)
  })

  it('停止这次推进走目标接口', async () => {
    await showGoal()
    const btn = wrapper.get('[data-task-id="g1"]').findAll('button')
      .find(b => b.text() === '停止这次推进')
    await btn.trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/goals/g1/stop', {})
    expect(toast).toHaveBeenCalledWith('已完成')
  })
})
