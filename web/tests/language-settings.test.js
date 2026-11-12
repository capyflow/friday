import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import LanguageSection from '../src/components/settings/LanguageSection.vue'
import SelectBox from '../src/components/SelectBox.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

// 「回复语言」这一节只管一件事：小五按哪个语言回话。
// 读回当前语言，改完保存提交给后端（热生效并写回 config.yaml）。
// 自定义语言名存的是自由文本，组件里用 CUSTOM 这个哨兵值代表「正在填自定义」。
const CUSTOM = '__custom__'

async function show(language = '中文') {
  api.mockImplementation(async (method, url) => {
    if (method === 'GET' && url === '/api/language') return { language }
    if (method === 'POST' && url === '/api/language') return { language, message: '回复语言已改为「' + language + '」' }
    return {}
  })
  const w = mount(LanguageSection)
  await flushPromises()
  return w
}
function saved() { return (api.mock.calls.find(([m, u]) => m === 'POST' && u === '/api/language') || [])[2] }
function select(w) { return w.findComponent(SelectBox) }
async function pick(w, value) { select(w).vm.$emit('update:modelValue', value); await flushPromises() }
async function save(w) {
  await w.findAll('button').find(b => b.text() === '保存并热生效').trigger('click')
  await flushPromises()
}

describe('设置 · 回复语言', () => {
  beforeEach(() => { api.mockReset() })

  it('读回当前语言，默认中文', async () => {
    const w = await show('中文')
    expect(select(w).props('modelValue')).toBe('中文')
    expect(w.text()).toContain('只改回复语言，界面语言不变')
  })

  it('保存把选中的语言提交给后端', async () => {
    const w = await show('中文')
    await pick(w, 'English')
    await save(w)
    expect(saved()).toEqual({ language: 'English' })
  })

  it('存的是自定义语言名时：回填输入框，不显示成第一个预设', async () => {
    const w = await show('Русский')
    expect(select(w).props('modelValue')).toBe(CUSTOM)
    expect(w.get('input[type="text"]').element.value).toBe('Русский')
  })

  it('自定义语言：保存提交输入框里的名字', async () => {
    const w = await show('中文')
    await pick(w, CUSTOM)
    await w.get('input[type="text"]').setValue('粤语')
    await save(w)
    expect(saved()).toEqual({ language: '粤语' })
  })

  it('自定义语言名为空：不提交并说明', async () => {
    const w = await show('中文')
    await pick(w, CUSTOM)
    await w.get('input[type="text"]').setValue('   ')
    await save(w)
    expect(saved()).toBeUndefined()
    expect(w.text()).toContain('语言名不能为空')
  })
})
