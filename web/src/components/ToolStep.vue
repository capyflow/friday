<script setup>
// 一次工具调用一行：收起就把「哪个工具」说清，点开才给参数与等宽结果。
// own=true（Friday 自己 agent 的工具）按 tool-catalog.js 给中文名与专属图标；
// 外部 ACP agent 的工具名我们改不动，照直显示 + 默认扳手。
// 纯展示：块对象归父组件（TimelineBlocks 的 reactive 数组）所有，这里只读它、
// 点击时发 toggle 事件让父组件改写 open。
// 单独出现时直接摆一行，连续多次时被收进「执行工具 N 次」那一叠里；和思考连在一起
// 时也可能被并进「N 次思考 · M 次工具调用」的过程行，点摘要才逐条列出。
import { computed } from 'vue'
import { toolDisplay } from '../tool-catalog.js'

const props = defineProps({
  b: { type: Object, required: true },
  own: { type: Boolean, default: false },
})
defineEmits(['toggle'])

const shown = computed(() => toolDisplay(props.b.name, props.own))

// 有工具名/参数/结果就能展开（运行中也允许看参数）。
function toolHasBody(b) {
  return !!(b.name || b.text || b.out)
}
// 可访问标题：出错时把状态说在前面，没有可展开内容时留空。
function toolTitle(b) {
  if (!toolHasBody(b)) return ''
  if (b.status === 'fail') return b.open ? '收起出错详情' : '工具执行出错，展开详情'
  return b.open ? '收起详情' : '展开详情'
}
</script>

<template>
  <div class="tool" :class="{ open: b.open, 'is-fail': b.status === 'fail' }">
    <button class="tool-head" type="button" :disabled="!toolHasBody(b)" :title="toolTitle(b)"
            @click="$emit('toggle')">
      <span class="step-ico" :class="b.status" aria-hidden="true">
        <!-- 运行中与失败仍走状态前导（转环 / 告警圈），定稿后才换成工具自己的图标 -->
        <i v-if="b.status === 'run'" class="spin step-ring"></i>
        <svg v-else-if="b.status === 'fail'" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/></svg>
        <svg v-else width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path v-for="(d, di) in shown.icon" :key="di" :d="d"/></svg>
      </span>
      <span class="step-name">{{ shown.label }}</span>
      <svg v-if="toolHasBody(b)" class="think-chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
    </button>
    <div v-show="b.open" class="tool-body">
      <div class="tool-call">
        <span class="step-name">{{ b.name }}</span>
        <span class="step-detail">{{ b.text }}</span>
      </div>
      <div v-if="b.out" class="tool-out">{{ b.out }}</div>
    </div>
  </div>
</template>
