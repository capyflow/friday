import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import NotificationsView from '../src/components/NotificationsView.vue'
import { api } from '../src/api.js'
import { store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

const failed = { id: 'notice-1', text: '喝水', status: 'failed', createdAt: '2026-10-10T10:00:00+08:00',
  attempts: 1, lastError: '钉钉不可达', deliveredTo: ['web'] }
let wrapper
function action(text) { return wrapper.findAll('tbody button').find(button => button.text() === text) }

beforeEach(() => {
  vi.useFakeTimers()
  api.mockReset().mockResolvedValue({ notifications: [failed] })
  store.loggedIn = true
  store.username = 'admin'
  store.unread = 0
  toast.mockClear()
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.useRealTimers()
})
async function showNotifications() {
  wrapper = mount(NotificationsView)
  await flushPromises()
}

describe('通知中心', () => {
  it('展示正文、投递状态与渠道，按最新在前，仅失败可重试', async () => {
    api.mockResolvedValue({ notifications: [
      { ...failed, id: 'older', createdAt: '2026-10-09T10:00:00+08:00' },
      { ...failed, id: 'newer', text: '<img onerror="alert(1)">', createdAt: '2026-10-10T11:00:00+08:00',
        status: 'delivered', deliveredTo: ['web', 'dingtalk', 'feishu', 'console'] },
      ...['pending', 'processing', 'silent'].map(status => ({ ...failed, id: status, status })),
    ] })
    await showNotifications()
    expect(wrapper.findAll('tbody tr')[0].text()).toContain('<img onerror="alert(1)">')
    expect(wrapper.find('img').exists()).toBe(false)
    for (const state of ['待处理', '处理中', '已送达', '失败', '静默']) expect(wrapper.text()).toContain(state)
    expect(wrapper.text()).toContain('通知中心、钉钉、飞书、本地输出')
    expect(wrapper.findAll('tbody button')).toHaveLength(1)
    expect(wrapper.text()).toContain('已尝试 1 次')
    expect(wrapper.text()).toContain('钉钉不可达')
  })

  it('重试失败保留记录和按钮，成功后更新投递状态', async () => {
    await showNotifications()
    api.mockRejectedValueOnce(new Error('断网'))
    await action('重试').trigger('click')
    await flushPromises()
    expect(action('重试').element.disabled).toBe(false)
    expect(toast).toHaveBeenCalledWith('重试失败：断网', true)
    api.mockResolvedValueOnce({ message: '已重试' })
      .mockResolvedValueOnce({ notifications: [{ ...failed, status: 'delivered' }] })
    await action('重试').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/notifications/notice-1/retry', {})
    expect(wrapper.text()).toContain('已送达')
    expect(wrapper.find('tbody button').exists()).toBe(false)
  })

  it('重试期间禁止重复点击，卸载停止轮询', async () => {
    let finish
    await showNotifications()
    api.mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
    await action('重试').trigger('click')
    expect(action('重试中…').element.disabled).toBe(true)
    await action('重试中…').trigger('click')
    expect(api.mock.calls.filter(([method]) => method === 'POST')).toHaveLength(1)
    wrapper.unmount()
    wrapper = null
    finish({ message: '已重试' })
    await flushPromises()
    await vi.advanceTimersByTimeAsync(15000)
    expect(api).toHaveBeenCalledTimes(2)
    expect(toast).not.toHaveBeenCalled()
  })

  it('五秒刷新，加载失败保留已有记录并提示', async () => {
    await showNotifications()
    api.mockRejectedValueOnce(new Error('服务器异常'))
    await vi.advanceTimersByTimeAsync(5000)
    expect(wrapper.text()).toContain('喝水')
    expect(toast).toHaveBeenCalledWith('通知加载失败：服务器异常', true)
    expect(api).toHaveBeenCalledTimes(2)
  })

  it('标记已读刷新条目和全局未读数，失败保留操作', async () => {
    const unread = { ...failed, status: 'delivered', prepared: true, push: true }
    api.mockResolvedValueOnce({ notifications: [unread], unread: 1 })
    await showNotifications()
    expect(store.unread).toBe(1)
    expect(wrapper.text()).toContain('未读')
    api.mockRejectedValueOnce(new Error('写入失败'))
    await action('标为已读').trigger('click')
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('写入失败', true)
    expect(action('标为已读').element.disabled).toBe(false)
    api.mockResolvedValueOnce({ message: '已读' }).mockResolvedValueOnce({ notifications: [{ ...unread, readAt: '2026-10-10T11:00:00+08:00' }], unread: 0 })
    await action('标为已读').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/notifications/notice-1/read', {})
    expect(store.unread).toBe(0)
    expect(action('标为已读')).toBeUndefined()
    expect(wrapper.text()).not.toContain('未读')
  })

  it('稍后提醒使用10分钟请求，失败可重试，成功后防重复安排', async () => {
    const unread = { ...failed, status: 'delivered', prepared: true, push: true }
    api.mockResolvedValueOnce({ notifications: [unread], unread: 1 })
    await showNotifications()
    api.mockRejectedValueOnce(new Error('任务队列不可写'))
    await action('10分钟后提醒').trigger('click')
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('任务队列不可写', true)
    expect(action('10分钟后提醒').element.disabled).toBe(false)
    api.mockResolvedValueOnce({ message: '已安排' }).mockResolvedValueOnce({ notifications: [{ ...unread, snoozeTaskId: 'snooze-1' }], unread: 1 })
    await action('10分钟后提醒').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/notifications/notice-1/snooze', { minutes: 10 })
    expect(action('10分钟后提醒')).toBeUndefined()
    expect(wrapper.text()).toContain('已安排稍后提醒')
  })

  it.each(['标为已读', '10分钟后提醒'])('%s期间所有同条目操作禁止双击', async label => {
    let finish
    api.mockResolvedValueOnce({ notifications: [{ ...failed, prepared: true, push: true }], unread: 1 })
    await showNotifications()
    api.mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
    await action(label).trigger('click')
    for (const button of wrapper.findAll('tbody button')) {
      expect(button.element.disabled).toBe(true)
      await button.trigger('click')
    }
    expect(api.mock.calls.filter(([method]) => method === 'POST')).toHaveLength(1)
    finish({ message: '成功' })
    await flushPromises()
  })

  it.each(['标为已读', '10分钟后提醒'])('退出后晚到的%s失败不发送旧会话提示', async label => {
    let reject
    api.mockResolvedValueOnce({ notifications: [{ ...failed, prepared: true, push: true }], unread: 1 })
    await showNotifications()
    api.mockReturnValueOnce(new Promise((_resolve, fail) => { reject = fail }))
    await action(label).trigger('click')
    wrapper.unmount()
    wrapper = null
    store.loggedIn = false
    reject(new Error('旧会话失败'))
    await flushPromises()
    expect(toast).not.toHaveBeenCalledWith('旧会话失败', true)
    expect(api.mock.calls.filter(([method]) => method === 'GET')).toHaveLength(1)
  })

  it('退出后晚到的稍后提醒成功不发送旧会话提示', async () => {
    let resolve
    api.mockResolvedValueOnce({ notifications: [{ ...failed, prepared: true, push: true }], unread: 1 })
    await showNotifications()
    api.mockReturnValueOnce(new Promise(ok => { resolve = ok }))
    await action('10分钟后提醒').trigger('click')
    wrapper.unmount()
    wrapper = null
    store.loggedIn = false
    resolve({ message: '旧会话成功' })
    await flushPromises()
    expect(toast).not.toHaveBeenCalledWith('旧会话成功')
  })
})
