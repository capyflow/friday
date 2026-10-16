import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ProviderSettings from '../src/components/ProviderSettings.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
let wrapper

const LIB = {
  active: 'step-5',
  globalWindowTokens: 0,
  models: [
    { name: 'step-5', baseURL: 'https://api.stepfun.com/step_plan/v1', model: 'step-5-preview', keySet: true, contextWindow: 128000, inputs: ['text', 'image'] },
    { name: 'ds', baseURL: 'https://api.deepseek.com/v1', model: 'deepseek-chat', keySet: false, contextWindow: 0, inputs: ['text'] },
  ],
}
const testOk = { ok: true, text: { status: 'supported', message: '文本连接测试通过' }, vision: { status: 'not_tested', message: '未测试图片理解' } }

beforeEach(() => {
  api.mockReset().mockImplementation((method, url) => {
    if (method === 'GET' && url === '/api/provider/models') return Promise.resolve(LIB)
    if (method === 'POST' && url === '/api/provider/models') return Promise.resolve({ message: '已保存' })
    return Promise.resolve(testOk)
  })
})
afterEach(() => { wrapper?.unmount(); wrapper = null })

async function show() {
  wrapper = mount(ProviderSettings)
  await flushPromises()
  return wrapper
}
const rows = () => wrapper.findAll('.pm-row')
const inRow = (row, sel) => row.find(sel)
async function clickIn(row, text) {
  await row.findAll('button').find(b => b.text() === text).trigger('click')
  await flushPromises()
}
async function clickSave() {
  await wrapper.findAll('button').find(b => b.text() === '保存并生效').trigger('click')
  await flushPromises()
}

describe('模型库设置', () => {
  it('加载渲染条目：激活 radio 命中、生效中徽标、密钥占位按 keySet 区分', async () => {
    await show()
    expect(rows()).toHaveLength(2)
    expect(wrapper.text()).toContain('生效中')
    expect(inRow(rows()[0], 'input[type="radio"]').element.checked).toBe(true)
    expect(inRow(rows()[0], 'input[type="radio"]').element.value).toBe('step-5')
    expect(inRow(rows()[0], 'input[type="password"]').attributes('placeholder')).toContain('留空保留')
    expect(inRow(rows()[1], 'input[type="password"]').attributes('placeholder')).toContain('填写该服务的 API Key')
  })

  it('行内校验：缺名称或地址不得保存，显示错误而不是发请求', async () => {
    await show()
    await inRow(rows()[1], 'input[type="url"]').setValue('')
    await clickSave()
    expect(api).not.toHaveBeenCalledWith('POST', '/api/provider/models', expect.anything())
    expect(wrapper.text()).toContain('API 地址无效')

    api.mockClear()
    await inRow(rows()[1], 'input[type="url"]').setValue('https://api.deepseek.com/v1')
    await inRow(rows()[1], 'input[placeholder="如 step-5-preview"]').setValue('')
    await clickSave()
    expect(api).not.toHaveBeenCalledWith('POST', '/api/provider/models', expect.anything())
    expect(wrapper.text()).toContain('每个模型都需要一个名称')
  })

  it('保存成功：全量 payload（窗口数值化、inputs 按 checkbox、密钥留空传空）、清空密钥输入并广播刷新', async () => {
    await show()
    // 第二行：填窗口、关掉图片勾选、填一次密钥，并切为激活
    await inRow(rows()[1], 'input[type="number"]').setValue('32768')
    await inRow(rows()[1], 'input[type="checkbox"]').setValue(false)
    await inRow(rows()[1], 'input[type="password"]').setValue('sk-ds')
    await inRow(rows()[1], 'input[type="radio"]').setValue(true)
    await clickSave()
    expect(api).toHaveBeenCalledWith('POST', '/api/provider/models', {
      active: 'ds',
      models: [
        expect.objectContaining({ name: 'step-5', inputs: ['text', 'image'], contextWindow: 128000, apiKey: '' }),
        expect.objectContaining({ name: 'ds', inputs: ['text'], contextWindow: 32768, apiKey: 'sk-ds' }),
      ],
    })
    expect(inRow(rows()[1], 'input[type="password"]').element.value).toBe('') // 保存后清空密钥输入
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect(api).toHaveBeenCalledWith('GET', '/api/provider/models') // 保存后重拉最新库
  })

  it('行内测试：带 checkVision 与行内密钥，结果展示后随字段编辑失效', async () => {
    api.mockImplementation((method, url) => {
      if (method === 'GET' && url === '/api/provider/models') return Promise.resolve(LIB)
      return Promise.resolve({ ok: true, text: { status: 'supported', message: '文本连接测试通过' },
        vision: { status: 'unsupported', message: '模型未接受图片输入' } })
    })
    await show()
    await clickIn(rows()[0], '测试')
    expect(api).toHaveBeenCalledWith('POST', '/api/provider/test', {
      baseURL: 'https://api.stepfun.com/step_plan/v1', model: 'step-5-preview', apiKey: '', checkVision: true,
    })
    expect(wrapper.text()).toContain('文本连接测试通过')
    expect(wrapper.text()).toContain('模型未接受图片输入')
    await inRow(rows()[0], 'input[placeholder="服务商提供的模型名称"]').setValue('step-5-changed')
    expect(wrapper.text()).not.toContain('文本连接测试通过')
  })

  it('保存失败保留输入与重试能力，不发 saved', async () => {
    api.mockImplementation((method, url) => {
      if (method === 'GET' && url === '/api/provider/models') return Promise.resolve(LIB)
      if (method === 'POST' && url === '/api/provider/models') return Promise.reject(new Error('配置保存失败'))
      return Promise.resolve(testOk)
    })
    await show()
    await inRow(rows()[0], 'input[type="password"]').setValue('secret')
    await clickSave()
    expect(wrapper.text()).toContain('配置保存失败')
    expect(inRow(rows()[0], 'input[type="password"]').element.value).toBe('secret')
    expect(wrapper.emitted('saved')).toBeUndefined()
  })

  it('删除激活条目后 radio 回落到剩余行', async () => {
    await show()
    await clickIn(rows()[0], '删除')
    expect(rows()).toHaveLength(1)
    expect(inRow(rows()[0], 'input[type="radio"]').element.checked).toBe(true)
  })
})
