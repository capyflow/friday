<script setup>
// ImageLightbox 是附件图片的全图预览层：对话里（或输入区待发缩略图）点一张图，
// 就把这一组图摊开看原图，多张可左右翻。
// 约定：
//   - 地址一律经 mediaURL()——/api/upload/ 的 GET 允许 ?token=，<img> 带不了自定义头；
//   - 一层浮层只管一件事：Esc / 点遮罩 / 关闭键都只关预览，且吃掉事件，
//     不让页面级 Esc（停止本轮、退全屏、关设置弹窗）顺手把下层也收了；
//   - 打开时把焦点收进关闭键，关闭后还给触发它的那颗缩略图，键盘用户不会掉焦点。
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { mediaURL } from '../auth.js'

const open = ref(false)
const urls = ref([])
const idx = ref(0)
const closeBtn = ref(null)
let lastFocus = null

const src = computed(() => urls.value[idx.value] || '')
const count = computed(() => urls.value.length)

function show(list, at = 0) {
  const items = (Array.isArray(list) ? list : [list]).filter(Boolean)
  if (!items.length) return
  lastFocus = document.activeElement
  urls.value = items
  idx.value = Math.min(Math.max(Number(at) || 0, 0), items.length - 1)
  open.value = true
  nextTick(() => closeBtn.value?.focus())
}

function close() {
  if (!open.value) return
  open.value = false
  urls.value = []
  idx.value = 0
  if (lastFocus && typeof lastFocus.focus === 'function') lastFocus.focus()
  lastFocus = null
}

function step(d) {
  if (count.value < 2) return
  idx.value = (idx.value + d + count.value) % count.value
}

function onKey(e) {
  if (!open.value) return
  if (e.key === 'Escape') {
    // stopImmediatePropagation：同层的 App.vue / WorkspaceView 也监听 document 的 Esc
    e.preventDefault()
    e.stopImmediatePropagation()
    close()
  } else if (e.key === 'ArrowLeft') {
    e.preventDefault()
    step(-1)
  } else if (e.key === 'ArrowRight') {
    e.preventDefault()
    step(1)
  }
}

onMounted(() => document.addEventListener('keydown', onKey))
onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKey)
  close()
})

defineExpose({ show, close })
</script>

<template>
  <div v-if="open" class="lb-mask" role="dialog" aria-modal="true" aria-label="附件预览"
       @click.self="close">
    <button ref="closeBtn" class="lb-close" type="button" aria-label="关闭预览" title="关闭（Esc）"
            @click="close">
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
    </button>
    <div class="lb-stage">
      <img class="lb-img" :src="mediaURL(src)" alt="附件原图">
    </div>
    <div v-if="count > 1" class="lb-bar">
      <button class="lb-nav" type="button" aria-label="上一张" title="上一张（←）" @click="step(-1)">
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
             stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m15 18-6-6 6-6"/></svg>
      </button>
      <span class="lb-count">{{ idx + 1 }} / {{ count }}</span>
      <button class="lb-nav" type="button" aria-label="下一张" title="下一张（→）" @click="step(1)">
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
             stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg>
      </button>
    </div>
  </div>
</template>
