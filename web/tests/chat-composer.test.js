import { afterEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ChatComposer from '../src/components/ChatComposer.vue'

// 键盘语义的单测：回车与输入法组字、Esc 与"停止本轮"。
// 这两条都是"看起来能跑、到了中文输入法/全屏场景才出问题"的地方，单独钉住。
let wrapper
afterEach(() => { wrapper?.unmount(); wrapper = null })

function show(props = {}) {
  wrapper = mount(ChatComposer, {
    props: { modelValue: '', ariaLabel: '测试输入', ...props },
    attachTo: document.body,
  })
  return wrapper.get('textarea')
}

describe('ChatComposer 键盘语义', () => {
  it('组字中的回车不上屏成发送（isComposing / keyCode 229 / compositionstart）', async () => {
    const ta = show()
    await ta.trigger('keydown', { key: 'Enter', isComposing: true })
    await ta.trigger('keydown', { key: 'Enter', keyCode: 229 })
    await ta.trigger('compositionstart')
    await ta.trigger('keydown', { key: 'Enter' })
    expect(wrapper.emitted('primary')).toBeUndefined()
  })

  it('compositionend 紧跟着的那次回车仍然吞掉（Safari 顺序），之后正常发送', async () => {
    const ta = show()
    await ta.trigger('compositionstart')
    await ta.trigger('compositionend')
    await ta.trigger('keydown', { key: 'Enter' }) // 上屏的那一下
    expect(wrapper.emitted('primary')).toBeUndefined()
    await new Promise(r => setTimeout(r, 0))      // 组字标志异步清除之后
    await ta.trigger('keydown', { key: 'Enter' })
    expect(wrapper.emitted('primary')).toHaveLength(1)
  })

  it('Shift+Enter 是换行，不发送', async () => {
    const ta = show()
    await ta.trigger('keydown', { key: 'Enter', shiftKey: true })
    expect(wrapper.emitted('primary')).toBeUndefined()
  })

  it('Esc 在回答中发 stop 并阻止默认行为；空闲时不发', async () => {
    const ta = show({ busy: true })
    ta.element.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
    expect(wrapper.emitted('stop')).toHaveLength(1)

    wrapper.unmount()
    const idle = show({ busy: false })
    const ev = new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true })
    idle.element.dispatchEvent(ev)
    expect(wrapper.emitted('stop')).toBeUndefined()
    expect(ev.defaultPrevented).toBe(false)
  })

  it('排队模式下主键提示与 Esc 提示并存', () => {
    show({ busy: true, queueOn: true })
    expect(wrapper.get('.kbd-hint').text()).toContain('加入队列')
    expect(wrapper.get('.kbd-hint').text()).toContain('Esc 停止')
  })
})

// 输入框高度是 JS 按 scrollHeight 写内联样式的，一旦被外部事件抹掉，只有再敲键才会恢复。
// jsdom 不做布局，所以这里把量到的尺寸打桩，专门验"什么时候该重算一次"。
describe('ChatComposer 自动增高', () => {
  function stubMetrics(el, h) {
    Object.defineProperty(el, 'scrollHeight', { configurable: true, value: h })
    el.getClientRects = () => [{ top: 0, left: 0, width: 300, height: 20 }]
  }

  it('外部回填长文本就撑开（发送后清空、取回队列项、切会话回草稿都走这条）', async () => {
    const ta = show()
    stubMetrics(ta.element, 120)
    await wrapper.setProps({ modelValue: '第一行\n第二行\n第三行' })
    await wrapper.vm.$nextTick()
    await wrapper.vm.$nextTick()
    expect(ta.element.style.height).toBe('120px')
  })

  it('窗口重新聚焦时按内容补算，不再停在 rows=1 的一行', () => {
    const ta = show({ modelValue: '一段已经写了很多的草稿' })
    stubMetrics(ta.element, 96)
    ta.element.style.height = '' // 切走再回来，内联高度被丢掉
    window.dispatchEvent(new window.Event('focus'))
    expect(ta.element.style.height).toBe('96px')
  })

  it('自己不可见（量不到尺寸）时不动高度，别把框压成 0', () => {
    const ta = show()
    const el = ta.element
    Object.defineProperty(el, 'scrollHeight', { configurable: true, value: 0 })
    el.getClientRects = () => []
    el.style.height = '48px'
    window.dispatchEvent(new window.Event('focus'))
    expect(el.style.height).toBe('48px')
  })
})
