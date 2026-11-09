import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import WorkspaceGit from '../src/components/WorkspaceGit.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

// Git 面板的写操作都要过审批闸门：POST 只是排队，落地与否看 GET /git/ops 的
// state 是不是终态。所以 mock 要能把同一条 op 从 awaiting 演到 ok/failed，
// 用例才验得了「轮询到终态再刷新仓库」这条语义。
const thread = { id: 'th1', cwd: '/home/u/proj', agent: 'codex' }

function repoStatus(over = {}) {
  return {
    isRepo: true,
    branch: 'main',
    detached: false,
    upstream: 'origin/main',
    ahead: 0,
    behind: 0,
    staged: 0,
    modified: 0,
    untracked: 0,
    deleted: 0,
    conflicted: 0,
    files: [],
    ...over,
  }
}

// 默认回包：一个干净的仓库 + 两条分支 + 一条历史 + 空的写操作记录。
function defaultApi(over = {}) {
  const status = over.status || repoStatus()
  const branches = over.branches || [
    { name: 'main', current: true, remote: false, last: 'aaa111', date: '2026-11-01T10:00:00Z' },
    { name: 'feature/x', current: false, remote: false, last: 'bbb222', date: '2026-11-02T10:00:00Z' },
    { name: 'origin/main', current: false, remote: true, last: 'aaa111', date: '2026-11-01T10:00:00Z' },
  ]
  const commits = over.commits || [
    { hash: 'c1', short: 'aaa111', author: '小五', email: 'a@b.c', date: '2026-11-01T10:00:00Z',
      subject: '第一次提交', body: '正文一行' },
  ]
  const ops = over.ops || []
  return async (method, url, body) => {
    if (method === 'GET' && url.endsWith('/git/status')) return { status }
    if (method === 'GET' && url.endsWith('/git/branches')) return { branches }
    if (method === 'GET' && url.includes('/git/log')) return { commits }
    if (method === 'GET' && url.endsWith('/git/ops')) return { ops }
    if (method === 'POST' && url.endsWith('/git/ops')) {
      return { op: { id: 'op1', op: body?.op, arg: body?.arg, state: 'awaiting' },
        message: '已提交，等你在审批卡上批准后执行' }
    }
    if (method === 'POST' && url.includes('/stop')) return { message: '已停止这次操作' }
    return {}
  }
}

let wrapper

function mountGit(props = { thread }) {
  wrapper = mount(WorkspaceGit, { props })
  return wrapper
}

beforeEach(() => {
  api.mockReset()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
})

describe('Git 管理 · 只读部分', () => {
  it('不是 git 仓库时只说原因，不发分支/历史请求', async () => {
    api.mockImplementation(defaultApi({
      status: { isRepo: false, note: '这个目录不是 git 仓库', files: [] },
    }))
    const w = mountGit()
    await flushPromises()

    expect(w.text()).toContain('这个目录不是 git 仓库')
    // 非仓库没有分支和历史可言，别再白跑两趟
    const urls = api.mock.calls.map(c => c[1])
    expect(urls.some(u => u.includes('/git/branches'))).toBe(false)
    expect(urls.some(u => u.includes('/git/log'))).toBe(false)
  })

  it('状态计数与改动文件列表渲染正确，已暂存/未暂存给对按钮', async () => {
    api.mockImplementation(defaultApi({
      status: repoStatus({
        branch: 'main', ahead: 2, behind: 1, staged: 1, modified: 1, untracked: 1, deleted: 1,
        files: [
          { path: 'src/a.js', status: 'M ', stage: 'staged' },
          { path: 'src/b.js', status: ' M', stage: 'unstaged' },
          { path: 'src/c.js', status: '??', stage: 'unstaged' },
        ],
      }),
    }))
    const w = mountGit()
    await flushPromises()

    expect(w.text()).toContain('领先 2')
    expect(w.text()).toContain('落后 1')
    expect(w.text()).toContain('暂存 1')
    expect(w.text()).toContain('修改 1')
    expect(w.text()).toContain('未跟踪 1')
    expect(w.text()).toContain('删除 1')
    expect(w.text()).toContain('src/a.js')
    expect(w.text()).toContain('src/b.js')
    // 状态中文标注
    expect(w.text()).toContain('修改')
    expect(w.text()).toContain('未跟踪')

    const labels = w.findAll('button').map(b => b.text())
    // src/a.js 已暂存 → 取消暂存；src/b.js 与未跟踪的 src/c.js 未暂存 → 暂存
    expect(labels.filter(t => t === '取消暂存')).toHaveLength(1)
    expect(labels.filter(t => t === '暂存')).toHaveLength(2)
    const stagedBtn = w.findAll('button').find(b => b.text() === '取消暂存')
    expect(stagedBtn.attributes('aria-label')).toContain('src/a.js')
    const unstagedBtn = w.findAll('button').find(b => b.text() === '暂存')
    expect(unstagedBtn.attributes('aria-label')).toContain('src/b.js')
  })

  it('分支列表本地在前远端在后，当前分支标「当前」且不给切换按钮', async () => {
    api.mockImplementation(defaultApi())
    const w = mountGit()
    await flushPromises()

    const rows = w.findAll('.wg-branch-row')
    expect(rows.length).toBe(3)
    expect(rows[0].text()).toContain('main')
    expect(rows[0].text()).toContain('当前')
    expect(rows[2].text()).toContain('origin/main')
    expect(rows[2].text()).toContain('远端')
    // 远端分支不给切换按钮
    expect(rows[2].find('button').exists()).toBe(false)
    // 非当前本地分支给切换按钮
    const switchBtns = w.findAll('button').filter(b => b.text() === '切换')
    expect(switchBtns).toHaveLength(1)
  })
})

