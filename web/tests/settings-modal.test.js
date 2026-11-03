import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SettingsModal from '../src/components/SettingsModal.vue'
import { api, traceAPI } from '../src/api.js'
import { store } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn(), traceAPI: vi.fn() }))
// 运行状态节经 anim.js 做数字滚动，模块加载即调 window.matchMedia（jsdom 未实现）。
vi.mock('../src/anim.js', () => ({ runCountUp: vi.fn() }))

const DATA = {
  '/api/status': { provider: { baseURL: 'https://api.example.com/v1', model: 'gpt-x' }, apiKeyConfigured: true,
    tasks: { total: 2 }, todos: { pending: 1 }, memories: 3, uptimeSeconds: 60,
    files: { tasks: 't', todos: 'd', memories: 'm' } },
  '/api/persona': { active: 'default', presets: [{ id: 'default', name: '默认', tagline: '贴心管家', prompt: '内置正文', builtin: true }] },
  '/api/heartbeat': { content: '- 检查提醒', path: '/data/HEARTBEAT.md' },
  '/api/mcp/config': { servers: [{ type: 'stdio', name: 'fs', command: 'npx', args: ['-y', 'srv'], env: { K: 'V' } }] },
  '/api/mcp': { servers: [{ name: 'fs', running: true, tools: 3 }] },
  '/api/sessions': { sessions: [{ key: 'web:1', messages: 4, updatedAt: '2026-10-14T02:00:00Z' }] },
  '/api/usage': { days: [{ date: '2026-10-15', calls: 3, in: 1200, out: 300 }], total: { in: 1200, out: 300 },
    byModel: [{ key: 'gpt-x', calls: 3, in: 1200, out: 300 }], byOrigin: [], bySession: [], byRun: [] },
  '/api/wiki/pages': { pages: [{ id: 'p1', title: '用户偏好', snippet: '喜欢深色', source: 'chat', updatedAt: '2026-10-14T02:00:00Z' }] },
  '/api/provider/models': { active: 'gpt-x', globalWindowTokens: 0,
    models: [{ name: 'gpt-x', baseURL: 'https://api.example.com/v1', model: 'gpt-x', keySet: true, contextWindow: 128000, inputs: ['text', 'image'] }] },
}

let wrapper
beforeEach(() => {
  api.mockReset().mockImplementation((_m, url) => Promise.resolve(DATA[url] ?? {}))
  traceAPI.mockReset().mockResolvedValue({ events: [], stats: { requests: 0, in: 0, out: 0 } })
  Object.assign(store, { settingsOpen: false, settingsSection: '', traceFilter: '' })
})
afterEach(() => { wrapper?.unmount(); wrapper = null })

// 弹窗 Teleport 到 body（stub 会让已挂载的节整体重挂，掩盖真实行为），故用 document 查询
const text = () => document.body.textContent
const byText = (sel, label) => Array.from(document.querySelectorAll(sel)).find(el => el.textContent === label)
const nav = label => byText('.set-nav-item', label)
async function click(el) {
  el.dispatchEvent(new window.MouseEvent('click', { bubbles: true, cancelable: true }))
  await flushPromises()
}

