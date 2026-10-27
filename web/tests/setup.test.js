import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SetupView from '../src/components/SetupView.vue'
import ProviderSettings from '../src/components/ProviderSettings.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
let wrapper
const first = { accountRequired: true, providerRequired: true, localSetupAllowed: true }
beforeEach(() => { api.mockReset() })
afterEach(() => { wrapper?.unmount(); wrapper = null })
function show(status = first) { wrapper = mount(SetupView, { props: { status } }); return wrapper }
async function fill(password = 'secret-password', confirmation = password) {
  await wrapper.get('input[name="username"]').setValue('owner')
  await wrapper.get('input[name="password"]').setValue(password)
  await wrapper.get('input[name="confirmPassword"]').setValue(confirmation)
}

describe('首次设置向导', () => {
  it('用户名输入关闭 macOS 自动大写与纠错，避免输入 aaron 被建议成 Aaron', () => {
    show()
    const u = wrapper.get('input[name="username"]')
    expect(u.attributes('autocapitalize')).toBe('none')
    expect(u.attributes('autocorrect')).toBe('off')
    expect(u.attributes('spellcheck')).toBe('false')
  })

  it('远程未初始化部署显示预置方式，不显示可提交账号表单', () => {
    show({ ...first, localSetupAllowed: false })
    expect(wrapper.find('form').exists()).toBe(false)
    expect(wrapper.text()).toContain('FRIDAY_ADMIN_USERNAME')
  })

  it('密码校验错误阻止请求，失败后保留输入供重试', async () => {
    show()
    await fill('12345')
    await wrapper.get('form').trigger('submit')
    expect(api).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('至少需要 6 个字符')
    await fill('secret-password', 'different')
    await wrapper.get('form').trigger('submit')
    expect(api).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('密码不一致')
    await fill()
    api.mockRejectedValueOnce(new Error('账号设置失败'))
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('账号设置失败')
    expect(wrapper.emitted('account-created')).toBeUndefined()
    expect(wrapper.get('input[name="password"]').element.value).toBe('secret-password')
  })

  it('账号创建成功只发 account-created，向导里不再有模型配置', async () => {
    show()
    await fill()
    api.mockResolvedValueOnce({ username: 'owner', accountRequired: false, providerRequired: true })
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.emitted('account-created')).toHaveLength(1)
    expect(wrapper.get('input[name="password"]').element.value).toBe('')
    // 模型配置移出向导：providerRequired 仍为 true 时也不该出现模型表单或"稍后设置"
    await wrapper.setProps({ status: { ...first, accountRequired: false } })
    await flushPromises()
    expect(wrapper.findComponent(ProviderSettings).exists()).toBe(false)
    expect(wrapper.find('.setup-skip').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('连接模型')
    expect(api).not.toHaveBeenCalledWith('GET', '/api/status')
  })
})
