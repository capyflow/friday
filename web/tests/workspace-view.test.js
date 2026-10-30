import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import WorkspaceView from '../src/components/WorkspaceView.vue'
import { api } from '../src/api.js'
import { store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
// WorkspaceView 静态引入 anim.js，模块加载即调 window.matchMedia（jsdom 未实现）。
vi.mock('../src/anim.js', () => ({ popIn: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

// 每次装数据都用副本：改名的用例会把 thread.title 改掉，共用同一个对象会串到后面的用例
const copy = v => JSON.parse(JSON.stringify(v))
const wsA = { id: 'ws1', name: 'proj-a', dirs: ['/home/u/proj', '/home/u/shared'] }
const thread = {
  id: 'th1', workspaceId: 'ws1', agent: 'codex', cwd: '/home/u/proj', title: '第一件事',
  state: 'idle', degraded: false, updatedAt: '2026-10-17T10:01:00Z',
  messages: [
    { role: 'user', text: '帮我看下目录', at: '2026-10-17T10:00:00Z' },
    { role: 'note', text: '上下文已重置（历史记录仍可见）' },
    { role: 'assistant', text: '看完了。', in: 10, out: 5, ms: 1200, stopped: 'end_turn' },
  ],
}
// 同工作区里的第二个会话（"一轮进行中可以切换会话"的用例靠它）
const second = {
  id: 'th2', workspaceId: 'ws1', agent: 'codex', cwd: '/home/u/proj', title: '第二件事',
  state: 'idle', degraded: false, updatedAt: '2026-10-18T09:00:00Z',
  messages: [{ role: 'user', text: '另一个问题' }, { role: 'assistant', text: '另一个回答' }],
}
const config = {
  enabled: true, maxConcurrent: 2,
  agents: [{ name: 'codex', command: 'npx', enabled: true, workspace: '/home/u/proj' },
    { name: 'opencode', command: 'opencode', enabled: true }],
}
// session 视图：后端 GET thread/options 都回带；known=false 表示还没握手过
const sessionOf = (cwd, dirs, supported) => ({
  cwd, dirs, extra: supported ? dirs.filter(d => d !== cwd) : [],
  known: supported !== null, additionalDirectories: !!supported,
})

let wrapper
let fetchMock
let data // 可变的服务端替身：用例直接改它，load() 就拿到新状态
let dirPages
let filesByDir
let changesByTurn

beforeEach(() => {
  // 视图选择与"已读"时刻是本地偏好（localStorage）：用例之间必须清干净，
  // 否则上一条把视图切到「最近更新」，后面几条就都在摊平列表里找分组行了
  try { localStorage.clear() } catch { /* 环境没给 localStorage 也不拦用例 */ }
  data = {
    workspaces: [copy(wsA)],
    threads: [copy(thread)],
    acp: config,
    caps: {},   // thread id -> 能力清单
    session: {}, // thread id -> 目录视图
  }
  dirPages = {}
  filesByDir = { '': [] }
  changesByTurn = {}
  api.mockReset().mockImplementation(server)
  toast.mockClear()
  fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('URL', { createObjectURL: vi.fn(() => 'blob:x'), revokeObjectURL: vi.fn() })
})
afterEach(() => { wrapper?.unmount(); wrapper = null; vi.unstubAllGlobals() })

// server 是 acp 接口的最小替身：形状照 internal/admin/acp.go 的回包，写操作直接改
// data（于是 load() 之后界面就是新状态）。回包一律深拷贝——真实 HTTP 每次都是新的
// JSON，替身若把 data 里的数组原样交出去，组件会把同一份引用当成"没变"而不重算。
const json = v => JSON.parse(JSON.stringify(v === undefined ? null : v))
async function server(method, url, body) {
  if (method === 'GET' && url === '/api/acp/threads') return json({ ...data })
  const threadMatch = url.match(/^\/api\/acp\/threads\/([^/]+)$/)
  if (method === 'GET' && threadMatch) {
    const t = data.threads.find(x => x.id === threadMatch[1])
    if (!t) throw new Error('未找到该对话')
    return json({ thread: t, session: data.session[t.id] })
  }
  const optMatch = url.match(/^\/api\/acp\/threads\/([^/]+)\/options$/)
  if (method === 'GET' && optMatch) {
    return json({ capabilities: data.caps[optMatch[1]] || [], session: data.session[optMatch[1]] })
  }
  if (method === 'GET' && url.startsWith('/api/acp/dirs')) {
    const path = decodeURIComponent(url.split('?path=')[1] || '')
    if (path.includes('nope')) throw new Error('目录不存在或不可读')
    return json(dirPages[path] || { path, parent: '/home/u', dirs: [{ name: 'proj', path: '/home/u/proj' }] })
  }
  const filesMatch = url.match(/^\/api\/acp\/threads\/([^/]+)\/files\?path=(.*)$/)
  if (method === 'GET' && filesMatch) {
    const rel = decodeURIComponent(filesMatch[2] || '')
    return json({ path: '/home/u/proj/' + rel, entries: filesByDir[rel] || [], truncated: false })
  }
  const changesMatch = url.match(/^\/api\/acp\/threads\/([^/]+)\/changes\?turn=(.*)$/)
  if (method === 'GET' && changesMatch) {
    const turn = decodeURIComponent(changesMatch[2])
    if (!changesByTurn[turn]) throw new Error('这一轮的改动快照已清理或不存在')
    return json({ changes: changesByTurn[turn] })
  }
  if (method === 'POST' && /^\/api\/acp\/threads\/[^/]+\/changes\/revert$/.test(url)) {
    const turn = body.turn
    if (!changesByTurn[turn]) throw new Error('这一轮的改动快照已清理或不存在')
    changesByTurn[turn].revertedAt = '2026-10-30T12:00:00Z'
    return json({ changes: changesByTurn[turn], message: '已还原本轮改动' })
  }
  if (method === 'GET' && url.startsWith('/api/files/preview')) {
    const path = decodeURIComponent(url.split('?path=')[1] || '')
    return json({ kind: 'text', name: path.split('/').pop(), path, size: 4,
      content: 'new\n', dataUrl: '', truncated: false, message: '' })
  }
  if (method === 'POST' && url === '/api/acp/workspaces') {
    const id = 'ws' + (data.workspaces.length + 1)
    const dirs = body.dirs.length ? body.dirs : ['/home/u/data/acp-sessions/' + id]
    const ws = { id, name: body.name || dirs[0].split('/').pop(), dirs }
    data.workspaces.push(ws)
    return json({ workspace: ws })
  }
  const wsMatch = url.match(/^\/api\/acp\/workspaces\/([^/]+)$/)
  if (method === 'POST' && wsMatch) {
    const ws = data.workspaces.find(w => w.id === wsMatch[1])
    if (!ws) throw new Error('未找到该工作区')
    if ('pinned' in body) ws.pinned = !!body.pinned
    if ('primaryPath' in body) {
      if (!ws.dirs.includes(body.primaryPath)) throw new Error('目录 ' + body.primaryPath + ' 不在该工作区里')
      ws.dirs = [body.primaryPath, ...ws.dirs.filter(d => d !== body.primaryPath)]
    }
    if ('name' in body) ws.name = body.name || ws.dirs[0].split('/').pop()
    if ('policy' in body) ws.policy = body.policy
    return json({ workspace: ws })
  }
  const wsDirAdd = url.match(/^\/api\/acp\/workspaces\/([^/]+)\/dirs$/)
  if (method === 'POST' && wsDirAdd) {
    const ws = data.workspaces.find(w => w.id === wsDirAdd[1])
    if (!ws.dirs.includes(body.path)) ws.dirs.push(body.path)
    return json({ workspace: ws })
  }
  if (method === 'DELETE' && wsDirAddRe(url)) {
    const { id, path } = wsDirAddRe(url)
    const ws = data.workspaces.find(w => w.id === id)
    if (ws.dirs.length <= 1) throw new Error('工作区至少要留一个目录（会话的主目录从这些目录里选）')
    ws.dirs = ws.dirs.filter(d => d !== path)
    return json({ workspace: ws })
  }
  if (method === 'DELETE' && wsMatch) {
    const ids = data.threads.filter(t => t.workspaceId === wsMatch[1]).map(t => t.id)
    data.threads = data.threads.filter(t => t.workspaceId !== wsMatch[1])
    data.workspaces = data.workspaces.filter(w => w.id !== wsMatch[1])
    return json({ message: '已删除该工作区及其会话', threadIds: ids })
  }
  if (method === 'POST' && /^\/api\/acp\/threads\/[^/]+$/.test(url)) {
    const id = url.split('/').pop()
    const t = data.threads.find(x => x.id === id)
    if (!t) throw new Error('未找到该会话')
    if (typeof body.title === 'string') t.title = body.title.trim().replace(/\s+/g, ' ')
    return json({ thread: t })
  }
  if (method === 'POST' && url === '/api/acp/threads') {
    const ws = data.workspaces.find(w => w.id === body.workspaceId)
    const t = {
      id: 'th' + (data.threads.length + 1), workspaceId: ws.id, agent: body.agent,
      cwd: body.cwd || ws.dirs[0], title: body.title, state: 'idle', messages: [],
    }
    data.threads.push(t)
    return json({ thread: t })
  }
  if (method === 'DELETE' && /^\/api\/acp\/threads\//.test(url)) {
    const id = url.split('/').pop()
    data.threads = data.threads.filter(t => t.id !== id)
    return json({ message: '已删除该对话' })
  }
  return json({ message: '已完成' })
}
function wsDirAddRe(url) {
  const m = url.match(/^\/api\/acp\/workspaces\/([^/]+)\/dirs\?path=(.*)$/)
  return m ? { id: m[1], path: decodeURIComponent(m[2]) } : null
}

async function show() {
  // teleport stub 成原地渲染：目录选择器弹窗内容才能被 wrapper 查到
  wrapper = mount(WorkspaceView, { global: { stubs: { teleport: true } } })
  await flushPromises()
}

function timeline() { return wrapper.find('.ag-scroller').text() }
function headMeta() { return wrapper.get('.ag-head-txt .meta').text() }
async function pickSession(title) {
  const item = wrapper.findAll('.ag-item').find(b => b.text().includes(title))
  await item.trigger('click')
  await flushPromises()
}
async function openMenu(name) {
  await wrapper.get(`[aria-label="${name} 的更多操作"]`).trigger('click')
  await flushPromises()
  return wrapper.find('.ws-menu')
}
async function openDialog(name) {
  const menu = await openMenu(name)
  await menu.get(`[aria-label="编辑工作区 ${name}"]`).trigger('click')
  await flushPromises()
  return wrapper.get('[role="dialog"]')
}
async function openThreadMenu(title) {
  await wrapper.get(`[aria-label="${title} 的更多操作"]`).trigger('click')
  await flushPromises()
  return wrapper.find('.ag-menu-wrap .ws-menu')
}
async function newSession(name) {
  await wrapper.get(`[aria-label="在 ${name} 新建会话"]`).trigger('click')
  await flushPromises()
}
function headOf(name) {
  return wrapper.findAll('.ws-head').find(h => h.text().includes(name))
}
async function pickWs(name) {
  const head = wrapper.findAll('.ws-head').find(b => b.text().includes(name))
  await head.trigger('click')
  await flushPromises()
}
async function pushText(text) {
  await wrapper.get('[aria-label="智能体输入"]').setValue(text)
  await wrapper.get('[aria-label="发送"]').trigger('click')
  await flushPromises()
}

// holdStream 开一条不主动收尾的 SSE：测试自己 push/关闭，用来观察"一轮进行中"的界面。
function holdStream() {
  let controller
  const enc = new TextEncoder()
  const response = new Response(new ReadableStream({ start(c) { controller = c } }),
    { headers: { 'Content-Type': 'text/event-stream' } })
  return {
    response,
    push(event) { controller.enqueue(enc.encode(`data: ${JSON.stringify(event)}\n\n`)) },
    close() { controller.close() },
  }
}
// startTurn 在当前会话上发一条消息，返回它握着的流（未收尾）
async function startTurn(text) {
  const held = holdStream()
  fetchMock.mockResolvedValueOnce(held.response)
  await pushText(text)
  return held
}

describe('工作区页：两级栏与投影', () => {
  it('工作区一行只有名字与计数，路径不铺在列表里', async () => {
    await show()
    expect(wrapper.text()).toContain('工作区')
    const head = headOf('proj-a')
    expect(head.text()).toContain('proj-a')
    expect(head.text()).toContain('2 个文件夹 · 1 条会话')
    expect(head.text()).not.toContain('/home/u')          // 组头不铺路径
    expect(wrapper.find('.ws-dirs').exists()).toBe(false) // 源文件夹收进「编辑工作区」弹窗
    // 会话行：agent + 多久之前动过，不带目录名
    const row = wrapper.get('.ag-item')
    expect(row.text()).toContain('第一件事')
    expect(row.text()).toContain('codex ·')
    expect(row.text()).not.toContain('shared')
    expect(timeline()).toContain('帮我看下目录')
    expect(timeline()).toContain('上下文已重置')
    expect(timeline()).toContain('看完了。')
    expect(timeline()).toContain('本轮 · ')
    expect(wrapper.find('[aria-label="复制回答正文"]').exists()).toBe(true)
  })

  it('头部一行说清目录语义：握手过且支持 → 扩展目录已下发', async () => {
    data.session = { th1: sessionOf('/home/u/proj', wsA.dirs, true) }
    await show()
    expect(headMeta()).toContain('proj-a · codex · /home/u/proj')
    expect(headMeta()).toContain('+1 个扩展目录已下发')
  })

  it('agent 不支持 additionalDirectories 时明说其余目录不生效', async () => {
    data.session = { th1: sessionOf('/home/u/proj', wsA.dirs, false) }
    await show()
    expect(headMeta()).toContain('另 1 个目录不生效（本 agent 只认主目录）')
    expect(headMeta()).not.toContain('已下发')
  })

  it('还没握手时不下结论（待确认）', async () => {
    data.session = { th1: sessionOf('/home/u/proj', wsA.dirs, null) }
    await show()
    expect(headMeta()).toContain('生效情况待 agent 握手确认')
  })

  it('只有一个目录时不出现「扩展」字样', async () => {
    data.workspaces = [{ id: 'ws1', name: 'solo', dirs: ['/home/u/only'] }]
    data.session = { th1: sessionOf('/home/u/only', ['/home/u/only'], true) }
    await show()
    expect(headMeta()).not.toContain('扩展目录')
  })

  it('agent 自报上下文时，composer 左组出现只读水位 chip，点开看 used/size', async () => {
    data.threads = [{ ...thread, contextUsed: 12000, contextSize: 30000 }]
    await show()
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

  it('中断/不可用才在标题后标一格，正常态没有任何圆点或标记', async () => {
    data.threads = [{ ...copy(thread), state: 'interrupted' }]
    await show()
    expect(wrapper.text()).toContain('上次会话被中断')
    expect(wrapper.get('.ag-item .ag-state').text()).toBe('已中断')
    wrapper.unmount()
    data.threads = [{ ...copy(thread), state: 'idle' }]
    wrapper = mount(WorkspaceView, { global: { stubs: { teleport: true } } })
    await flushPromises()
    expect(wrapper.find('.ag-item .ag-state').exists()).toBe(false)
    expect(wrapper.find('.ag-dot').exists()).toBe(false)
  })
})

describe('工作区：新建、目录增删、改名、删除', () => {
  it('未开启功能时显示引导并深链设置', async () => {
    data = { workspaces: [], threads: [], acp: { enabled: false, agents: [] } }
    await show()
    expect(wrapper.text()).toContain('智能体功能未开启')
    const go = wrapper.findAll('button').find(b => b.text() === '去设置')
    await go.trigger('click')
    expect(store.settingsSection).toBe('agent')
    expect(store.settingsOpen).toBe(true)
  })

  it('没有工作区时明确说「还没有工作区」，并给新建入口', async () => {
    data = { workspaces: [], threads: [], acp: config }
    await show()
    expect(wrapper.text()).toContain('还没有工作区')
    expect(wrapper.text()).not.toContain('正在加载')
    await wrapper.findAll('button').find(b => b.text() === '新建工作区').trigger('click')
    expect(wrapper.get('[aria-label="工作区名字"]').exists()).toBe(true)
  })

  it('新建工作区：预填 agent 配置的默认目录，提交 POST /api/acp/workspaces 并摊开新建会话表单', async () => {
    await show()
    await wrapper.get('[aria-label="新建工作区"]').trigger('click')
    expect(wrapper.text()).toContain('/home/u/proj') // 预填自 agent 的默认目录
    const name = wrapper.get('[aria-label="工作区名字"]')
    await name.setValue('notes')
    api.mockClear()
    await wrapper.findAll('button').find(b => b.text() === '创建').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/workspaces', { name: 'notes', dirs: ['/home/u/proj'] })
    // 建完直接进下一步：会话表单在新技术区里摊开，agent 默认选中第一个
    expect(wrapper.get('.ws-item.active').text()).toContain('notes')
    expect(wrapper.get('[aria-label="选择智能体"]').element.value).toBe('codex')
  })

  it('新建工作区：目录一个都不给也能创建（后端回退临时工作区目录）', async () => {
    data = { workspaces: [], threads: [], acp: { ...config, agents: [{ name: 'codex', command: 'npx', enabled: true }] }, caps: {}, session: {} }
    await show()
    await wrapper.get('[aria-label="新建工作区"]').trigger('click')
    expect(wrapper.text()).toContain('都不填则用临时工作区目录')
    await wrapper.findAll('button').find(b => b.text() === '创建').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/workspaces', { name: '', dirs: [] })
    expect(toast).toHaveBeenCalledWith('已创建工作区，选个 agent 就能开聊')
    // 回退出来的目录照样能用：会话表单里写明它当主目录（完整路径在 title 里）
    const hint = wrapper.get('.ws-session-hint')
    expect(hint.text()).toContain('ws1')
    expect(hint.attributes('title')).toBe('/home/u/data/acp-sessions/ws1')
  })

  it('目录选择器：逐层浏览、上一级、路径栏直达，选中后追加进新建表单', async () => {
    dirPages[''] = { path: '/home/u', parent: '/home', dirs: [{ name: 'proj', path: '/home/u/proj' }, { name: 'ref', path: '/home/u/ref' }] }
    dirPages['/home/u/ref'] = { path: '/home/u/ref', parent: '/home/u', dirs: [] }
    data = { workspaces: [], threads: [], acp: { ...config, agents: [{ name: 'codex', command: 'npx', enabled: true }] } }
    await show()
    await wrapper.get('[aria-label="新建工作区"]').trigger('click')
    await wrapper.findAll('button').find(b => b.text().includes('添加源文件夹')).trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('选择工作目录')
    // 点进 ref，再上一级回 /home/u，用路径栏直达 /home/u/ref 后确认
    await wrapper.findAll('.ag-dirlist button').find(b => b.text() === 'ref').trigger('click')
    await flushPromises()
    expect(wrapper.get('.ag-dir-cur').text()).toBe('/home/u/ref')
    await wrapper.get('[aria-label="上一级"]').trigger('click')
    await flushPromises()
    const bar = wrapper.get('[aria-label="目录路径"]')
    await bar.setValue('/home/u/ref')
    await bar.trigger('keydown.enter')
    await flushPromises()
    await wrapper.findAll('button').find(b => b.text() === '使用此目录').trigger('click')
    // 追加进表单（不是覆盖）：目录行只露名字，完整路径在 title
    const rows = wrapper.findAll('.ws-dir')
    expect(rows.map(r => r.get('.ws-dir-txt').text())).toEqual(['ref'])
    expect(rows[0].get('.ws-dir-txt').attributes('title')).toBe('/home/u/ref')
    expect(rows[0].find('.ws-tag').exists()).toBe(true)
    await wrapper.findAll('button').find(b => b.text() === '创建').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/workspaces',
      { name: '', dirs: ['/home/u/ref'] })
  })

  it('「编辑工作区」弹窗里改名字与源文件夹，点保存才落后端', async () => {
    dirPages['/home/u/proj'] = { path: '/home/u/proj', parent: '/home/u',
      dirs: [{ name: 'ref', path: '/home/u/proj/ref' }] }
    dirPages['/home/u/proj/ref'] = { path: '/home/u/proj/ref', parent: '/home/u/proj', dirs: [] }
    await show()
    const dlg = await openDialog('proj-a')
    expect(dlg.text()).toContain('编辑工作区')
    // 弹窗里才铺完整路径，主目录带「主」标、其余可「设为主」
    const rows = dlg.findAll('.ws-dir')
    expect(rows.map(r => r.get('.ws-dir-txt').text())).toEqual(['/home/u/proj', '/home/u/shared'])
    expect(rows[0].find('.ws-tag').exists()).toBe(true)
    expect(rows[1].find('.ws-link').text()).toContain('设为主')

    api.mockClear()
    // 改名 + 移掉一个 + 加一个 + 换主目录：全在弹窗里暂存
    await dlg.get('[aria-label="工作区名字"]').setValue('改过的名字')
    await dlg.get('[aria-label="默认审批档位"]').setValue('readonly')
    await rows[1].get('[aria-label="从工作区移出 /home/u/shared"]').trigger('click')
    await dlg.findAll('button').find(b => b.text().includes('添加源文件夹')).trigger('click')
    await flushPromises()
    await wrapper.findAll('.ag-dirlist button').find(b => b.text() === 'ref').trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find(b => b.text() === '使用此目录').trigger('click')
    await flushPromises()
    await wrapper.get('[aria-label="设为主目录 /home/u/proj/ref"]').trigger('click')
    const writes = api.mock.calls.filter(([m]) => m !== 'GET')
    expect(writes).toEqual([]) // 没点保存之前一个写请求都不该发
    await wrapper.get('[aria-label="保存"]').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE',
      '/api/acp/workspaces/ws1/dirs?path=' + encodeURIComponent('/home/u/shared'))
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/workspaces/ws1/dirs', { path: '/home/u/proj/ref' })
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/workspaces/ws1',
      { name: '改过的名字', primaryPath: '/home/u/proj/ref', policy: 'readonly' })
    expect(wrapper.text()).toContain('改过的名字')
    expect(data.workspaces[0].dirs[0]).toBe('/home/u/proj/ref')
    expect(data.workspaces[0].policy).toBe('readonly')
  })

  it('取消弹窗就放弃这次编辑；已经在列表里的文件夹不重复加', async () => {
    await show()
    const dlg = await openDialog('proj-a')
    await dlg.get('[aria-label="工作区名字"]').setValue('白改的名字')
    await dlg.findAll('.ws-dir')[1].get('[aria-label="从工作区移出 /home/u/shared"]').trigger('click')
    api.mockClear()
    await dlg.get('[aria-label="关闭编辑工作区"]').trigger('click')
    await flushPromises()
    expect(api).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('proj-a')
    expect(data.workspaces[0].dirs).toHaveLength(2)

    // 弹窗里选一个已在列表里的文件夹：提示而不是塞出重复项
    await (async () => {
      const d = await openDialog('proj-a')
      await d.findAll('button').find(b => b.text().includes('添加源文件夹')).trigger('click')
      await flushPromises()
      await wrapper.findAll('button').find(b => b.text() === '使用此目录').trigger('click')
      await flushPromises()
    })()
    expect(toast).toHaveBeenCalledWith('这个文件夹已经在工作区里')
    expect(wrapper.findAll('.ws-dir')).toHaveLength(2)
  })

  it('弹窗里清空源文件夹时挡住保存并说清原因（不必等后端报错）', async () => {
    await show()
    await openDialog('proj-a')
    await wrapper.get('[aria-label="从工作区移出 /home/u/proj"]').trigger('click')
    await wrapper.get('[aria-label="从工作区移出 /home/u/shared"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="dialog"]').text()).toContain('至少留一个源文件夹')
    expect(wrapper.get('[aria-label="保存"]').element.disabled).toBe(true)
  })

  it('加完文件夹，会话头部的扩展根数字立刻跟上', async () => {
    data.session = { th1: sessionOf('/home/u/proj', wsA.dirs, true) }
    dirPages['/home/u/proj'] = { path: '/home/u/proj', parent: '/home/u',
      dirs: [{ name: 'ref', path: '/home/u/proj/ref' }] }
    dirPages['/home/u/proj/ref'] = { path: '/home/u/proj/ref', parent: '/home/u/proj', dirs: [] }
    await show()
    expect(headMeta()).toContain('+1 个扩展目录已下发')
    const dlg = await openDialog('proj-a')
    await dlg.findAll('button').find(b => b.text().includes('添加源文件夹')).trigger('click')
    await flushPromises()
    await wrapper.findAll('.ag-dirlist button').find(b => b.text() === 'ref').trigger('click')
    await flushPromises()
    expect(wrapper.get('.ag-dir-cur').text()).toBe('/home/u/proj/ref')
    await wrapper.findAll('button').find(b => b.text() === '使用此目录').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('[role="dialog"] .ws-dir')).toHaveLength(3)
    await wrapper.get('[aria-label="保存"]').trigger('click')
    await flushPromises()
    expect(headMeta()).toContain('+2 个扩展目录已下发')
  })

  it('文件夹被删掉时组头标「缺N」，弹窗里那一行写「已不存在」', async () => {
    data.workspaces[0] = { ...data.workspaces[0], missingDirs: ['/home/u/shared'] }
    await show()
    expect(headOf('proj-a').text()).toContain('缺1')
    expect(wrapper.find('.ws-missing').exists()).toBe(false) // 列表里不铺，所以也不在这里啰嗦
    const dlg = await openDialog('proj-a')
    expect(dlg.get('.ws-dir.missing').text()).toContain('已不存在')
    expect(dlg.get('.ws-dir.missing').text()).toContain('/home/u/shared')
  })

  it('删除工作区要二次确认，并断掉里面在跑的会话流', async () => {
    await show()
    const held = await startTurn('跑着删')
    await openMenu('proj-a')
    await flushPromises()
    expect(wrapper.find('.ws-menu').exists()).toBe(true) // 武装确认后菜单还开着，才点得到第二下
    const signal = fetchMock.mock.calls[0][1].signal
    const del = wrapper.get('[aria-label="删除工作区 proj-a"]')
    expect(del.text()).toContain('删除工作区')
    await del.trigger('click')
    expect(api).not.toHaveBeenCalledWith('DELETE', '/api/acp/workspaces/ws1')
    await wrapper.get('[aria-label="删除工作区 proj-a"]').trigger('click') // 再点一次才真删
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE', '/api/acp/workspaces/ws1')
    expect(signal.aborted).toBe(true)
    expect(wrapper.text()).toContain('还没有工作区')
    held.close()
  })
})

describe('会话：在工作区里创建与删除', () => {
  it('新建会话表单只选 agent：主目录由工作区的第一个源文件夹决定', async () => {
    data.workspaces = [{ id: 'ws1', name: 'solo', dirs: ['/home/u/only'] }]
    data.threads = []
    await show()
    await newSession('solo')
    expect(wrapper.get('.ws-session-hint').text()).toContain('only')
    expect(wrapper.find('[aria-label="选择主目录"]').exists()).toBe(false) // 没有主目录下拉
    expect(wrapper.find('[aria-label="会话标题"]').exists()).toBe(false)   // 也没有标题输入
    api.mockClear()
    await wrapper.findAll('button').find(b => b.text() === '创建会话').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads', { workspaceId: 'ws1', agent: 'codex' })
  })

  it('多源文件夹的工作区也不弹主目录选择：提示写着用第一个，换主目录去编辑弹窗', async () => {
    data.threads = []
    await show()
    await newSession('proj-a')
    expect(wrapper.find('[aria-label="选择主目录"]').exists()).toBe(false)
    expect(wrapper.get('.ws-session-hint').text()).toContain('proj')
    await wrapper.get('[aria-label="选择智能体"]').setValue('opencode')
    api.mockClear()
    await wrapper.findAll('button').find(b => b.text() === '创建会话').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads', { workspaceId: 'ws1', agent: 'opencode' })
    expect(wrapper.get('.ag-head-txt b').text()).toBe('新会话') // 建好即进对话页，标题等首轮
  })

  it('新建会话只留组头一个入口：按钮是开关，列出会话的底部不再有第二个按钮', async () => {
    await show()
    expect(wrapper.find('.ws-body .primary-act').exists()).toBe(false)
    expect(wrapper.findAll('button').filter(b => b.text() === '新建会话')).toHaveLength(0)
    const plus = wrapper.get('[aria-label="在 proj-a 新建会话"]')
    await plus.trigger('click')
    await flushPromises()
    expect(wrapper.get('[aria-label="选择智能体"]').exists()).toBe(true) // 表单摊开
    expect(plus.classes()).toContain('active')
    await plus.trigger('click')                                          // 再点一次收起
    await flushPromises()
    expect(wrapper.find('[aria-label="选择智能体"]').exists()).toBe(false)
  })

  it('没发过消息的会话在列表里叫「新会话」，不是空白也不是（未命名）', async () => {
    data.threads = [{ ...copy(thread), title: '' }]
    await show()
    expect(wrapper.get('.ag-item').text()).toContain('新会话')
    expect(wrapper.get('.ag-head-txt b').text()).toBe('新会话')
  })

  it('停用的 agent 不能选（下拉里标出来且 disabled）', async () => {
    data.acp = { ...config, agents: [{ name: 'codex', command: 'npx', enabled: false }] }
    data.threads = []
    await show()
    await newSession('proj-a')
    const opt = wrapper.get('[aria-label="选择智能体"] option')
    expect(opt.text()).toContain('已停用')
    expect(opt.element.disabled).toBe(true)
  })

  it('双击会话标题就地改名：回车提交，列表与头部一起换；留空回到新会话', async () => {
    await show()
    const title = wrapper.get('.ag-item .ag-title span')
    await title.trigger('dblclick')
    const input = wrapper.get('[aria-label="会话标题"]')
    expect(input.element.value).toBe('第一件事')
    await input.setValue('  换个   名字 ')
    await input.trigger('keydown.enter')
    await flushPromises()
    // 改名之后紧跟一次列表刷新（GET），所以按"改名请求"过滤后再断言，别被最后一次 GET 骗了
    const titlePosts = () => api.mock.calls
      .filter(([m, u]) => m === 'POST' && u === '/api/acp/threads/th1')
      .map(([, , body]) => body.title)
    expect(titlePosts()).toEqual(['换个   名字'])
    expect(wrapper.get('.ag-item').text()).toContain('换个 名字')
    expect(wrapper.get('.ag-head-txt b').text()).toBe('换个 名字')

    // 清空 = 回到自动：行上显示「新会话」，请求里传空串
    await wrapper.get('.ag-item .ag-title span').trigger('dblclick')
    await wrapper.get('[aria-label="会话标题"]').setValue('')
    await wrapper.get('[aria-label="会话标题"]').trigger('keydown.enter')
    await flushPromises()
    expect(titlePosts()).toEqual(['换个   名字', ''])
    expect(wrapper.get('.ag-item').text()).toContain('新会话')
  })

  it('菜单里的「重命名」进同一个行内编辑', async () => {
    await show()
    const menu = await openThreadMenu('第一件事')
    await menu.get('[aria-label="重命名 第一件事"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[aria-label="会话标题"]').element.value).toBe('第一件事')
    expect(wrapper.find('.ag-menu-wrap .ws-menu').exists()).toBe(false) // 进编辑就收起菜单
  })

  it('会话行「…」里删除要二次确认，删掉后从工作区里消失', async () => {
    await show()
    api.mockClear()
    const menu = await openThreadMenu('第一件事')
    await menu.get('[aria-label="删除 第一件事"]').trigger('click')
    expect(menu.text()).toContain('再点一次确认删除')
    expect(api).not.toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')
    await menu.get('[aria-label="删除 第一件事"]').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')
    expect(wrapper.text()).toContain('这个工作区还没有会话')
  })
})

