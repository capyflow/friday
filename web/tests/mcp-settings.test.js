import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import McpSection from '../src/components/settings/McpSection.vue'
import SelectBox from '../src/components/SelectBox.vue'
import { api } from '../src/api.js'
import { toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

// 一台配了 OAuth 的远程 notion + 一台本地 fs。authed = 令牌是否落库，
// connected = 后端是否已把这台服务器连上（两者之间差着一次自动重启的握手）。
let authed
let connected

function configRows() {
  return {
    servers: [
      { name: 'notion', type: 'http', url: 'https://mcp.notion.com/mcp', auth: 'oauth', headers: {} },
      { name: 'fs', type: 'stdio', command: 'npx', args: ['-y', 'fs-mcp'], env: {} },
    ],
  }
}
function statusRows() {
  return {
    servers: [
      { name: 'notion', url: 'https://mcp.notion.com/mcp', running: connected, tools: connected ? 3 : 0,
        auth: 'oauth', authState: authed ? 'authorized' : 'required',
        authMessage: authed ? '' : '登录态已失效，需要重新授权',
        lastError: connected ? '' : '需要授权：点「授权」在浏览器里登录服务商' },
      { name: 'fs', running: true, tools: 2 },
    ],
  }
}

beforeEach(() => {
  authed = false
  connected = false
  vi.stubGlobal('open', vi.fn(() => ({})))
  api.mockReset().mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/mcp/config') return configRows()
    if (method === 'GET' && url === '/api/mcp') return statusRows()
    if (method === 'POST' && url === '/api/mcp/notion/authorize') {
      return { url: 'https://mcp.notion.com/authorize?state=abc', message: '请在浏览器里完成授权' }
    }
    if (method === 'GET' && url === '/api/mcp/notion/auth') {
      return { state: authed ? 'authorized' : 'authorizing', url: authed ? '' : 'https://mcp.notion.com/authorize?state=abc' }
    }
    if (method === 'POST' && url === '/api/mcp/notion/disconnect') return { message: '已断开 notion 的授权' }
    return { message: '已保存' }
  })
})
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals() })

async function show() {
  // teleport 原地渲染：新增弹窗与下拉弹层才查得到
  const wrapper = mount(McpSection, { global: { stubs: { teleport: true } } })
  await flushPromises()
  return wrapper
}
function cards(wrapper) { return wrapper.findAll('.svc-card') }
function cardOf(wrapper, name) { return cards(wrapper).find(c => c.get('.svc-name').text() === name) }
function buttons(scope) { return scope.findAll('button') }
function buttonIn(scope, text) { return buttons(scope).find(b => b.text() === text) }
function hasButtonIn(scope, text) { return buttons(scope).some(b => b.text().includes(text)) }
// 卡片默认收起：点整行（.svc-disclose）把表单摊开，授权/保存/取消都在里面
async function openCard(wrapper, name) {
  await cardOf(wrapper, name).get('.svc-disclose').trigger('click')
  await flushPromises()
  return cardOf(wrapper, name)
}
// 「更多」菜单：重连与删除收在这儿，卡片收起时也点得到
async function openMore(wrapper, name) {
  await cardOf(wrapper, name).get('.ws-menu-wrap button[aria-haspopup="menu"]').trigger('click')
  await flushPromises()
  return cardOf(wrapper, name)
}
function menuOf(card) { return card.find('.ws-menu') }
function savedPayload() {
  const call = [...api.mock.calls].reverse().find(([m, u]) => m === 'POST' && u === '/api/mcp/config')
  return call?.[2]?.servers
}

