import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { api } from '../src/api.js'
import { store } from '../src/store.js'
import { notificationSettings, startNotifications, stopNotifications } from '../src/notifications.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

function deferred() {
  let resolve
  const promise = new Promise(done => { resolve = done })
  return { promise, resolve }
}

beforeEach(() => {
  vi.useFakeTimers()
  stopNotifications()
  store.loggedIn = false
  store.username = ''
  notificationSettings.status = '未开启'
  notificationSettings.busy = false
  localStorage.clear()
  api.mockReset().mockResolvedValue({ notifications: [], unread: 0 })
})

afterEach(() => {
  stopNotifications()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('桌面原生通知状态的生命周期', () => {
  it('启动后登录信息变更不丢弃异步返回的已开启状态', async () => {
    const request = deferred()
    const invoke = vi.fn().mockReturnValue(request.promise)
    vi.stubGlobal('__TAURI__', { core: { invoke } })
    startNotifications()
    store.username = 'owner'
    store.loggedIn = true
    await flushPromises()
    request.resolve(true)
    await flushPromises()
    expect(notificationSettings.status).toBe('系统通知已开启')
    expect(invoke).toHaveBeenCalledExactlyOnceWith('notifications_enabled')
  })

  it('停止后旧异步查询不能更新通知状态或恢复轮询', async () => {
    const request = deferred()
    const invoke = vi.fn().mockReturnValue(request.promise)
    vi.stubGlobal('__TAURI__', { core: { invoke } })
    startNotifications()
    stopNotifications()
    request.resolve(true)
    await flushPromises()
    await vi.advanceTimersByTimeAsync(15000)
    expect(notificationSettings.status).toBe('未开启')
    expect(api).not.toHaveBeenCalled()
    expect(invoke).toHaveBeenCalledExactlyOnceWith('notifications_enabled')
  })
})
