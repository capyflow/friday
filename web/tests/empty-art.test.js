import { readFileSync, readdirSync, statSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import EmptyState from '../src/components/art/EmptyState.vue'

const SRC = `${process.cwd()}/src`
const KINDS = ['trace', 'todo', 'wiki', 'wikiPick', 'notif', 'mcp', 'usage',
               'session', 'skill', 'schedule', 'error']

function vueFiles(dir) {
  return readdirSync(dir).flatMap((name) => {
    const p = `${dir}/${name}`
    return statSync(p).isDirectory() ? vueFiles(p) : name.endsWith('.vue') ? [p] : []
  })
}

describe('空状态插画', () => {
  it.each(KINDS)('%s 帧渲染线稿与插槽文案', (kind) => {
    const w = mount(EmptyState, { props: { kind }, slots: { default: '还没有内容' } })
    expect(w.find('.art svg').exists()).toBe(true)
    expect(w.find('.art svg').html()).toContain('<path')
    expect(w.get('.txt').text()).toBe('还没有内容')
    w.unmount()
  })

  // kind 写错不会报错、只会静默渲染空白，故在此把调用点与帧表对齐。
  it('调用点的 kind 都有对应帧', () => {
    const frames = readFileSync(`${SRC}/components/art/EmptyState.vue`, 'utf8')
    const used = vueFiles(SRC).flatMap((f) =>
      [...readFileSync(f, 'utf8').matchAll(/<EmptyState\b[^>]*kind="(\w+)"/g)].map((m) => m[1]))
    // 13 处：内置终端页多了一处"不可用"空态（kind=error）
    expect(used).toHaveLength(13)
    expect(used.filter((k) => !KINDS.includes(k))).toEqual([])
    used.forEach((k) => expect(frames).toContain(`  ${k}: `))
  })
})
