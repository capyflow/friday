<script setup>
// ChatComposer 是聊天/智能体两页共用的输入框（两行结构：输入独占一行 +
// 底部工具条）。外观与交互统一在这里：Enter 发送 / Shift+Enter 换行、自动增高、
// 发送键一位多态（发送/停止/暂停/继续由父组件经 send-label 决定语义）。
// 各页差异全部走插槽：#previews 附件预览条、#left 工具条左侧（附件/模式/权限等 chips）。
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

const props = defineProps({
  modelValue: { type: String, default: '' },
  placeholder: { type: String, default: '' },
  ariaLabel: { type: String, required: true },
  // busy=回复进行中：输入禁用、发送键显示停止图标并发 stop 事件
  busy: { type: Boolean, default: false },
  // disabled=整条输入栏禁用（如清空历史中），优先于 busy
  disabled: { type: Boolean, default: false },
  sendLabel: { type: String, default: '发送' },
  sendDisabled: { type: Boolean, default: false },
})
const emit = defineEmits(['update:modelValue', 'primary', 'paste'])

const inputEl = ref(null)
function autoGrow() {
  const el = inputEl.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = Math.min(el.scrollHeight, 160) + 'px'
}
watch(() => props.modelValue, v => { if (!v) nextTick(autoGrow) }) // 外部清空（发送后）回落一行

function onInput(e) {
  emit('update:modelValue', e.target.value)
  autoGrow()
}
function onKeydown(e) {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault()
    if (!props.busy) emit('primary') // busy 时 Enter 不动作（停止只走按钮，语义由父组件定）
  }
}
function focus() { inputEl.value && inputEl.value.focus() }
defineExpose({ focus })

/* 工具条 chip 只按自然宽度排，装不下就横向滚动（不收缩成省略号）。滚动条本身
   不显示，所以：滚轮/触控板悬停在 chip 上时把纵向滚动转成横向；两端按实际
   滚动位置打 can-l/can-r，用渐隐提示还有内容。 */
const barEl = ref(null)
const barEdges = ref({ left: false, right: false })
let barRO = null
let barMO = null
function syncBar() {
  const el = barEl.value
  if (!el) return
  const max = el.scrollWidth - el.clientWidth
  barEdges.value = { left: el.scrollLeft > 1, right: max > 1 && el.scrollLeft < max - 1 }
}
function onBarWheel(e) {
  const el = barEl.value
  if (!el) return
  const max = el.scrollWidth - el.clientWidth
  if (max <= 0) return
  const delta = Math.abs(e.deltaX) > Math.abs(e.deltaY) ? e.deltaX : e.deltaY
  if (!delta) return
  const next = Math.min(max, Math.max(0, el.scrollLeft + delta))
  if (next === el.scrollLeft) return // 已经在头/尾：把滚轮让回页面
  e.preventDefault()
  el.scrollLeft = next
}
onMounted(() => {
  syncBar()
  // 容器宽度变化（窗口缩放）与 chip 增减（能力清单异步到）都要重算两端提示
  if (typeof window.ResizeObserver !== 'undefined' && barEl.value) {
    barRO = new window.ResizeObserver(syncBar)
    barRO.observe(barEl.value)
  }
  if (typeof window.MutationObserver !== 'undefined' && barEl.value) {
    barMO = new window.MutationObserver(syncBar)
    barMO.observe(barEl.value, { childList: true, subtree: true, characterData: true, attributes: true })
  }
})
onBeforeUnmount(() => { barRO && barRO.disconnect(); barMO && barMO.disconnect() })
</script>

<template>
  <div class="inputbar" :aria-busy="busy || disabled">
    <!-- 附件预览条：有图时撑起整个输入框（各页自带 #attachPreviews 结构） -->
    <slot name="previews"></slot>
    <!-- 第一行：输入区独占整行并自动增高；操作键全部落到第二行 -->
    <textarea ref="inputEl" rows="1" :value="modelValue" :placeholder="placeholder"
              :aria-label="ariaLabel" :disabled="disabled || busy"
              @keydown="onKeydown" @input="onInput" @paste="$emit('paste', $event)"></textarea>
    <!-- 第二行：左=各页自己的 chips（插槽），右=快捷键提示与主按钮 -->
    <div class="composer-bar">
      <div ref="barEl" class="bar-left"
           :class="{ 'can-l': barEdges.left, 'can-r': barEdges.right }"
           @wheel="onBarWheel" @scroll.passive="syncBar">
        <slot name="left"></slot>
      </div>
      <div class="bar-right">
        <span class="kbd-hint" aria-hidden="true">⏎ 发送 · ⇧⏎ 换行</span>
        <button class="ib-btn send-btn"
                :class="{ 'halt-btn': busy, 'resume-btn': sendLabel === '继续上次任务' }"
                :aria-label="sendLabel" :title="sendLabel" :disabled="sendDisabled"
                @click="$emit('primary')">
          <svg v-if="busy" width="15" height="15" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><rect x="6" y="5" width="4" height="14" rx="1.2"/><rect x="14" y="5" width="4" height="14" rx="1.2"/></svg>
          <svg v-else-if="sendLabel === '继续上次任务'" width="17" height="17" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5.6v12.8c0 .9 1 1.5 1.8 1L20 13a1.2 1.2 0 0 0 0-2L9.8 4.6C9 4.1 8 4.7 8 5.6Z"/></svg>
          <svg v-else width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-send"/></svg>
        </button>
      </div>
    </div>
  </div>
</template>
