import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SettingsModal from '../src/components/SettingsModal.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

const DATA = {
  '/api/status': { provider: { baseURL: 'https://api.example.com/v1', model: 'gpt-x' }, files: { tasks: 't', todos: 'd', memories: 'm' } },
  '/api/persona': { active: 'default', presets: [{ id: 'default', name: '默认', tagline: '贴心管家', prompt: '内置正文', builtin: true }] },
  '/api/heartbeat': { content: '- 检查提醒', path: '/data/HEARTBEAT.md' },
  '/api/mcp/config': { servers: [{ type: 'stdio', name: 'fs', command: 'npx', args: ['-y', 'srv'], env: { K: 'V' } }] },
  '/api/mcp': { servers: [{ name: 'fs', running: true, tools: 3 }] },
  '/api/sessions': { sessions: [{ key: 'web:1', messages: 4, updatedAt: '2026-10-14T02:00:00Z' }] },
}

let wrapper
beforeEach(() => {
  api.mockReset().mockImplementation((_m, url) => Promise.resolve(DATA[url] ?? {}))
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
    expect(api).toHaveBeenCalledWith('GET', '/api/status')
    expect(api).not.toHaveBeenCalledWith('GET', '/api/mcp/config')
    expect(api).not.toHaveBeenCalledWith('GET', '/api/sessions')
  })

  it('导航切换右侧内容，并按需加载该节数据', async () => {
    wrapper = mount(SettingsModal)
    await flushPromises()
    await click(nav('MCP 服务器'))
    expect(document.getElementById('setDialogTitle').textContent).toBe('MCP 服务器')
    expect(api).toHaveBeenCalledWith('GET', '/api/mcp/config')
    expect(document.querySelectorAll('.mcp-row')).toHaveLength(1)
    expect(document.querySelector('.mr-name').value).toBe('fs')
    expect(document.querySelector('.mr-state').textContent).toContain('在线 · 3 工具')
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
    expect(text()).toContain('当前生效：深色')

    await click(byText('button', '跟随系统'))
    expect(localStorage.getItem('friday-theme')).toBe(null)
    expect(document.documentElement.dataset.theme).toBe('light')
    expect(text()).toContain('当前生效：浅色')
    expect(byText('button', '跟随系统').getAttribute('aria-pressed')).toBe('true')
    expect(byText('button', '深色').getAttribute('aria-pressed')).toBe('false')
  })
})
