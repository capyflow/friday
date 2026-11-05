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
async function openDialog() {
  await wrapper.get('[aria-label="新建闪念"]').trigger('click')
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

  it('悬浮 + 打开编辑弹窗，默认笔记；切到待办后时间字段按需展开', async () => {
    await showBlinko()
    expect(wrapper.find('[aria-label="笔记内容"]').exists()).toBe(false)
    await openDialog()
    expect(wrapper.find('[aria-label="笔记内容"]').exists()).toBe(true)
    await button('.composer-tabs', '待办').trigger('click')
    expect(wrapper.find('[aria-label="待办内容"]').exists()).toBe(true)
    // 时间字段默认收起，保持编辑区干净
    expect(wrapper.find('[aria-label="截止时间"]').exists()).toBe(false)
    await button('.dialog-foot', '截止 / 提醒').trigger('click')
    expect(wrapper.find('[aria-label="截止时间"]').exists()).toBe(true)
    await wrapper.get('[aria-label="待办内容"]').setValue('还书')
    await button('.dialog-foot', '记下').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/entries', { kind: 'todo', text: '还书', dueAt: null, remindAt: null })
    expect(wrapper.find('[aria-label="待办内容"]').exists()).toBe(false)
    // 再开一次，填截止时间保存
    await openDialog()
    await button('.composer-tabs', '待办').trigger('click')
    await button('.dialog-foot', '截止 / 提醒').trigger('click')
    await wrapper.get('[aria-label="截止时间"]').setValue('2026-10-12T18:00')
    await wrapper.get('[aria-label="待办内容"]').setValue('带截止')
    await button('.dialog-foot', '记下').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/entries', expect.objectContaining({
      kind: 'todo', text: '带截止', dueAt: new Date('2026-10-12T18:00').toISOString(), remindAt: null }))
  })

  it('# 按钮打开标签选择：点选已有标签、回车新建，保存时规整进正文', async () => {
    await showBlinko()
    await openDialog()
    await wrapper.get('[aria-label="标签"]').trigger('click')
    await wrapper.get('[aria-label="新建标签"]').setValue('工作')
    await wrapper.get('[aria-label="新建标签"]').trigger('keydown', { key: 'Enter' })
    await wrapper.find('.tag-list').findAll('button').find(b => b.text() === '#灵感').trigger('click')
    await wrapper.get('[aria-label="笔记内容"]').setValue('新想法')
    await button('.dialog-foot', '记下').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/entries', { kind: 'note', text: '新想法 #工作 #灵感' })
  })

  it('Markdown 工具栏在选区上增删语法并保持选区，待办不提供格式条', async () => {
    await showBlinko()
    await openDialog()
    const editor = () => wrapper.get('[aria-label="笔记内容"]')
    expect(wrapper.find('.md-toolbar').exists()).toBe(true)
    await editor().setValue('闪念')
    editor().element.setSelectionRange(0, 2)
    await button('.md-toolbar', 'B').trigger('click')
    await flushPromises()
    expect(editor().element.value).toBe('**闪念**')
    expect([editor().element.selectionStart, editor().element.selectionEnd]).toEqual([2, 4])
    // 原地再点一次取消包裹
    await button('.md-toolbar', 'B').trigger('click')
    await flushPromises()
    expect(editor().element.value).toBe('闪念')
    // 行首标记按整行切换，链接把选区交给 URL
    await editor().setValue('第一行')
    editor().element.setSelectionRange(0, 3)
    await button('.md-toolbar', 'H').trigger('click')
    await flushPromises()
    expect(editor().element.value).toBe('## 第一行')
    await editor().setValue('百度')
    editor().element.setSelectionRange(0, 2)
    await button('.md-toolbar', '链接').trigger('click')
    await flushPromises()
    expect(editor().element.value).toBe('[百度](https://)')
    expect([editor().element.selectionStart, editor().element.selectionEnd]).toEqual([5, 13])
    // 待办与笔记一样支持 Markdown，格式条同样可用
    await button('.composer-tabs', '待办').trigger('click')
    expect(wrapper.find('.md-toolbar').exists()).toBe(true)
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

  it('点卡片正文打开详情弹窗：完整正文与全部元信息，待办正文也渲染 Markdown', async () => {
    const mdTodo = { id: 't3', kind: 'todo', text: '买 **牛奶**', done: false, tags: ['买菜'],
      createdAt: '2026-10-10T10:00:00+08:00', dueAt: '2026-10-11T18:00:00+08:00', remindAt: '2026-10-11T17:00:00+08:00' }
    await showBlinko({ ...fixture, entries: [mdTodo, note], tags: ['灵感', '买菜'] })
    // 列表里待办正文走 Markdown：加粗渲染成 strong
    const firstCard = wrapper.findAll('.entry-md')[0]
    expect(firstCard.find('strong').exists()).toBe(true)
    // 点正文打开详情：完整内容不截断，元信息齐全
    await firstCard.trigger('click')
    const detail = wrapper.get('[aria-label="闪念详情"]')
    expect(detail.find('strong').exists()).toBe(true)
    expect(detail.text()).toContain('买 牛奶')
    expect(detail.text()).toContain('截止')
    expect(detail.text()).toContain('提醒')
    expect(detail.text()).toContain('创建于')
    expect(detail.find('.detail-body .entry-clamp').exists()).toBe(false) // 详情正文不截断
    // Esc 关闭
    await detail.trigger('keydown', { key: 'Escape' })
    expect(wrapper.find('[aria-label="闪念详情"]').exists()).toBe(false)
    // 再开一次，从详情转编辑：预填原文本
    await wrapper.findAll('.entry-md')[0].trigger('click')
    await button('.detail-dialog', '编辑').trigger('click')
    expect(wrapper.find('[aria-label="闪念详情"]').exists()).toBe(false)
    expect(wrapper.get('.blinko-dialog textarea').element.value).toBe('买 **牛奶**')
  })

  it('列表正文高度上限：短内容不截断，详情里链接点击保持默认跳转', async () => {
    const longNote = { id: 'n2', kind: 'note', text: Array.from({ length: 40 }, (_, i) => `第${i + 1}行`).join('\n\n'),
      tags: [], createdAt: '2026-10-10T11:00:00+08:00' }
    await showBlinko({ ...fixture, entries: [longNote, note] })
    // jsdom 不做布局，clamp 截断只验证样式挂上了，渐隐类由运行时测量决定
    const longCard = wrapper.findAll('.entry-clamp')[0]
    expect(longCard.classes()).toContain('entry-clamp')
    // 详情正文不带截断样式
    await wrapper.findAll('.entry-md')[0].trigger('click')
    const detail = wrapper.get('[aria-label="闪念详情"]')
    expect(detail.get('.detail-body').classes()).not.toContain('entry-clamp')
    await detail.trigger('keydown', { key: 'Escape' })
  })

  it('编辑复用弹窗：预填内容与时间，保存完整替换并同步标签', async () => {
    await showBlinko()
    await button('.entry-actions', '编辑').trigger('click')
    const dialog = wrapper.get('.blinko-dialog')
    expect(dialog.get('textarea').element.value).toBe('交房租')
    expect(dialog.find('[aria-label="截止时间"]').exists()).toBe(true) // 已有时间默认展开
    await dialog.get('textarea').setValue('交水费')
    await dialog.get('[aria-label="提醒时间"]').setValue('')
    await button('.dialog-foot', '保存').trigger('click')
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
