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
