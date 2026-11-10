// 工作区两级视图 + 编辑能力：项目列表/进入项目/建项目/删会话/改名/加目录/删项目。
import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const apiMock = vi.hoisted(() => vi.fn())
vi.mock('../src/api.js', () => ({ api: apiMock }))
vi.mock('../src/store.js', () => ({ toast: vi.fn() }))
vi.mock('../src/components/SelectBox.vue', () => ({ default: { template: '<select />' } }))

import MobileThreads from '../src/mobile/MobileThreads.vue'

const WORKSPACES = [
  { id: 'ws-plugins', name: 'Plugins', dirs: ['~/code/plugins'] },
  { id: 'ws-photos', name: '照片服务', dirs: ['~/photos'] },
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
      const q = decodeURIComponent(String(path).split('path=')[1] || '')
      if (q.endsWith('/projects')) return { path: q, parent: '/home/aaron', dirs: [] }
      return { path: '/home/aaron', parent: '', dirs: [{ name: 'projects', path: '/home/aaron/projects' }] }
    }
    if (path === '/api/acp/workspaces') return { workspace: { id: 'ws-new', name: 'projects', dirs: [] } }
    return { ok: true } // PUT/DELETE 等变更接口统一回成功壳
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
    w.unmount()
  })

  it('点项目进入会话列表：只显示该项目的会话，返回键回项目列表', async () => {
    const w = await mountView()
    await w.findAll('.m-row')[0].trigger('click')
    const rows = w.findAll('.m-row')
    expect(rows.length).toBe(2)
    expect(rows[0].text()).toContain('Plugins 最新')
    await w.find('.m-ico-btn').trigger('click')
    expect(w.findAll('.m-row').length).toBe(2)
    w.unmount()
  })

  it('会话按最近活动倒序：接口给的创建顺序不等于活跃顺序时，最新的排前面', async () => {
    apiMock.mockImplementation(async (_m, path) => {
      if (path === '/api/acp/threads') {
        return {
          workspaces: WORKSPACES,
          // 模拟投影文件的创建顺序：最旧的 t3 排在数组最前
          threads: [THREADS[2], THREADS[1], THREADS[0]],
          acp: { agents: [{ name: 'zcode' }] },
        }
      }
      return { ok: true }
    })
    const w = mount(MobileThreads)
    await flushPromises()
    await w.findAll('.m-row')[0].trigger('click') // 进入 Plugins
    const rows = w.findAll('.m-row')
    expect(rows.length).toBe(2)
    expect(rows[0].text()).toContain('Plugins 最新') // t1(10:00) 排在 t3(11-06) 前
    w.unmount()
  })

  it('项目列表按最近活动倒序：活跃晚的项目排前面', async () => {
    apiMock.mockImplementation(async (_m, path) => {
      if (path === '/api/acp/threads') {
        return {
          // 照片服务放在数组前面，但它的最新会话（09:00）晚于 Plugins（10:00）
          workspaces: [WORKSPACES[1], WORKSPACES[0]],
          threads: THREADS,
          acp: { agents: [{ name: 'zcode' }] },
        }
      }
      return { ok: true }
    })
    const w = mount(MobileThreads)
    await flushPromises()
    const rows = w.findAll('.m-row')
    expect(rows[0].text()).toContain('Plugins')
    expect(rows[1].text()).toContain('照片服务')
    w.unmount()
  })

  it('新建项目：浏览目录 → 在此创建 → POST workspaces 并进入新项目', async () => {
    const w = await mountView()
    await w.find('.m-pane-head .m-btn').trigger('click')
    expect(w.find('.m-dir-path').text()).toContain('/home/aaron')
    await w.findAll('.m-dir-row').find(b => b.text() === 'projects').trigger('click')
    await flushPromises()
    await w.find('.m-sheet input.m-input').setValue('projects')
    await w.find('.m-sheet .m-btn.primary').trigger('click')
    await flushPromises()
    const post = apiMock.mock.calls.find(c => c[0] === 'POST' && c[1] === '/api/acp/workspaces')
    expect(post[2]).toEqual({ name: 'projects', dirs: ['/home/aaron/projects'] })
    expect(w.find('.m-pane-title').text()).toContain('projects')
    w.unmount()
  })

  it('会话删除：行 × → 确认 → DELETE threads/:id', async () => {
    const w = await mountView()
    await w.findAll('.m-row')[0].trigger('click')
    await w.find('.m-row-x').trigger('click')
    expect(w.find('.m-sheet-title').text()).toBe('删除会话？')
    await w.find('.m-sheet-acts .m-btn.danger').trigger('click')
    await flushPromises()
    const del = apiMock.mock.calls.find(c => c[0] === 'DELETE' && String(c[1]).includes('/threads/'))
    expect(del[1]).toBe('/api/acp/threads/t1')
    w.unmount()
  })

  it('项目设置：改名 PUT、加目录 POST dirs、删项目 DELETE', async () => {
    const w = await mountView()
    await w.findAll('.m-row')[0].trigger('click')
    await w.find('[aria-label="项目设置"]').trigger('click')
    // 改名
    await w.find('.m-sheet input.m-input').setValue('新名')
    const saveBtn = w.findAll('.m-inline-row .m-btn').find(b => b.text() === '保存')
    await saveBtn.trigger('click')
    await flushPromises()
    const put = apiMock.mock.calls.find(c => c[0] === 'POST' && c[1] === '/api/acp/workspaces/ws-plugins')
    expect(put[2]).toEqual({ name: '新名' })
    // 加目录
    const addOpen = w.findAll('.m-field button.m-btn.block').find(b => b.text().includes('添加电脑目录'))
    await addOpen.trigger('click')
    await flushPromises()
    const addBtn = w.findAll('.m-inline-row .m-btn').find(b => b.text() === '添加当前目录')
    await addBtn.trigger('click')
    await flushPromises()
    const add = apiMock.mock.calls.find(c => c[0] === 'POST' && String(c[1]).includes('/dirs'))
    expect(add).toBeTruthy()
    // 删项目：askConfirm → 确认
    const delBtn = w.findAll('.m-sheet-acts .m-btn.danger').find(b => b.text() === '删除项目')
    await delBtn.trigger('click')
    await flushPromises()
    await w.find('[role="alertdialog"] .m-btn.danger').trigger('click')
    await flushPromises()
    const delWs = apiMock.mock.calls.find(c => c[0] === 'DELETE' && c[1] === '/api/acp/workspaces/ws-plugins')
    expect(delWs).toBeTruthy()
    w.unmount()
  })
})
