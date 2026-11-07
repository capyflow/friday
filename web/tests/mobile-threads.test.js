// 工作区会话列表按项目分组：组头 = 工作区名，组间按各组最新会话活跃时间排序。
import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const apiMock = vi.hoisted(() => vi.fn())
vi.mock('../src/api.js', () => ({ api: apiMock }))
vi.mock('../src/store.js', () => ({ toast: vi.fn() }))
vi.mock('../src/components/SelectBox.vue', () => ({ default: { template: '<select />' } }))

import MobileThreads from '../src/mobile/MobileThreads.vue'

const THREADS = [
  { id: 't1', title: 'Plugins 最新', workspaceId: 'ws-plugins', agent: 'zcode', updatedAt: '2026-11-07T10:00:00Z' },
  { id: 't2', title: 'Photos 更新', workspaceId: 'ws-photos', agent: 'codex', updatedAt: '2026-11-07T09:00:00Z' },
  { id: 't3', title: 'Plugins 旧', workspaceId: 'ws-plugins', agent: 'claude', updatedAt: '2026-11-06T08:00:00Z' },
  { id: 't4', title: 'Codis', workspaceId: 'ws-codis', agent: 'zcode', updatedAt: '2026-11-05T08:00:00Z' },
]
const WORKSPACES = [
  { id: 'ws-plugins', name: 'Plugins' },
  { id: 'ws-photos', name: '照片服务' },
  { id: 'ws-codis', name: 'codis' },
]

const mountList = async () => {
  apiMock.mockImplementation(async (_m, path) => {
    if (path === '/api/acp/threads') return { workspaces: WORKSPACES, threads: THREADS, acp: { agents: [] } }
    return {} // 容忍测试环境的杂散调用（Vue dev 模式初始化触达）
  })
  const w = mount(MobileThreads)
  await flushPromises()
  return w
}

describe('mobile threads grouping', () => {
  beforeEach(() => apiMock.mockReset())

  it('按工作区分组出组头，组间按最近活跃排序', async () => {
    const w = await mountList()
    const groups = w.findAll('.m-row-group')
    // Plugins 最新(11-07 10:00) > 照片服务(11-07 09:00) > codis(11-05)
    expect(groups.map(g => g.text())).toEqual(['Plugins', '照片服务', 'codis'])
    // 组内顺序保持接口时间序，且组内连续：Plugins 两行相邻，然后照片服务、codis
    const rows = w.findAll('.m-row .m-row-title')
    expect(rows.map(r => r.text())).toEqual(['Plugins 最新', 'Plugins 旧', 'Photos 更新', 'Codis'])
    // 分组后行副标题不再重复工作区名（组头已表达）
    expect(w.find('.m-row-sub').text()).not.toContain('Plugins')
    w.unmount()
  })

  it('未知工作区的会话归入「未知工作区」组', async () => {
    apiMock.mockImplementation(async () => ({
      workspaces: [],
      threads: [{ id: 'x', title: '孤儿', workspaceId: 'gone', agent: 'a', updatedAt: '2026-11-07T00:00:00Z' }],
      acp: { agents: [] },
    }))
    const w = mount(MobileThreads)
    await flushPromises()
    expect(w.find('.m-row-group').text()).toBe('未知工作区')
    w.unmount()
  })
})
