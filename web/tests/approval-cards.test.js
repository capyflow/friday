import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ApprovalCards from '../src/components/ApprovalCards.vue'
import { api } from '../src/api.js'
import { store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

let wrapper
beforeEach(() => {
  store.loggedIn = true
  api.mockReset().mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/approvals') {
      return { approvals: [{ id: 7, tool: '智能体权限', summary: '执行 rm', detail: 'rm -rf tmp/' }] }
    }
    return { message: '已批准' }
  })
  toast.mockClear()
})
afterEach(() => { wrapper?.unmount(); wrapper = null })
async function show() {
  wrapper = mount(ApprovalCards)
  await flushPromises()
}
function approveButton() {
  return wrapper.findAll('button').find(b => b.text() === '批准')
}

describe('审批卡（含智能体权限卡被 turn 结束撤下的场景）', () => {
  it('轮询展示待审批卡，批准成功后移除并提示', async () => {
    await show()
    expect(wrapper.text()).toContain('执行 rm')
    await approveButton().trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/approvals/7', { approve: true, remember: false })
    expect(wrapper.text()).not.toContain('执行 rm')
    expect(toast).toHaveBeenCalledWith('已批准')
  })

  it('决定打在已撤下的卡上（对话已结束）时提示失效原因，而非无响应', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/approvals') {
        return { approvals: [{ id: 8, tool: '智能体权限', summary: '无人处理的授权' }] }
      }
      throw new Error('未找到待审批操作（可能已超时或已处理）')
    })
    await show()
    expect(wrapper.text()).toContain('无人处理的授权')
    await approveButton().trigger('click')
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('未找到待审批操作（可能已超时或已处理）', true)
    // 卡保留在本会话列表里（服务端 5s 轮询对齐后自然消失），用户不会误以为点成功
    expect(wrapper.text()).toContain('无人处理的授权')
  })
})
