import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { ref } from 'vue'
import ImageLightbox from '../src/components/ImageLightbox.vue'
import { setToken } from '../src/auth.js'

// ImageLightbox 是附件的全图预览层。守四件事：
//   1) 地址经 mediaURL——<img> 带不了 Authorization，只能靠 ?token=；
//   2) 多张可翻（按钮 + ←/→，到头循环），只有一张时不铺翻页条；
//   3) Esc 只关预览：document 上还挂着 App.vue 的关设置弹窗、WorkspaceView 的
//      停止本轮/退全屏，预览层必须把这一键吃掉；
//   4) 焦点进得来、还得得回去——不然键盘用户关掉图就掉回 body。
const UPLOADS = ['/api/upload/u1.png', '/api/upload/u2.png', '/api/upload/u3.png']

let wrapper

beforeEach(() => {
  try { localStorage.clear() } catch { /* 环境没给 localStorage 也不拦用例 */ }
  setToken('jwt-lb')
  // attachTo：jsdom 里只有挂进 document 的元素才吃得到 focus()
  wrapper = mount(ImageLightbox, { attachTo: document.body })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  document.body.innerHTML = ''
})

// press 也等一帧：键盘事件同步改状态，DOM 要到下一个 tick 才跟上
async function press(key) {
  document.dispatchEvent(new window.KeyboardEvent('keydown', { key, bubbles: true, cancelable: true }))
  await flushPromises()
}

describe('附件全图预览', () => {
  it('没打开时不占层，show 之后按当前这张出图', async () => {
    expect(wrapper.find('.lb-mask').exists()).toBe(false)
    wrapper.vm.show(UPLOADS, 1)
    await flushPromises()
    expect(wrapper.get('.lb-img').attributes('src')).toBe('/api/upload/u2.png?token=jwt-lb')
    expect(wrapper.get('.lb-count').text()).toBe('2 / 3')
  })

  it('空列表不打开，越界下标收进范围内', async () => {
    wrapper.vm.show([], 0)
    await flushPromises()
    expect(wrapper.find('.lb-mask').exists()).toBe(false)

    wrapper.vm.show(UPLOADS, 99)
    await flushPromises()
    expect(wrapper.get('.lb-img').attributes('src')).toContain('u3.png')

    wrapper.vm.show('/api/upload/single.png')
    await flushPromises()
    expect(wrapper.get('.lb-img').attributes('src')).toContain('single.png')
    // 单张不铺翻页条
    expect(wrapper.find('.lb-bar').exists()).toBe(false)
  })

  it('左右翻页按按钮和方向键都走，翻到头回到另一侧', async () => {
    wrapper.vm.show(UPLOADS, 2)
    await flushPromises()
    const src = () => wrapper.get('.lb-img').attributes('src')

    await wrapper.get('.lb-nav[aria-label="上一张"]').trigger('click')
    expect(src()).toContain('u2.png')
    await wrapper.get('.lb-nav[aria-label="下一张"]').trigger('click')
    expect(src()).toContain('u3.png')
    // → 到头循环回第一张
    await press('ArrowRight')
    expect(src()).toContain('u1.png')
    await press('ArrowLeft')
    expect(src()).toContain('u3.png')
    expect(wrapper.get('.lb-count').text()).toBe('3 / 3')
  })

  it('Esc、关闭键、点遮罩都只关预览，且不再往下传', async () => {
    const later = vi.fn() // 模拟后挂上来的页面级 Esc 处理（App.vue / WorkspaceView）
    document.addEventListener('keydown', later)
    wrapper.vm.show(UPLOADS, 0)
    await flushPromises()

    await press('Escape')
    expect(wrapper.find('.lb-mask').exists()).toBe(false)
    expect(later).not.toHaveBeenCalled()
    document.removeEventListener('keydown', later)

    wrapper.vm.show(UPLOADS, 0)
    await flushPromises()
    await wrapper.get('.lb-close').trigger('click')
    expect(wrapper.find('.lb-mask').exists()).toBe(false)

    wrapper.vm.show(UPLOADS, 0)
    await flushPromises()
    await wrapper.get('.lb-mask').trigger('click')
    expect(wrapper.find('.lb-mask').exists()).toBe(false)
  })

  it('关闭后焦点还给触发它的那颗缩略图', async () => {
    const host = mount({
      components: { ImageLightbox },
      setup() {
        const box = ref(null)
        const open = () => box.value.show(UPLOADS, 0)
        return { box, open }
      },
      template: '<button aria-label="预览附件" @click="open">图</button><ImageLightbox ref="box" />',
    }, { attachTo: document.body })
    const trigger = host.get('button[aria-label="预览附件"]').element
    trigger.focus()
    await host.get('button[aria-label="预览附件"]').trigger('click')
    await flushPromises()
    expect(document.activeElement).toBe(host.get('.lb-close').element)

    await press('Escape')
    expect(document.activeElement).toBe(trigger)
    host.unmount()
  })
})
