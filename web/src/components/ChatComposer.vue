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
  // queueOn=busy 时仍允许输入（父组件把"发送"解释为"加入队列"）：
  // 输入框不再禁用、Enter 照样触发 primary、主按钮回到发送形态。
  // 「停止本轮」由父组件另给一个控件，不占这个键。
  queueOn: { type: Boolean, default: false },
  // disabled=整条输入栏禁用（如清空历史中），优先于 busy
  disabled: { type: Boolean, default: false },
  sendLabel: { type: String, default: '发送' },
  sendDisabled: { type: Boolean, default: false },
})
const emit = defineEmits(['update:modelValue', 'primary', 'pause', 'stop', 'paste'])

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

/* 中文/日文输入法组字期间的 Enter 是在"上屏"，不是发送。
   isComposing 在 Chrome/WKWebView 里通常为真，但 Safari 与部分输入法会先
   compositionend 再发这一次 keydown（此时 isComposing 已是 false），所以再维护
   一个组字标志，并延迟到下一个宏任务才清——刚结束组字的那一下回车仍然吞掉。 */
let composing = false
function onCompositionStart() { composing = true }
function onCompositionEnd() {
  // 用 0ms 定时器而不是直接置 false：Safari 把 compositionend 排在 keydown 之前，
  // 同步清掉会把"上屏那次回车"放过去
  setTimeout(() => { composing = false }, 0)
}
function isImeKey(e) {
  return composing || e.isComposing || e.keyCode === 229
}

function onKeydown(e) {
  // Esc：正在回答时中断本轮（有弹层时由父组件的文档级处理先收弹层）
  if (e.key === 'Escape') {
    if (props.busy) {
      e.preventDefault()
      emit('stop')
    }
    return
  }
  if (e.key === 'Enter' && !e.shiftKey) {
    if (isImeKey(e)) return // 组字中/上屏回车：交给输入法，别发送
    e.preventDefault()
    // busy 时 Enter 默认不动作（停止只走按钮/Esc）；父组件开了排队就照常触发
    if (!props.busy || props.queueOn) emit('primary')
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
              :aria-label="ariaLabel" :disabled="disabled || (busy && !queueOn)"
              @keydown="onKeydown" @input="onInput" @paste="$emit('paste', $event)"
              @compositionstart="onCompositionStart" @compositionend="onCompositionEnd"></textarea>
    <!-- 第二行：左=各页自己的 chips（插槽），右=快捷键提示与主按钮 -->
    <div class="composer-bar">
      <div ref="barEl" class="bar-left"
           :class="{ 'can-l': barEdges.left, 'can-r': barEdges.right }"
           @wheel="onBarWheel" @scroll.passive="syncBar">
        <slot name="left"></slot>
      </div>
      <div class="bar-right">
        <span class="kbd-hint" aria-hidden="true">{{ (busy && queueOn ? '⏎ 加入队列 · ⇧⏎ 换行' : '⏎ 发送 · ⇧⏎ 换行') + (busy ? ' · Esc 停止' : '') }}</span>
        <button class="ib-btn send-btn"
                :class="{ 'halt-btn': busy && !queueOn, 'queue-btn': busy && queueOn, 'resume-btn': sendLabel === '继续上次任务' }"
                :aria-label="sendLabel" :title="sendLabel" :disabled="sendDisabled"
                @click="$emit('primary')">
          <svg v-if="busy && !queueOn" width="15" height="15" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><rect x="6" y="5" width="4" height="14" rx="1.2"/><rect x="14" y="5" width="4" height="14" rx="1.2"/></svg>
          <svg v-else-if="busy && queueOn" width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 5v9M8.5 11 12 14.5 15.5 11"/><path d="M5 19h14"/></svg>
          <svg v-else-if="sendLabel === '继续上次任务'" width="17" height="17" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5.6v12.8c0 .9 1 1.5 1.8 1L20 13a1.2 1.2 0 0 0 0-2L9.8 4.6C9 4.1 8 4.7 8 5.6Z"/></svg>
          <svg v-else width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-send"/></svg>
        </button>
      </div>
    </div>
  </div>
</template>
