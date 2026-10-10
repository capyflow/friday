import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ProviderSettings from '../src/components/ProviderSettings.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
let wrapper
const status = { provider: { baseURL: 'https://api.example.com/v1', model: 'model' }, apiKeyConfigured: true }
const success = { ok: true, text: { status: 'supported', message: '文本连接测试通过' }, vision: { status: 'not_tested', message: '未测试图片理解' } }

beforeEach(() => { api.mockReset().mockResolvedValue(success) })
afterEach(() => { wrapper?.unmount(); wrapper = null })
function show(props = status) { wrapper = mount(ProviderSettings, { props: { status: props } }); return wrapper }
async function clickTest() { await wrapper.get('button[type="button"]').trigger('click'); await flushPromises() }

describe('模型连接诊断', () => {
  it('同地址可保留已配置密钥；返回的文本和图片结果分别显示', async () => {
    api.mockResolvedValue({ ...success, vision: { status: 'unsupported', message: '模型未接受图片输入' } })
    show()
    await wrapper.get('input[name="checkVision"]').setValue(true)
    await clickTest()
    expect(api).toHaveBeenCalledWith('POST', '/api/provider/test', { baseURL: status.provider.baseURL, model: 'model', apiKey: '', checkVision: true })
    expect(wrapper.text()).toContain('文本连接测试通过')
    expect(wrapper.text()).toContain('模型未接受图片输入')
  })

  it.each(['baseURL', 'model'])('字段为空不得发送诊断或显示成功：%s', async field => {
    show()
    await wrapper.get(`input[name="${field}"]`).setValue('')
    await clickTest()
    expect(api).not.toHaveBeenCalled()
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('文本连接测试通过')
  })

  it('更换API地址必须填写新密钥，不能沿用旧key', async () => {
    show()
    await wrapper.get('input[name="baseURL"]').setValue('https://other.example/v1')
    await clickTest()
    expect(api).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('请填写该 API 地址的 API Key')
    await wrapper.get('input[name="apiKey"]').setValue('new-secret')
    await clickTest()
    expect(api).toHaveBeenCalledOnce()
  })

  it('诊断成功在表单变化后立即失效', async () => {
    show()
    await clickTest()
    expect(wrapper.text()).toContain('文本连接测试通过')
    await wrapper.get('input[name="model"]').setValue('different-model')
    expect(wrapper.text()).not.toContain('文本连接测试通过')
  })

  it('请求期间禁止重复操作；过期响应不能标记修改后的表单成功', async () => {
    let resolve
    api.mockReturnValue(new Promise(ok => { resolve = ok }))
    show()
    await wrapper.get('button[type="button"]').trigger('click')
    expect(wrapper.attributes('aria-busy')).toBe('true')
    expect(wrapper.get('button[type="submit"]').element.disabled).toBe(true)
    await wrapper.get('button[type="button"]').trigger('click')
    expect(api).toHaveBeenCalledOnce()
    // 配置刷新也可能在测试在途时改变表单。
    await wrapper.setProps({ status: { ...status, provider: { ...status.provider, model: 'new-model' } } })
    resolve(success)
    await flushPromises()
    expect(wrapper.text()).not.toContain('文本连接测试通过')
  })

  it('诊断失败显示可读错误，保存失败保留输入和重试按钮', async () => {
    api.mockResolvedValue({ ok: false, text: { status: 'unknown', message: '鉴权失败，请检查 API Key' }, vision: { status: 'not_tested' } })
    show({ ...status, apiKeyConfigured: false })
    await wrapper.get('input[name="apiKey"]').setValue('secret')
    await clickTest()
    expect(wrapper.text()).toContain('鉴权失败')
    api.mockRejectedValueOnce(new Error('配置保存失败'))
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('配置保存失败')
    expect(wrapper.get('input[name="apiKey"]').element.value).toBe('secret')
    expect(wrapper.emitted('saved')).toBeUndefined()
    expect(wrapper.get('button[type="submit"]').element.disabled).toBe(false)
  })

  it('保存成功清除输入密钥并发出状态刷新事件', async () => {
    api.mockResolvedValue({ message: '已保存' })
    show({ ...status, apiKeyConfigured: false })
    await wrapper.get('input[name="apiKey"]').setValue('secret')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.get('input[name="apiKey"]').element.value).toBe('')
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect(wrapper.html()).not.toContain('secret')
    expect(wrapper.text()).toContain('已保存')
  })
})