describe('Git 管理 · 写操作', () => {
  it('点「暂存」提交 op=stage，arg 是那个文件路径', async () => {
    api.mockImplementation(defaultApi({
      status: repoStatus({ files: [{ path: 'src/b.js', status: ' M', stage: 'unstaged' }] }),
    }))
    const w = mountGit()
    await flushPromises()

    await w.findAll('button').find(b => b.text() === '暂存').trigger('click')
    await flushPromises()

    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/git/ops',
      { op: 'stage', arg: 'src/b.js' })
    expect(w.text()).toContain('已提交，等你在审批卡上批准后执行')
  })

  it('点分支的「切换」提交 op=checkout，arg 是分支名', async () => {
    api.mockImplementation(defaultApi())
    const w = mountGit()
    await flushPromises()

    await w.findAll('button').find(b => b.text() === '切换').trigger('click')
    await flushPromises()

    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/git/ops',
      { op: 'checkout', arg: 'feature/x' })
  })

  it('提交信息为空时提交按钮禁用，填上后提交 op=commit', async () => {
    api.mockImplementation(defaultApi())
    const w = mountGit()
    await flushPromises()

    const commitBtn = w.findAll('button').find(b => b.text() === '提交')
    expect(commitBtn.attributes('disabled')).toBeDefined()

    await w.get('textarea[aria-label="提交信息"]').setValue('补一个说明')
    await flushPromises()
    expect(w.findAll('button').find(b => b.text() === '提交').attributes('disabled')).toBeUndefined()

    await w.findAll('button').find(b => b.text() === '提交').trigger('click')
    await flushPromises()

    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/git/ops',
      { op: 'commit', arg: '补一个说明' })
  })

  it('轮询到终态 ok 后显示成功并刷新仓库状态', async () => {
    let submitted = false
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url.endsWith('/git/status')) {
        return { status: repoStatus({ files: [{ path: 'src/b.js', status: ' M', stage: 'unstaged' }] }) }
      }
      if (method === 'GET' && url.endsWith('/git/branches')) return { branches: [] }
      if (method === 'GET' && url.includes('/git/log')) return { commits: [] }
      if (method === 'GET' && url.endsWith('/git/ops')) {
        // 提交之前都还在等批准；提交之后下一轮轮询才落地
        const state = submitted ? 'ok' : 'awaiting'
        const msg = submitted ? '暂存成功' : ''
        return { ops: [{ id: 'op1', op: 'stage', arg: 'src/b.js', state, message: msg }] }
      }
      if (method === 'POST' && url.endsWith('/git/ops')) {
        submitted = true
        return { op: { id: 'op1', op: 'stage', arg: 'src/b.js', state: 'awaiting' },
          message: '已提交，等你在审批卡上批准后执行' }
      }
      return {}
    })
    const w = mountGit({ thread, pollInterval: 5 })
    await flushPromises()

    await w.findAll('button').find(b => b.text() === '暂存').trigger('click')
    await flushPromises()
    // 等轮询那一拍过去
    await new Promise(resolve => setTimeout(resolve, 80))
    await flushPromises()

    expect(w.text()).toContain('暂存成功')
    // 终态之后要把仓库状态重拉一遍
    const statusCalls = api.mock.calls.filter(c => c[0] === 'GET' && c[1].endsWith('/git/status'))
    expect(statusCalls.length).toBeGreaterThan(1)
  })

  it('终态 failed 时显示后端给的 message 与输出', async () => {
    let submitted = false
    api.mockImplementation(async (method, url) => {
      if (method === 'GET' && url.endsWith('/git/status')) return { status: repoStatus() }
      if (method === 'GET' && url.endsWith('/git/branches')) return { branches: [] }
      if (method === 'GET' && url.includes('/git/log')) return { commits: [] }
      if (method === 'GET' && url.endsWith('/git/ops')) {
        // 提交之前在等批准；提交之后下一轮轮询拿到失败
        const state = submitted ? 'failed' : 'awaiting'
        const msg = submitted ? '提交失败：没有暂存的改动' : ''
        const out = submitted ? 'nothing added to commit' : ''
        return { ops: [{ id: 'op1', op: 'commit', arg: 'x', state, message: msg, output: out }] }
      }
      if (method === 'POST' && url.endsWith('/git/ops')) {
        submitted = true
        return { op: { id: 'op1', op: 'commit', arg: 'x', state: 'awaiting' },
          message: '已提交，等你在审批卡上批准后执行' }
      }
      return {}
    })
    const w = mountGit({ thread, pollInterval: 5 })
    await flushPromises()

    await w.get('textarea[aria-label="提交信息"]').setValue('一个说明')
    await flushPromises()
    await w.findAll('button').find(b => b.text() === '提交').trigger('click')
    await flushPromises()
    await new Promise(resolve => setTimeout(resolve, 80))
    await flushPromises()

    expect(api).toHaveBeenCalledWith('POST', '/api/acp/threads/th1/git/ops',
      { op: 'commit', arg: '一个说明' })
    expect(w.text()).toContain('提交失败：没有暂存的改动')
    expect(w.text()).toContain('nothing added to commit')
    expect(w.find('pre.wg-out').exists()).toBe(true)
  })
})

