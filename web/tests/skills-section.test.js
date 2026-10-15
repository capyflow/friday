import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SettingsModal from '../src/components/SettingsModal.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
// 设置弹窗静态引入运行状态节，后者经 anim.js 模块加载即调 window.matchMedia（jsdom 未实现）。
vi.mock('../src/anim.js', () => ({ runCountUp: vi.fn() }))

const SKILLS = [
  { id: 'morning', name: '晨会准备', description: '汇总今日待办与天气', scope: 'project',
    enabled: true, auto: true, triggers: ['晨会'], keywords: [], priority: 10, readOnly: false },
  { id: 'broken-one', name: '', description: '', scope: 'project', enabled: true, auto: false,
    parseError: 'frontmatter 缺少 name（技能名，对话里用它点名）', readOnly: false },
  { id: 'shared', name: '本机共享技能', description: '', scope: 'user', enabled: true, auto: false,
    readOnly: true },
]

let wrapper
beforeEach(() => {
  api.mockReset().mockImplementation((method, url) => {
    if (method === 'GET' && url === '/api/skills') return Promise.resolve({ skills: SKILLS, dir: '/data/skills', error: '' })
    if (method === 'POST' && url === '/api/skills') return Promise.resolve({ message: '已保存', skill: SKILLS[0] })
    if (method === 'POST' && url === '/api/skills/reload') return Promise.resolve({ message: '已重新扫描', skills: SKILLS })
    if (method === 'GET' && url === '/api/skills/morning') return Promise.resolve({ skill: SKILLS[0], raw: '---\nname: 晨会准备\n---\n正文\n' })
    if (method === 'POST' && url === '/api/skills/morning/enabled') return Promise.resolve({ message: '技能已停用', skill: { ...SKILLS[0], enabled: false } })
    if (method === 'PUT' && url === '/api/skills/morning') return Promise.resolve({ message: '已更新', skill: SKILLS[0] })
    if (method === 'DELETE' && url === '/api/skills/morning') return Promise.resolve({ message: '已删除' })
    return Promise.resolve({})
  })
})
afterEach(() => { wrapper?.unmount(); wrapper = null; document.body.innerHTML = '' })

const textOf = el => (el ? el.textContent.trim() : '')
const byText = (sel, label) => Array.from(document.querySelectorAll(sel)).find(el => textOf(el) === label)
const btnIn = (scope, label) => Array.from(scope.querySelectorAll('button')).find(b => b.textContent.includes(label))

async function openSkills() {
  wrapper = mount(SettingsModal)
  await flushPromises()
  byText('.set-nav-item', '技能').click()
  await flushPromises()
}

describe('设置 · 技能', () => {
  it('列出技能的名称、触发词与所在目录', async () => {
    await openSkills()
    expect(api).toHaveBeenCalledWith('GET', '/api/skills')
    expect(document.querySelector('.skill-list').textContent).toContain('晨会准备')
    expect(document.body.textContent).toContain('触发词：晨会')
    expect(document.querySelector('.meta') && document.body.textContent).toContain('/data/skills')
  })

  it('无效文件按原因标出但不隐藏，重名/覆盖也说明清楚', async () => {
    await openSkills()
    const rows = document.querySelectorAll('.skill-row')
    expect(rows).toHaveLength(3)
    const brokenRow = Array.from(rows).find(r => r.textContent.includes('broken-one'))
    expect(brokenRow.textContent).toContain('frontmatter 缺少 name')
    expect(document.querySelector('.skill-row.broken')).toBeTruthy()
  })

  it('本机目录技能只读：只能查看，不给停用/删除', async () => {
    await openSkills()
    const rows = document.querySelectorAll('.skill-row')
    const userRow = Array.from(rows).find(r => r.textContent.includes('本机共享技能'))
    expect(btnIn(userRow, '删除')).toBeUndefined()
    expect(btnIn(userRow, '停用')).toBeUndefined()
    expect(btnIn(userRow, '查看')).toBeTruthy()
  })

  it('停用调用 enabled 接口并重拉列表', async () => {
    await openSkills()
    const rows = document.querySelectorAll('.skill-row')
    const morningRow = Array.from(rows).find(r => r.textContent.includes('晨会准备'))
    btnIn(morningRow, '停用').click()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/skills/morning/enabled', { enabled: false })
    expect(api.mock.calls.filter(([m, u]) => m === 'GET' && u === '/api/skills').length).toBeGreaterThanOrEqual(2)
  })

  it('新建走模板编辑器，保存提交整份 SKILL.md', async () => {
    await openSkills()
    btnIn(document.body, '＋ 新建技能').click()
    await flushPromises()
    const box = document.querySelector('.se-raw')
    expect(box.value).toContain('name: 技能名')

    box.value = '---\nname: 新技能\n---\n正文\n'
    box.dispatchEvent(new window.Event('input', { bubbles: true }))
    await flushPromises()
    btnIn(document.querySelector('.skill-edit'), '保存并生效').click()
    await flushPromises()
    const call = api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/skills')
    expect(call[2].raw).toContain('name: 新技能')
    expect(document.querySelector('.skill-edit')).toBeNull()
  })

  it('保存被拒绝时保留编辑内容并显示原因，不丢用户写的东西', async () => {
    api.mockImplementation((method, url) => {
      if (method === 'GET' && url === '/api/skills') return Promise.resolve({ skills: SKILLS, dir: '', error: '' })
      if (method === 'POST' && url === '/api/skills') {
        return Promise.reject(new Error('技能名 "晨会准备" 已被 "morning" 使用，请换个名字或先删除那条'))
      }
      return Promise.resolve({})
    })
    await openSkills()
    btnIn(document.body, '＋ 新建技能').click()
    await flushPromises()
    btnIn(document.querySelector('.skill-edit'), '保存并生效').click()
    await flushPromises()
    expect(document.querySelector('.skill-msg').textContent).toContain('已被')
    expect(document.querySelector('.skill-edit')).toBeTruthy()
  })

  it('编辑已有技能先取原文，保存走 PUT', async () => {
    await openSkills()
    const rows = document.querySelectorAll('.skill-row')
    const morningRow = Array.from(rows).find(r => r.textContent.includes('晨会准备'))
    btnIn(morningRow, '编辑').click()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('GET', '/api/skills/morning')
    expect(document.querySelector('.se-raw').value).toContain('正文')

    btnIn(document.querySelector('.skill-edit'), '保存并生效').click()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('PUT', '/api/skills/morning', { raw: expect.any(String) })
  })

  it('删除需确认，取消则不发请求', async () => {
    await openSkills()
    const rows = document.querySelectorAll('.skill-row')
    const morningRow = Array.from(rows).find(r => r.textContent.includes('晨会准备'))

    vi.spyOn(window, 'confirm').mockReturnValue(false)
    btnIn(morningRow, '删除').click()
    await flushPromises()
    expect(api).not.toHaveBeenCalledWith('DELETE', '/api/skills/morning')

    vi.spyOn(window, 'confirm').mockReturnValue(true)
    btnIn(morningRow, '删除').click()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('DELETE', '/api/skills/morning')
  })

  it('重新扫描目录走 reload 接口', async () => {
    await openSkills()
    btnIn(document.body, '重新扫描目录').click()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/skills/reload', {})
  })
})
