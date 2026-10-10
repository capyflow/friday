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
