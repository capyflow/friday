import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import StatusSection from '../src/components/settings/StatusSection.vue'
import SettingsModal from '../src/components/SettingsModal.vue'
import { api } from '../src/api.js'
import { fmtTokens } from '../src/format.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
// StatusSection 经 anim.js 做数字滚动，模块加载即调 window.matchMedia（jsdom 未实现）。
vi.mock('../src/anim.js', () => ({ runCountUp: vi.fn() }))

const localToday = new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 10)

const USAGE = {
  days: [{ date: localToday, calls: 3, in: 1200, out: 300 }],
  total: { date: 'total', calls: 3, in: 1200, out: 300 },
  currency: 'CNY',
  byModel: [
    { key: 'deepseek-chat', calls: 2, in: 1000, out: 280, cacheRead: 400, zeroCalls: 0, cost: 0.0042, priced: true },
    { key: 'mystery-llm', calls: 1, in: 200, out: 20, cacheRead: 0, zeroCalls: 1, cost: 0, priced: false },
  ],
  byOrigin: [{ key: 'web', calls: 2, in: 1000, out: 280 }, { key: 'task', calls: 1, in: 200, out: 20 }],
  bySession: [{ key: 'web', calls: 3, in: 1200, out: 300 }],
  byRun: [{ key: 'goal:g1', calls: 1, in: 200, out: 20 }],
}

const COST_OK = {
  state: 'ok', ratio: 0.35, tightest: 'dailyTokens', todayTokens: 1500, todayCost: 0.0042,
  monthlyTokens: 9000, monthlyCost: 0.02, currency: 'CNY',
  budget: { softRatio: 0.8, dailyTokens: 2000000 },
  unknownModels: [], zeroCalls: 0, manualPaused: false, proactivePaused: false,
}

function statusWith(cost) {
  return {
    provider: { model: 'deepseek-chat', baseURL: 'https://api.example.com/v1' },
    apiKeyConfigured: true,
    tasks: { total: 2, byKind: { once: 1, cron: 1 } },
    todos: { pending: 3 },
    memories: 4,
    uptimeSeconds: 60,
    files: { tasks: 'tasks.json', todos: 'todos.json', memories: 'memories' },
    cost,
  }
}

let wrapper
let cost = COST_OK
let usagePayload = USAGE

function routeApi() {
  return vi.fn((method, url) => {
    if (url === '/api/usage') return Promise.resolve(usagePayload)
    if (url === '/api/cost') return Promise.resolve({ cost })
    if (url === '/api/status') return Promise.resolve(statusWith(cost))
    if (url.startsWith('/api/cost/')) return Promise.resolve({ message: '已处理' })
    return Promise.resolve({})
  })
}

beforeEach(() => {
  cost = { ...COST_OK }
  usagePayload = { ...USAGE }
  api.mockReset().mockImplementation(routeApi())
})
afterEach(() => { wrapper?.unmount(); wrapper = null; document.body.innerHTML = '' })

const textOf = el => (el ? el.textContent.trim() : '')
const byText = (sel, label) => Array.from(document.querySelectorAll(sel)).find(el => textOf(el) === label)

async function mountStatus() {
  // StatusSection 不用 Teleport，需挂到 body 才能用 document 查询（与弹窗节一致的查法）。
  wrapper = mount(StatusSection, { attachTo: document.body })
  await flushPromises()
}

async function openCostSection() {
  wrapper = mount(SettingsModal)
  await flushPromises()
  byText('.set-nav-item', '成本与预算').click()
  await flushPromises()
}

describe('fmtTokens 单位缩写', () => {
  it.each([
    [0, '0'], [999, '999'],
    [1234, '1.2k'], [1500, '1.5k'], [999950, '1百万'],
    [1234567, '1.23百万'], [25000000, '25百万'],
    [123456789, '1.23亿'], [1500000000, '15亿'],
  ])('%i → %s', (n, s) => expect(fmtTokens(n)).toBe(s))
})

