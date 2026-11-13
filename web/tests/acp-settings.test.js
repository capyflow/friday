import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AcpSection from '../src/components/settings/AcpSection.vue'
import { api } from '../src/api.js'
import { store } from '../src/store.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

const CATALOG = {
  catalog: [
    {
      id: 'codex', name: 'Codex（OpenAI）', installed: true,
      path: '/opt/homebrew/bin/codex', check: 'codex',
      template: { name: 'codex', command: 'npx', args: ['-y', '@agentclientprotocol/codex-acp'] },
    },
    {
      id: 'opencode', name: 'opencode', installed: true,
      path: '/Users/u/.opencode/bin/opencode', check: 'opencode',
      template: { name: 'opencode', command: 'opencode', args: ['acp'] },
    },
  ],
}
let wrapper

beforeEach(() => {
  api.mockReset().mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/acp/config') return { acp: { enabled: false, agents: [] } }
    if (method === 'GET' && url === '/api/acp/catalog') return CATALOG
    return { message: '已保存' }
  })
})
afterEach(() => { wrapper?.unmount(); wrapper = null })

async function show() {
  wrapper = mount(AcpSection)
  await flushPromises()
}

describe('设置 → 智能体', () => {
  it('升级后保存沿用新启动路径、参数和版本，保留自定义环境', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') return { acp: { agents: [{
        name: 'binary', command: '/old/agent', args: ['--old'], version: '1.0.0', env: { CUSTOM: 'kept' },
      }] } }
      if (method === 'GET' && url === '/api/acp/catalog') return { catalog: [{
        id: 'binary', name: 'Binary', installed: true, registered: true, updateAvailable: true,
      }] }
      if (method === 'POST' && url === '/api/acp/install') return { agent: {
        name: 'binary', command: '/new/agent', args: ['--new'], version: '2.0.0',
      } }
      return { message: '已保存' }
    })
    await show()
    await wrapper.findAll('button').find(b => b.text() === '一键升级').trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find(b => b.text().includes('保存')).trigger('click')
    await flushPromises()
    const saved = api.mock.calls.find(([method, url]) => method === 'POST' && url === '/api/acp/config')[2]
    expect(saved.acp.agents[0]).toMatchObject({
      command: '/new/agent', args: ['--new'], version: '2.0.0', env: { CUSTOM: 'kept' },
    })
  })

  it('安装失败展示命令输出，便于定位 PATH / 权限问题', async () => {
    const installErr = new Error('安装命令失败: exit status 127（输出见下）')
    installErr.output = 'sh: npm: command not found'
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') return { acp: { enabled: false, agents: [] } }
      if (method === 'GET' && url === '/api/acp/catalog') return { catalog: [{
        id: 'codex', name: 'Codex（OpenAI）', installed: false,
        check: 'codex', template: { name: 'codex', command: 'npx', args: ['-y', '@agentclientprotocol/codex-acp'] },
      }] }
      if (method === 'POST' && url === '/api/acp/install') return Promise.reject(installErr)
      return { message: '已保存' }
    })
    await show()
    await wrapper.findAll('button').find(b => b.text().includes('一键安装')).trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('安装失败')
    expect(wrapper.get('[aria-label="安装命令输出"]').text()).toContain('npm: command not found')
  })

  it('目录模板：npx 适配器保留启动器，CLI 模板回填绝对路径', async () => {
    await show()
    const cards = wrapper.findAll('.acp-cat-card')
    const add = name => cards.find(c => c.text().includes(name)).find('button')
    await add('Codex').trigger('click')
    await add('opencode').trigger('click')

    const rows = wrapper.findAll('.acp-agent')
    expect(rows).toHaveLength(2)
    // 第二列是「启动命令」；codex 的 args 是适配器参数，不能被替换成 codex 二进制
    expect(rows[0].findAll('input')[1].element.value).toBe('npx')
    expect(rows[1].findAll('input')[1].element.value).toBe('/Users/u/.opencode/bin/opencode')
  })

  it('agent 卡片默认收起：折叠态看启动摘要，展开后无工作目录字段且原样带回手写的值', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') return { acp: { agents: [{
        name: 'codex', command: 'npx', args: ['-y', '@agentclientprotocol/codex-acp'],
        workspace: '~/projects', enabled: false, version: '2.3.0',
      }] } }
      if (method === 'GET' && url === '/api/acp/catalog') return { catalog: [] }
      return { message: '已保存' }
    })
    await show()
    const row = wrapper.get('.acp-agent')
    // 收起：字段不渲染，标题行给足信息（名称/停用/版本/启动命令）
    expect(row.find('.acp-agent-body').exists()).toBe(false)
    expect(row.get('.acp-agent-toggle').text()).toContain('codex')
    expect(row.get('.acp-agent-toggle').text()).toContain('已停用')
    expect(row.get('.acp-agent-toggle').text()).toContain('v2.3.0')
    expect(row.get('.acp-agent-sum').text()).toBe('npx -y @agentclientprotocol/codex-acp')
    expect(row.get('.acp-agent-toggle').attributes('aria-expanded')).toBe('false')
    // 折叠态仍可认证/测试/删除
    expect(row.findAll('.pm-acts button').map(b => b.text())).toEqual(['认证', '测试', '删除'])

    await row.get('.acp-agent-toggle').trigger('click')
    expect(row.find('.acp-agent-body').exists()).toBe(true)
    expect(row.get('.acp-agent-toggle').attributes('aria-expanded')).toBe('true')
    // 工作目录不在卡片上填（每个对话建 thread 时自己选）
    expect(row.text()).not.toContain('预填工作目录')

    await wrapper.findAll('button').find(b => b.text().includes('保存')).trigger('click')
    await flushPromises()
    const saved = api.mock.calls.find(([method, url]) => method === 'POST' && url === '/api/acp/config')[2]
    expect(saved.acp.agents[0]).toMatchObject({ name: 'codex', workspace: '~/projects', enabled: false })
  })

  it('新增与安装/添加模板的卡片直接展开，其余保持收起', async () => {
    await show()
    await wrapper.findAll('button').find(b => b.text().includes('+ 添加 agent')).trigger('click')
    expect(wrapper.get('.acp-agent').find('.acp-agent-body').exists()).toBe(true)
    await wrapper.get('.acp-agent').get('.acp-agent-toggle').trigger('click') // 手动收起
    const cards = wrapper.findAll('.acp-cat-card')
    await cards.find(c => c.text().includes('Codex')).find('button').trigger('click')
    expect(wrapper.findAll('.acp-agent')[1].find('.acp-agent-body').exists()).toBe(true)
    expect(wrapper.findAll('.acp-agent')[0].find('.acp-agent-body').exists()).toBe(false)
  })
})