describe('一轮进行中切换会话', () => {
  beforeEach(() => {
    data.threads = [copy(thread), copy(second)]
    store.view = 'agent'
  })
  afterEach(() => { store.view = 'chat' })

  it('切到别的会话不打断进行中的一轮，切回来还是那份实时缓冲', async () => {
    await show()
    const held = await startTurn('先跑着')
    held.push({ delta: '写到一半' })
    await flushPromises()
    expect(timeline()).toContain('写到一半')

    await pickSession('第二件事')
    expect(wrapper.get('.ag-item.active').text()).toContain('第二件事')
    expect(timeline()).toContain('另一个回答')
    expect(wrapper.find('.ag-gen').exists()).toBe(true) // 后台那轮在列表上标出来

    held.push({ delta: '继续写' }) // 后台增量只进它自己的缓冲
    await flushPromises()
    expect(timeline()).not.toContain('继续写')

    await pickSession('第一件事')
    expect(timeline()).toContain('写到一半')
    expect(timeline()).toContain('继续写')
    expect(wrapper.get('[aria-label="停止本轮"]').exists()).toBe(true) // 仍是进行中
    expect(wrapper.find('.ag-gen').exists()).toBe(false) // 正在看的这个不占标记
    held.close()
  })

  it('折叠的工作区也显示里面有几个会话在生成', async () => {
    data.workspaces = [
      { id: 'ws1', name: 'proj-a', dirs: ['/home/u/proj'] },
      { id: 'ws2', name: 'notes', dirs: ['/home/u/notes'] },
    ]
    data.threads = [copy(thread), { ...copy(second), workspaceId: 'ws2', cwd: '/home/u/notes' }]
    await show()
    const held = await startTurn('跑起来') // 当前展开的是 th1 所属的 ws1
    expect(wrapper.get('.ag-item.active').text()).toContain('第一件事')

    await pickWs('notes') // 收起 ws1、展开 notes：折叠那行的徽标顶上来自代
    expect(wrapper.get('.ws-item.active').text()).toContain('notes')
    expect(headOf('proj-a').text()).toContain('1 生成中')
    expect(wrapper.get('.ag-item.active').text()).toContain('第二件事')
    held.close()
  })

  it('回答进行中可以接着输入：这条进队列，本轮跑完才发出去', async () => {
    data.threads = [copy(thread), copy(second)]
    await show()
    const turnA = holdStream()
    const turnB = holdStream()
    const turnC = holdStream()
    fetchMock.mockResolvedValueOnce(turnA.response)
      .mockResolvedValueOnce(turnB.response)
      .mockResolvedValueOnce(turnC.response)
    await pushText('第一件事在跑')
    // 流式进行中：输入框仍可用，主键变成「加入队列」，此时不发第二个请求
    const ta = wrapper.get('[aria-label="智能体输入"]')
    expect(ta.element.disabled).toBe(false)
    await ta.setValue('排队一')
    await wrapper.get('[aria-label="加入队列"]').trigger('click')
    await ta.setValue('排队二')
    await wrapper.get('[aria-label="加入队列"]').trigger('click')
    await flushPromises()
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(wrapper.findAll('.qrow')).toHaveLength(2)
    expect(wrapper.get('.qbar-head').text()).toContain('2 条排队中')
    expect(wrapper.get('.qbar-head').text()).toContain('本轮跑完依次发出')
    // 排队项也进了这条会话的时间线之外的地方：气泡还没出现
    expect(timeline()).not.toContain('排队一')

    turnA.close()                      // 本轮结束 → 队列第一条自动上路
    await flushPromises(); await flushPromises()
    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(JSON.parse(fetchMock.mock.calls[1][1].body).message).toBe('排队一')
    expect(wrapper.findAll('.qrow')).toHaveLength(1)

    turnB.close()                      // 再下一条
    await flushPromises(); await flushPromises()
    expect(fetchMock).toHaveBeenCalledTimes(3)
    expect(JSON.parse(fetchMock.mock.calls[2][1].body).message).toBe('排队二')
    expect(wrapper.find('.qbar').exists()).toBe(false)
    turnC.close()
  })

  it('队列可暂停、提到队首、取回编辑、撤掉与清空', async () => {
    await show()
    const turnA = holdStream()
    fetchMock.mockResolvedValueOnce(turnA.response)
    await pushText('正在跑的')
    for (const t of ['第一条', '第二条', '第三条']) {
      await wrapper.get('[aria-label="智能体输入"]').setValue(t)
      await wrapper.get('[aria-label="加入队列"]').trigger('click')
    }
    await flushPromises()
    expect(wrapper.findAll('.qrow').map(r => r.get('.qtext').text())).toEqual(['第一条', '第二条', '第三条'])

    // 提到队首
    await wrapper.findAll('.qrow')[2].get('[aria-label="把这条提到队首"]').trigger('click')
    expect(wrapper.findAll('.qrow').map(r => r.get('.qtext').text())).toEqual(['第三条', '第一条', '第二条'])
    // 撤掉一条
    await wrapper.findAll('.qrow')[1].get('[aria-label="撤掉这条"]').trigger('click')
    expect(wrapper.findAll('.qrow').map(r => r.get('.qtext').text())).toEqual(['第三条', '第二条'])
    // 取回编辑：文本回到输入框，队列少一条
    await wrapper.findAll('.qrow')[0].get('[aria-label="取回这条编辑"]').trigger('click')
    expect(wrapper.get('[aria-label="智能体输入"]').element.value).toBe('第三条')
    expect(wrapper.findAll('.qrow')).toHaveLength(1)

    // 暂停：本轮跑完不该再发
    await wrapper.get('[aria-label="暂停发送队列"]').trigger('click')
    expect(wrapper.get('.qbar-head').text()).toContain('已暂停')
    turnA.close()
    await flushPromises(); await flushPromises()
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(wrapper.findAll('.qrow')).toHaveLength(1)
    // 继续：这才发出去
    await wrapper.get('[aria-label="继续发送队列"]').trigger('click')
    await flushPromises(); await flushPromises()
    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(JSON.parse(fetchMock.mock.calls[1][1].body).message).toBe('第二条')
  })

  it('清空队列；队列属于各自会话，切走再回来还在', async () => {
    data.threads = [copy(thread), copy(second)]
    await show()
    const turnA = holdStream()
    fetchMock.mockResolvedValueOnce(turnA.response)
    await pushText('在跑的')
    await wrapper.get('[aria-label="智能体输入"]').setValue('属于第一件事的排队')
    await wrapper.get('[aria-label="加入队列"]').trigger('click')
    await flushPromises()
    await pickSession('第二件事')
    expect(wrapper.find('.qbar').exists()).toBe(false)  // 另一条会话看不到这条队列
    await pickSession('第一件事')
    expect(wrapper.get('.qtext').text()).toContain('属于第一件事的排队')
    await wrapper.get('[aria-label="清空队列"]').trigger('click')
    expect(wrapper.find('.qbar').exists()).toBe(false)
    turnA.close()
    await flushPromises()
    expect(fetchMock).toHaveBeenCalledTimes(1) // 清空后不会再发
  })

  it('Esc：焦点在对话区时中断本轮；Esc 先收弹层、不误伤本轮', async () => {
    await show()
    const held = await startTurn('跑着的时候按 Esc')
    api.mockClear()
    // 弹层开着：Esc 先收弹层，不触发停止
    await wrapper.get('[aria-label="添加"]').trigger('click')
    // v-show 用内联 display 表达开关；组件是挂到游离节点上的，别用 isVisible（对游离节点恒 true）
    expect(wrapper.find('#agAddPopover').element.style.display).toBe('')
    document.body.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
    await flushPromises()
    expect(wrapper.find('#agAddPopover').element.style.display).toBe('none')
    expect(api).not.toHaveBeenCalledWith('POST', '/api/acp/threads/th1/stop', {})

    // 焦点在输入框（组字之外的普通 Esc）：中断本轮
    const ta = wrapper.get('[aria-label="智能体输入"]').element
    ta.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/stop', {})
    held.close()
  })

  it('应用内全屏：按钮切换；Esc 空闲退出、忙时只停本轮；在普通布局下 Esc 不做动作', async () => {
    await show()
    const btn = wrapper.get('[aria-label="进入全屏对话"]')
    await btn.trigger('click')
    expect(store.fullscreen).toBe(true)
    expect(wrapper.get('[aria-label="退出全屏对话"]').exists()).toBe(true)

    // 空闲时 Esc：退出应用内全屏
    document.body.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
    await flushPromises()
    expect(store.fullscreen).toBe(false)

    // 普通布局下 Esc 不该有任何动作（不调停止）
    api.mockClear()
    document.body.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
    await flushPromises()
    expect(api).not.toHaveBeenCalledWith('POST', '/api/acp/threads/th1/stop', {})

    // 全屏 + 正在跑：Esc 只停本轮，不退出全屏
    const held = await startTurn('全屏里按 Esc')
    store.fullscreen = true
    await flushPromises()
    const ta = wrapper.get('[aria-label="智能体输入"]').element
    ta.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/stop', {})
    expect(store.fullscreen).toBe(true)
    held.close()
    store.fullscreen = false
  })

  it('切过去后可以再派一个会话的活（两轮各自发到自己的 thread）', async () => {
    data.threads = [copy(thread), copy(second)]
    await show()
    const first = await startTurn('第一件事在跑')
    await pickSession('第二件事')
    const secondStream = holdStream()
    fetchMock.mockResolvedValueOnce(secondStream.response)
    await pushText('第二件事也做起')
    expect(fetchMock.mock.calls.map(([url]) => url))
      .toEqual(['/api/acp/threads/th1/chat', '/api/acp/threads/th2/chat'])
    secondStream.push({ delta: '第二个回答' })
    await flushPromises()
    expect(timeline()).toContain('第二个回答') // 两条流各写各的时间线
    expect(timeline()).not.toContain('第一件事在跑')
    first.close(); secondStream.close()
  })

  it('能力 chips 跟着会话走，别的会话在跑不影响这里的切换', async () => {
    data.threads = [copy(thread), copy(second)]
    data.caps = {
      th1: [{ id: 'model', kind: 'model', name: '模型', currentValue: 'm1', values: [{ value: 'm1', name: 'M1' }] }],
      th2: [{ id: 'model', kind: 'model', name: '模型', currentValue: 'm2', values: [{ value: 'm2', name: 'M2' }] }],
    }
    await show()
    const held = await startTurn('先跑着')
    expect(wrapper.get('.model-btn').text()).toContain('M1')
    expect(wrapper.get('.model-btn').element.disabled).toBe(true) // 自己这一轮在跑：先不能切

    await pickSession('第二件事')
    expect(wrapper.get('.model-btn').text()).toContain('M2') // 清单不串台
    expect(wrapper.get('.model-btn').element.disabled).toBe(false) // 空闲会话照常可切
    api.mockClear()
    await pickSession('第一件事') // 切回正在生成的会话：不再发 options（服务端会卡到轮末）
    expect(api.mock.calls.filter(([, url]) => url.endsWith('/options'))).toHaveLength(0)
    expect(wrapper.get('.model-btn').element.disabled).toBe(true)
    held.close()
  })

  it('输入草稿跟着会话走，不跟到切过去的另一个会话', async () => {
    data.threads = [copy(thread), copy(second)]
    await show()
    await wrapper.get('[aria-label="智能体输入"]').setValue('还没发的那句')
    await pickSession('第二件事')
    expect(wrapper.get('[aria-label="智能体输入"]').element.value).toBe('')
    await pickSession('第一件事')
    expect(wrapper.get('[aria-label="智能体输入"]').element.value).toBe('还没发的那句')
  })

  it('删除正在生成的会话时断开它那条流', async () => {
    await show()
    const held = await startTurn('跑着也要删')
    await flushPromises()
    const signal = fetchMock.mock.calls[0][1].signal
    expect(signal.aborted).toBe(false)
    const menu = await openThreadMenu('第一件事')
    await menu.get('[aria-label="删除 第一件事"]').trigger('click')
    await menu.get('[aria-label="删除 第一件事"]').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')
    expect(signal.aborted).toBe(true)
    held.close()
  })
})

