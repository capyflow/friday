import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import StatusView from '../src/components/StatusView.vue'
import SettingsModal from '../src/components/SettingsModal.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
// StatusView 经 anim.js 做数字滚动，模块加载即调 window.matchMedia（jsdom 未实现）。
vi.mock('../src/anim.js', () => ({ runCountUp: vi.fn() }))

const USAGE = {
  days: [{ date: '2026-10-14', calls: 3, in: 1200, out: 300 }],
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
const PRICING_OK = {
  currency: 'CNY',
  prices: [{ model: 'deepseek-chat', inputPerMTok: 2, outputPerMTok: 8, cacheReadPerMTok: 0.5 }],
  unknownModels: ['mystery-llm'],
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
let pricing = PRICING_OK

function routeApi() {
  return vi.fn((method, url) => {
    if (url === '/api/usage') return Promise.resolve(USAGE)
    if (url === '/api/cost') return Promise.resolve({ cost })
    if (url === '/api/cost/pricing') return Promise.resolve(pricing)
    if (url === '/api/status') return Promise.resolve(statusWith(cost))
    if (url.startsWith('/api/cost/')) return Promise.resolve({ message: '已处理' })
    return Promise.resolve({})
  })
}

beforeEach(() => {
  cost = { ...COST_OK }
  pricing = { ...PRICING_OK }
  api.mockReset().mockImplementation(routeApi())
})
afterEach(() => { wrapper?.unmount(); wrapper = null; document.body.innerHTML = '' })

const textOf = el => (el ? el.textContent.trim() : '')
const byText = (sel, label) => Array.from(document.querySelectorAll(sel)).find(el => textOf(el) === label)
const btnWith = label => Array.from(document.querySelectorAll('button')).find(b => b.textContent.includes(label))

async function mountStatus() {
  // StatusView 不用 Teleport，需挂到 body 才能用 document 查询（与弹窗节一致的查法）。
  wrapper = mount(StatusView, { attachTo: document.body })
  await flushPromises()
}

async function openCostSection() {
  wrapper = mount(SettingsModal)
  await flushPromises()
  byText('.set-nav-item', '成本与预算').click()
  await flushPromises()
}

describe('运行状态的成本面板', () => {
  it('预算正常时显示今日用量与最紧一档占比', async () => {
    await mountStatus()
    const banner = document.querySelector('.cost-banner')
    expect(banner.className).not.toContain('over')
    expect(banner.textContent).toContain('预算正常')
    expect(banner.textContent).toContain('最紧一档 35%')
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

  it('未配价与未回传用量都必须在页面说清楚，不得装作准确', async () => {
    cost = { ...COST_OK, unknownModels: ['mystery-llm'], zeroCalls: 1 }
    await mountStatus()
    const banner = document.querySelector('.cost-banner')
    expect(banner.textContent).toContain('1 个模型未配价')
    expect(banner.textContent).toContain('1 次调用未回传用量')
    expect(document.querySelector('.dim-panel').textContent).toContain('价格未配置')
    expect(document.querySelector('.dim-panel').textContent).toContain('补单价后才会算出金额')
  })

  it('默认按模型（含金额列），切到按来源后金额列消失', async () => {
    await mountStatus()
    expect(byText('.dim-tab', '按模型').className).toContain('active')
    expect(document.querySelector('.dim-panel thead').textContent).toContain('金额')
    expect(document.querySelectorAll('.dim-panel tbody tr')).toHaveLength(2)

    byText('.dim-tab', '按来源').click()
    await flushPromises()
    expect(document.querySelector('.dim-panel thead').textContent).not.toContain('金额')
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

describe('设置 · 成本与预算', () => {
  it('进入该节才拉成本与价格表，并渲染预算与价格行', async () => {
    await openCostSection()
    expect(api).toHaveBeenCalledWith('GET', '/api/cost')
    expect(api).toHaveBeenCalledWith('GET', '/api/cost/pricing')
    expect(document.querySelector('.cost-state').textContent).toContain('预算正常')
    const rows = document.querySelectorAll('.pr-row')
    expect(rows).toHaveLength(1)
    expect(document.querySelector('.pr-model').value).toBe('deepseek-chat')
    expect(document.body.textContent).toContain('有用量但未配价')
    expect(document.querySelector('.budget-list').textContent).toContain('2000000')
  })

  it('添加未配价模型后保存，提交完整价格表（空单价按 0）', async () => {
    await openCostSection()
    byText('.unknown-box button', '＋ mystery-llm').click()
    await flushPromises()
    expect(document.querySelectorAll('.pr-row')).toHaveLength(2)

    btnWith('保存并热生效').click()
    await flushPromises()
    const call = api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/cost/pricing')
    expect(call).toBeTruthy()
    expect(call[2].prices.map(p => p.model).sort()).toEqual(['deepseek-chat', 'mystery-llm'])
    expect(call[2].prices.find(p => p.model === 'mystery-llm').inputPerMTok).toBe(0)
    expect(call[2].currency).toBe('CNY')
  })

  it('暂停与恢复分别打对应接口', async () => {
    await openCostSection()
    btnWith('暂停主动任务').click()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/cost/pause', {})

    cost = { ...COST_OK, manualPaused: true, proactivePaused: true }
    wrapper.unmount(); wrapper = null; document.body.innerHTML = ''
    await openCostSection()
    const resume = btnWith('恢复主动任务')
    expect(resume).toBeTruthy()
    resume.click()
    await flushPromises()
    expect(api).toHaveBeenCalledWith('POST', '/api/cost/resume', {})
  })

  it('价格表加载失败给出可读错误而不是静默空表', async () => {
    api.mockImplementation((method, url) => {
      if (url === '/api/cost') return Promise.resolve({ cost })
      if (url === '/api/cost/pricing') return Promise.reject(new Error('价格表损坏'))
      return Promise.resolve({})
    })
    await openCostSection()
    expect(document.querySelector('.msg.err').textContent).toContain('价格表加载失败')
  })
})
