import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
let notifications, store, api, NativeNotification, settings

const notice = { id: 'notice-1', text: '该喝水了', prepared: true, push: true, status: 'delivered' }

beforeEach(async () => {
  vi.resetModules()
  vi.useFakeTimers()
  localStorage.clear()
  NativeNotification = vi.fn(function (_title, options) { this.options = options; this.close = vi.fn() })
  NativeNotification.permission = 'default'
  NativeNotification.requestPermission = vi.fn().mockResolvedValue('granted')
  vi.stubGlobal('Notification', NativeNotification)
  vi.stubGlobal('__TAURI__', undefined)
  ;({ store } = await import('../src/store.js'))
  ;({ api } = await import('../src/api.js'))
  notifications = await import('../src/notifications.js')
  settings = notifications.notificationSettings
  api.mockReset().mockResolvedValue({ notifications: [notice], unread: 1 })
  store.loggedIn = true
  store.username = 'owner'
  store.unread = 0
  store.view = 'chat'
})
afterEach(() => {
  notifications.stopNotifications()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('系统通知权限和登录生命周期', () => {
  it('正常加载只更新未读，显式开启才申请权限', async () => {
    notifications.startNotifications()
    await flushPromises()
    expect(store.unread).toBe(1)
    expect(NativeNotification.requestPermission).not.toHaveBeenCalled()
    expect(NativeNotification).not.toHaveBeenCalled()
    await notifications.enableSystemNotifications()
    expect(NativeNotification.requestPermission).toHaveBeenCalledTimes(1)
    expect(settings.status).toBe('系统通知已开启')
    expect(localStorage.getItem('friday-notifications-enabled')).toBe('1')
  })

  it('浏览器拒绝权限时保留通知中心和未读，不调用原生弹窗', async () => {
    NativeNotification.permission = 'denied'
    NativeNotification.requestPermission.mockResolvedValue('denied')
    await notifications.enableSystemNotifications()
    await notifications.pollNotifications()
    expect(settings.status).toContain('使用通知中心')
    expect(store.unread).toBe(1)
    expect(NativeNotification).not.toHaveBeenCalled()
    expect(localStorage.getItem('friday-notifications-enabled')).toBe('0')
  })

  it('授权后的未读新通知只弹一次，已读/静默/未生成通知不弹', async () => {
    NativeNotification.permission = 'granted'
    await notifications.enableSystemNotifications()
    api.mockResolvedValue({ notifications: [notice,
      { ...notice, id: 'read', readAt: '2026-10-10T12:00:00+08:00' },
      { ...notice, id: 'silent', push: false }, { ...notice, id: 'pending', prepared: false }], unread: 1 })
    await notifications.pollNotifications()
    await notifications.pollNotifications()
    expect(NativeNotification).toHaveBeenCalledExactlyOnceWith('小五提醒', { body: notice.text, tag: notice.id })
    expect(JSON.parse(localStorage.getItem('friday-notices-owner'))).toEqual([notice.id])
  })

  it('第一次加载不重弹服务器已读或本地已经提示的通知', async () => {
    NativeNotification.permission = 'granted'
    localStorage.setItem('friday-notifications-enabled', '1')
    localStorage.setItem('friday-notices-owner', JSON.stringify([notice.id]))
    api.mockResolvedValue({ notifications: [notice, { ...notice, id: 'read', readAt: '2026-10-10T12:00:00+08:00' }], unread: 1 })
    notifications.startNotifications()
    await flushPromises()
    expect(NativeNotification).not.toHaveBeenCalled()
    expect(NativeNotification.requestPermission).not.toHaveBeenCalled()
    expect(store.unread).toBe(1)
  })

  it('显式开启过程中禁止重复申请，异常退回通知中心', async () => {
    let fail
    NativeNotification.requestPermission.mockReturnValueOnce(new Promise((_resolve, reject) => { fail = reject }))
    const first = notifications.enableSystemNotifications()
    await notifications.enableSystemNotifications()
    expect(settings.busy).toBe(true)
    expect(NativeNotification.requestPermission).toHaveBeenCalledTimes(1)
    fail(new Error('权限接口不可用'))
    await first
    expect(settings.busy).toBe(false)
    expect(settings.status).toContain('使用通知中心')
  })

  it('桌面明确调用通知授权命令，浏览器不额外申请或弹窗', async () => {
    const invoke = vi.fn().mockResolvedValue(true)
    vi.stubGlobal('__TAURI__', { core: { invoke } })
    await notifications.enableSystemNotifications()
    expect(invoke).toHaveBeenCalledExactlyOnceWith('enable_notifications')
    expect(NativeNotification.requestPermission).not.toHaveBeenCalled()
    await notifications.pollNotifications()
    expect(NativeNotification).not.toHaveBeenCalled()
  })

  it('退出再以同账号登录后旧轮询不能更新未读或弹窗', async () => {
    let finish
    NativeNotification.permission = 'granted'
    await notifications.enableSystemNotifications()
    api.mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
    const oldPoll = notifications.pollNotifications()
    store.loggedIn = false
    await flushPromises()
    store.loggedIn = true
    await flushPromises()
    finish({ notifications: [notice], unread: 99 })
    await oldPoll
    expect(store.unread).toBe(0)
    expect(NativeNotification).not.toHaveBeenCalled()
  })

  it('切换账号后旧轮询不污染新账号，退出立即清空未读', async () => {
    let finish
    store.unread = 5
    api.mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
    const oldPoll = notifications.pollNotifications()
    store.loggedIn = false
    await flushPromises()
    expect(store.unread).toBe(0)
    store.username = 'new-owner'
    store.loggedIn = true
    finish({ notifications: [notice], unread: 99 })
    await oldPoll
    expect(store.unread).toBe(0)
  })

  it('停止后晚到的请求不继续更新状态或弹窗，计时器已清理', async () => {
    let finish
    NativeNotification.permission = 'granted'
    localStorage.setItem('friday-notifications-enabled', '1')
    api.mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
    notifications.startNotifications()
    notifications.stopNotifications()
    finish({ notifications: [notice], unread: 99 })
    await flushPromises()
    expect(store.unread).toBe(0)
    expect(NativeNotification).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(15000)
    expect(api).toHaveBeenCalledTimes(1)
  })
})
