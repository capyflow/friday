import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ToolsSection from '../src/components/settings/ToolsSection.vue'
import SelectBox from '../src/components/SelectBox.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

// 供应商下拉守两件事：换档真的落到 provider 上（SelectBox 用 :model-value +
// @update:model-value 收口，接错参数不会报错、只会永远停在默认档），
// 以及切回免密钥的必应时不把上家的 Key 跟着提交。
async function show(config = { provider: 'tavily', apiKey: 'tv-key' }) {
  api.mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/tools/search') return config
    if (method === 'POST' && url === '/api/tools/search') return { message: '已保存并生效' }
    return {}
  })
  const w = mount(ToolsSection, { global: { stubs: { teleport: true } } })
  await flushPromises()
  return w
}
async function pick(wrapper, optionLabel) {
  await wrapper.get('.selbox-trigger[aria-label="搜索供应商"]').trigger('click')
  await flushPromises()
  const opt = wrapper.findAll('.selbox-opt').find(o => o.text() === optionLabel)
  if (!opt) throw new Error(`下拉里没有选项：${optionLabel}`)
  await opt.trigger('click')
  await flushPromises()
}
function box(wrapper) { return wrapper.findAllComponents(SelectBox)[0] }
function saved() { return (api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/tools/search') || [])[2] }

describe('设置 · 工具（联网搜索）', () => {
  beforeEach(() => { api.mockReset() })

  it('读回的配置摊进下拉与 Key 输入框', async () => {
    const w = await show()
    expect(box(w).props('modelValue')).toBe('tavily')
    expect(w.get('.selbox-label').text()).toBe('Tavily（免费 1000 次/月）')
    expect(w.get('input[type="password"]').element.value).toBe('tv-key')
  })

  it('换到必应：免密钥档清掉 Key，保存按新档提交', async () => {
    const w = await show()
    await pick(w, '必应（免费，无需 Key）')
    expect(box(w).props('modelValue')).toBe('bing')
    expect(w.get('input[type="password"]').element.value).toBe('')
    expect(w.get('input[type="password"]').element.disabled).toBe(true)
    await w.findAll('button').find(b => b.text() === '保存并生效').trigger('click')
    await flushPromises()
    expect(saved()).toEqual({ provider: 'bing', apiKey: '' })
  })

  it('换到要密钥的档位：Key 输入框放开，填了就按填的存', async () => {
    const w = await show({ provider: 'bing', apiKey: '' })
    await pick(w, '博查（按次付费，中文优）')
    const key = w.get('input[type="password"]')
    expect(key.element.disabled).toBe(false)
    await key.setValue('bc-key')
    await w.findAll('button').find(b => b.text() === '保存并生效').trigger('click')
    await flushPromises()
    expect(saved()).toEqual({ provider: 'bocha', apiKey: 'bc-key' })
  })
})
