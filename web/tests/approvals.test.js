import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ApprovalCards from '../src/components/ApprovalCards.vue'
import { api } from '../src/api.js'
import { store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

const item = { id: 'approval-1', summary: '删除文件', detail: 'notes.txt' }
let wrapper

function deferred() {
  let resolve, reject
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail })
  return { promise, resolve, reject }
}

beforeEach(() => {
  vi.useFakeTimers()
  store.loggedIn = true
  store.username = 'admin'
  toast.mockClear()
  api.mockReset().mockImplementation(method => Promise.resolve(method === 'GET' ? { approvals: [item] } : { message: '已批准' }))
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.useRealTimers()
})

async function showCards() {
  wrapper = mount(ApprovalCards)
  await flushPromises()
}

describe('审批失败与登录切换', () => {
  it.each(['断网', '请求超时', '服务端保存失败', '审批已超时'])('失败保留卡片，可再次提交：%s', async message => {
    const post = vi.fn().mockRejectedValueOnce(new Error(message)).mockResolvedValueOnce({ message: '已批准' })
    api.mockImplementation(method => method === 'GET' ? Promise.resolve({ approvals: [item] }) : post())
    await showCards()
    await wrapper.get('.primary').trigger('click')
    await flushPromises()
    expect(wrapper.find('.approval-card').exists()).toBe(true)
    expect(wrapper.get('.primary').element.disabled).toBe(false)
    expect(toast).toHaveBeenCalledWith(message, true)
    await vi.advanceTimersByTimeAsync(5000)
    expect(wrapper.find('.approval-card').exists()).toBe(true)
    await wrapper.get('.primary').trigger('click')
    await flushPromises()
    expect(post).toHaveBeenCalledTimes(2)
    expect(wrapper.find('.approval-card').exists()).toBe(false)
    // 服务端列表短暂保留该 ID 时，成功结果也不会重新弹出。
    await vi.advanceTimersByTimeAsync(5000)
    expect(wrapper.find('.approval-card').exists()).toBe(false)
  })

  it('处理期间保留卡片与忙碌反馈，禁止重复点击', async () => {
    const request = deferred()
    api.mockImplementation(method => method === 'GET' ? Promise.resolve({ approvals: [item] }) : request.promise)
    await showCards()
    await wrapper.get('.primary').trigger('click')
    expect(wrapper.get('.approval-card').attributes('aria-busy')).toBe('true')
    expect(wrapper.get('.primary').text()).toBe('处理中…')
    for (const button of wrapper.findAll('button')) {
      expect(button.element.disabled).toBe(true)
      await button.trigger('click')
    }
    expect(api.mock.calls.filter(([method]) => method === 'POST')).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(5000)
    expect(wrapper.get('.primary').element.disabled).toBe(true)
    request.resolve({ message: '已批准' })
    await flushPromises()
    expect(wrapper.find('.approval-card').exists()).toBe(false)
  })

  it('退出登录立即清除卡片，旧轮询响应不能出现在新登录中', async () => {
    const oldPoll = deferred()
    api.mockReturnValueOnce(oldPoll.promise)
    await showCards()
    store.loggedIn = false
    await flushPromises()
    expect(wrapper.find('#approvalBar').exists()).toBe(false)
    store.username = 'new-admin'
    store.loggedIn = true
    await flushPromises()
    expect(wrapper.find('.approval-card').exists()).toBe(true)
    oldPoll.resolve({ approvals: [{ id: 'old', summary: '旧账户操作' }] })
    await flushPromises()
    expect(wrapper.text()).not.toContain('旧账户操作')
  })

  it('登录切换后旧审批响应不隐藏新卡片，也不发出旧提示', async () => {
    const oldPost = deferred()
    api.mockImplementation(method => method === 'GET' ? Promise.resolve({ approvals: [item] }) : oldPost.promise)
    await showCards()
    await wrapper.get('.primary').trigger('click')
    store.loggedIn = false
    store.loggedIn = true
    await flushPromises()
    oldPost.resolve({ message: '旧请求成功' })
    await flushPromises()
    expect(wrapper.find('.approval-card').exists()).toBe(true)
    expect(wrapper.get('.primary').element.disabled).toBe(false)
    expect(toast).not.toHaveBeenCalledWith('旧请求成功')
  })

  it('卸载后停止轮询', async () => {
    await showCards()
    wrapper.unmount()
    wrapper = null
    await vi.advanceTimersByTimeAsync(15000)
    expect(api).toHaveBeenCalledTimes(1)
  })
})
