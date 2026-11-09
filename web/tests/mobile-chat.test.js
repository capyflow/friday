// 移动端会话配置：模型 / 思考强度在输入框上方排成 chips，点开底部弹层切换。
import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const apiMock = vi.hoisted(() => vi.fn())
const toastMock = vi.hoisted(() => vi.fn())
vi.mock('../src/api.js', () => ({ api: apiMock }))
vi.mock('../src/store.js', () => ({ toast: toastMock }))

import MobileChat from '../src/mobile/MobileChat.vue'

const CAPS = () => [
  { id: 'model', kind: 'model', name: 'model', currentValue: 'a',
    values: [{ value: 'a', name: '模型 A' }, { value: 'b', name: '模型 B' }] },
  { id: 'thought_level', kind: 'thought_level', name: 'thought_level', currentValue: 'low',
    values: [{ value: 'low', name: '低' }, { value: 'high', name: '高' }] },
]

function mockServer() {
  let caps = CAPS()
  apiMock.mockImplementation(async (method, path, body) => {
    if (method === 'GET' && path === '/api/acp/threads/t1') return { thread: { id: 't1', messages: [] } }
    if (method === 'GET' && path === '/api/acp/threads/t1/options') return { capabilities: caps }
    if (method === 'POST' && path === '/api/acp/threads/t1/options') {
      caps = caps.map(c => (c.id === body.id ? { ...c, currentValue: body.value } : c))
      return { capabilities: caps }
    }
    return {}
  })
}

async function mountChat(kind, thread) {
  const w = mount(MobileChat, { props: { kind, thread } })
  await flushPromises()
  return w
}

describe('移动端会话配置', () => {
  beforeEach(() => { apiMock.mockReset(); toastMock.mockClear() })

  it('会话打开后拉能力清单，模型与思考强度各一颗 chip', async () => {
    mockServer()
    const w = await mountChat('thread', { id: 't1', agent: 'codex' })
    const chips = w.findAll('.m-cap-chip')
    expect(chips).toHaveLength(2)
    expect(chips[0].text()).toContain('模型')
    expect(chips[0].text()).toContain('模型 A')
    expect(chips[1].text()).toContain('思考强度')
    expect(apiMock).toHaveBeenCalledWith('GET', '/api/acp/threads/t1/options')
    w.unmount()
  })

  it('点 chip 弹底部弹层，选中后 POST 并更新 chip', async () => {
    mockServer()
    const w = await mountChat('thread', { id: 't1', agent: 'codex' })
    await w.findAll('.m-cap-chip')[0].trigger('click')
    const sheet = w.get('.m-cap-sheet')
    expect(sheet.attributes('aria-label')).toBe('模型')
    const opts = sheet.findAll('.m-opt')
    expect(opts).toHaveLength(2)
    expect(opts[0].classes()).toContain('on')

    await opts[1].trigger('click')
    await flushPromises()
    expect(apiMock).toHaveBeenCalledWith('POST', '/api/acp/threads/t1/options', { id: 'model', value: 'b' })
    expect(w.find('.m-cap-sheet').exists()).toBe(false)
    expect(w.findAll('.m-cap-chip')[0].text()).toContain('模型 B')
    w.unmount()
  })

  it('主会话没有会话级配置：不渲染 chips，也不发 options 请求', async () => {
    mockServer()
    const w = await mountChat('main', null)
    expect(w.find('.m-cap-row').exists()).toBe(false)
    expect(apiMock).not.toHaveBeenCalledWith('GET', '/api/acp/threads/t1/options')
    w.unmount()
  })
})