describe('工作区页加载与滚动', () => {
  it('首次加载完成前显示加载态，不把"还没拉到"误报成功能未开启', async () => {
    let finish
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/threads') return new Promise(resolve => { finish = resolve })
      return {}
    })
    wrapper = mount(WorkspaceView, { global: { stubs: { teleport: true } } })
    await flushPromises()
    expect(wrapper.text()).toContain('正在加载工作区…')
    expect(wrapper.text()).not.toContain('智能体功能未开启')
    finish({ ...data })
    await flushPromises()
    expect(wrapper.text()).toContain('第一件事')
  })

  it('首次加载失败给出重试入口，恢复后正常显示', async () => {
    let fail = true
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/threads') {
        if (fail) throw new Error('网络断了')
        return { ...data }
      }
      return server(method, url)
    })
    await show()
    expect(wrapper.text()).toContain('加载失败')
    fail = false
    await wrapper.findAll('button').find(b => b.text() === '重试').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('第一件事')
  })

  it('会话历史拉不到时留在原来的界面，不切成一片空', async () => {
    data.threads = [copy(thread), copy(second)] // 同一个工作区里的两个会话，行都在眼前
    api.mockImplementation(async (method, url, body) => {
      if (method === 'GET' && url === '/api/acp/threads/th2') throw new Error('服务在重启')
      return server(method, url, body)
    })
    await show()
    await pickSession('第二件事')
    expect(toast).toHaveBeenCalledWith('加载对话失败：服务在重启', true)
    expect(wrapper.get('.ag-item.active').text()).toContain('第一件事')
    expect(timeline()).toContain('看完了。')
  })

  it('打开会话默认滚到最底部，不沿用上一个会话的位置', async () => {
    data.threads = [copy(thread), copy(second)]
    store.view = 'agent'
    await show()
    const el = wrapper.get('.ag-scroller').element
    Object.defineProperty(el, 'scrollHeight', { configurable: true, value: 2000 })
    Object.defineProperty(el, 'clientHeight', { configurable: true, value: 500 })
    el.scrollTop = 0 // 上一个会话停在顶部
    await pickSession('第二件事')
    await flushPromises()
    expect(el.scrollTop).toBe(2000)
    store.view = 'chat'
  })
})