describe('内置 friday 条目', () => {
  it('不进可编辑清单，改为一句说明；保存时也不回传它', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') return { acp: { enabled: true, agents: [
        { name: 'friday', command: '', builtin: true },
        { name: 'codex', command: 'npx', args: ['-y', 'x'] },
      ] } }
      if (method === 'GET' && url === '/api/acp/catalog') return { catalog: [] }
      return { message: '已保存' }
    })
    await show()
    const rows = wrapper.findAll('.acp-agent')
    expect(rows).toHaveLength(1) // 只有可编辑的那台（内置那条 command 为空，摊开就是一片空框）
    expect(rows[0].text()).toContain('codex')
    expect(wrapper.get('.acp-builtin-hint').text()).toContain('friday（内置）')
    await wrapper.findAll('button').find(b => b.text().includes('保存')).trigger('click')
    await flushPromises()
    const saved = api.mock.calls.find(([method, url]) => method === 'POST' && url === '/api/acp/config')[2]
    expect(saved.acp.agents.map(a => a.name)).toEqual(['codex'])
  })
})

describe('升级徽标的版本号', () => {
  // 场景（2026-11-06 实测）：官方注册表已到 2.180.0，但 npm 缓存还停在 2.179.2，
  // 徽标一度显示「可升级（v2.179.3 → v2.179.2）」——高版本向低版本升级。
  // 后端已让 latestVersion 与 updateAvailable 同源，前端再兜一层：目标版本
  // 不严格高于当前版本就不画箭头。
  const catWith = (c) => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') return { acp: { enabled: true, agents: [
        { name: 'codebuddy-code', command: 'npx', args: ['-y', '@tencent-ai/codebuddy-code@2.179.3'], version: '2.179.3' },
      ] } }
      if (method === 'GET' && url === '/api/acp/catalog') return { catalog: [c] }
      return { message: '已保存' }
    })
  }

  it('目标版本低于当前版本时不显示降级箭头', async () => {
    catWith({
      id: 'codebuddy-code', name: 'Codebuddy Code', version: '2.180.0',
      latestVersion: '2.179.2', registered: true, updateAvailable: true,
      template: { name: 'codebuddy-code', command: 'npx' },
    })
    await show()
    const badge = wrapper.get('.acp-cat-badge')
    expect(badge.text()).toContain('可升级')
    expect(badge.text()).not.toContain('→')
  })

  it('目标版本等于当前版本时不显示箭头', async () => {
    catWith({
      id: 'codebuddy-code', name: 'Codebuddy Code', version: '2.179.3',
      latestVersion: '2.179.3', registered: true, updateAvailable: true,
      template: { name: 'codebuddy-code', command: 'npx' },
    })
    await show()
    expect(wrapper.get('.acp-cat-badge').text()).toContain('可升级')
    expect(wrapper.get('.acp-cat-badge').text()).not.toContain('→')
  })

  it('目标版本确实更高时正常显示 v旧 → v新', async () => {
    catWith({
      id: 'codebuddy-code', name: 'Codebuddy Code', version: '2.180.0',
      latestVersion: '2.180.0', registered: true, updateAvailable: true,
      template: { name: 'codebuddy-code', command: 'npx' },
    })
    await show()
    const badge = wrapper.get('.acp-cat-badge')
    expect(badge.text()).toContain('可升级')
    expect(badge.text()).toContain('v2.179.3 → v2.180.0')
  })
})
// ---------- 认证面板 ----------