describe('运行状态的成本面板', () => {
  it('预算正常时显示今日用量（token 缩写）与最紧一档占比，不出现金额', async () => {
    await mountStatus()
    const banner = document.querySelector('.cost-banner')
    expect(banner.className).not.toContain('over')
    expect(banner.textContent).toContain('预算正常')
    expect(banner.textContent).toContain('最紧一档 35%')
    expect(banner.textContent).toContain('今日 1.5k tokens')
    expect(banner.textContent).not.toContain('0.0042')
  })

  it('warn 与 over 分别是不同边框，over 说明主动任务已停', async () => {
    cost = { ...COST_OK, state: 'warn', ratio: 0.86 }
    await mountStatus()
    expect(document.querySelector('.cost-banner').className).toContain('warn')
    wrapper.unmount(); wrapper = null

    cost = { ...COST_OK, state: 'over', ratio: 1.04, proactivePaused: true }
    await mountStatus()
    const b = document.querySelector('.cost-banner')
    expect(b.className).toContain('over')
    expect(b.textContent).toContain('主动任务已暂停')
    expect(b.textContent).toContain('对话与提醒不受影响')
  })

  it('未回传用量必须说清楚，且不再有配价相关的文案', async () => {
    cost = { ...COST_OK, unknownModels: ['mystery-llm'], zeroCalls: 1 }
    await mountStatus()
    const banner = document.querySelector('.cost-banner')
    expect(banner.textContent).toContain('1 次调用未回传用量')
    expect(banner.textContent).not.toContain('未配价')
    expect(document.querySelector('.dim-panel').textContent).not.toContain('补单价')
  })

  it('默认按模型；任何维度都没有金额列', async () => {
    await mountStatus()
    expect(byText('.dim-tab', '按模型').className).toContain('active')
    expect(document.querySelector('.dim-panel thead').textContent).not.toContain('金额')
    expect(document.querySelectorAll('.dim-panel tbody tr')).toHaveLength(2)

    byText('.dim-tab', '按来源').click()
    await flushPromises()
    const rows = Array.from(document.querySelectorAll('.dim-panel tbody tr')).map(r => r.textContent)
    expect(rows.join(' ')).toContain('web')
    expect(rows.join(' ')).toContain('task')
  })

  it('闸门未装配（status 无 cost）时不显示横幅，用量表格照常', async () => {
    cost = undefined
    await mountStatus()
    expect(document.querySelector('.cost-banner')).toBeNull()
    expect(document.querySelector('.dim-panel')).toBeTruthy()
  })
})

describe('设置 · 成本与预算（token-only）', () => {
  it('进入该节拉成本态势与用量账本，不再请求价格表', async () => {
    await openCostSection()
    expect(api).toHaveBeenCalledWith('GET', '/api/cost')
    expect(api).toHaveBeenCalledWith('GET', '/api/usage')
    expect(api).not.toHaveBeenCalledWith('GET', '/api/cost/pricing')
    expect(document.querySelector('.cost-state').textContent).toContain('预算正常')
    expect(document.querySelector('.cs-grid').textContent).toContain('1.5k')
    expect(document.body.textContent).not.toContain('今日金额')
    expect(document.querySelector('.budget-list')).toBeNull() // 预算额度暂时隐藏
  })

  it('热力图渲染 12 周网格，有用量的一天带缩写 token 提示', async () => {
    await openCostSection()
    const cells = document.querySelectorAll('.heat-col .hc')
    expect(cells).toHaveLength(84) // jsdom 量不到宽度，回落最小窗口 12 周
    const day = Array.from(cells).find(c => (c.getAttribute('title') || '').includes(localToday))
    expect(day).toBeTruthy()
    expect(day.getAttribute('title')).toContain('1.5k tokens')
    expect(day.className).toContain('l4')
  })

  it('用量账本加载失败给出可读提示而不是空白热力图', async () => {
    api.mockImplementation((method, url) => {
      if (url === '/api/cost') return Promise.resolve({ cost })
      if (url === '/api/usage') return Promise.reject(new Error('账本损坏'))
      return Promise.resolve({})
    })
    await openCostSection()
    expect(document.body.textContent).toContain('用量账本加载失败：账本损坏')
  })
})