describe('工作区栏：置顶、视图切换与源文件夹管理', () => {
  const wsNotes = { id: 'ws2', name: 'notes', dirs: ['/home/u/notes'], updatedAt: '2026-10-18T09:00:00Z' }

  beforeEach(() => {
    data.workspaces = [
      { id: 'ws1', name: 'proj-a', dirs: ['/home/u/proj', '/home/u/shared'], updatedAt: '2026-10-17T10:01:00Z' },
      wsNotes,
    ]
    data.threads = [copy(thread), { ...copy(second), workspaceId: 'ws2', cwd: '/home/u/notes' }]
  })

  it('默认按最近活动排，置顶的工作区永远排最前', async () => {
    await show()
    // notes 里那条会话更新时间更晚 → 默认在它后面
    expect(headOf('notes').text()).toContain('1 个文件夹')
    expect(wrapper.findAll('.ws-head')[0].text()).toContain('notes') // 活动更晚的那组在前
    api.mockClear()
    await (await openMenu('proj-a')).get('[aria-label="置顶 proj-a"]').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/workspaces/ws1', { pinned: true })
    expect(wrapper.findAll('.ws-head')[0].text()).toContain('proj-a')
    expect(wrapper.get('.ws-head .ws-pin-mark').text()).toBe('★')
    // 再点一次取消置顶，顺序回落到按活动时间
    await (await openMenu('proj-a')).get('[aria-label="取消置顶 proj-a"]').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/workspaces/ws1', { pinned: false })
    expect(wrapper.findAll('.ws-head')[0].text()).toContain('notes')
  })

  it('「最近更新」把会话摊平，行上标所属工作区，点一条直接进对话', async () => {
    await show()
    await wrapper.findAll('.ws-seg button')[1].trigger('click')
    expect(wrapper.findAll('.ws-seg button')[1].attributes('aria-pressed')).toBe('true')
    const rows = wrapper.findAll('.ag-item')
    expect(rows).toHaveLength(2)
    // notes 里那条更新更晚 → 排第一，且带工作区名
    expect(rows[0].text()).toContain('第二件事')
    expect(rows[0].text()).toContain('notes · codex')
    expect(rows[0].text()).not.toContain('/home/u')
    await rows[0].trigger('click')
    await flushPromises()
    expect(timeline()).toContain('另一个回答')
    // 点完再切回「按工作区」，展开的应是它所属的那一组
    await wrapper.findAll('.ws-seg button')[0].trigger('click')
    await flushPromises()
    expect(wrapper.get('.ws-item.active').text()).toContain('notes')
  })

  it('视图选择记在本地偏好里，重开页面还是那个视图', async () => {
    await show()
    await wrapper.findAll('.ws-seg button')[1].trigger('click')
    wrapper.unmount()
    wrapper = mount(WorkspaceView, { global: { stubs: { teleport: true } } })
    await flushPromises()
    expect(wrapper.findAll('.ws-seg button')[1].attributes('aria-pressed')).toBe('true')
  })

  it('后台跑完的会话标「未读」，展开读掉就消失', async () => {
    await show()
    await pickWs('notes')          // 展开 notes 并进入那条会话：此刻算"已读"
    await pickSession('第二件事')
    await pickWs('proj-a')         // 回到 proj-a 的会话，notes 折起来
    await pickSession('第一件事')
    expect(wrapper.text()).not.toContain('未读')

    // 服务端那条会话又动了一次（后台跑完一轮）：刷新列表后折叠组头上应有未读点
    data.threads = data.threads.map(t => (t.id === 'th2' ? { ...t, updatedAt: '2026-10-19T08:00:00Z' } : t))
    await (await openMenu('proj-a')).get('[aria-label="置顶 proj-a"]').trigger('click') // 置顶会顺带刷新列表
    await flushPromises()
    expect(headOf('notes').text()).toContain('1 未读')

    await pickWs('notes')          // 点进去读掉：未读与组头计数一起消
    await flushPromises()
    expect(wrapper.text()).not.toContain('未读')
  })

  it('会话多于三条先收着，「显示更多」再露全', async () => {
    const extra = ['th3', 'th4'].map((id, i) => ({
      id, workspaceId: 'ws1', agent: 'codex', cwd: '/home/u/proj', title: '第' + (i + 3) + '件事',
      state: 'idle', updatedAt: '2026-10-1' + (6 - i) + 'T08:00:00Z', messages: [],
    }))
    data.threads = [copy(thread), copy(second), ...extra]
    await show()
    expect(wrapper.findAll('.ag-item')).toHaveLength(3)
    const more = wrapper.findAll('button').find(b => b.text().includes('显示更多'))
    expect(more.text()).toContain('还有 1 条')
    await more.trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.ag-item')).toHaveLength(4)
  })

  it('「收起全部」把组全折起来，只留一行行标题', async () => {
    await show()
    expect(wrapper.find('.ws-body').exists()).toBe(true)
    await wrapper.get('[aria-label="收起全部工作区"]').trigger('click')
    expect(wrapper.find('.ws-body').exists()).toBe(false)
    expect(wrapper.findAll('.ws-head')).toHaveLength(2)
    // 折叠状态下没有选中会话时，右侧给的是"选一个工作区"的指引而不是空白
    expect(headOf('notes').text()).toContain('个文件夹')
  })

  it('工作区文件抽屉：文件树预览、本轮 diff 与还原', async () => {
    data.threads[0].messages[2].changes = {
      turnId: 't1', at: '2026-10-30T11:00:00Z', snapshot: true,
      files: [{ path: 'src/app.js', status: 'modified', additions: 1, deletions: 1 }],
      additions: 1, deletions: 1,
    }
    filesByDir = {
      '': [{ name: 'src', path: 'src', dir: true }, { name: 'README.md', path: 'README.md', dir: false, size: 12 }],
      src: [{ name: 'app.js', path: 'src/app.js', dir: false, size: 20 }],
    }
    changesByTurn.t1 = {
      turnId: 't1', at: '2026-10-30T11:00:00Z', vcs: 'git', snapshot: true,
      files: [{ path: 'src/app.js', status: 'modified', additions: 1, deletions: 1,
        diff: '@@ -1,2 +1,2 @@\n-old\n+new\n same' }],
      additions: 1, deletions: 1,
    }
    await show()

    await wrapper.get('[aria-label="工作区文件与改动"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('.wfd-panel').exists()).toBe(true)
    expect(wrapper.findAll('.wfd-row')).toHaveLength(2)

    await wrapper.findAll('.wfd-row').find(r => r.text().includes('src')).trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.wfd-row')).toHaveLength(3)
    await wrapper.findAll('.wfd-row').find(r => r.text().includes('app.js')).trigger('click')
    await flushPromises()
    expect(wrapper.get('.wfd-code').text()).toContain('new')

    await wrapper.findAll('.wfd-tabs button').find(b => b.text().includes('本轮改动')).trigger('click')
    await flushPromises()
    expect(wrapper.get('.wfd-diff').text()).toContain('old')
    expect(wrapper.find('.wfd-diff-row.add').text()).toContain('new')

    const revert = wrapper.findAll('.wfd-revert button')[0]
    await revert.trigger('click')
    expect(revert.text()).toContain('确认还原 1 个文件')
    await revert.trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/changes/revert', { turn: 't1' })
    expect(wrapper.text()).toContain('本轮已还原')
  })
})
