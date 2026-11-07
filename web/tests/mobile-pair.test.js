// 扫码配对（?pair=）自动登录：boot() 里 redeem 成功 → 存 token 进主界面；
// 失败（码无效/过期）→ toast 并回退到正常登录流程；URL 参数消费后清理。
import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { apiMock, setTokenMock } = vi.hoisted(() => ({
  apiMock: vi.fn(),
  setTokenMock: vi.fn(),
}))

vi.mock('../src/api.js', () => ({ api: apiMock }))
vi.mock('../src/auth.js', () => ({
  setToken: setTokenMock,
  hydrateToken: vi.fn(async () => {}),
  getToken: () => localStorage.getItem('friday.authToken') || '',
  authHeaders: extra => ({ ...extra }),
  mediaURL: u => u,
}))
vi.mock('../src/mobile/push.js', () => ({
  push: { status: 'off', unread: 0 },
  notices: [],
  startPush: vi.fn(),
  stopPush: vi.fn(),
}))

import MobileApp from '../src/mobile/MobileApp.vue'

// stub 掉三个子页面（它们的接口/交互不在本测范围）与原生桥。
vi.mock('../src/mobile/MobileChat.vue', () => ({ default: { template: '<div class="stub-chat" />' } }))
vi.mock('../src/mobile/MobileThreads.vue', () => ({ default: { template: '<div class="stub-threads" />' } }))
vi.mock('../src/mobile/MobileFiles.vue', () => ({ default: { template: '<div class="stub-files" />' } }))
vi.mock('../src/mobile/MobileNotifications.vue', () => ({ default: { template: '<div class="stub-notices" />' } }))

const mountApp = async () => {
  const w = mount(MobileApp, { attachTo: document.body })
  await flushPromises()
  return w
}

describe('mobile pair login', () => {
  beforeEach(() => {
    localStorage.clear()
    apiMock.mockReset()
    setTokenMock.mockReset()
    history.replaceState(null, '', '/mobile')
  })

  it('redeem 成功：存 token、进主界面、清 URL 参数', async () => {
    history.replaceState(null, '', '/mobile?pair=ABC23456')
    apiMock.mockImplementation(async (method, path) => {
      if (method === 'POST' && path === '/api/pair/redeem') return { token: 'jwt-pair', username: 'aaron' }
      if (path === '/api/setup/status') return { accountRequired: false }
      if (path === '/api/session') return { authenticated: true, username: 'aaron' }
      throw new Error('unexpected ' + method + ' ' + path)
    })
    const w = await mountApp()
    expect(apiMock).toHaveBeenCalledWith('POST', '/api/pair/redeem', { code: 'ABC23456' })
    expect(setTokenMock).toHaveBeenCalledWith('jwt-pair')
    expect(w.find('.m-head b').text()).toBe('小五')
    expect(location.search).toBe('')
    w.unmount()
  })

  it('redeem 失败：不存 token，回退到登录页', async () => {
    history.replaceState(null, '', '/mobile?pair=EXPIRED1')
    apiMock.mockImplementation(async (method, path) => {
      if (method === 'POST' && path === '/api/pair/redeem') throw new Error('配对码无效或已过期，请在电脑端重新生成')
      if (path === '/api/setup/status') return { accountRequired: false }
      if (path === '/api/session') return { authenticated: false }
      throw new Error('unexpected ' + method + ' ' + path)
    })
    const w = await mountApp()
    // 允许 boot 流程的 setToken('') 清残留，但不允许写入任何真实凭据。
    expect(setTokenMock.mock.calls.every(c => c[0] === '')).toBe(true)
    expect(w.find('.m-auth').exists()).toBe(true)
    expect(location.search).toBe('')
    w.unmount()
  })

  it('无 pair 参数：完全走原流程，不调 redeem', async () => {
    apiMock.mockImplementation(async (_m, path) => {
      if (path === '/api/setup/status') return { accountRequired: false }
      if (path === '/api/session') return { authenticated: false }
      throw new Error('unexpected ' + path)
    })
    await mountApp()
    expect(apiMock).not.toHaveBeenCalledWith('POST', '/api/pair/redeem', expect.anything())
  })
})
