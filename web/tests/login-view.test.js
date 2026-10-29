import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import LoginView from '../src/components/LoginView.vue'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))
// LoginView 静态引入 anim.js，模块加载即调 window.matchMedia（jsdom 未实现）。
vi.mock('../src/anim.js', () => ({ authIn: vi.fn() }))

let wrapper
afterEach(() => { wrapper?.unmount(); wrapper = null })

describe('登录页', () => {
  it('用户名输入关闭自动大写与纠错，避免 macOS 把 aaron 建议成 Aaron', () => {
    wrapper = mount(LoginView)
    const input = wrapper.get('#loginUser')
    expect(input.attributes('autocapitalize')).toBe('none')
    expect(input.attributes('autocorrect')).toBe('off')
    expect(input.attributes('spellcheck')).toBe('false')
  })
})
