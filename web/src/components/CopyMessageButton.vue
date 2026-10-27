<script setup>
// 消息复制：一问一答各一个图标按钮（Qoder 风——只在悬停/聚焦时现身，纯图标、无文字）。
// 提问/错误行传 text（整条原文，发出即可复制）；回答行传 blocks，只拼正文块，
// 思考过程与工具调用不进剪贴板，且要等本轮回复结束（streaming 结束）才挂出来。
import { computed, onBeforeUnmount, ref } from 'vue'
import { answerText } from '../blocks.js'
import { toast } from '../store.js'
import { copyText } from '../clipboard.js'

const props = defineProps({
  blocks: { type: Array, default: null },
  text: { type: String, default: '' },
  // answer=回答正文，question=自己的提问，error=错误提示：只影响提示文案。
  kind: { type: String, default: 'answer' },
})

const value = computed(() => (props.blocks ? answerText(props.blocks) : props.text))
const LABEL = { answer: '复制回答正文', question: '复制提问', error: '复制错误信息' }
const hint = computed(() => LABEL[props.kind] || '复制')
const copied = ref(false)
let timer = null

async function copy() {
  if (!value.value) return
  try {
    await copyText(value.value)
    copied.value = true
    clearTimeout(timer)
    timer = setTimeout(() => { copied.value = false }, 1600)
  } catch (e) {
    toast('复制失败：' + (e?.message || e), true)
  }
}
onBeforeUnmount(() => clearTimeout(timer))
</script>

<template>
  <button v-if="value" class="copy-btn" type="button" :title="copied ? '已复制' : hint"
          :aria-label="copied ? '已复制到剪贴板' : hint" @click.stop="copy">
    <svg v-if="copied" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor"
         stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M20 6 9 17l-5-5"/>
    </svg>
    <svg v-else width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor"
         stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <rect x="9" y="9" width="11" height="11" rx="2"/>
      <path d="M5 15V5a2 2 0 0 1 2-2h10"/>
    </svg>
  </button>
</template>
