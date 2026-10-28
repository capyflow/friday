import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AgentView from '../src/components/AgentView.vue'
import { api } from '../src/api.js'
import { toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
// AgentView 静态引入 anim.js，模块加载即调 window.matchMedia（jsdom 未实现）。
vi.mock('../src/anim.js', () => ({ popIn: vi.fn() }))
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
async function show() {
  // teleport stub 成原地渲染：目录选择器弹窗内容才能被 wrapper 查到
  wrapper = mount(AgentView, { global: { stubs: { teleport: true } } })
  await flushPromises()
}

describe('智能体对话空间', () => {
  it('加载 thread 列表并投影消息（用户气泡 / note 标注 / bot 正文与轮末汇总）', async () => {
    await show()
    expect(wrapper.text()).toContain('第一件事')
    expect(wrapper.text()).toContain('codex · /home/u/proj')
    expect(wrapper.text()).toContain('帮我看下目录')
    expect(wrapper.text()).toContain('上下文已重置')
    expect(wrapper.text()).toContain('看完了。')
    expect(wrapper.text()).toContain('本轮 · ')
    expect(wrapper.find('[aria-label="复制回答正文"]').exists()).toBe(true)
  })

  it('agent 自报上下文时，composer 左组出现只读水位 chip，点开看 used/size', async () => {
    const withCtx = { ...thread, contextUsed: 12000, contextSize: 30000 }
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/threads') return { threads: [withCtx], acp: config }
      if (method === 'GET' && url === '/api/acp/threads/th1') return { thread: withCtx }
      return {}
    })
    wrapper = mount(AgentView, { global: { stubs: { teleport: true } } })
    await flushPromises()
    const chip = wrapper.get('[aria-label="上下文使用率"]')
    expect(chip.text()).toContain('40%')
    await chip.trigger('click')
    expect(wrapper.get('#agCtxPopover').text()).toContain('占用约 12k / 窗口 30k tokens')
    // 只读：弹层里没有压缩/清除动作
    expect(wrapper.findAll('#agCtxPopover button')).toHaveLength(0)
  })

  it('agent 未上报上下文时不显示水位 chip', async () => {
    await show()
    expect(wrapper.find('[aria-label="上下文使用率"]').exists()).toBe(false)
  })

  it('输入框挂载为可用的 ChatComposer（textarea，可输入）', async () => {
    await show()
    const ta = wrapper.get('[aria-label="智能体输入"]')
    expect(ta.element.tagName).toBe('TEXTAREA')
    await ta.setValue('看一下目录')
    expect(ta.element.value).toBe('看一下目录')
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

  it('新建对话（浏览器）：预填 agent 默认工作目录，提交 POST /api/acp/threads', async () => {
    await show()
    await wrapper.get('[aria-label="新建对话"]').trigger('click')
    expect(wrapper.get('.ag-ws-pick').text()).toContain('/home/u/proj') // 预填
    api.mockClear()
    await wrapper.findAll('button').find(b => b.text() === '创建').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads',
      { agent: 'codex', workspace: '/home/u/proj', title: '' })
  })

  it('新建对话：工作目录留空也可创建（后端回退临时会话目录）', async () => {
    api.mockImplementation(async (method, url) =>
      method === 'GET' && url === '/api/acp/threads'
        ? { threads: [], acp: { enabled: true, agents: [{ name: 'codex', command: 'npx', enabled: true }] } }
        : {})
    await show()
    await wrapper.get('[aria-label="新建对话"]').trigger('click')
    expect(wrapper.text()).toContain('留空则用临时会话目录')
    expect(wrapper.get('.ag-ws-pick').text()).toContain('选择目录')
    await wrapper.findAll('button').find(b => b.text() === '创建').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads',
      { agent: 'codex', workspace: '', title: '' })
  })

  it('目录选择器：逐层浏览、上一级、路径栏直达，回填所选绝对路径', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/threads')
        return { threads: [], acp: { enabled: true, agents: [{ name: 'codex', command: 'npx', enabled: true }] } }
      if (method === 'GET' && url === '/api/acp/dirs')
        return { path: '/home/u', parent: '/home', dirs: [{ name: 'proj', path: '/home/u/proj' }] }
      if (method === 'GET' && url === '/api/acp/dirs?path=%2Fhome%2Fu%2Fproj')
        return { path: '/home/u/proj', parent: '/home/u', dirs: [] }
      if (method === 'GET' && url === '/api/acp/dirs?path=%2Fhome%2Fu')
        return { path: '/home/u', parent: '/home', dirs: [{ name: 'proj', path: '/home/u/proj' }] }
      return {}
    })
    await show()
    await wrapper.get('[aria-label="新建对话"]').trigger('click')
    await wrapper.get('.ag-ws-pick').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('选择工作目录')
    // 逐层点进 proj
    await wrapper.findAll('.ag-dirlist button').find(b => b.text() === 'proj').trigger('click')
    await flushPromises()
    expect(wrapper.get('.ag-dir-cur').text()).toBe('/home/u/proj')
    // 上一级回到 /home/u，再用路径栏直达回去
    await wrapper.get('[aria-label="上一级"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('.ag-dir-cur').text()).toBe('/home/u')
    const bar = wrapper.get('[aria-label="目录路径"]')
    await bar.setValue('/home/u/proj')
    await bar.trigger('keydown.enter')
    await flushPromises()
    expect(wrapper.get('.ag-dir-cur').text()).toBe('/home/u/proj')
    // 使用此目录 → 回填表单并提交
    await wrapper.findAll('button').find(b => b.text() === '使用此目录').trigger('click')
    expect(wrapper.get('.ag-ws-pick').text()).toContain('/home/u/proj')
    api.mockClear()
    await wrapper.findAll('button').find(b => b.text() === '创建').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads',
      { agent: 'codex', workspace: '/home/u/proj', title: '' })
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

