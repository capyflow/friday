import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AcpSection from '../src/components/settings/AcpSection.vue'
import { api } from '../src/api.js'

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
    // 折叠态仍可测试/删除
    expect(row.findAll('.pm-acts button').map(b => b.text())).toEqual(['测试', '删除'])

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
    { name: 'read_file', description: '读文件', denied: false, inDefault: true },
    { name: 'list_files', description: '列目录', denied: false, inDefault: true },
    { name: 'write_file', description: '写文件', denied: true, inDefault: false },
  ],
}

describe('设置 → 智能体 → 工具门面', () => {
  beforeEach(() => {
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url === '/api/acp/config') {
        // 后端回的是归一化后的值：facade.tools 为空会补默认只读清单，这里照实给。
        return { acp: { enabled: false,
          facade: { enabled: true, disableAuth: false, tools: ['read_file', 'list_files'] },
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

  it('审批类工具的勾选框禁用并带标记，勾不上', async () => {
    await show()
    const box = facadeBox('write_file')
    expect(box.find('input').attributes('disabled')).toBeDefined()
    expect(box.text()).toContain('审批类')
    await box.find('input').setValue(true) // disabled，事件不应改状态
    const saved = await saveAndCapture()
    expect(saved.acp.facade.tools).not.toContain('write_file')
  })

  it('勾选全局清单随 facade.tools 保存；agent 卡片勾选落到 allowedFacadeTools，未勾的 agent 为空数组', async () => {
    await show()
    await facadeBox('read_file').find('input').setValue(true)
    await facadeBox('list_files').find('input').setValue(true)
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

  it('全局清单全部取消 = 保存空数组（后端回默认只读清单）', async () => {
    await show() // config 无 facade 段 → 默认清单回显；目录里 read_file/list_files 应勾上
    expect(facadeBox('read_file').find('input').element.checked).toBe(true)
    await facadeBox('read_file').find('input').setValue(false)
    await facadeBox('list_files').find('input').setValue(false)
    const saved = await saveAndCapture()
    expect(saved.acp.facade.tools).toEqual([])
  })

  async function saveAndCapture() {
    await wrapper.findAll('button').find(b => b.text().includes('保存')).trigger('click')
    await flushPromises()
    return api.mock.calls.find(([method, url]) => method === 'POST' && url === '/api/acp/config')[2]
  }
})
