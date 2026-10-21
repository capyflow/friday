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
})
