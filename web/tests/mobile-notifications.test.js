import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import MobileNotifications from '../src/mobile/MobileNotifications.vue'
import { push, notices, loadNotices, markRead, retryNotice, snoozeNotice } from '../src/mobile/push.js'

// 通知页组件测试：列表来自 push.js 的共享 reactive（长链接并入），这里直接
// mock 掉 push 模块控制数据源，验证渲染与动作接线（已读/重试/延后各只对
// 该出现的卡片出现），长链接行为本身在 mobile-push.test.js。
vi.mock('../src/mobile/push.js', async () => {
  const { reactive } = await import('vue')
  return {
    push: reactive({ status: 'on', unread: 0 }),
    notices: reactive([]),
    loadNotices: vi.fn().mockResolvedValue(undefined),
    markRead: vi.fn().mockResolvedValue(undefined),
    retryNotice: vi.fn().mockResolvedValue(undefined),
    snoozeNotice: vi.fn().mockResolvedValue(undefined),
  }
})

let wrapper

const seed = list => {
  notices.splice(0, notices.length, ...list)
}

beforeEach(() => {
  // vitest 配了 restoreMocks: true（每用例前重置实现），按仓库惯例在此重新装配。
  loadNotices.mockResolvedValue(undefined)
  markRead.mockResolvedValue(undefined)
  retryNotice.mockResolvedValue(undefined)
  snoozeNotice.mockResolvedValue(undefined)
  push.status = 'on'
  push.unread = 0
  seed([])
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.clearAllMocks()
})

const card = i => wrapper.findAll('.m-notice')[i]

describe('移动端通知页', () => {
  it('进页面全量拉一次，空列表有占位', async () => {
    wrapper = mount(MobileNotifications)
    await flushPromises()
    expect(loadNotices).toHaveBeenCalled()
    expect(wrapper.text()).toContain('还没有通知')
  })

  it('展示状态徽标、未读点、渠道与错误；已读的不出现已读按钮', async () => {
    seed([
      { id: '1', text: '喝水提醒', status: 'delivered', createdAt: '2026-11-06T10:00:00+08:00',
        deliveredTo: ['web', 'dingtalk'], readAt: null },
      { id: '2', text: '报告跑完了', status: 'failed', createdAt: '2026-11-06T11:00:00+08:00',
        deliveredTo: ['web'], readAt: '2026-11-06T11:05:00+08:00', lastError: '通道超时' },
    ])
    wrapper = mount(MobileNotifications)
    await flushPromises()

    expect(card(0).text()).toContain('喝水提醒')
    expect(card(0).text()).toContain('已送达')
    expect(card(0).text()).toContain('钉钉')
    expect(card(0).classes()).toContain('unread')
    const readBtn = card(0).findAll('button').find(b => b.text() === '已读')
    await readBtn.trigger('click')
    expect(markRead).toHaveBeenCalledWith('1')

    expect(card(1).text()).toContain('失败')
    expect(card(1).text()).toContain('通道超时')
    expect(card(1).classes()).not.toContain('unread')
    const retryBtn = card(1).findAll('button').find(b => b.text() === '重试')
    await retryBtn.trigger('click')
    expect(retryNotice).toHaveBeenCalledWith('2')
    // 已读卡不再给「已读」按钮
    expect(card(1).findAll('button').some(b => b.text() === '已读')).toBe(false)
  })

  it('未读且未失败才给延后按钮', async () => {
    seed([{ id: '3', text: '站起来了', status: 'pending', createdAt: '2026-11-06T12:00:00+08:00',
      deliveredTo: [], readAt: null }])
    wrapper = mount(MobileNotifications)
    await flushPromises()
    const snoozeBtn = card(0).findAll('button').find(b => b.text() === '延后 10 分钟')
    expect(snoozeBtn).toBeTruthy()
    await snoozeBtn.trigger('click')
    expect(snoozeNotice).toHaveBeenCalledWith('3', 10)
  })

  it('长链接状态显示在页头', async () => {
    push.status = 'off'
    wrapper = mount(MobileNotifications)
    await flushPromises()
    expect(wrapper.find('.m-push-state').text()).toContain('未连接')
    push.status = 'on'
    await flushPromises()
    expect(wrapper.find('.m-push-state').text()).toContain('实时已连接')
  })
})
