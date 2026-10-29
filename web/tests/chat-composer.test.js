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