describe('设置 → MCP 卡片', () => {
  it('头部是「整行展开 + 更多」，状态与摘要说清是哪一台', async () => {
    const wrapper = await show()
    const head = cardOf(wrapper, 'notion').get('.svc-head')
    // 展开开关就是这一行本身，右侧只剩「更多」一个按钮；保存/取消/重连/删除都不在头部
    expect(head.findAll('.svc-disclose')).toHaveLength(1)
    expect(head.get('.svc-disclose').attributes('aria-expanded')).toBe('false')
    expect(buttons(head).map(b => b.attributes('aria-haspopup'))).toEqual([undefined, 'menu'])
    expect(hasButtonIn(head, '保存')).toBe(false)
    expect(hasButtonIn(head, '删除')).toBe(false)
    expect(head.get('.svc-desc').text()).toBe('远程 URL · https://mcp.notion.com/mcp')
    expect(cardOf(wrapper, 'fs').get('.svc-desc').text()).toBe('本地命令 · npx -y fs-mcp')
    // fs 在线、notion 没连上：徽章各自说清
    expect(cardOf(wrapper, 'fs').findAll('.svc-badge')[0].text()).toBe('在线 · 2 工具')
    expect(cardOf(wrapper, 'notion').findAll('.svc-badge')[0].text()).toBe('离线')
  })

  it('点整行展开字段，再点一次收回', async () => {
    const wrapper = await show()
    const card = await openCard(wrapper, 'fs')
    expect(card.get('.svc-disclose').attributes('aria-expanded')).toBe('true')
    expect(card.get('.svc-body').attributes('style') || '').not.toContain('display: none')
    await card.get('.svc-disclose').trigger('click')
    await flushPromises()
    expect(cardOf(wrapper, 'fs').get('.svc-disclose').attributes('aria-expanded')).toBe('false')
    expect(cardOf(wrapper, 'fs').get('.svc-body').attributes('style')).toContain('display: none')
  })

  it('展开区底部只有「取消 / 保存」，取消在左、保存在右', async () => {
    const wrapper = await show()
    const acts = (await openCard(wrapper, 'fs')).get('.svc-acts')
    expect(buttons(acts).map(b => b.text())).toEqual(['取消', '保存'])
    // 靠右：整组前面挂着一条撑开的 grow
    expect(acts.element.firstElementChild.className).toContain('grow')
  })

  it('「更多」里给重连与删除，点完即收起菜单', async () => {
    const wrapper = await show()
    const card = await openMore(wrapper, 'fs')
    expect(buttons(menuOf(card)).map(b => b.text())).toEqual(['重连', '删除'])
    await buttonIn(menuOf(card), '重连').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/mcp/fs/reconnect', {})
    expect(menuOf(cardOf(wrapper, 'fs')).exists()).toBe(false)
  })

  it('菜单一次只开一个，点外面与 Esc 都只收菜单', async () => {
    const wrapper = await show()
    await openMore(wrapper, 'notion')
    await openMore(wrapper, 'fs')
    expect(wrapper.findAll('.ws-menu')).toHaveLength(1)
    expect(menuOf(cardOf(wrapper, 'notion')).exists()).toBe(false)
    // 点外面
    document.dispatchEvent(new window.Event('pointerdown'))
    await flushPromises()
    expect(wrapper.findAll('.ws-menu')).toHaveLength(0)
    // 再开一个按 Esc：菜单收起，且不把事件冒给设置弹窗那层（否则整页设置被关掉）
    await openMore(wrapper, 'fs')
    const esc = new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true })
    const stopped = vi.spyOn(esc, 'stopImmediatePropagation')
    document.dispatchEvent(esc)
    await flushPromises()
    expect(wrapper.findAll('.ws-menu')).toHaveLength(0)
    expect(stopped).toHaveBeenCalled()
  })

  it('没起名字的服务器：重连点不动，删除照样能点', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/mcp/config') {
        return { servers: [{ name: '', type: 'stdio', command: 'npx', args: [], env: {}, url: '', headers: {}, auth: '' }] }
      }
      if (method === 'GET' && url === '/api/mcp') return { servers: [] }
      return { message: '已保存' }
    })
    const wrapper = await show()
    const card = await openMore(wrapper, '未命名服务器')
    expect(buttonIn(menuOf(card), '重连').element.disabled).toBe(true)
    expect(buttonIn(menuOf(card), '删除').element.disabled).toBe(false)
  })

  it('授权入口在展开的表单里，不在卡片头部', async () => {
    const wrapper = await show()
    // 头部整行的文字里本来就带「待授权」徽章，所以按按钮原文比对，别用 includes
    expect(buttons(cardOf(wrapper, 'notion').get('.svc-head')).map(b => b.text().trim())).not.toContain('授权')
    const card = await openCard(wrapper, 'notion')
    expect(buttonIn(card, '授权').exists()).toBe(true)
    expect(card.text()).toContain('待授权')
    expect(card.text()).toContain('登录态已失效，需要重新授权')
  })

  it('已经授权过的不再摆「授权」，给的是「重新授权」与「断开授权」', async () => {
    authed = true
    connected = true
    const wrapper = await show()
    const card = await openCard(wrapper, 'notion')
    expect(buttons(card).map(b => b.text())).not.toContain('授权')  // 只剩「重新授权」
    expect(buttonIn(card, '重新授权').exists()).toBe(true)
    expect(buttonIn(card, '断开授权').exists()).toBe(true)
    expect(card.text()).toContain('已授权')
  })

  it('逐卡保存只提交这一张：另一张没保存的草稿不会被裹进去', async () => {
    const wrapper = await show()
    const a = await openCard(wrapper, 'notion')
    await a.get('[aria-label="服务器名称"]').setValue('notion-改过')
    const b = await openCard(wrapper, 'fs')
    await b.get('[aria-label="服务器名称"]').setValue('fs-没保存')
    api.mockClear()
    await buttonIn(cardOf(wrapper, 'notion-改过'), '保存').trigger('click')
    await flushPromises()
    expect(savedPayload().map(r => r.name)).toEqual(['notion-改过', 'fs'])
    // 保存完这张卡自动收起；另一张仍摊着、草稿还在
    const b2 = cardOf(wrapper, 'fs-没保存')
    expect(b2.get('.svc-body').attributes('style') || '').not.toContain('display: none')
    expect(b2.get('[aria-label="服务器名称"]').element.value).toBe('fs-没保存')
  })

  it('「取消」回到上次保存的值并收起，不写后端', async () => {
    const wrapper = await show()
    const card = await openCard(wrapper, 'fs')
    await card.get('[aria-label="服务器名称"]').setValue('改到一半')
    api.mockClear()
    await buttonIn(cardOf(wrapper, '改到一半'), '取消').trigger('click')
    await flushPromises()
    expect(api.mock.calls.filter(([m]) => m === 'POST')).toEqual([])
    expect(cardOf(wrapper, 'fs').get('[aria-label="服务器名称"]').element.value).toBe('fs')
    expect(cardOf(wrapper, 'fs').get('.svc-body').attributes('style')).toContain('display: none')
  })

  it('删除即时提交剩下的配置（入口在「更多」里，不用先展开表单）', async () => {
    const wrapper = await show()
    const card = await openMore(wrapper, 'fs')
    await buttonIn(menuOf(card), '删除').trigger('click')
    await flushPromises()
    expect(savedPayload().map(r => r.name)).toEqual(['notion'])
    expect(cards(wrapper)).toHaveLength(1)
    expect(wrapper.findAll('.ws-menu')).toHaveLength(0)   // 行号已经变了，菜单不能留着
  })

  it('缺名字或缺目标时保存被挡下，说清下一步', async () => {
    const wrapper = await show()
    const card = await openCard(wrapper, 'fs')
    await card.get('[aria-label="服务器名称"]').setValue('')
    api.mockClear()
    await buttonIn(cardOf(wrapper, '未命名服务器'), '保存').trigger('click')
    await flushPromises()
    expect(api.mock.calls.filter(([m]) => m === 'POST')).toEqual([])
    expect(wrapper.get('.msg').text()).toBe('先给这台服务器起个名字')
  })
})

