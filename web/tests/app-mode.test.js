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
    chatBusy: false, settingsOpen: false, setupStatus: null, unread: 0 })
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
  it('生活模式没有「工作区」入口，工作模式才有', async () => {
    await showApp()
    expect(navViews()).not.toContain('agent')
    expect(navViews()).toContain('chat')

    await modeButton('工作模式').trigger('click')
    expect(store.mode).toBe('work')
    expect(navViews()).toContain('agent')
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

  it('内置管家正在回复时先不切模式，并说明为什么', async () => {
    await showApp()
    store.chatBusy = true
    await modeButton('工作模式').trigger('click')
    expect(store.mode).toBe('life')
    expect(toast).toHaveBeenCalledWith(expect.stringContaining('正在回复'), true)
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
