import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import WorkspaceView from '../src/components/WorkspaceView.vue'
import SelectBox from '../src/components/SelectBox.vue'
import { api } from '../src/api.js'
import { store, toast } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
// WorkspaceView 静态引入 anim.js，模块加载即调 window.matchMedia（jsdom 未实现）。
vi.mock('../src/anim.js', () => ({ popIn: vi.fn() }))
vi.mock('../src/store.js', async importOriginal => ({ ...(await importOriginal()), toast: vi.fn() }))

// 内嵌终端的替身（与 workspace-terminal.test.js 同一套）：默认用例不打开终端面板
// （组件是开过才挂载的），要验「面板跟会话走 / 分隔线拖拽」时才需要它们。
// xterm 在 jsdom 里量不出尺寸，WebSocket 也没有真内核可连。
const termMocks = vi.hoisted(() => {
  const termInstances = []
  class FakeTerminal {
    constructor(options) { Object.assign(this, { options, disposed: false }); termInstances.push(this) }
    loadAddon() {} open() {} write() {} writeln() {} focus() {} dispose() { this.disposed = true }
    onData() {} onResize() {}
  }
  const sockets = []
  class FakeSocket {
    static OPEN = 1
    constructor(url) {
      this.url = url; this.readyState = 0; this.frames = []
      sockets.push(this)
      Promise.resolve().then(() => { this.readyState = FakeSocket.OPEN; this.onopen?.() })
    }
    send(text) { this.frames.push(JSON.parse(text)) }
    close() { this.readyState = 3; this.onclose?.() }
  }
  return { FakeTerminal, termInstances, FakeSocket, sockets }
})
const { termInstances, FakeSocket, sockets } = termMocks
vi.mock('@xterm/xterm', () => ({ Terminal: termMocks.FakeTerminal }))
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { fit() {} } }))

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
  enabled: true,
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
let workspaceMemory
let checkRuns
let checkSuggestions
// 后端在新会话上抄来的"上次这台 agent 的选择"（capChoices）：用例设它来演沿用场景
let inheritChoices
let uploadSeq

beforeEach(() => {
  // 视图选择与"已读"时刻是本地偏好（localStorage）：用例之间必须清干净，
  // 否则上一条把视图切到「最近更新」，后面几条就都在摊平列表里找分组行了
  try { localStorage.clear() } catch { /* 环境没给 localStorage 也不拦用例 */ }
  store.acpOpenThread = ''
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
  workspaceMemory = {}
  inheritChoices = null
  checkRuns = []
  checkSuggestions = [{ label: 'npm test', command: 'npm test', source: 'package.json 的 scripts.test' }]
  uploadSeq = 0
  api.mockReset().mockImplementation(server)
  toast.mockClear()
  termInstances.length = 0
  sockets.length = 0
  fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('URL', { createObjectURL: vi.fn(() => 'blob:x'), revokeObjectURL: vi.fn() })
  vi.stubGlobal('WebSocket', FakeSocket)
  vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
})
afterEach(() => { wrapper?.unmount(); wrapper = null; vi.unstubAllGlobals() })

