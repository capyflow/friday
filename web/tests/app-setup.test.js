import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import App from '../src/App.vue'
import { api } from '../src/api.js'
import { store } from '../src/store.js'
import { startNotifications, stopNotifications } from '../src/notifications.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/anim.js', () => ({ auroraDrift: vi.fn(), shellIn: vi.fn() }))
vi.mock('../src/theme.js', () => ({ initTheme: vi.fn(), setTheme: vi.fn() }))
vi.mock('../src/notifications.js', () => ({ startNotifications: vi.fn(), stopNotifications: vi.fn() }))

const SetupStub = defineComponent({ name: 'SetupView', props: ['status'], emits: ['account-created'],
  template: '<section data-setup><button @click="$emit(\'account-created\')">account</button></section>' })
let wrapper, setupStatus, session
beforeEach(() => {
  Object.assign(store, { loggedIn: false, username: '', view: 'chat', settingsOpen: false, settingsSection: '', setupStatus: null, unread: 0 })
  setupStatus = { accountRequired: false, providerRequired: false }
  session = { authenticated: false }
  api.mockReset().mockImplementation((_method, url) => Promise.resolve(url === '/api/setup/status' ? setupStatus : session))
  startNotifications.mockClear()
  stopNotifications.mockClear()
})
afterEach(() => { wrapper?.unmount(); wrapper = null })
async function showApp() {
  wrapper = mount(App, { global: { stubs: {
    SetupView: SetupStub, LoginView: { template: '<div data-login />' }, ChatView: true,
    AutoView: true, TodoView: true, NotificationsView: true,
    SettingsModal: true, ApprovalCards: true,
  } } })
  await flushPromises()
}

describe('App 首次设置路由和通知接线', () => {
  it('未设置账号进入向导，向导不再提供跳过账号的出口', async () => {
    setupStatus = { accountRequired: true, providerRequired: true, localSetupAllowed: true }
    await showApp()
    expect(wrapper.findComponent(SetupStub).exists()).toBe(true)
    expect(wrapper.find('[data-login]').exists()).toBe(false)
    expect(wrapper.find('#app').exists()).toBe(false)
    // 组件只声明 account-created：模型步骤已删除，账号未建好时没有任何路径能进主界面
    expect(SetupStub.emits).toEqual(['account-created'])
  })

  it('账号创建后刷新状态与会话，直接进主界面（不再引导配模型）', async () => {
    setupStatus = { accountRequired: true, providerRequired: true }
    await showApp()
    setupStatus = { accountRequired: false, providerRequired: true }
    session = { authenticated: true, username: 'owner' }
    wrapper.getComponent(SetupStub).vm.$emit('account-created')
    await flushPromises()
    expect(store.loggedIn).toBe(true)
    expect(store.username).toBe('owner')
    expect(wrapper.findComponent(SetupStub).exists()).toBe(false)
    expect(wrapper.find('#app').exists()).toBe(true)
  })

  it('模型未配置只影响状态位，不再把用户拦在向导里', async () => {
    setupStatus = { accountRequired: false, providerRequired: true }
    session = { authenticated: true, username: 'owner' }
    await showApp()
    expect(wrapper.findComponent(SetupStub).exists()).toBe(false)
    expect(wrapper.find('#app').exists()).toBe(true)
  })

  it('设置完整但未登录显示登录页，刷新事件重读会话', async () => {
    await showApp()
    expect(wrapper.find('[data-login]').exists()).toBe(true)
    session = { authenticated: true, username: 'owner' }
    window.dispatchEvent(new CustomEvent('friday:setup-refresh'))
    await flushPromises()
    expect(wrapper.find('#app').exists()).toBe(true)
    expect(store.loggedIn).toBe(true)
  })

  it('侧栏设置按钮弹出设置弹窗，关闭事件复位开关', async () => {
    session = { authenticated: true, username: 'owner' }
    await showApp()
    expect(wrapper.findComponent({ name: 'SettingsModal' }).exists()).toBe(false)
    await wrapper.get('.foot-btn').trigger('click')
    expect(store.settingsOpen).toBe(true)
    expect(wrapper.get('.foot-btn').attributes('aria-expanded')).toBe('true')
    wrapper.getComponent({ name: 'SettingsModal' }).vm.$emit('close')
    await flushPromises()
    expect(store.settingsOpen).toBe(false)
  })

  it('退出登录同时关闭设置弹窗', async () => {
    session = { authenticated: true, username: 'owner' }
    await showApp()
    store.settingsOpen = true
    await flushPromises()
    wrapper.getComponent({ name: 'SettingsModal' }).vm.$emit('logout')
    await flushPromises()
    expect(store.settingsOpen).toBe(false)
    expect(store.loggedIn).toBe(false)
  })

  it('登录后通知导航显示未读数，启动轮询，卸载停止并移除事件', async () => {
    session = { authenticated: true, username: 'owner' }
    await showApp()
    expect(startNotifications).toHaveBeenCalledTimes(1)
    store.unread = 3
    await flushPromises()
    expect(wrapper.get('[data-view=notifications] [aria-label="3 条未读通知"]').text()).toBe('3')
    await wrapper.get('[data-view=notifications]').trigger('click')
    expect(store.view).toBe('notifications')
    wrapper.unmount()
    wrapper = null
    expect(stopNotifications).toHaveBeenCalledTimes(1)
    const calls = api.mock.calls.length
    window.dispatchEvent(new CustomEvent('friday:setup-refresh'))
    await flushPromises()
    expect(api).toHaveBeenCalledTimes(calls)
  })
})