// agent 装好之后能不能干活取决于它自己的登录态。这块要做成「可主动查 + 可就地做」，
// 不能只等「测试」失败时在报错末尾附一句指引。
describe('设置 → 智能体 → 认证', () => {
  const AUTH_STATUS = {
    status: {
      agent: 'codex', state: 'logged_out', message: '未登录：not logged in',
      hint: '在终端运行 codex login 完成 ChatGPT 登录', command: 'codex login',
      creds: [],
      methods: [{
        id: 'key', name: 'API Key', type: 'env_var',
        vars: [{ name: 'OPENAI_API_KEY', secret: true }],
      }],
    },
  }

  beforeEach(() => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') {
        return { acp: { enabled: false, agents: [
          { name: 'codex', command: 'npx', args: ['-y', '@agentclientprotocol/codex-acp'] },
        ] } }
      }
      if (method === 'GET' && url === '/api/acp/catalog') return { catalog: [] }
      if (method === 'GET' && url.startsWith('/api/acp/auth?agent=')) return AUTH_STATUS
      if (method === 'POST' && url === '/api/acp/auth') {
        return { result: { state: 'logged_in', message: '认证成功：API Key' } }
      }
      return { message: '已保存' }
    })
  })

  async function openAuthPanel() {
    await show()
    await wrapper.findAll('.pm-acts button').find(b => b.text() === '认证').trigger('click')
    await flushPromises()
    return wrapper.get('.acp-auth')
  }

  it('点「认证」展开面板：给出登录态、登录指引与可执行的命令', async () => {
    const panel = await openAuthPanel()
    expect(panel.text()).toContain('未登录')
    expect(panel.text()).toContain('codex login')
    expect(panel.text()).toContain('在终端运行 codex login')
    // 卡片头上也有同一份状态的徽章（收起时一眼能看见）
    expect(wrapper.findAll('.acp-auth-badge')[0].text()).toBe('未登录')
  })

  // 装完 agent 到登录之间不能有空档：面板自动展开、卡片高亮，登录入口就在眼前。
  it('安装完成后自动展开那台 agent 的认证面板', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') return { acp: { enabled: false, agents: [] } }
      if (method === 'GET' && url === '/api/acp/catalog') return { catalog: [{
        id: 'codex', name: 'Codex（OpenAI）', installed: false, check: 'codex',
        template: { name: 'codex', command: 'npx', args: ['-y', '-p', '@agentclientprotocol/codex-acp', 'codex-acp'] },
      }] }
      if (method === 'POST' && url === '/api/acp/install') return { message: '安装完成', agent: {
        name: 'codex', command: 'npx', args: ['-y', '-p', '@agentclientprotocol/codex-acp', 'codex-acp'],
      } }
      if (method === 'GET' && url.startsWith('/api/acp/auth?agent=')) return { status: {
        agent: 'codex', state: 'logged_out', message: '未登录',
        hint: '在终端运行 codex login', command: 'codex login', methods: [],
      } }
      return { message: '已保存' }
    })
    await show()
    await wrapper.findAll('button').find(b => b.text().includes('一键安装')).trigger('click')
    await flushPromises()
    const panel = wrapper.get('.acp-auth')
    expect(panel.text()).toContain('codex login')
    // 卡片被高亮，指过去就是刚装的那台
    expect(wrapper.get('.acp-agent').classes()).toContain('acp-agent-focus')
  })

  it('env_var 型：填了密钥点「保存并认证」，变量交给后端落盘', async () => {
    const panel = await openAuthPanel()
    await panel.get('.acp-auth-vars input').setValue('sk-test')
    await panel.findAll('.acp-auth-acts button').find(b => b.text() === '保存并认证').trigger('click')
    await flushPromises()
    const call = api.mock.calls.find(([method, url]) => method === 'POST' && url === '/api/acp/auth')
    expect(call[2]).toMatchObject({
      name: 'codex', methodId: 'key', vars: { OPENAI_API_KEY: 'sk-test' },
    })
  })

  // codebuddy 这类 agent 不声明 env_var 认证方式，只认环境变量/CLI 登录：
  // 面板必须给出推荐变量的输入框，否则人根本没有地方填凭据。
  it('推荐的环境变量能直接填进这台 agent（codebuddy 这类）', async () => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') return { acp: { enabled: false, agents: [
        { name: 'codebuddy-code', command: 'npx' },
      ] } }
      if (method === 'GET' && url === '/api/acp/catalog') return { catalog: [] }
      if (method === 'GET' && url.startsWith('/api/acp/auth?agent=')) return { status: {
        agent: 'codebuddy-code', state: 'unknown', message: 'agent 没有回应认证请求',
        hint: '网页端登录不算 CLI 的凭据', command: 'codebuddy',
        suggestedVars: ['CODEBUDDY_API_KEY', 'CODEBUDDY_AUTH_TOKEN'],
        methods: [],
      } }
      if (method === 'POST' && url === '/api/acp/auth') return { result: { state: 'logged_in', message: '凭据已保存' } }
      return { message: '已保存' }
    })
    await show()
    await wrapper.findAll('.pm-acts button').find(b => b.text() === '认证').trigger('click')
    await flushPromises()
    const inputs = wrapper.findAll('.acp-auth-vars input')
    expect(inputs).toHaveLength(2)
    await inputs[0].setValue('cb-xxxx')
    await wrapper.findAll('.acp-auth-acts button').find(b => b.text() === '保存凭据').trigger('click')
    await flushPromises()
    const call = api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/acp/auth')
    expect(call[2]).toMatchObject({
      name: 'codebuddy-code', methodId: '', vars: { CODEBUDDY_API_KEY: 'cb-xxxx' },
    })
  })

  it('「在终端里登录」把命令交给终端页去跑', async () => {
    const panel = await openAuthPanel()
    await panel.findAll('.acp-auth-cmd button').find(b => b.text() === '在终端里登录').trigger('click')
    expect(store.terminalCommand).toBe('codex login')
    expect(store.view).toBe('terminal')
  })
})

