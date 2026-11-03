import { readFileSync, readdirSync, statSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SelectBox from '../src/components/SelectBox.vue'

// SelectBox 是全站唯一的下拉框形态（原生 <select> 的展开列表由操作系统渲染，样式改不动）。
// 这里守住替换原生控件时补上的三件事：禁用项选不中、键盘跳过禁用项、属性透传。

const SRC = `${process.cwd()}/src`
const OPTS = [
  { value: 'ask', label: '每次都问（推荐）' },
  { value: 'readonly', label: '只读' },
  { value: 'auto', label: '自动放行', disabled: true },
]

function vueFiles(dir) {
  return readdirSync(dir).flatMap((name) => {
    const p = `${dir}/${name}`
    return statSync(p).isDirectory() ? vueFiles(p) : name.endsWith('.vue') ? [p] : []
  })
}

// teleport 原地渲染：弹层是组件的第二个根节点，不 stub 就查不到
function mountBox(props = {}, attrs = {}) {
  return mount(SelectBox, {
    props: { modelValue: 'ask', options: OPTS, ariaLabel: '档位', ...props },
    attrs,
    global: { stubs: { teleport: true } },
  })
}
async function open(w) {
  await w.get('.selbox-trigger').trigger('click')
  await flushPromises()
  return w.findAll('.selbox-opt')
}

describe('下拉框组件', () => {
  // 约定靠机器守：漏一个原生 select 就会在页面上冒出一块系统默认样式。
  it('src 下不再有原生 <select>', () => {
    const hits = vueFiles(SRC)
      .filter(f => !f.endsWith('components/SelectBox.vue'))
      .filter(f => /<select[\s>]/.test(readFileSync(f, 'utf8')))
      .map(f => f.replace(`${process.cwd()}/`, ''))
    expect(hits).toEqual([])
  })

  it('收起态显示选中档的文案，没选中时回落到第一个可选档', () => {
    expect(mountBox().get('.selbox-label').text()).toBe('每次都问（推荐）')
    expect(mountBox({ modelValue: '' }).get('.selbox-label').text()).toBe('每次都问（推荐）')
    const onlyDisabled = mountBox({
      modelValue: '',
      options: [{ value: 'x', label: '已停用', disabled: true }],
    })
    expect(onlyDisabled.get('.selbox-label').text()).toBe('已停用')
  })

  it('禁用项列出来但点不动：不 emit，选中值不变', async () => {
    const w = mountBox()
    const opts = await open(w)
    expect(opts).toHaveLength(3)
    expect(opts[2].text()).toContain('自动放行')
    expect(opts[2].element.disabled).toBe(true)
    // VTU 的 trigger 会跳过 disabled 元素，手动派发才测得到组件里那道挡门
    opts[2].element.dispatchEvent(new window.MouseEvent('click', { bubbles: true }))
    await flushPromises()
    expect(w.emitted('update:modelValue')).toBeFalsy()
  })

  it('键盘上下键跳过禁用项，落在禁用项上按回车也不选', async () => {
    const w = mountBox({ modelValue: 'readonly' })
    await open(w)
    const trigger = w.get('.selbox-trigger')
    // 只读（第 2 项）往下走应跨过「自动放行」回到第 1 项
    await trigger.trigger('keydown', { key: 'ArrowDown' })
    await flushPromises()
    expect(w.findAll('.selbox-opt')[0].classes()).toContain('active')
    // End 直接停在禁用项上，回车不该把它当选择
    await trigger.trigger('keydown', { key: 'End' })
    await flushPromises()
    expect(w.findAll('.selbox-opt')[2].classes()).toContain('active')
    await trigger.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(w.emitted('update:modelValue')).toBeFalsy()
    expect(w.get('.selbox-trigger').attributes('aria-expanded')).toBe('true')
  })

  it('数字档位原样回传（验证超时用的是数值比较）', async () => {
    const w = mountBox({
      modelValue: 300,
      options: [{ value: 120, label: '2 分钟' }, { value: 300, label: '5 分钟' }],
    })
    expect(w.get('.selbox-label').text()).toBe('5 分钟')
    const opts = await open(w)
    await opts[0].trigger('click')
    expect(w.emitted('update:modelValue')).toEqual([[120]])
  })

  it('id 落到触发按钮（label for 才连得上），class 落到外层包装（布局盒）', async () => {
    const w = mountBox({}, { id: 'wsDlgPolicy', class: 'wfd-turn-select' })
    expect(w.get('button').attributes('id')).toBe('wsDlgPolicy')
    expect(w.get('span.selbox').classes()).toContain('wfd-turn-select')
    // 整块禁用：触发器 disabled，点它不会展开
    const off = mountBox({ disabled: true })
    await off.get('.selbox-trigger').trigger('click')
    await flushPromises()
    expect(off.find('.selbox-pop').exists()).toBe(false)
  })
})