describe('Git 管理 · 切会话', () => {
  it('切 thread 后重新拉数据，不残留上一个 thread 的分支列表', async () => {
    api.mockImplementation(async (method, url) => {
      const id = (url.match(/threads\/([^/]+)\/git/) || [])[1]
      if (url.endsWith('/git/status')) return { status: repoStatus({ branch: id === 'th1' ? 'alpha-main' : 'beta-main' }) }
      if (url.endsWith('/git/branches')) {
        return { branches: id === 'th1'
          ? [{ name: 'alpha-main', current: true }, { name: 'alpha-feature', current: false }]
          : [{ name: 'beta-main', current: true }] }
      }
      if (url.includes('/git/log')) return { commits: [] }
      if (url.endsWith('/git/ops')) return { ops: [] }
      return {}
    })
    const w = mountGit()
    await flushPromises()
    expect(w.text()).toContain('alpha-feature')

    await w.setProps({ thread: { id: 'th2', cwd: '/home/u/other', agent: 'codex' } })
    await flushPromises()

    expect(api).toHaveBeenCalledWith('GET', '/api/acp/threads/th2/git/status')
    expect(api).toHaveBeenCalledWith('GET', '/api/acp/threads/th2/git/branches')
    expect(w.text()).toContain('beta-main')
    expect(w.text()).not.toContain('alpha-feature')
  })

  it('没有 thread.id 时不发任何请求，只显示「没有会话」', async () => {
    const w = mountGit({ thread: null })
    await flushPromises()
    expect(api).not.toHaveBeenCalled()
    expect(w.text()).toContain('没有会话')
  })
})
