import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AgentView from '../src/components/AgentView.vue'
import { api } from '../src/api.js'
import { toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

const thread = {
  id: 'th1', agent: 'codex', workspace: '/home/u/proj', title: '第一件事',
  state: 'idle', degraded: false,
  messages: [
    { role: 'user', text: '帮我看下目录', at: '2026-10-17T10:00:00Z' },
    { role: 'note', text: '上下文已重置（历史记录仍可见）' },
    { role: 'assistant', text: '看完了。', in: 10, out: 5, ms: 1200, stopped: 'end_turn' },
  ],
}
const config = {
  enabled: true, maxConcurrent: 2,
  agents: [{ name: 'codex', command: 'npx', enabled: true, workspace: '/home/u/proj' }],
}
let wrapper
beforeEach(() => {
  api.mockReset().mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/acp/threads') return { threads: [thread], acp: config }
    if (method === 'GET' && url === '/api/acp/threads/th1') return { thread }
    return { message: '已完成' }
  })
  toast.mockClear()
  vi.stubGlobal('URL', { createObjectURL: vi.fn(() => 'blob:x'), revokeObjectURL: vi.fn() })
})
afterEach(() => { wrapper?.unmount(); wrapper = null; vi.unstubAllGlobals() })
async function show() { wrapper = mount(AgentView); await flushPromises() }

describe('智能体对话空间', () => {
  it('加载 thread 列表并投影消息（用户气泡 / note 标注 / bot 正文与轮末汇总）', async () => {
    await show()
    expect(wrapper.text()).toContain('第一件事')
    expect(wrapper.text()).toContain('codex · /home/u/proj')
    expect(wrapper.text()).toContain('帮我看下目录')
    expect(wrapper.text()).toContain('上下文已重置')
    expect(wrapper.text()).toContain('看完了。')
    expect(wrapper.text()).toContain('本轮 · ')
  })

  it('未开启功能时显示引导并深链设置', async () => {
    api.mockImplementation(async (method, url) =>
      method === 'GET' && url === '/api/acp/threads' ? { threads: [], acp: { enabled: false, agents: [] } } : {})
    await show()
    expect(wrapper.text()).toContain('智能体功能未开启')
    const go = wrapper.findAll('button').find(b => b.text() === '去设置')
    await go.trigger('click')
    const { store } = await import('../src/store.js')
    expect(store.settingsSection).toBe('agent')
    expect(store.settingsOpen).toBe(true)
  })

  it('新建对话：表单预填 agent 的默认工作目录，提交 POST /api/acp/threads', async () => {
    await show()
    await wrapper.get('[aria-label="新建对话"]').trigger('click')
    const ws = wrapper.get('.ag-form input')
    expect(ws.element.value).toBe('/home/u/proj') // 预填
    api.mockClear()
    ws.setValue('/home/u/other')
    await wrapper.findAll('button').find(b => b.text() === '创建').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads',
      { agent: 'codex', workspace: '/home/u/other', title: '' })
  })

  it('删除需要二次确认', async () => {
    await show()
    api.mockClear()
    await wrapper.get('[aria-label="删除 第一件事"]').trigger('click')
    expect(wrapper.text()).toContain('确认删除?')
    expect(api).not.toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')
    await wrapper.get('[aria-label="删除 第一件事"]').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')
  })

  it('orphaned 状态展示提示且带中断提示文案', async () => {
    await show()
    // interrupted/orphaned 提示映射存在（当前 idle 不显示）
    expect(wrapper.text()).not.toContain('上次会话被中断')
    const interrupted = { ...thread, state: 'interrupted' }
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/threads') return { threads: [interrupted], acp: config }
      if (method === 'GET' && url === '/api/acp/threads/th1') return { thread: interrupted }
      return {}
    })
    wrapper.unmount()
    wrapper = mount(AgentView)
    await flushPromises()
    expect(wrapper.text()).toContain('上次会话被中断')
  })
})