describe('设置弹窗', () => {
  it('默认展示模型服务一节，只拉该节的接口', async () => {
    wrapper = mount(SettingsModal)
    await flushPromises()
    expect(document.getElementById('setDialogTitle').textContent).toBe('模型服务')
    expect(api).toHaveBeenCalledWith('GET', '/api/provider/models')
    expect(api).not.toHaveBeenCalledWith('GET', '/api/mcp/config')
    expect(api).not.toHaveBeenCalledWith('GET', '/api/sessions')
    // 库条目渲染成收起的行卡片，激活条目带「生效中」徽标
    expect(document.querySelector('input[aria-label="模型显示名"]').value).toBe('gpt-x')
    expect(document.body.textContent).toContain('生效中')
  })

  it('模型库：radio 换激活、窗口数值化、保存全量提交', async () => {
    wrapper = mount(SettingsModal)
    await flushPromises()
    api.mockClear()
    // 改名 + 关掉图片输入 + 换激活到新条目：先加一条再选它
    const nameInputs = () => Array.from(document.querySelectorAll('input[aria-label="模型显示名"]'))
    nameInputs()[0].value = 'gpt-x-renamed'
    nameInputs()[0].dispatchEvent(new window.Event('input', { bubbles: true }))
    const addBtn = byText('button', '＋ 添加模型')
    await click(addBtn)
    nameInputs()[1].value = 'deepseek'
    nameInputs()[1].dispatchEvent(new window.Event('input', { bubbles: true }))
    const urlInputs = Array.from(document.querySelectorAll('input[type="url"]'))
    urlInputs[1].value = 'https://api.deepseek.com/v1'
    urlInputs[1].dispatchEvent(new window.Event('input', { bubbles: true }))
    const modelInputs = Array.from(document.querySelectorAll('input[placeholder="服务商提供的模型名称"]'))
    modelInputs[1].value = 'deepseek-chat'
    modelInputs[1].dispatchEvent(new window.Event('input', { bubbles: true }))
    await flushPromises()
    const radio = Array.from(document.querySelectorAll('input[type="radio"]')).find(r => r.getAttribute('aria-label') === '使用模型 deepseek')
    radio.click()
    await flushPromises()
    await click(byText('button', '保存并生效'))
    expect(api).toHaveBeenCalledWith('POST', '/api/provider/models', expect.objectContaining({
      active: 'deepseek',
      models: expect.arrayContaining([
        expect.objectContaining({ name: 'gpt-x-renamed', inputs: ['text', 'image'], contextWindow: 128000 }),
        expect.objectContaining({ name: 'deepseek', baseURL: 'https://api.deepseek.com/v1', model: 'deepseek-chat', inputs: ['text', 'image'], contextWindow: 0 }),
      ]),
    }))
  })

  it('导航切换右侧内容，并按需加载该节数据', async () => {
    wrapper = mount(SettingsModal)
    await flushPromises()
    await click(nav('MCP 服务器'))
    expect(document.getElementById('setDialogTitle').textContent).toBe('MCP 服务器')
    expect(api).toHaveBeenCalledWith('GET', '/api/mcp/config')
    expect(document.querySelectorAll('.svc-card')).toHaveLength(1)
    expect(document.querySelector('[aria-label="服务器名称"]').value).toBe('fs')
    expect(document.querySelector('.svc-card .svc-badge').textContent).toContain('在线 · 3 工具')
    // 卡片默认收起：表单字段不在展开态之外摊开（v-show 留着 DOM，但标记为不可见）
    expect(document.querySelector('.svc-body').style.display).toBe('none')
    expect(nav('MCP 服务器').classList.contains('active')).toBe(true)

    await click(nav('会话管理'))
    expect(text()).toContain('web:1')
  })

  it('已访问的节保持挂载：切走再切回不丢未保存的编辑，也不重复拉取', async () => {
    wrapper = mount(SettingsModal)
    await flushPromises()
    await click(nav('人设提示词'))
    const box = document.querySelector('#personaPrompt')
    box.value = '草稿正文'
    box.dispatchEvent(new window.Event('input', { bubbles: true }))
    await click(nav('心跳清单'))
    await click(nav('人设提示词'))
    expect(document.querySelector('#personaPrompt').value).toBe('草稿正文')
    expect(api.mock.calls.filter(c => c[1] === '/api/persona')).toHaveLength(1)
    expect(api.mock.calls.filter(c => c[1] === '/api/heartbeat')).toHaveLength(1)
  })

  it('关闭按钮与 Esc 发出 close，遮罩只认落在自身的点击', async () => {
    wrapper = mount(SettingsModal)
    await flushPromises()
    await click(document.querySelector('.set-head-txt'))
    expect(wrapper.emitted('close')).toBeUndefined()

    await click(document.querySelector('.set-mask'))
    expect(wrapper.emitted('close')).toHaveLength(1)

    await click(document.querySelector('.set-close'))
    expect(wrapper.emitted('close')).toHaveLength(2)

    document.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape' }))
    await flushPromises()
    expect(wrapper.emitted('close')).toHaveLength(3)
  })

  it('关于一节展示配置快照，退出登录经弹窗透传给外壳', async () => {
    wrapper = mount(SettingsModal)
    await flushPromises()
    await click(nav('关于与配置'))
    expect(document.querySelector('#settingsRows').textContent).toContain('https://api.example.com/v1')
    await click(Array.from(document.querySelectorAll('.set-pane .btn.danger')).find(b => b.textContent.includes('退出登录')))
    expect(wrapper.emitted('logout')).toHaveLength(1)
  })

  it('外观一节切主题即时生效：深色落 localStorage，跟随系统清掉显式选择', async () => {
    localStorage.removeItem('friday-theme')
    wrapper = mount(SettingsModal)
    await flushPromises()
    await click(nav('外观'))
    // 未选过 = 跟随系统（jsdom 没有 matchMedia，systemTheme 回落 light）
    expect(byText('button', '跟随系统').getAttribute('aria-pressed')).toBe('true')

    await click(byText('button', '深色'))
    expect(document.documentElement.dataset.theme).toBe('dark')
    expect(localStorage.getItem('friday-theme')).toBe('dark')

    await click(byText('button', '跟随系统'))
    expect(localStorage.getItem('friday-theme')).toBe(null)
    expect(document.documentElement.dataset.theme).toBe('light')
    expect(byText('button', '跟随系统').getAttribute('aria-pressed')).toBe('true')
    expect(byText('button', '深色').getAttribute('aria-pressed')).toBe('false')
  })

  it('外部深链定位到指定节，定位值读走即清零', async () => {
    store.settingsSection = 'trace'
    store.traceFilter = 'goal-g1'
    traceAPI.mockResolvedValue({ events: [{ kind: 'llm_in', text: '{}', time: '2026-10-15T10:00:00Z' }], stats: { requests: 1, in: 10, out: 5 } })
    wrapper = mount(SettingsModal)
    await flushPromises()
    expect(document.getElementById('setDialogTitle').textContent).toBe('调用轨迹')
    expect(store.settingsSection).toBe('')
    expect(text()).toContain('只看 goal-g1')
    expect(traceAPI).toHaveBeenCalledWith('/api/traces?session=goal-g1')

    // 定位值已消费：关掉重开（App 里 v-if 重挂）回到默认节
    wrapper.unmount()
    wrapper = mount(SettingsModal)
    await flushPromises()
    expect(document.getElementById('setDialogTitle').textContent).toBe('模型服务')
  })

  it('运行状态节渲染状态卡与用量账本', async () => {
    wrapper = mount(SettingsModal)
    await flushPromises()
    await click(nav('运行状态'))
    expect(text()).toContain('今日调用')
    const rows = document.querySelector('.dim-panel tbody')
    expect(rows).toBeTruthy()
    expect(rows.textContent).toContain('gpt-x')
  })

  it('记忆节渲染目录与页数', async () => {
    wrapper = mount(SettingsModal)
    await flushPromises()
    await click(nav('记忆'))
    expect(document.querySelector('.toc-item').textContent).toContain('用户偏好')
    expect(document.querySelector('.wiki-count').textContent).toBe('1 页')
  })
})
