<script setup>
// TimelineBlocks 是聊天时间线内容块的共享渲染组件（ChatView 与 AgentView 共用，
// 样式在全局 style.css）：思考块（可折叠，流式一行预览）、工具调用块（一行，
// 可展开结果）与正文块（markdown）按到达顺序排列。
// 块对象是父组件的 reactive 数组元素，这里的 open 切换直接改写它们。
import { renderMD } from '../md.js'
import { blockIndex, thinkPreview } from '../blocks.js'

const props = defineProps({
  blocks: { type: Array, required: true },
  streaming: { type: Boolean, default: false },
  paused: { type: Boolean, default: false },
  round: { type: Number, default: 0 },
})

// 正在写的思考块（本轮那一块且轮次未定稿）：标题实时一行预览、灯泡呼吸。
function liveThink(bi, b) {
  return props.streaming && !props.paused && !b.meta && bi === blockIndex(props, 'think')
}
function thinkTitle(b, bi) {
  if (liveThink(bi, b)) return thinkPreview(b.text) || '思考中…'
  return b.meta ? '思考 · ' + b.meta : '思考'
}
// 工具调用折叠行的可访问标题：出错时把状态说在前面，运行中没有可展开的内容故留空。
function toolTitle(b) {
  if (b.status === 'fail') {
    if (!b.out) return '工具执行出错'
    return b.open ? '收起出错详情' : '工具执行出错，展开详情'
  }
  if (!b.out) return ''
  return b.open ? '收起详情' : '展开详情'
}
// 光标跟着本轮正在写的正文走（思考分片插进来时不该跑到思考行下面）。
function cursorOn(bi) {
  return props.streaming && bi === blockIndex(props, 'text')
}
</script>

<template>
  <template v-for="(b, bi) in blocks" :key="bi">
    <!-- 思考块：收起时一行实时预览（流式）或轮次摘要，点击展开全文 -->
    <div v-if="b.kind === 'think'" class="think" :class="{ open: b.open }">
      <button class="think-head" type="button" :title="b.open ? '收起思考' : '展开思考'"
              @click="b.open = !b.open">
        <span class="step-ico think-ico" :class="{ live: liveThink(bi, b) }" aria-hidden="true">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 14c.2-1 .7-1.7 1.5-2.4 1.1-1 2.5-2.4 2.5-4.6A6 6 0 0 0 6 7c0 2 1.4 3.4 2.5 4.6.8.7 1.3 1.4 1.5 2.4"/><path d="M9 18h6"/><path d="M10 22h4"/></svg>
        </span>
        <span class="think-title">{{ thinkTitle(b, bi) }}</span>
        <svg class="think-chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
      </button>
      <div v-show="b.open" class="think-body">
        <div class="think-text">{{ b.text }}</div>
      </div>
    </div>
    <!-- 工具调用块：收起只报"工具调用"，工具名与参数、结果都收进展开区 -->
    <div v-else-if="b.kind === 'tool'" class="tool"
         :class="{ open: b.open, 'is-fail': b.status === 'fail' }">
      <button class="tool-head" type="button" :disabled="!b.out" :title="toolTitle(b)"
              @click="b.open = b.out ? !b.open : false">
        <span class="step-ico" :class="b.status" aria-hidden="true">
          <i v-if="b.status === 'run'" class="spin step-ring"></i>
          <svg v-else-if="b.status === 'fail'" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/></svg>
          <svg v-else width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/></svg>
        </span>
        <span class="step-name">工具调用</span>
        <svg v-if="b.out" class="think-chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
      </button>
      <div v-show="b.open" class="tool-body">
        <div class="tool-call">
          <span class="step-name">{{ b.name }}</span>
          <span class="step-detail">{{ b.text }}</span>
        </div>
        <div class="tool-out">{{ b.out }}</div>
      </div>
    </div>
    <!-- 正文块 -->
    <div v-else class="turn-text chat-md"><span v-html="renderMD(b.text)"></span><span v-if="cursorOn(bi)" class="cursor"></span></div>
  </template>
</template>