describe('智能体会话滚动位置', () => {
  it('打开会话默认滚到最底部，不沿用上一个 thread 的位置', async () => {
    const second = {
      id: 'th2', agent: 'codex', workspace: '/home/u/proj', title: '第二件事',
      state: 'idle', degraded: false,
      messages: [
        { role: 'user', text: '第二个问题' },
        { role: 'assistant', text: '第二个回答', in: 1, out: 1, ms: 10, stopped: 'end_turn' },
      ],
    }
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/threads') return { threads: [thread, second], acp: config }
      if (method === 'GET' && url === '/api/acp/threads/th1') return { thread }
      if (method === 'GET' && url === '/api/acp/threads/th2') return { thread: second }
      return {}
    })
    const { store } = await import('../src/store.js')
    store.view = 'agent' // 真机上本组件只在智能体视图可见
    await show()
    const el = wrapper.get('.ag-scroller').element
    Object.defineProperty(el, 'scrollHeight', { configurable: true, value: 2000 })
    Object.defineProperty(el, 'clientHeight', { configurable: true, value: 500 })
    el.scrollTop = 0 // 上一个会话停在顶部
    await wrapper.findAll('.ag-item')[1].trigger('click')
    await flushPromises()
    await flushPromises()
    expect(el.scrollTop).toBe(2000)
    store.view = 'chat'
  })
})

describe('智能体页加载态', () => {
  it('首次加载完成前显示加载态，不把“还没拉到”误报成功能未开启', async () => {
    let finish
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/threads')
        return new Promise(resolve => { finish = resolve })
      return {}
    })
    wrapper = mount(AgentView, { global: { stubs: { teleport: true } } })
    await flushPromises()
    expect(wrapper.text()).toContain('正在加载智能体…')
    expect(wrapper.text()).not.toContain('智能体功能未开启')
    finish({ threads: [thread], acp: config })
    await flushPromises()
    expect(wrapper.text()).toContain('第一件事')
  })

  it('首次加载失败给出重试入口，恢复后正常显示', async () => {
    let fail = true
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/threads') {
        if (fail) throw new Error('网络断了')
        return { threads: [thread], acp: config }
      }
      if (method === 'GET' && url === '/api/acp/threads/th1') return { thread }
      return {}
    })
    await show()
    expect(wrapper.text()).toContain('加载失败')
    fail = false
    await wrapper.findAll('button').find(b => b.text() === '重试').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('第一件事')
  })
})