describe('设置 → MCP 新增弹窗', () => {
  it('「＋ 添加服务器」排在卡片列表之前（右上角工具栏），不是列表底部', async () => {
    const wrapper = await show()
    // findAll 在多根 fragment 上不保证文档顺序，按真实 DOM 的先后断言
    const bar = wrapper.get('.mcp-bar').element
    const kids = [...bar.parentElement.children]
    expect(kids.indexOf(bar)).toBe(0)
    expect(kids.indexOf(bar)).toBeLessThan(kids.findIndex(k => k.classList.contains('svc-list')))
    expect(bar.textContent.trim()).toBe('＋ 添加服务器')
  })

  it('一台都没配时按钮仍是第一个节点，空态文案指向上方', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/mcp/config') return { servers: [] }
      if (method === 'GET' && url === '/api/mcp') return { servers: [] }
      return { message: '已保存' }
    })
    const wrapper = await show()
    expect(wrapper.findAll('.svc-card')).toHaveLength(0)
    const bar = wrapper.get('.mcp-bar').element
    expect([...bar.parentElement.children].indexOf(bar)).toBe(0)
    expect(wrapper.text()).toContain('点右上角「添加服务器」开始')
  })

  it('「＋ 添加服务器」弹表单，保存才写后端并多出一张卡', async () => {
    const wrapper = await show()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    await buttonIn(wrapper, '＋ 添加服务器').trigger('click')
    await flushPromises()
    const dlg = wrapper.get('[role="dialog"]')
    expect(dlg.get('.dlg-title').text()).toBe('添加 MCP 服务器')
    api.mockClear()
    await dlg.get('[aria-label="服务器名称"]').setValue('github')
    const target = dlg.findAll('.mr-field input')[1]   // 名称下面那条：本地命令 / 远程地址
    await target.setValue('npx')
    await flushPromises()
    await buttonIn(dlg, '保存').trigger('click')
    await flushPromises()
    expect(savedPayload().map(r => r.name)).toEqual(['notion', 'fs', 'github'])
    expect(cards(wrapper)).toHaveLength(3)
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })

  it('取消或按 Esc 都算放弃，不发写请求', async () => {
    const wrapper = await show()
    await buttonIn(wrapper, '＋ 添加服务器').trigger('click')
    await flushPromises()
    const dlg = wrapper.get('[role="dialog"]')
    await dlg.get('[aria-label="服务器名称"]').setValue('半成品')
    api.mockClear()
    await buttonIn(dlg, '取消').trigger('click')
    await flushPromises()
    expect(api.mock.calls.filter(([m]) => m === 'POST')).toEqual([])
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(cards(wrapper)).toHaveLength(2)
  })

  it('重名当场挡下并说明，不把错误丢给后端', async () => {
    const wrapper = await show()
    await buttonIn(wrapper, '＋ 添加服务器').trigger('click')
    await flushPromises()
    const dlg = wrapper.get('[role="dialog"]')
    await dlg.get('[aria-label="服务器名称"]').setValue('fs')
    await dlg.findAll('.mr-field input')[1].setValue('npx')
    api.mockClear()
    await buttonIn(dlg, '保存').trigger('click')
    await flushPromises()
    expect(api.mock.calls.filter(([m]) => m === 'POST')).toEqual([])
    // 状态一变，Teleport 原地渲染的那棵子树可能换成新节点：断言前重新查一次
    expect(wrapper.get('[role="dialog"] [role="alert"]').text()).toContain('已经有叫「fs」的服务器')
  })
})

