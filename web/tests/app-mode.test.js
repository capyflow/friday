import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import App from '../src/App.vue'
import { api } from '../src/api.js'
import { store, setMode, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/anim.js', () => ({ auroraDrift: vi.fn(), shellIn: vi.fn() }))
vi.mock('../src/theme.js', () => ({ initTheme: vi.fn(), setTheme: vi.fn() }))
vi.mock('../src/notifications.js', () => ({ startNotifications: vi.fn(), stopNotifications: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

let wrapper

async function showApp() {
  wrapper = mount(App, { global: { stubs: {
    SetupView: true, LoginView: { template: '<div data-login />' }, ChatView: true,
    WorkspaceView: true, RunsView: true, AutoView: true, BlinkoView: true,
    NotificationsView: true, SettingsModal: true, ApprovalCards: true,
  } } })
  await flushPromises()
}

function navViews() {
  return wrapper.findAll('.nav-item').map(b => b.attributes('data-view'))
}

function modeButton(mode) {
  return wrapper.get(`#sidebar .mode-seg button[aria-label="${mode}"]`)
}

beforeEach(() => {
  localStorage.clear()
  Object.assign(store, { loggedIn: true, username: 'owner', view: 'chat', mode: 'life',
    settingsOpen: false, setupStatus: null, unread: 0 })
  api.mockReset().mockImplementation((_method, url) => Promise.resolve(url === '/api/setup/status'
    ? { accountRequired: false, providerRequired: false }
    : { authenticated: true, username: 'owner', sessions: [] }))
  toast.mockClear()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  localStorage.clear()
})

describe('侧栏的使用模式开关', () => {
  it('生活模式只有聊天、没有「工作区」；工作模式反过来没有聊天、有「工作区」', async () => {
    await showApp()
    expect(navViews()).not.toContain('agent')
    expect(navViews()).toContain('chat')

    await modeButton('工作模式').trigger('click')
    expect(store.mode).toBe('work')
    expect(navViews()).toContain('agent')
    // 工作模式里跟小五干活走工作区（新建会话时选内置 friday 或外部 agent）：
    // 两处都能聊就是两份上下文，用户得自己记住该在哪边说话
    expect(navViews()).not.toContain('chat')
  })

  it('切到工作模式落在工作区，切回生活模式落在聊天', async () => {
    await showApp()
    await modeButton('工作模式').trigger('click')
    expect(store.view).toBe('agent')
    await modeButton('生活模式').trigger('click')
    expect(store.view).toBe('chat')
  })

  it('模式开关标出当前在哪一侧', async () => {
    await showApp()
    expect(modeButton('生活模式').classes()).toContain('on')
    expect(modeButton('工作模式').attributes('aria-pressed')).toBe('false')
    await modeButton('工作模式').trigger('click')
    expect(modeButton('工作模式').classes()).toContain('on')
    expect(modeButton('生活模式').attributes('aria-pressed')).toBe('false')
  })

  it('切回生活模式时，停在工作区的人回到聊天页', async () => {
    await showApp()
    await modeButton('工作模式').trigger('click')
    store.view = 'agent'
    await modeButton('生活模式').trigger('click')
    expect(store.view).toBe('chat')
  })

  it('回复进行中照样能切模式：聊天页不随模式换归属，也不拦这一跳', async () => {
    await showApp()
    // 过去 store.chatBusy 会拦住这一跳（切模式会换掉对话归属，半截回答会掉到看不见的
    // 地方）。现在聊天页只在生活模式、恒为主会话，切模式只是把它藏起来（v-show），
    // 那一轮继续在后台收字——标志位已经删掉，这里守的是"别再拦回去"。
    expect(store.chatBusy).toBeUndefined()
    await modeButton('工作模式').trigger('click')
    expect(store.mode).toBe('work')
    expect(toast).not.toHaveBeenCalledWith(expect.stringContaining('正在回复'), true)
  })

  it('模式记在本地：重新加载页面读回原来那一侧', async () => {
    await showApp()
    setMode('work')
    expect(localStorage.getItem('friday-mode')).toBe('work')
    vi.resetModules()
    const fresh = await import('../src/store.js')
    expect(fresh.store.mode).toBe('work')
  })
})