// ---------- 工具门面卡片 ----------

const FACADE = {
  status: {
    enabled: true, authDisabled: false, url: 'http://127.0.0.1:58080/mcp',
    lists: 2, calls: 3, rejects: 1,
    grants: [{ agent: 'minimax', tools: ['get_time'], expires: '2026-11-07T21:00:00+08:00' }],
    records: [
      { at: '2026-11-06T21:45:27.5+08:00', agent: 'minimax', tool: 'list_files', outcome: 'ok', detail: '12ms' },
      { at: '2026-11-06T21:44:00.0+08:00', agent: 'minimax', tool: 'write_file', outcome: 'denied', detail: '不在该会话白名单' },
    ],
  },
  tools: [
    { name: 'read_file', description: '读文件' },
    { name: 'list_files', description: '列目录' },
    { name: 'write_file', description: '写文件' },
  ],
}

describe('设置 → 智能体 → 工具门面', () => {
  beforeEach(() => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') {
        // 后端回的是归一化后的值：白名单为空 = 全部工具（默认形态）。
        return { acp: { enabled: false,
          facade: { enabled: true, disableAuth: false, tools: [] },
          agents: [{ name: 'minimax', command: 'minimax-acp' }] } }
      }
      if (method === 'GET' && url === '/api/acp/catalog') return { catalog: [] }
      if (method === 'GET' && url === '/api/acp/facade') return FACADE
      return { message: '已保存' }
    })
  })

  function facadeBox(name) {
    const card = wrapper.get('.acp-facade')
    return card.findAll('label').find(l => l.text().startsWith(name))
  }

  it('渲染状态行、调用记录（最新在前、结果中文化）与可勾选工具目录', async () => {
    await show()
    const status = wrapper.get('.acp-facade-status')
    expect(status.text()).toContain('http://127.0.0.1:58080/mcp')
    expect(status.text()).toContain('调用 3 次')
    expect(status.text()).toContain('活授权：minimax')
    const rows = wrapper.findAll('.acp-facade-records tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('成功')
    expect(rows[1].text()).toContain('拒绝')
    expect(rows[1].text()).toContain('minimax')
  })

  it('默认全部工具：目录里的每一项都勾上（白名单为空 = 不过滤）', async () => {
    await show()
    for (const name of ['read_file', 'list_files', 'write_file']) {
      expect(facadeBox(name).find('input').element.checked).toBe(true)
    }
  })

  it('取消一个工具 → 展开成全量减一的白名单；agent 卡片勾选落到 allowedFacadeTools', async () => {
    await show()
    await facadeBox('write_file').find('input').setValue(false)
    // 展开 agent 卡片，勾一个逐 agent 白名单。
    await wrapper.get('.acp-agent-toggle').trigger('click')
    const agentCard = wrapper.get('.acp-agent')
    const agentBox = agentCard.findAll('label').find(l => l.text() === 'read_file')
    await agentBox.find('input').setValue(true)

    const saved = await saveAndCapture()
    expect(saved.acp.facade.tools).toEqual(['read_file', 'list_files'])
    expect(saved.acp.agents[0].allowedFacadeTools).toEqual(['read_file'])
    expect(saved.acp.facade.disableAuth).toBe(false)
  })

  it('全部取消 = 空数组（后端按"全部工具"解释）', async () => {
    await show()
    for (const name of ['read_file', 'list_files', 'write_file']) {
      await facadeBox(name).find('input').setValue(false)
    }
    const saved = await saveAndCapture()
    expect(saved.acp.facade.tools).toEqual([])
  })

  async function saveAndCapture() {
    await wrapper.findAll('button').find(b => b.text().includes('保存')).trigger('click')
    await flushPromises()
    return api.mock.calls.find(([method, url]) => method === 'POST' && url === '/api/acp/config')[2]
  }
})