describe('设置 → MCP 授权', () => {
  it('点授权打开服务商页面并轮询状态', async () => {
    vi.useFakeTimers()
    const wrapper = await show()
    const card = await openCard(wrapper, 'notion')
    await buttonIn(card, '授权').trigger('click')
    await flushPromises()
    expect(window.open).toHaveBeenCalledWith('https://mcp.notion.com/authorize?state=abc', '_blank', 'noopener')
    // 还在等回调：按钮不可重复点
    await vi.advanceTimersByTimeAsync(1500)
    await flushPromises()
    expect(buttonIn(cardOf(wrapper, 'notion'), '等待授权…')?.attributes('disabled')).toBeDefined()
  })

  it('桌面外壳里点授权走外壳命令打开系统浏览器，不用 window.open', async () => {
    vi.useFakeTimers()
    const invoke = vi.fn(async () => ({}))
    vi.stubGlobal('__TAURI__', { core: { invoke } })
    const wrapper = await show()
    const card = await openCard(wrapper, 'notion')
    await buttonIn(card, '授权').trigger('click')
    await flushPromises()
    expect(invoke).toHaveBeenCalledWith('open_url', { url: 'https://mcp.notion.com/authorize?state=abc' })
    expect(window.open).not.toHaveBeenCalled()
    expect(hasButtonIn(cardOf(wrapper, 'notion'), '复制授权链接')).toBe(true)
  })

  it('浏览器没弹出时露出「复制授权链接」，不给「允许弹窗」这种误导提示', async () => {
    vi.useFakeTimers()
    window.open.mockReturnValue(null)
    const wrapper = await show()
    const card = await openCard(wrapper, 'notion')
    await buttonIn(card, '授权').trigger('click')
    await flushPromises()
    expect(hasButtonIn(cardOf(wrapper, 'notion'), '复制授权链接')).toBe(true)
    expect(wrapper.text()).not.toContain('请允许弹窗')
  })

  // 这条守的是「授权完不用再去点重连」：令牌落库和后端握手完成之间有一截时间，
  // 一看到 authorized 就收尾刷新，界面会停在「已授权 + 离线」，用户只能再点一次。
  it('令牌到手后继续等后端连上才收尾提示「已连接」', async () => {
    vi.useFakeTimers()
    const wrapper = await show()
    const card = await openCard(wrapper, 'notion')
    await buttonIn(card, '授权').trigger('click')
    await flushPromises()
    toast.mockClear()

    authed = true                       // 第二轮：令牌已落库，但后端还在重连
    await vi.advanceTimersByTimeAsync(1500)
    await flushPromises()
    expect(toast).not.toHaveBeenCalled()

    connected = true                    // 第三轮：握手完成，状态里 running 为真
    await vi.advanceTimersByTimeAsync(1500)
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('已连接 notion')
    expect(cardOf(wrapper, 'notion').findAll('.svc-badge')[0].text()).toBe('在线 · 3 工具')
    // 收尾后不再轮询
    const before = api.mock.calls.filter(([m, u]) => m === 'GET' && u === '/api/mcp/notion/auth').length
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.mock.calls.filter(([m, u]) => m === 'GET' && u === '/api/mcp/notion/auth').length).toBe(before)
  })

  it('等满预算还没连上时如实说「连接没起来」，不谎报已连接', async () => {
    vi.useFakeTimers()
    const wrapper = await show()
    const card = await openCard(wrapper, 'notion')
    await buttonIn(card, '授权').trigger('click')
    await flushPromises()
    toast.mockClear()
    authed = true
    await vi.advanceTimersByTimeAsync(1500 * 20)
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('已授权，但连接还没起来——右上角「更多」里点「重连」', true)
  })

  it('静态请求头服务器不出现授权入口，切到浏览器授权才写 oauth', async () => {
    const wrapper = await show()
    const card = await openCard(wrapper, 'fs')
    expect(hasButtonIn(card, '授权')).toBe(false)
    // 接入方式切到远程 URL，才会出现鉴权方式；再切「浏览器授权」
    const type = card.findAllComponents(SelectBox).find(s => s.props('ariaLabel') === '接入方式')
    type.vm.$emit('update:modelValue', 'http')
    await flushPromises()
    const auth = cardOf(wrapper, 'fs').findAllComponents(SelectBox).find(s => s.props('ariaLabel') === '鉴权方式')
    auth.vm.$emit('update:modelValue', 'oauth')
    await flushPromises()
    await buttonIn(cardOf(wrapper, 'fs'), '保存').trigger('click')
    await flushPromises()
    expect(savedPayload()[1]).toMatchObject({ name: 'fs', type: 'http', auth: 'oauth' })
  })

  it('断开授权调用后端并刷新状态', async () => {
    authed = true
    connected = true
    const wrapper = await show()
    const card = await openCard(wrapper, 'notion')
    await buttonIn(card, '断开授权').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/mcp/notion/disconnect', {})
  })
})