// server 是 acp 接口的最小替身：形状照 internal/admin/acp.go 的回包，写操作直接改
// data（于是 load() 之后界面就是新状态）。回包一律深拷贝——真实 HTTP 每次都是新的
// JSON，替身若把 data 里的数组原样交出去，组件会把同一份引用当成"没变"而不重算。
const json = v => JSON.parse(JSON.stringify(v === undefined ? null : v))
async function server(method, url, body) {
  if (method === 'GET' && url === '/api/acp/threads') return json({ ...data })
  if (method === 'GET' && url === '/api/terminal') {
    return json({ enabled: true, local: true, active: 0, maxSessions: 2 })
  }
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
  const memList = url.match(/^\/api\/acp\/workspaces\/([^/]+)\/memory$/)
  if (method === 'GET' && memList) {
    return json({ pages: workspaceMemory[memList[1]] || [] })
  }
  if (method === 'POST' && memList) {
    const list = workspaceMemory[memList[1]] || []
    let page = body.id ? list.find(x => x.id === body.id) : list.find(x => x.title === body.title)
    if (page) {
      page.title = body.title
      page.body = body.body
      page.updatedAt = new Date().toISOString()
    } else {
      page = { id: 'm' + (list.length + 1), title: body.title, body: body.body,
        scope: memList[1], updatedAt: new Date().toISOString() }
      list.push(page)
    }
    workspaceMemory[memList[1]] = list
    return json({ page, created: true, message: '已保存' })
  }
  const memPage = url.match(/^\/api\/acp\/workspaces\/([^/]+)\/memory\/([^/]+)$/)
  if (method === 'GET' && memPage) {
    const page = (workspaceMemory[memPage[1]] || []).find(x => x.id === memPage[2])
    if (!page) throw new Error('未找到这页项目记忆')
    return json({ page })
  }
  if (method === 'DELETE' && memPage) {
    workspaceMemory[memPage[1]] = (workspaceMemory[memPage[1]] || []).filter(x => x.id !== memPage[2])
    return json({ message: '已删除' })
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
  const checkList = url.match(/^\/api\/acp\/threads\/([^/]+)\/checks$/)
  if (method === 'GET' && checkList) {
    // 列表不带输出（和后端一致），选中那条走 /checks/:run 取详情
    const runs = checkRuns.map(({ output, ...r }) => ({ ...r, output: '', hasOutput: !!output }))
    return json({ runs, suggestions: checkSuggestions })
  }
  if (method === 'POST' && checkList) {
    if (!String(body.command || '').trim()) throw new Error('请求体须为 {"command": "..."}')
    if (/rm -rf/.test(body.command)) throw new Error('命令含毁灭性片段 "rm -rf"，Friday 不执行')
    if (checkRuns.some(r => r.state === 'awaiting' || r.state === 'running')) {
      throw new Error('这条会话已有验证命令在跑或待批，先停掉它')
    }
    const run = { id: 'c' + (checkRuns.length + 1), command: body.command, cwd: '/home/u/proj',
      state: 'awaiting', exitCode: -1, timeoutSec: body.timeoutSec || 300,
      startedAt: new Date().toISOString() }
    checkRuns.unshift(run)
    return json({ run, message: '已提交，等你在审批卡上批准后执行' })
  }
  const checkOne = url.match(/^\/api\/acp\/threads\/([^/]+)\/checks\/([^/]+)$/)
  if (method === 'GET' && checkOne) {
    const run = checkRuns.find(r => r.id === checkOne[2])
    if (!run) throw new Error('未找到这次验证运行')
    return json({ run })
  }
  if (method === 'POST' && /^\/api\/acp\/threads\/[^/]+\/checks\/[^/]+\/stop$/.test(url)) {
    const run = checkRuns.find(r => r.id === url.split('/checks/')[1].split('/stop')[0])
    if (!run) throw new Error('未找到这次验证运行')
    run.state = 'killed'
    run.note = '已手动停止'
    return json({ message: '已停止这次验证' })
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
  if (method === 'POST' && /^\/api\/acp\/threads\/[^/]+\/handoff$/.test(url)) {
    const old = data.threads.find(t => t.id === url.split('/')[4])
    if (!old) throw new Error('未找到该会话')
    // body {"agent": "..."} 可指定交接目标；留空 = 同 agent。未知/停用一律报错，
    // 形状照 internal/admin/acp.go 的 handleAcpThreadHandoff。
    const target = String(body.agent || old.agent).trim()
    const hit = (data.acp.agents || []).find(a => a.name === target)
    if (!hit || hit.enabled === false) throw new Error(`没有叫 "${target}" 的智能体（可在设置 → 智能体里添加）`)
    const nt = {
      ...old, id: 'th' + (data.threads.length + 1), title: old.title + ' · 续', agent: target,
      state: 'idle', handoff: '交接摘要',
      messages: [{ role: 'note', text: hit.name === old.agent
        ? '已从上一个会话交接上下文'
        : `已从上一个会话交接上下文（上一个由 ${old.agent} 跑，这次交给 ${target}）` }],
    }
    data.threads.push(nt)
    return json({ thread: nt, message: '已新开续会话' })
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
      ...(inheritChoices ? { capChoices: { ...inheritChoices } } : {}),
    }
    data.threads.push(t)
    return json({ thread: t })
  }
  if (method === 'DELETE' && /^\/api\/acp\/threads\//.test(url)) {
    const id = url.split('/').pop()
    data.threads = data.threads.filter(t => t.id !== id)
    return json({ message: '已删除该对话' })
  }
  if (method === 'POST' && url === '/api/upload') {
    uploadSeq += 1
    return json({ id: 'up' + uploadSeq })
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
// 下拉一律是 SelectBox 自绘控件（原生 select 已下线）：按 aria-label 找，别按下标数。
// 一律从根 wrapper 查：aria-label 在页内唯一，而弹窗那层套了 Teleport，
// 从 DOMWrapper（如 openDialog 的返回值）再往下找会拿到另一份节点，点不到活的实例。
function selectBox(label) {
  const box = wrapper.findAllComponents(SelectBox).find(s => s.props('ariaLabel') === label)
  if (!box) throw new Error(`找不到下拉：${label}`)
  return box
}
function selectedText(label) {
  return wrapper.get(`.selbox-trigger[aria-label="${label}"] .selbox-label`).text()
}
async function openOptions(label) {
  await wrapper.get(`.selbox-trigger[aria-label="${label}"]`).trigger('click')
  await flushPromises()
  return wrapper.get('.selbox-pop')
}
// 走真实交互：点触发器展开 → 点选项文案
async function pickOption(label, optionLabel) {
  const opt = (await openOptions(label)).findAll('.selbox-opt').find(o => o.text() === optionLabel)
  if (!opt) throw new Error(`下拉「${label}」里没有选项：${optionLabel}`)
  await opt.trigger('click')
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
    // 只读 + 一个“总结并新开”入口（带交接目标下拉）；没有压缩/清除
    expect(wrapper.get('#agCtxPopover').text()).toContain('总结并新开')
    // 没有压缩/清除这类动作键（协议不支持，只在说明文字里提到）
    const acts = wrapper.findAll('#agCtxPopover button').map(b => b.text())
    expect(acts.filter(t => /压缩|清除/.test(t))).toHaveLength(0)
    expect(wrapper.findAll('#agCtxPopover button')).toHaveLength(2) // 下拉触发键 + 交接键
    expect(wrapper.get('#agCtxPopover .selbox-trigger').text()).toContain('codex')
  })

  it('上下文弹层里总结并新开：建续会话、选中并带交接说明', async () => {
    data.threads = [{ ...thread, contextUsed: 24000, contextSize: 30000 }]
    await show()
    await wrapper.get('[aria-label="上下文使用率"]').trigger('click')
    // 弹层里第一个键现在是交接目标的下拉，按键文案找交接那颗
    await wrapper.findAll('#agCtxPopover button').find(b => b.text() === '总结并新开').trigger('click')
    await flushPromises()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/handoff', { agent: 'codex' })
    expect(wrapper.get('.ag-head-txt b').text()).toContain('续')
    expect(wrapper.text()).toContain('已从上一个会话交接上下文')
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
    expect(selectedText('选择智能体')).toBe('codex')
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
    await pickOption('默认审批档位', '只读')
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

  it('删除工作区先弹确认，确认后连会话一起删并断掉在跑的流', async () => {
    await show()
    const held = await startTurn('跑着删')
    await openMenu('proj-a')
    await flushPromises()
    const signal = fetchMock.mock.calls[0][1].signal
    const del = wrapper.get('[aria-label="删除工作区 proj-a"]')
    expect(del.text()).toContain('删除工作区')
    await del.trigger('click')
    await flushPromises()
    expect(wrapper.find('.ws-menu').exists()).toBe(false) // 菜单让位给确认弹窗
    expect(api).not.toHaveBeenCalledWith('DELETE', '/api/acp/workspaces/ws1')
    const dlg = wrapper.get('[aria-label="删除工作区确认"]')
    expect(dlg.text()).toContain('「proj-a」')
    expect(dlg.text()).toContain('会话记录')
    expect(dlg.text()).toContain('磁盘上的文件夹不动')
    await dlg.get('[aria-label="确认删除"]').trigger('click')
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
    await pickOption('选择智能体', 'opencode')
    api.mockClear()
    await wrapper.findAll('button').find(b => b.text() === '创建会话').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads', { workspaceId: 'ws1', agent: 'opencode' })
    expect(wrapper.get('.ag-head-txt b').text()).toBe('新会话') // 建好即进对话页，标题等首轮
  })

  it('新建会话会说明沿用了上次这台 agent 的哪几项选择', async () => {
    inheritChoices = { model: 'builtin/b', thought_level: 'high' }
    await show()
    await newSession('proj-a')
    await wrapper.findAll('button').find(b => b.text() === '创建会话').trigger('click')
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('已创建会话，沿用上次这台智能体的模型、思考强度')
  })

  it('没有可沿用的选择时提示照旧说"正在启动 agent"', async () => {
    await show()
    await newSession('proj-a')
    await wrapper.findAll('button').find(b => b.text() === '创建会话').trigger('click')
    await flushPromises()
    expect(toast).toHaveBeenCalledWith('已创建会话，正在启动 agent（就绪后即可切模型/思考强度）')
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
    const box = selectBox('选择智能体')
    const opt = (await openOptions('选择智能体')).get('.selbox-opt')
    expect(opt.text()).toContain('已停用')
    expect(opt.element.disabled).toBe(true)
    // 真的点下去也不改变选中值：创建会话仍然是禁用态
    // （VTU 的 trigger 会跳过 disabled 元素，这里手动派发事件才测得到组件里的挡门）
    opt.element.dispatchEvent(new window.MouseEvent('click', { bubbles: true }))
    await flushPromises()
    expect(box.props('modelValue')).toBe('')
    expect(wrapper.get('.ag-form-acts .primary').element.disabled).toBe(true)
  })

  it('双击会话标题弹窗改名：回车提交，列表与头部一起换；留空回到新会话', async () => {
    await show()
    const title = wrapper.get('.ag-item .ag-title span')
    await title.trigger('dblclick')
    const input = wrapper.get('[aria-label="重命名会话"] [aria-label="会话标题"]')
    expect(input.element.value).toBe('第一件事')
    await input.setValue('  换个   名字 ')
    await input.trigger('keydown.enter')
    await flushPromises()
    expect(wrapper.find('[aria-label="重命名会话"]').exists()).toBe(false) // 提交后关窗
    // 改名之后紧跟一次列表刷新（GET），所以按"改名请求"过滤后再断言，别被最后一次 GET 骗了
    const titlePosts = () => api.mock.calls
      .filter(([m, u]) => m === 'POST' && u === '/api/acp/threads/th1')
      .map(([, , body]) => body.title)
    expect(titlePosts()).toEqual(['换个   名字'])
    expect(wrapper.get('.ag-item').text()).toContain('换个 名字')
    expect(wrapper.get('.ag-head-txt b').text()).toBe('换个 名字')

    // 清空 = 回到自动：行上显示「新会话」，请求里传空串
    await wrapper.get('.ag-item .ag-title span').trigger('dblclick')
    const again = wrapper.get('[aria-label="重命名会话"] [aria-label="会话标题"]')
    await again.setValue('')
    await again.trigger('keydown.enter')
    await flushPromises()
    expect(titlePosts()).toEqual(['换个   名字', ''])
    expect(wrapper.get('.ag-item').text()).toContain('新会话')
  })

  it('重命名弹窗按 Esc 关闭：放弃输入，不发请求', async () => {
    await show()
    api.mockClear()
    await wrapper.get('.ag-item .ag-title span').trigger('dblclick')
    await wrapper.get('[aria-label="重命名会话"] [aria-label="会话标题"]').setValue('不要了')
    await wrapper.get('[aria-label="重命名会话"]').trigger('keydown.esc')
    await flushPromises()
    expect(wrapper.find('[aria-label="重命名会话"]').exists()).toBe(false)
    expect(api).not.toHaveBeenCalledWith('POST', '/api/acp/threads/th1', expect.anything())
  })

  it('菜单里的「重命名」打开同一个弹窗', async () => {
    await show()
    const menu = await openThreadMenu('第一件事')
    await menu.get('[aria-label="重命名 第一件事"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[aria-label="重命名会话"] [aria-label="会话标题"]').element.value).toBe('第一件事')
    expect(wrapper.find('.ag-menu-wrap .ws-menu').exists()).toBe(false) // 进编辑就收起菜单
  })

  it('会话行「…」里删除先弹确认：取消不删，确认后才从工作区消失', async () => {
    await show()
    api.mockClear()
    const menu = await openThreadMenu('第一件事')
    await menu.get('[aria-label="删除 第一件事"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('.ag-menu-wrap .ws-menu').exists()).toBe(false) // 菜单让位给确认弹窗
    const dlg = wrapper.get('[aria-label="删除会话确认"]')
    expect(dlg.text()).toContain('「第一件事」')
    expect(dlg.text()).toContain('磁盘上的文件不动')
    expect(api).not.toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')
    await dlg.get('[aria-label="取消删除"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[aria-label="删除会话确认"]').exists()).toBe(false)
    expect(api).not.toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')

    const again = await openThreadMenu('第一件事')
    await again.get('[aria-label="删除 第一件事"]').trigger('click')
    await flushPromises()
    await wrapper.get('[aria-label="删除会话确认"] [aria-label="确认删除"]').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')
    expect(wrapper.text()).toContain('这个工作区还没有会话')
  })

  it('删除弹窗支持回车确认、Esc 取消', async () => {
    await show()
    api.mockClear()
    const menu = await openThreadMenu('第一件事')
    await menu.get('[aria-label="删除 第一件事"]').trigger('click')
    await flushPromises()
    await wrapper.get('[aria-label="删除会话确认"]').trigger('keydown.esc')
    await flushPromises()
    expect(wrapper.find('[aria-label="删除会话确认"]').exists()).toBe(false)
    expect(api).not.toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')

    const again = await openThreadMenu('第一件事')
    await again.get('[aria-label="删除 第一件事"]').trigger('click')
    await flushPromises()
    await wrapper.get('[aria-label="删除会话确认"]').trigger('keydown.enter')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE', '/api/acp/threads/th1')
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
    await flushPromises()
    await wrapper.get('[aria-label="删除会话确认"] [aria-label="确认删除"]').trigger('click')
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

  it('会话多于三条先收着，「显示更多」露全后还能再收起', async () => {
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
    const less = wrapper.findAll('button').find(b => b.text() === '收起会话')
    expect(less.attributes('aria-expanded')).toBe('true')
    await less.trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.ag-item')).toHaveLength(3) // 收回预览态
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

  it('工作区抽屉可读写项目记忆，消息也能一键记到项目', async () => {
    workspaceMemory.ws1 = [{ id: 'm1', title: '项目约定', body: '这个仓库用 pnpm', updatedAt: '2026-10-30T10:00:00Z' }]
    await show()
    await wrapper.get('[aria-label="工具面板"]').trigger('click')
    await flushPromises()
    await wrapper.findAll('.ag-tools-tab').find(b => b.text().includes('项目记忆')).trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('项目约定')
    await wrapper.findAll('.wfd-change-row').find(b => b.text().includes('项目约定')).trigger('click')
    await flushPromises()
    expect(wrapper.get('[aria-label="项目记忆正文"]').element.value).toContain('pnpm')
    await wrapper.get('[aria-label="项目记忆正文"]').setValue('这个仓库用 pnpm；验收跑 npm run check')
    await wrapper.findAll('.wfd-memory-actions button').find(b => b.text() === '保存').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/workspaces/ws1/memory',
      { id: 'm1', title: '项目约定', body: '这个仓库用 pnpm；验收跑 npm run check' })
    expect(wrapper.text()).toContain('已保存')

    await wrapper.get('[aria-label="记到项目记忆"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[aria-label="项目记忆标题"]').element.value).toBe('第一件事')
    expect(wrapper.get('[aria-label="项目记忆正文"]').element.value).toContain('看完了')
  })

  it('运行看板带目标会话时，加载完直接选中它', async () => {
    data.threads = [copy(thread), copy(second)]
    store.acpOpenThread = 'th2'
    await show()
    expect(wrapper.get('.ag-head-txt b').text()).toBe('第二件事')
    expect(store.acpOpenThread).toBe('')
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

    await wrapper.get('[aria-label="工具面板"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('.wfd-panel').exists()).toBe(true)
    expect(wrapper.findAll('.wfd-row')).toHaveLength(2)

    await wrapper.findAll('.wfd-row').find(r => r.text().includes('src')).trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.wfd-row')).toHaveLength(3)
    await wrapper.findAll('.wfd-row').find(r => r.text().includes('app.js')).trigger('click')
    await flushPromises()
    expect(wrapper.get('.wfd-code').text()).toContain('new')

    await wrapper.findAll('.ag-tools-tab').find(b => b.text().includes('改动')).trigger('click')
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

  it('验证标签：本地预览、检查命令提交与运行输出', async () => {
    data.threads[0].messages[2].text = '服务起来了，看 http://localhost:5173 就行'
    await show()
    await wrapper.get('[aria-label="工具面板"]').trigger('click')
    await flushPromises()
    const verifyTab = () => wrapper.findAll('.ag-tools-tab').find(b => b.text().includes('验证'))
    await verifyTab().trigger('click')
    await flushPromises()

    // 预览地址从时间线里认出来
    expect(wrapper.get('.wfd-check-main').text()).toBe('http://localhost:5173')
    // 手填地址只收本机：外站明确拒绝，不悄悄收下
    await wrapper.get('[aria-label="添加本地预览地址"]').setValue('https://example.com')
    await wrapper.findAll('.wfd-check-add button').find(b => b.text() === '添加').trigger('click')
    expect(wrapper.text()).toContain('只能预览本机地址')
    await wrapper.get('[aria-label="添加本地预览地址"]').setValue('http://127.0.0.1:3000')
    await wrapper.findAll('.wfd-check-add button').find(b => b.text() === '添加').trigger('click')
    expect(wrapper.get('.wfd-frame').attributes('src')).toBe('http://127.0.0.1:3000')
    // 内嵌 iframe 不带同源权限：预览页读不到 Friday 的登录态
    expect(wrapper.get('.wfd-frame').attributes('sandbox')).not.toContain('allow-same-origin')

    // 建议命令一键跑，提交后进审批队列
    await wrapper.findAll('.wfd-sugs button')[0].trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/checks',
      { command: 'npm test', timeoutSec: 300 })
    expect(checkRuns[0].state).toBe('awaiting')
    expect(wrapper.text()).toContain('等你在审批卡上批准后执行')
    expect(wrapper.text()).toContain('待批准')
    // 有命令待批时运行按钮收着（避免排队歧义），但可以在详情里停止
    expect(wrapper.findAll('.wfd-check-run button').find(b => b.text() === '运行')
      .attributes('disabled')).toBeDefined()
    expect(wrapper.findAll('.wfd-detail-acts button').find(b => b.text() === '停止').exists()).toBe(true)

    // 审批通过后刷新：状态、退出码与输出尾部（失败行标红）
    checkRuns[0].state = 'failed'
    checkRuns[0].exitCode = 1
    checkRuns[0].note = '退出码 1'
    checkRuns[0].durationMs = 8200
    checkRuns[0].output = 'PASS a.test.js\nFAIL b.test.js\n  期望 1 得到 2'
    await wrapper.findAll('.ag-tools-tab').find(b => b.text().includes('文件')).trigger('click')
    await flushPromises()
    await verifyTab().trigger('click')
    await flushPromises()
    expect(wrapper.get('.wfd-out').text()).toContain('期望 1 得到 2')
    expect(wrapper.findAll('.wfd-out-line.bad')).toHaveLength(1)
    expect(wrapper.text()).toContain('8.2 秒')
    expect(wrapper.text()).toContain('退出码 1')
    // 退出码不重复播报，跑完了「等你批准」的提示也该撤掉
    expect(wrapper.find('.wfd-notice').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('等你在审批卡上批准后执行')

    // 再跑一条并停止
    await wrapper.get('[aria-label="验证命令"]').setValue('npm run build')
    await wrapper.findAll('.wfd-check-run button').find(b => b.text() === '运行').trigger('click')
    await flushPromises()
    expect(checkRuns[0].command).toBe('npm run build')
    await wrapper.findAll('.wfd-change-row').find(b => b.text().includes('npm run build')).trigger('click')
    await flushPromises()
    await wrapper.findAll('.wfd-detail-acts button').find(b => b.text() === '停止').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/checks/c2/stop')
    expect(checkRuns[0].state).toBe('killed')
    expect(wrapper.text()).toContain('已停止')

    // 毁灭性命令：后端拒绝时把原因原样显示出来
    await wrapper.get('[aria-label="验证命令"]').setValue('rm -rf /tmp/x')
    await wrapper.findAll('.wfd-check-run button').find(b => b.text() === '运行').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('毁灭性片段')
    expect(wrapper.text()).not.toContain('undefined')
  })

  it('审批档位退让到别的档时，头部标出实际落点', async () => {
    Object.assign(data.threads[0], {
      policyApplied: 'ask', policyValue: 'workspace-write', policyValueName: 'Workspace Write',
      policyFallback: true,
      policyNote: '该 agent 没有「每次都问」的等价取值，已退到最接近的保守档 Workspace Write（workspace-write）：它不是逐条审批。',
    })
    await show()
    expect(headMeta()).toContain('审批档位实际是 Workspace Write')
    expect(headMeta()).not.toContain('审批档位未下发')
  })

  it('档位压根没下发时仍说「审批档位未下发」', async () => {
    Object.assign(data.threads[0], {
      policyNote: '该 agent 没有上报审批模式，工作区默认档位无法下发；改动仍会记录并可还原。',
    })
    await show()
    expect(headMeta()).toContain('审批档位未下发')
  })
})

// 附件展示：图排在用户气泡上方（同 ZCode），点缩略图看原图。
// 守的是三件事——图不再进气泡、纯图消息不留空气泡、预览层能被 Esc 收掉。
describe('工作区页：附件展示与预览', () => {
  const withUserMsg = msg => {
    data.threads = [{ ...copy(thread), messages: [msg, { role: 'assistant', text: '收到。' }] }]
  }

  it('附件缩略图排在气泡上方，气泡里不再夹图', async () => {
    withUserMsg({ role: 'user', text: '看下这张图', files: ['u1', 'u2'], images: 2 })
    await show()
    const col = wrapper.get('.mrow.user .msg-col')
    const kids = col.element.children
    expect(kids[0].className).toContain('msg-imgs')
    expect(kids[1].className).toContain('bubble')
    expect(col.findAll('.bubble img').length).toBe(0)
    expect(wrapper.findAll('.msg-img').length).toBe(2)
    expect(wrapper.get('.msg-img img').attributes('src')).toBe('/api/upload/u1')
  })

  it('只发图片时不留一个空气泡', async () => {
    withUserMsg({ role: 'user', text: '', files: ['u1'], images: 1 })
    await show()
    expect(wrapper.findAll('.msg-img').length).toBe(1)
    expect(wrapper.find('.mrow.user .bubble').exists()).toBe(false)
  })

  it('历史里图片地址丢了就退回「📷 ×N」文字，不画空图框', async () => {
    withUserMsg({ role: 'user', text: '那条带图的', images: 2 })
    await show()
    expect(wrapper.find('.msg-imgs').exists()).toBe(false)
    expect(wrapper.get('.mrow.user .bubble').text()).toContain('📷 ×2')
  })

  it('点缩略图开全图预览，Esc 只收预览不停本轮', async () => {
    withUserMsg({ role: 'user', text: '两张图', files: ['u1', 'u2'], images: 2 })
    await show()
    await wrapper.findAll('.msg-img')[1].trigger('click')
    await flushPromises()
    expect(wrapper.get('.lb-img').attributes('src')).toBe('/api/upload/u2')
    expect(wrapper.get('.lb-count').text()).toBe('2 / 2')

    document.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
    await flushPromises()
    expect(wrapper.find('.lb-mask').exists()).toBe(false)
  })

  it('待发缩略图也能点开预览，「移除」按钮照旧生效', async () => {
    withUserMsg({ role: 'user', text: '准备发图' })
    await show()
    const ev = new window.Event('paste', { bubbles: true, cancelable: true })
    ev.clipboardData = { files: [new File(['photo'], 'photo.png', { type: 'image/png' })] }
    wrapper.get('textarea').element.dispatchEvent(ev)
    await flushPromises()
    expect(wrapper.get('.thumb-open img').attributes('src')).toBe('blob:x')

    await wrapper.get('.thumb-open').trigger('click')
    await flushPromises()
    expect(wrapper.get('.lb-img').attributes('src')).toBe('blob:x')
    await wrapper.get('.lb-close').trigger('click')

    await wrapper.get('.thumb .rm').trigger('click')
    expect(wrapper.find('.thumb').exists()).toBe(false)
  })
})

  it('总结并新开可以换 agent：下拉选谁就交给谁，并提示档位不沿用', async () => {
    data.threads = [{ ...thread, contextUsed: 24000, contextSize: 30000 }]
    await show()
    await wrapper.get('[aria-label="上下文使用率"]').trigger('click')
    // 打开交接目标下拉，挑 opencode
    await wrapper.get('#agCtxPopover .selbox-trigger').trigger('click')
    await flushPromises()
    const opt = wrapper.findAll('.selbox-opt').find(o => o.text().includes('opencode'))
    await opt.trigger('click')
    await flushPromises()
    // 换了人就说明白：模型/审批档位不沿用
    expect(wrapper.get('#agCtxPopover p.meta').text()).toContain('不沿用')

    await wrapper.findAll('#agCtxPopover button').find(b => b.text() === '总结并新开').trigger('click')
    await flushPromises()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/handoff', { agent: 'opencode' })
    expect(wrapper.get('.ag-head-txt b').text()).toContain('续')
    expect(wrapper.text()).toContain('这次交给 opencode')
  })

  it('交接目标被停用时报错说清去哪儿加，不静默换人', async () => {
    data.acp = { ...config, agents: [{ name: 'codex', command: 'npx', enabled: true },
      { name: 'opencode', command: 'opencode', enabled: false }] }
    data.threads = [{ ...thread, contextUsed: 24000, contextSize: 30000 }]
    await show()
    await wrapper.get('[aria-label="上下文使用率"]').trigger('click')
    await wrapper.get('#agCtxPopover .selbox-trigger').trigger('click')
    await flushPromises()
    const opt = wrapper.findAll('.selbox-opt').find(o => o.text().includes('opencode'))
    expect(opt.attributes('aria-disabled')).toBe('true') // 停用的列出来但选不中
    expect(toast).not.toHaveBeenCalled()
  })

describe('工作区清单里的内置 Friday', () => {
  // 后端 Normalized 会把内置条目注入清单最前（config.yaml 里没有它）
  function withBuiltin() {
    data.acp = { ...config, agents: [{ name: 'friday', command: '', builtin: true, enabled: true }, ...config.agents] }
  }

  it('内置那条出现在下拉里并标注内置，默认仍选用户自己配的那台', async () => {
    withBuiltin()
    data.threads = []
    await show()
    await newSession('proj-a')
    const opts = (await openOptions('选择智能体')).findAll('.selbox-opt')
    expect(opts.map(o => o.text())).toEqual(['friday（内置）', 'codex', 'opencode'])
    api.mockClear()
    await wrapper.findAll('button').find(b => b.text() === '创建会话').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads', { workspaceId: 'ws1', agent: 'codex' })
  })

  it('一台外部 agent 都没配时，默认就是内置那条（不装 agent 也能开工作区会话）', async () => {
    data.acp = { enabled: true, agents: [{ name: 'friday', command: '', builtin: true, enabled: true }] }
    data.threads = []
    await show()
    await newSession('proj-a')
    expect(selectedText('选择智能体')).toBe('friday（内置）')
    api.mockClear()
    await wrapper.findAll('button').find(b => b.text() === '创建会话').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads', { workspaceId: 'ws1', agent: 'friday' })
  })

  it('选中内置那条先说清没有会话级档位，创建后的提示也不提"切模型"', async () => {
    withBuiltin()
    data.threads = []
    await show()
    await newSession('proj-a')
    await pickOption('选择智能体', 'friday（内置）')
    const hints = wrapper.findAll('.ws-session-hint').map(h => h.text())
    expect(hints.join('\n')).toContain('没有会话级的模型与审批档位')
    api.mockClear()
    await wrapper.findAll('button').find(b => b.text() === '创建会话').trigger('click')
    await flushPromises()
    expect(toast).toHaveBeenCalledWith(expect.stringContaining('模型与权限用全局设置'))
    expect(toast).not.toHaveBeenCalledWith(expect.stringContaining('即可切模型'))
  })

  it('内置会话的头部标明它是内置那条（解释为什么没有能力 chips）', async () => {
    withBuiltin()
    data.threads = [{ ...copy(thread), id: 'thB', agent: 'friday', title: '内置这条' }]
    await show()
    await pickSession('内置这条')
    expect(headMeta()).toContain('friday（内置，模型与权限走全局设置）')
    // 档位没下发的那句仍按既有形态出现（不新加一颗开关）
    expect(headMeta()).not.toContain('undefined')
  })

  it('新建工作区的默认目录仍取外部 agent 配的那份（内置那条没有默认目录）', async () => {
    withBuiltin()
    await show()
    await wrapper.get('[aria-label="新建工作区"]').trigger('click')
    expect(wrapper.text()).toContain('/home/u/proj') // 预填自 codex.workspace，没被清单第一条的内置条目带掉
    await wrapper.findAll('button').find(b => b.text() === '创建').trigger('click')
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/acp/workspaces', { name: '', dirs: ['/home/u/proj'] })
    expect(selectedText('选择智能体')).toBe('codex')
  })
})

describe('统一工具面板：窄图标条 / 展开 / 分栏拖拽', () => {
  // 面板三种形态：rail（默认，只剩图标条）/ panel（对话在左、面板在右）/ focus
  // （工作区一分为二，左会话右工具视图）。入口按钮在会话头部。
  async function openPanel() {
    await wrapper.get('[aria-label="工具面板"]').trigger('click')
    await flushPromises()
  }
  async function pickTool(label) {
    await wrapper.get('.ag-tools-rail-btn[aria-label="' + label + '"]').trigger('click')
    await flushPromises()
  }

  it('默认只有一条窄图标条，展开后工具面板在对话右边', async () => {
    await show()
    await pickSession('第一件事')
    // 默认形态：图标条在，面板收起（v-show：元素还在，只是不显示——
    // 里面的终端实例不能被卸载，收起时那条 shell 还得继续跑）
    expect(wrapper.find('.ag-tools-rail').exists()).toBe(true)
    expect(wrapper.find('.ag-tools').attributes('style')).toContain('display: none')
    expect(wrapper.find('.wt-divider').attributes('style')).toContain('display: none')

    await openPanel()
    expect(wrapper.find('.ag-tools').attributes('style')).not.toContain('display: none')
    expect(wrapper.find('.wt-divider').attributes('style')).not.toContain('display: none')
    expect(wrapper.find('.wt-divider').exists()).toBe(true)
    // 面板上排着六个工具标签
    const labels = wrapper.findAll('.ag-tools-tab').map(b => b.text())
    expect(labels).toEqual(['终端', '文件', '改动', 'Git 管理', '项目记忆', '验证'])
  })

  it('右上角展开按钮把工作区一分为二：左边会话、右边工具视图', async () => {
    await show()
    await pickSession('第一件事')
    await openPanel()
    expect(wrapper.find('.ag-tools').classes()).not.toContain('focus')

    await wrapper.get('[aria-label="展开工具视图"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('.ag-tools').classes()).toContain('focus')
    // 会话列被收窄（左列宽度由面板报上来）
    expect(wrapper.find('.ag-chat').attributes('style')).toContain('width:')

    await wrapper.get('[aria-label="退出展开视图"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('.ag-tools').classes()).not.toContain('focus')
  })

  it('终端标签下每条会话各自一份实例，切会话不打断连接', async () => {
    data.threads.push({ ...copy(second), cwd: '/home/u/shared' })
    await show()
    await pickSession('第一件事')
    await openPanel()
    await pickTool('终端')
    expect(wrapper.findAll('.wt-panel')).toHaveLength(1)
    expect(sockets).toHaveLength(1)

    await pickSession('第二件事')
    // 还没看过这条会话的终端：实例没挂
    expect(wrapper.findAll('.wt-panel')).toHaveLength(1)
    expect(sockets).toHaveLength(1)

    await pickTool('终端')
    expect(wrapper.findAll('.wt-panel')).toHaveLength(2)
    expect(sockets).toHaveLength(2)
    // 每条会话的终端落在各自的主目录
    expect(sockets[0].frames[0]).toMatchObject({ t: 'start', cwd: '/home/u/proj' })
    expect(sockets[1].frames[0]).toMatchObject({ t: 'start', cwd: '/home/u/shared' })

    // 切回第一条：连接还在，不重开
    await pickSession('第一件事')
    await pickTool('终端')
    expect(sockets).toHaveLength(2)
    expect(termInstances[0].disposed).toBe(false)
  })

  it('分隔线拖动把面板宽度钳在 28%–78%，松手后不再跟随', async () => {
    await show()
    await pickSession('第一件事')
    await openPanel()
    wrapper.get('.ag-split').element.getBoundingClientRect =
      () => ({ left: 0, right: 1000, top: 0, bottom: 100, width: 1000, height: 100 })
    await wrapper.get('.wt-divider').trigger('pointerdown', { clientX: 300 })
    expect(document.body.classList.contains('wt-resizing')).toBe(true)
    // Vue 的样式 patch 是批处理的：每动一下等一次 nextTick 再断言
    const move = async clientX => {
      window.dispatchEvent(new MouseEvent('pointermove', { clientX }))
      await nextTick()
    }
    await move(200) // 0.8 -> 顶到 78%
    expect(wrapper.get('.ag-tools').attributes('style')).toContain('width: 78%')
    await move(850) // 0.15 -> 压到 28%
    expect(wrapper.get('.ag-tools').attributes('style')).toContain('width: 28%')
    await move(500)
    expect(wrapper.get('.wt-divider').attributes('aria-valuenow')).toBe('50')
    window.dispatchEvent(new Event('pointerup'))
    expect(document.body.classList.contains('wt-resizing')).toBe(false)
    await move(100) // 松手后指针移动不再影响比例
    expect(wrapper.get('.ag-tools').attributes('style')).toContain('width: 50%')
  })
})
