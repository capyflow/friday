import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import RuntimeSection from '../src/components/settings/RuntimeSection.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

// 运行上限这一节只管一件事：一轮回答里允许多少次工具调用。
// 读回当前值与可填区间，保存走 POST /api/runtime，填 0 恢复默认。
async function show(runtime = { maxIterations: 100, defaultMaxIterations: 100, minIterations: 1, maxIterationsLimit: 500 }) {
  api.mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/runtime') return runtime
    if (method === 'POST' && url === '/api/runtime') return { maxIterations: runtime.maxIterations, message: '已保存' }
    return {}
  })
  const w = mount(RuntimeSection)
  await flushPromises()
  return w
}
function saved() { return (api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/runtime') || [])[2] }

describe('设置 · 运行上限', () => {
  beforeEach(() => { api.mockReset() })

  it('读回当前上限与可填区间', async () => {
    const w = await show({ maxIterations: 60, defaultMaxIterations: 100, minIterations: 1, maxIterationsLimit: 500 })
    expect(w.get('input[type="number"]').element.value).toBe('60')
    expect(w.text()).toContain('默认 100 轮')
    expect(w.text()).toContain('填 0 恢复默认')
  })

  it('保存把新值提交给后端', async () => {
    const w = await show()
    await w.get('input[type="number"]').setValue('200')
    await w.findAll('button').find(b => b.text() === '保存并热生效').trigger('click')
    await flushPromises()
    expect(saved()).toEqual({ maxIterations: 200 })
  })

  it('越界不提交：本地先拦下并说明可填范围', async () => {
    const w = await show()
    await w.get('input[type="number"]').setValue('999')
    await w.findAll('button').find(b => b.text() === '保存并热生效').trigger('click')
    await flushPromises()
    expect(saved()).toBeUndefined()
    expect(w.text()).toContain('1-500')
  })

  it('恢复默认：填 0 提交，由后端回到默认值', async () => {
    const w = await show()
    await w.get('input[type="number"]').setValue('0')
    await w.findAll('button').find(b => b.text() === '保存并热生效').trigger('click')
    await flushPromises()
    expect(saved()).toEqual({ maxIterations: 0 })
  })
})
