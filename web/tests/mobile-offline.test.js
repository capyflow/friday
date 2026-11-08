// 「服务器不在线」离线卡片：填了新地址点「重试连接」时，整页跳到那台电脑。
//
// 离线卡片上唯一能改变状态的动作就是重试连接——所以它必须先认地址：
// 地址与当前服务器不同 → 先换 origin（接口是同源相对路径，跨源调不动），
// 相同 → 原样重新探测。
import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { apiMock, connectToMock } = vi.hoisted(() => ({
  apiMock: vi.fn(),
  connectToMock: vi.fn(),
}))

vi.mock('../src/api.js', () => ({ api: apiMock }))
vi.mock('../src/auth.js', () => ({
  setToken: vi.fn(),
  hydrateToken: vi.fn(async () => {}),
  getToken: () => '',
  authHeaders: extra => ({ ...extra }),
  mediaURL: u => u,
}))
vi.mock('../src/mobile/push.js', () => ({
  push: { status: 'off', unread: 0 },
  notices: [],
  startPush: vi.fn(),
  stopPush: vi.fn(),
}))
vi.mock('../src/mobile/native.js', () => ({
  connectTo: connectToMock,
  currentOrigin: () => location.origin,
}))
vi.mock('../src/store.js', () => ({
  store: { loggedIn: false, username: '', setupStatus: null },
  toast: vi.fn(),
  toastState: { show: false, err: false, text: '' },
  hideModal: vi.fn(),
  modalState: { show: false, title: '', text: '' },
}))

import MobileApp from '../src/mobile/MobileApp.vue'

vi.mock('../src/mobile/MobileChat.vue', () => ({ default: { template: '<div class="stub-chat" />' } }))
vi.mock('../src/mobile/MobileThreads.vue', () => ({ default: { template: '<div class="stub-threads" />' } }))
vi.mock('../src/mobile/MobileFiles.vue', () => ({ default: { template: '<div class="stub-files" />' } }))
vi.mock('../src/mobile/MobileNotifications.vue', () => ({ default: { template: '<div class="stub-notices" />' } }))

// 探测失败（服务器不在线）→ boot 走 catch → 出离线卡片
const offlineApi = async () => {
  throw new Error('Failed to fetch')
}

describe('离线卡片的服务器地址', () => {
  beforeEach(() => {
    apiMock.mockReset()
    connectToMock.mockReset()
    apiMock.mockImplementation(offlineApi)
  })

  it('探测失败时给离线卡片，带地址输入框', async () => {
    const w = mount(MobileApp, { attachTo: document.body })
    await flushPromises()
    expect(w.find('.m-offline').exists()).toBe(true)
    expect(w.find('#m-srv-addr').exists()).toBe(true)
    // 地址框初值 = 当前服务器
    expect(w.find('#m-srv-addr').element.value).toBe(location.host)
    w.unmount()
  })

  it('填了新地址点重试连接：整页跳到那台电脑', async () => {
    const w = mount(MobileApp, { attachTo: document.body })
    await flushPromises()
    await w.find('#m-srv-addr').setValue('10.0.0.9:45679')
    const btn = w.findAll('.m-offline-acts button').find(b => b.text().includes('重试连接'))
    await btn.trigger('click')
    await flushPromises()
    const scheme = location.protocol === 'https:' ? 'https:' : 'http:'
    expect(connectToMock).toHaveBeenCalledWith(scheme + '//10.0.0.9:45679/mobile')
    w.unmount()
  })

  it('地址没改点重试连接：不跳转，原样重新探测', async () => {
    const w = mount(MobileApp, { attachTo: document.body })
    await flushPromises()
    const calls = apiMock.mock.calls.length
    const btn = w.findAll('.m-offline-acts button').find(b => b.text().includes('重试连接'))
    await btn.trigger('click')
    await flushPromises()
    expect(connectToMock).not.toHaveBeenCalled()
    expect(apiMock.mock.calls.length).toBeGreaterThan(calls)
    w.unmount()
  })
})
