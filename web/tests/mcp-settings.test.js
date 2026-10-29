import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import McpSection from '../src/components/settings/McpSection.vue'
import SelectBox from '../src/components/SelectBox.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

// 一台配了 OAuth 但还没授权的远程服务器；授权成功后 running/authState 会翻转。
let authorized

beforeEach(() => {
  authorized = false
  vi.stubGlobal('open', vi.fn(() => ({})))
  api.mockReset().mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/mcp/config') {
      return { servers: [{ name: 'notion', url: 'https://mcp.notion.com/mcp', auth: 'oauth', headers: {} }] }
    }
    if (method === 'GET' && url === '/api/mcp') {
      return {
        servers: [{
          name: 'notion',
          url: 'https://mcp.notion.com/mcp',
          running: authorized,
          tools: authorized ? 3 : 0,
          auth: 'oauth',
          authState: authorized ? 'authorized' : 'required',
          authMessage: authorized ? '' : '登录态已失效，需要重新授权',
          lastError: authorized ? '' : '需要授权：点「授权」在浏览器里登录服务商',
        }],
      }
    }
    if (method === 'POST' && url === '/api/mcp/notion/authorize') {
      return { url: 'https://mcp.notion.com/authorize?state=abc', message: '请在浏览器里完成授权' }
    }
    if (method === 'GET' && url === '/api/mcp/notion/auth') {
      return { state: authorized ? 'authorized' : 'authorizing', message: '' }
    }
    return { message: '已保存' }
  })
})
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals() })

async function show() {
  const wrapper = mount(McpSection)
  await flushPromises()
  return wrapper
}

function buttonByText(wrapper, text) {
  return wrapper.findAll('button').find(b => b.text().includes(text))
}
function hasButton(wrapper, text) {
  return wrapper.findAll('button').some(b => b.text().includes(text))
}

describe('设置 → MCP 授权', () => {
  it('待授权时显示徽标与原因，保存时把鉴权方式写回 auth', async () => {
    const wrapper = await show()
    expect(wrapper.text()).toContain('待授权')
    expect(wrapper.text()).toContain('登录态已失效，需要重新授权')

    await buttonByText(wrapper, '保存并生效').trigger('click')
    await flushPromises()
    const saved = api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/mcp/config')[2]
    expect(saved.servers[0]).toMatchObject({ name: 'notion', url: 'https://mcp.notion.com/mcp', auth: 'oauth' })
  })

  it('点授权打开服务商页面，轮询到已授权后自动刷新状态并出现断开按钮', async () => {
    vi.useFakeTimers()
    const wrapper = await show()
    const auth = buttonByText(wrapper, '授权')
    expect(auth.exists()).toBe(true)

    await auth.trigger('click')
    await flushPromises()
    expect(window.open).toHaveBeenCalledWith('https://mcp.notion.com/authorize?state=abc', '_blank', 'noopener')

    // 后端还在等回调：继续轮询，按钮此时不可重复点。
    await vi.advanceTimersByTimeAsync(1500)
    await flushPromises()
    expect(buttonByText(wrapper, '授权').attributes('disabled')).toBeDefined()

    authorized = true
    await vi.advanceTimersByTimeAsync(1500)
    await flushPromises()
    expect(wrapper.text()).toContain('已授权')
    expect(buttonByText(wrapper, '断开授权').exists()).toBe(true)
    // 授权完成即停止轮询（此后再不查 /auth）。
    const before = api.mock.calls.filter(([m, u]) => m === 'GET' && u === '/api/mcp/notion/auth').length
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.mock.calls.filter(([m, u]) => m === 'GET' && u === '/api/mcp/notion/auth').length).toBe(before)
  })

  it('桌面外壳里点授权走外壳命令打开系统浏览器，不用 window.open', async () => {
    vi.useFakeTimers()
    const invoke = vi.fn(async () => ({}))
    vi.stubGlobal('__TAURI__', { core: { invoke } })
    const wrapper = await show()

    await buttonByText(wrapper, '授权').trigger('click')
    await flushPromises()
    expect(invoke).toHaveBeenCalledWith('open_url', { url: 'https://mcp.notion.com/authorize?state=abc' })
    expect(window.open).not.toHaveBeenCalled()
    // 等待回调期间始终能复制授权链接（刷新页面后也靠它兜底）。
    expect(hasButton(wrapper, '复制授权链接')).toBe(true)
  })

  it('浏览器没弹出时露出「复制授权链接」，不再给「允许弹窗」这种误导提示', async () => {
    vi.useFakeTimers()
    window.open.mockReturnValue(null)
    const wrapper = await show()

    await buttonByText(wrapper, '授权').trigger('click')
    await flushPromises()
    expect(hasButton(wrapper, '复制授权链接')).toBe(true)
    expect(wrapper.text()).not.toContain('请允许弹窗')
  })

  it('静态请求头服务器不出现授权入口，切到浏览器授权才写 oauth', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/mcp/config') {
        return { servers: [{ name: 'blinko', url: 'https://mcp.blinko.me/mcp', headers: { Authorization: 'Bearer x' } }] }
      }
      if (method === 'GET' && url === '/api/mcp') return { servers: [{ name: 'blinko', running: true, tools: 2 }] }
      return { message: '已保存' }
    })
    const wrapper = await show()
    // 徽标只出现在 .mr-state 里（底部说明文字也提到「待授权」，不能整体断言）。
    expect(wrapper.findAll('.mr-state').map(s => s.text()).join(' ')).not.toContain('待授权')
    expect(hasButton(wrapper, '授权')).toBe(false)

    // 鉴权下拉切到「浏览器授权」后保存，auth 落到配置里。
    const select = wrapper.findAllComponents(SelectBox).find(s => s.props('ariaLabel') === '鉴权方式')
    select.vm.$emit('update:modelValue', 'oauth')
    await flushPromises()
    await buttonByText(wrapper, '保存并生效').trigger('click')
    await flushPromises()
    const saved = api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/mcp/config')[2]
    expect(saved.servers[0].auth).toBe('oauth')
  })

  it('断开授权调用后端并刷新状态', async () => {
    authorized = true
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/mcp/config') {
        return { servers: [{ name: 'notion', url: 'https://mcp.notion.com/mcp', auth: 'oauth' }] }
      }
      if (method === 'GET' && url === '/api/mcp') {
        return { servers: [{ name: 'notion', running: true, tools: 3, auth: 'oauth', authState: 'authorized' }] }
      }
      if (method === 'POST' && url === '/api/mcp/notion/disconnect') return { message: '已断开 notion 的授权' }
      return { message: '已保存' }
    })
    const wrapper = await show()
    await buttonByText(wrapper, '断开授权').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/mcp/notion/disconnect', {})
  })
})
