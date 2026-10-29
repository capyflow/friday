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
    { name: 'step-5', baseURL: 'https://api.stepfun.com/step_plan/v1', model: 'step-5-preview', keySet: true, contextWindow: 128000, maxTokens: 8192, inputs: ['text', 'image'] },
    { name: 'ds', baseURL: 'https://api.deepseek.com/v1', model: 'deepseek-chat', keySet: false, contextWindow: 0, maxTokens: 0, inputs: ['text'] },
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
  it('首次没有模型：显示空状态并禁用保存，引导添加第一个模型', async () => {
    api.mockImplementation((method, url) => {
      if (method === 'GET' && url === '/api/provider/models')
        return Promise.resolve({ active: '', globalWindowTokens: 0, models: [] })
      return Promise.resolve({})
    })
    await show()
    expect(rows()).toHaveLength(0)
    expect(wrapper.text()).toContain('还没有配置模型')
    const save = wrapper.findAll('button').find(b => b.text() === '保存并生效')
    expect(save.attributes('disabled')).toBeDefined()
    const add = wrapper.findAll('button').find(b => b.text() === '＋ 添加第一个模型')
    expect(add).toBeTruthy()
    await add.trigger('click')
    await flushPromises()
    expect(rows()).toHaveLength(1)
    expect(wrapper.find('.pm-empty').exists()).toBe(false)
  })

  it('只有一个新模型时自动选中，填完直接保存，不需要再点 radio', async () => {
    api.mockImplementation((method, url) => {
      if (method === 'GET' && url === '/api/provider/models')
        return Promise.resolve({ active: '', globalWindowTokens: 0, models: [] })
      if (method === 'POST' && url === '/api/provider/models') return Promise.resolve({ message: '已保存' })
      return Promise.resolve({})
    })
    await show()
    await wrapper.findAll('button').find(b => b.text() === '＋ 添加第一个模型').trigger('click')
    await flushPromises()
    const row = rows()[0]
    expect(inRow(row, 'input[type="radio"]').element.checked).toBe(true)
    await inRow(row, 'input[aria-label="模型显示名"]').setValue('日常对话')
    await inRow(row, 'input[type="url"]').setValue('https://api.stepfun.com/step_plan/v1')
    await inRow(row, 'input[placeholder="服务商提供的模型名称"]').setValue('step-5-preview')
    await clickSave()
    expect(wrapper.text()).not.toContain('请选择一个要生效的模型')
    expect(api).toHaveBeenCalledWith('POST', '/api/provider/models', {
      active: '日常对话',
      models: [expect.objectContaining({ name: '日常对话', model: 'step-5-preview' })],
    })
  })

  it('加载渲染条目：激活 radio 命中、生效中徽标、密钥占位按 keySet 区分', async () => {
    await show()
    expect(rows()).toHaveLength(2)
    expect(wrapper.text()).toContain('生效中')
    expect(inRow(rows()[0], 'input[type="radio"]').element.checked).toBe(true)
    expect(inRow(rows()[0], 'input[type="radio"]').element.value).toBe('0')
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
    await inRow(rows()[1], 'input[aria-label="模型显示名"]').setValue('')
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

  it('最大输出 token：回填条目值，留空按 4096 提交，填了用填的，新行默认 4096', async () => {
    await show()
    const budget = row => row.findAll('input[type="number"]')[1] // [0] 是上下文窗口
    expect(budget(rows()[0]).element.value).toBe('8192')
    expect(budget(rows()[1]).element.value).toBe('')
    expect(budget(rows()[1]).attributes('placeholder')).toContain('4096')

    await clickSave()
    const posted = () => api.mock.calls
      .filter(c => c[0] === 'POST' && c[1] === '/api/provider/models').pop()
    expect(posted()[2].models[0].maxTokens).toBe(8192)
    expect(posted()[2].models[1].maxTokens).toBe(4096) // 留空 → 按默认提交

    await budget(rows()[1]).setValue('16000')
    await clickSave()
    expect(posted()[2].models[1].maxTokens).toBe(16000) // 填了就用填的

    await wrapper.findAll('button').find(b => b.text() === '＋ 添加模型').trigger('click')
    await flushPromises()
    expect(budget(rows()[2]).element.value).toBe('4096') // 新行自带默认
  })
})
