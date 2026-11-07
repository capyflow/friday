// 工作区两级视图：①项目列表（名称 + 会话数 + 最近活跃）；②点进项目看该
// 项目会话。新建项目用 /api/acp/dirs 浏览电脑目录后 POST /api/acp/workspaces。
import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const apiMock = vi.hoisted(() => vi.fn())
vi.mock('../src/api.js', () => ({ api: apiMock }))
vi.mock('../src/store.js', () => ({ toast: vi.fn() }))
vi.mock('../src/components/SelectBox.vue', () => ({ default: { template: '<select />' } }))

import MobileThreads from '../src/mobile/MobileThreads.vue'

const WORKSPACES = [
  { id: 'ws-plugins', name: 'Plugins' },
  { id: 'ws-photos', name: '照片服务' },
]
const THREADS = [
  { id: 't1', title: 'Plugins 最新', workspaceId: 'ws-plugins', agent: 'zcode', updatedAt: '2026-11-07T10:00:00Z' },
  { id: 't2', title: 'Photos 更新', workspaceId: 'ws-photos', agent: 'codex', updatedAt: '2026-11-07T09:00:00Z' },
  { id: 't3', title: 'Plugins 旧', workspaceId: 'ws-plugins', agent: 'claude', updatedAt: '2026-11-06T08:00:00Z' },
]
const mountView = async () => {
  apiMock.mockImplementation(async (_m, path) => {
    if (path === '/api/acp/threads') return { workspaces: WORKSPACES, threads: THREADS, acp: { agents: [{ name: 'zcode' }] } }
    if (String(path).startsWith('/api/acp/dirs')) {
      // 解析 query 返回对应层级的列表（组件按返回的 path/parent 继续浏览）
      const q = decodeURIComponent(String(path).split('path=')[1] || '')
      if (q.endsWith('/projects')) return { path: q, parent: '/home/aaron', dirs: [] }
      return { path: '/home/aaron', parent: '', dirs: [{ name: 'projects', path: '/home/aaron/projects' }] }
    }
    if (path === '/api/acp/workspaces') return { workspace: { id: 'ws-new', name: 'projects' } }
    return {} // 容忍测试环境的杂散调用
  })
  const w = mount(MobileThreads)
  await flushPromises()
  return w
}

describe('mobile workspace two-level view', () => {
  beforeEach(() => apiMock.mockReset())

  it('项目列表：每个项目一行，带会话数与最近活跃', async () => {
    const w = await mountView()
    const rows = w.findAll('.m-row')
    expect(rows.length).toBe(2)
    expect(rows[0].text()).toContain('Plugins')
    expect(rows[0].text()).toContain('2 个会话')
    expect(rows[1].text()).toContain('照片服务')
    expect(rows[1].text()).toContain('1 个会话')
    w.unmount()
  })

  it('点项目进入会话列表：只显示该项目的会话，返回键回项目列表', async () => {
    const w = await mountView()
    await w.findAll('.m-row')[0].trigger('click')
    const rows = w.findAll('.m-row')
    expect(rows.length).toBe(2) // Plugins 的两个会话
    expect(rows[0].text()).toContain('Plugins 最新')
    expect(rows[1].text()).toContain('Plugins 旧')
    await w.find('.m-ico-btn').trigger('click')
    expect(w.findAll('.m-row').length).toBe(2) // 回到项目列表
    w.unmount()
  })

  it('新建项目：浏览目录 → 在此创建 → POST workspaces 并进入新项目', async () => {
    const w = await mountView()
    await w.find('.m-pane-head .m-btn').trigger('click') // ＋ 新项目
    expect(w.find('.m-dir-path').text()).toContain('/home/aaron')
    await w.findAll('.m-dir-row')[0].trigger('click') // 进入 projects 子目录
    await flushPromises()
    // 名称默认取首层目录名（aaron），用户改成 projects 后创建
    await w.find('.m-sheet input.m-input').setValue('projects')
    await w.find('.m-sheet .m-btn.primary').trigger('click') // 在此创建项目
    await flushPromises()
    const post = apiMock.mock.calls.find(c => c[0] === 'POST' && c[1] === '/api/acp/workspaces')
    expect(post).toBeTruthy()
    expect(JSON.parse(JSON.stringify(post[2]))).toEqual({ name: 'projects', dirs: ['/home/aaron/projects'] })
    // 创建成功后直接进入新项目的会话视图
    expect(w.find('.m-pane-title').text()).toContain('projects')
    w.unmount()
  })
})
