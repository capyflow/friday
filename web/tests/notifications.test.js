import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import NotificationsView from '../src/components/NotificationsView.vue'
import { api } from '../src/api.js'
import { toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

const failed = { id: 'notice-1', text: '喝水', status: 'failed', createdAt: '2026-10-10T10:00:00+08:00',
  attempts: 1, lastError: '钉钉不可达', deliveredTo: ['web'] }
let wrapper

beforeEach(() => {
  vi.useFakeTimers()
  api.mockReset().mockResolvedValue({ notifications: [failed] })
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
    expect(wrapper.findAll('button')).toHaveLength(1)
    expect(wrapper.text()).toContain('已尝试 1 次')
    expect(wrapper.text()).toContain('钉钉不可达')
  })

  it('重试失败保留记录和按钮，成功后更新投递状态', async () => {
    await showNotifications()
    api.mockRejectedValueOnce(new Error('断网'))
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.get('button').element.disabled).toBe(false)
    expect(toast).toHaveBeenCalledWith('重试失败：断网', true)
    api.mockResolvedValueOnce({ message: '已重试' })
      .mockResolvedValueOnce({ notifications: [{ ...failed, status: 'delivered' }] })
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/notifications/notice-1/retry', {})
    expect(wrapper.text()).toContain('已送达')
    expect(wrapper.find('button').exists()).toBe(false)
  })

  it('重试期间禁止重复点击，卸载停止轮询', async () => {
    let finish
    await showNotifications()
    api.mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
    await wrapper.get('button').trigger('click')
    expect(wrapper.get('button').element.disabled).toBe(true)
    await wrapper.get('button').trigger('click')
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
})
