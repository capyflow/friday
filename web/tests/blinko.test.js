import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import BlinkoView from '../src/components/BlinkoView.vue'
import { api } from '../src/api.js'
import { toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

let wrapper
const note = { id: 'n1', kind: 'note', text: '#灵感 闪念想法', tags: ['灵感'], done: false,
  createdAt: '2026-10-10T08:00:00+08:00' }
const todoItem = { id: 't1', kind: 'todo', text: '交房租', done: false,
  createdAt: '2026-10-10T09:00:00+08:00', dueAt: '2026-10-11T18:00:00+08:00', remindAt: '2026-10-11T17:00:00+08:00' }
const fixture = { entries: [note, todoItem], tags: ['灵感'], syncError: '' }

function button(scope, text) {
  return wrapper.find(scope).findAll('button').find(button => button.text() === text)
}
async function showBlinko(data = fixture) {
  api.mockReset().mockResolvedValue(data)
  wrapper = mount(BlinkoView)
  await flushPromises()
}
beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-10-10T12:00:00+08:00'))
  toast.mockClear()
  vi.stubGlobal('confirm', vi.fn(() => true))
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('闪念：笔记与待办统一时间线', () => {
  it('按天分组渲染笔记与待办，统计行汇总两类条目', async () => {
    await showBlinko()
    expect(wrapper.text()).toContain('今天')
    expect(wrapper.text()).toContain('1 条笔记 · 1 项未完成 · 共 2 条')
    expect(wrapper.text()).toContain('闪念想法')
    expect(wrapper.text()).toContain('交房租')
    expect(wrapper.find('.entry-md').exists()).toBe(true)
  })

  it('composer 默认记笔记，切到待办展开时间字段并分开发送', async () => {
    await showBlinko()
    expect(wrapper.find('[aria-label="笔记内容"]').exists()).toBe(true)
    await wrapper.get('[aria-label="笔记内容"]').setValue('新想法')
    await button('.composer-tabs', '待办').trigger('click')
    expect(wrapper.find('[aria-label="笔记内容"]').exists()).toBe(false)
    expect(wrapper.find('[aria-label="待办内容"]').exists()).toBe(true)
    await wrapper.get('[aria-label="待办内容"]').setValue('还书')
    await wrapper.get('.composer-foot .btn').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/entries', { kind: 'todo', text: '还书', dueAt: null, remindAt: null })
    await wrapper.get('[aria-label="截止时间"]').setValue('2026-10-12T18:00')
    await wrapper.get('[aria-label="待办内容"]').setValue('带截止')
    await wrapper.get('.composer-foot .btn').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/entries', expect.objectContaining({
      kind: 'todo', text: '带截止', dueAt: new Date('2026-10-12T18:00').toISOString(), remindAt: null }))
  })

  it('tabs 过滤：待办页只看未完成，已完成页按完成时间展示', async () => {
    const done = { id: 't2', kind: 'todo', text: '已完成项', done: true, doneAt: '2026-10-09T20:00:00+08:00',
      createdAt: '2026-10-09T10:00:00+08:00' }
    await showBlinko({ ...fixture, entries: [note, todoItem, done] })
    await button('.blinko-tabs', '待办').trigger('click')
    expect(wrapper.text()).toContain('交房租')
    expect(wrapper.text()).not.toContain('已完成项')
    await button('.blinko-tabs', '已完成').trigger('click')
    expect(wrapper.text()).toContain('已完成项')
    expect(wrapper.text()).toContain('完成于')
    expect(wrapper.text()).not.toContain('闪念想法')
    await button('.blinko-tabs', '全部').trigger('click')
    expect(wrapper.text()).toContain('闪念想法')
  })

  it('搜索与标签筛选', async () => {
    await showBlinko()
    await wrapper.get('[aria-label="搜索闪念"]').setValue('房租')
    expect(wrapper.text()).toContain('交房租')
    expect(wrapper.text()).not.toContain('闪念想法')
    await wrapper.get('[aria-label="搜索闪念"]').setValue('')
    await wrapper.get('.blinko-tagbar .chip:nth-child(2)').trigger('click')
    expect(wrapper.text()).toContain('闪念想法')
    expect(wrapper.text()).not.toContain('交房租')
  })

  it('勾选完成、置顶与删除调用对应端点并刷新', async () => {
    await showBlinko()
    // 时间线按创建时间倒序，第一张卡是待办 t1。
    await wrapper.get('input[type=checkbox]').setValue(true)
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/entries/t1', { done: true })
    await button('.entry-actions', '★').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/entries/t1/pin', { pinned: true })
    await button('.entry-actions', '删除').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE', '/api/entries/t1')
    expect(confirm).toHaveBeenCalled()
  })

  it('内联编辑待办完整替换内容与时间', async () => {
    await showBlinko()
    await button('.entry-actions', '编辑').trigger('click')
    const editor = wrapper.get('.entry-editor')
    expect(editor.get('textarea').element.value).toBe('交房租')
    await editor.get('textarea').setValue('交水费')
    await editor.get('[aria-label="提醒时间"]').setValue('')
    await button('.editor-actions', '保存').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('PUT', '/api/entries/t1', expect.objectContaining({ text: '交水费', remindAt: null }))
  })

  it('空清单按 tab 展示对应引导文案', async () => {
    await showBlinko({ entries: [], tags: [], syncError: '' })
    expect(wrapper.text()).toContain('清单是空的')
    await button('.blinko-tabs', '已完成').trigger('click')
    expect(wrapper.text()).toContain('还没有已完成的待办')
  })
})
