import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import TodoView from '../src/components/TodoView.vue'
import { api } from '../src/api.js'
import { toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

let wrapper
const td = { id: 'todo-1', text: '交房租', done: false, createdAt: '2026-10-10T08:00:00+08:00',
  dueAt: '2026-10-11T18:00:00+08:00', remindAt: '2026-10-11T17:00:00+08:00', reminderTaskId: 'reminder-1' }
function button(text) { return wrapper.findAll('button').find(button => button.text() === text) }
async function showTodos() { wrapper = mount(TodoView); await flushPromises() }
beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-10-10T12:00:00+08:00'))
  api.mockReset().mockResolvedValue({ todos: [td] })
  toast.mockClear()
  vi.stubGlobal('confirm', vi.fn(() => true))
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('待办截止时间和关联提醒', () => {
  it('分别显示截止和提醒，展示待完成/已完成/逾期状态', async () => {
    api.mockResolvedValue({ todos: [td, { ...td, id: 'done', text: '已完成项', done: true },
      { ...td, id: 'late', text: '逾期项', dueAt: '2026-10-09T18:00:00+08:00' }] })
    await showTodos()
    for (const text of ['待完成', '已完成', '已逾期', '截止', '提醒']) expect(wrapper.text()).toContain(text)
    expect(wrapper.text()).toContain('2 项待完成 / 共 3 条')
  })

  it('保存输入的本地截止和提醒时间，只有截止也可保存', async () => {
    await showTodos()
    await wrapper.get('[aria-label="待办内容"]').setValue('还书')
    await wrapper.get('[aria-label="截止时间"]').setValue('2026-10-12T18:00')
    await wrapper.get('[aria-label="提醒时间"]').setValue('2026-10-12T17:00')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/todos', { text: '还书',
      dueAt: new Date('2026-10-12T18:00').toISOString(), remindAt: new Date('2026-10-12T17:00').toISOString() })
    expect(wrapper.get('[aria-label="待办内容"]').element.value).toBe('')
    await wrapper.get('[aria-label="待办内容"]').setValue('只有截止')
    await wrapper.get('[aria-label="截止时间"]').setValue('2026-10-13T18:00')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/todos', expect.objectContaining({ text: '只有截止', remindAt: null }))
  })

  it('编辑完整替换内容与时间，清空提醒发送null', async () => {
    await showTodos()
    await button('编辑').trigger('click')
    expect(wrapper.get('[aria-label="待办内容"]').element.value).toBe('交房租')
    expect(wrapper.get('[aria-label="提醒时间"]').element.value).not.toBe('')
    await wrapper.get('[aria-label="待办内容"]').setValue('交水费')
    await wrapper.get('[aria-label="提醒时间"]').setValue('')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('PUT', '/api/todos/todo-1', expect.objectContaining({ text: '交水费', remindAt: null }))
    expect(button('添加')).toBeDefined()
  })

  it('提交失败保留草稿，可直接重试', async () => {
    await showTodos()
    api.mockRejectedValueOnce(new Error('磁盘不可写'))
    await wrapper.get('[aria-label="待办内容"]').setValue('不能丢失')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.get('[aria-label="待办内容"]').element.value).toBe('不能丢失')
    expect(wrapper.text()).toContain('磁盘不可写')
    expect(button('添加').element.disabled).toBe(false)
  })

  it('源事实已保存但任务同步失败时防重复创建，展示保存状态和恢复同步', async () => {
    await showTodos()
    api.mockRejectedValueOnce(new Error('待办已保存，但提醒同步失败；修复任务存储后重试同步'))
      .mockResolvedValueOnce({ todos: [td], syncError: '任务文件不可写' })
    await wrapper.get('[aria-label="待办内容"]').setValue('已保存')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.get('[aria-label="待办内容"]').element.value).toBe('')
    expect(wrapper.text()).toContain('待办已保存，提醒同步尚未完成')
    api.mockResolvedValueOnce({ message: '已恢复' }).mockResolvedValueOnce({ todos: [td] })
    await button('重试同步').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/todos/reconcile', {})
    expect(button('重试同步')).toBeUndefined()
    expect(toast).toHaveBeenCalledWith('提醒已恢复同步')
  })

  it('保存/完成处理中阻止重复点击并刷新真实状态', async () => {
    let complete
    await showTodos()
    api.mockReturnValueOnce(new Promise(resolve => { complete = resolve }))
    await wrapper.get('input[type=checkbox]').setValue(true)
    expect(wrapper.get('input[type=checkbox]').element.disabled).toBe(true)
    expect(button('删除').element.disabled).toBe(true)
    complete({ todo: { ...td, done: true } })
    api.mockResolvedValueOnce({ todos: [{ ...td, done: true }] })
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/todos/todo-1', { done: true })
    expect(wrapper.text()).toContain('已完成')
    expect(wrapper.get('input[type=checkbox]').element.checked).toBe(true)
  })

  it('删除后刷新并清空正在编辑的条目', async () => {
    await showTodos()
    await button('编辑').trigger('click')
    api.mockResolvedValueOnce({ message: '已删除' }).mockResolvedValueOnce({ todos: [] })
    await button('删除').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE', '/api/todos/todo-1')
    expect(wrapper.get('[aria-label="待办内容"]').element.value).toBe('')
    expect(wrapper.find('table').exists()).toBe(false)
  })
})
