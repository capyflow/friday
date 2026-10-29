import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import CopyMessageButton from '../src/components/CopyMessageButton.vue'

let writeText
beforeEach(() => {
  writeText = vi.fn().mockResolvedValue(undefined)
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { writeText },
  })
})
afterEach(() => {
  delete navigator.clipboard
})

describe('消息复制按钮', () => {
  it('回答只把正文写入剪贴板，忽略思考与工具调用', async () => {
    const wrapper = mount(CopyMessageButton, {
      props: {
        kind: 'answer',
        blocks: [
          { kind: 'think', text: '思考过程' },
          { kind: 'text', text: '这是正文' },
          { kind: 'tool', name: 'ls', text: '运行 ls', out: 'a.txt' },
        ],
      },
    })
    expect(wrapper.get('button').attributes('title')).toBe('复制回答正文')
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('这是正文')
    // Qoder 风：纯图标按钮，反馈只换图标与提示文案
    expect(wrapper.text()).toBe('')
    expect(wrapper.get('button').attributes('title')).toBe('已复制')
    expect(wrapper.get('button').attributes('aria-label')).toBe('已复制到剪贴板')
  })

  it('提问整条原文可复制，提示文案按类型区分', async () => {
    const wrapper = mount(CopyMessageButton, { props: { kind: 'question', text: '明天上海天气怎么样' } })
    expect(wrapper.get('button').attributes('title')).toBe('复制提问')
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('明天上海天气怎么样')
  })

  it('没有内容时不渲染按钮', () => {
    const onlyThink = mount(CopyMessageButton, {
      props: { blocks: [{ kind: 'think', text: '只有思考' }] },
    })
    expect(onlyThink.find('button').exists()).toBe(false)
    const empty = mount(CopyMessageButton, { props: { text: '' } })
    expect(empty.find('button').exists()).toBe(false)
  })
})
