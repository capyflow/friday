<script setup>
// TimelineBlocks 是聊天时间线内容块的共享渲染组件（ChatView 与 WorkspaceView 共用，
// 样式在全局 style.css）：思考块（可折叠，流式一行预览）、工具调用块与正文块
// 按到达顺序排列。连续多次工具调用会归成一叠（「执行工具 N 次」），点开才逐条列出，
// 每条仍是可再展开的 ToolStep。块对象是父组件的 reactive 数组元素，这里的开合切换
// 直接改写它们。
import { computed } from 'vue'
import { renderMD } from '../md.js'
import ToolStep from './ToolStep.vue'
import { blockIndex, thinkPreview } from '../blocks.js'

const emit = defineEmits(['preview'])

const props = defineProps({
  blocks: { type: Array, required: true },
  streaming: { type: Boolean, default: false },
  paused: { type: Boolean, default: false },
  round: { type: Number, default: 0 },
  // 这些工具调用是不是 Friday 自己 agent 发的（ChatView）。ACP 会话页不传，
  // 外部 agent 的工具名照直显示、图标退回扳手。
  ownTools: { type: Boolean, default: false },
})

// 正文里的文件路径 chip 由后端只读预览（v-html 内的点击走委托）。
function onTextClick(e) {
  const chip = e.target.closest?.('.file-chip')
  if (chip?.dataset.path) emit('preview', chip.dataset.path)
}

// 相邻的工具调用并成一叠；只有一次的退回普通行（多套一层折叠只会让人多点一下）。
// 叠的开合记在这一叠的第一块上，后续到达的块不会把它挤掉。
const rows = computed(() => {
  const out = []
  props.blocks.forEach((b, bi) => {
    const last = out[out.length - 1]
    if (b.kind === 'tool' && last && last.stack) { last.items.push(b); last.keys.push(bi); return }
    if (b.kind === 'tool') { out.push({ stack: true, items: [b], keys: [bi] }); return }
    out.push({ stack: false, b, bi })
  })
  return out.map(r => (r.stack && r.items.length > 1 ? r : { ...r, stack: false, b: r.b ?? r.items[0], bi: r.bi ?? r.keys[0] }))
})

// 正在写的思考块（本轮那一块且轮次未定稿）：标题实时一行预览、灯泡呼吸。
function liveThink(bi, b) {
  return props.streaming && !props.paused && !b.meta && bi === blockIndex(props, 'think')
}
function thinkTitle(b, bi) {
  if (liveThink(bi, b)) return thinkPreview(b.text) || '思考中…'
  return b.meta ? '思考 · ' + b.meta : '思考'
}
// 一叠的对外状态：出错要说在前面（收起时也不能把失败藏起来），运行中跟前导转环一致。
function stackFails(items) { return items.filter(b => b.status === 'fail').length }
function stackState(items) {
  if (stackFails(items)) return 'fail'
  return items.some(b => b.status === 'run') ? 'run' : 'ok'
}
function stackLabel(items) {
  const f = stackFails(items)
  return `执行工具 ${items.length} 次${f ? ` · ${f} 次出错` : ''}`
}
function stackTitle(head, items) {
  const verb = head.gopen ? '收起' : '展开'
  const f = stackFails(items)
  return `${verb} ${items.length} 次工具调用${f ? `（${f} 次出错）` : ''}`
}
// 光标跟着本轮正在写的正文走（思考分片插进来时不该跑到思考行下面）。
function cursorOn(bi) {
  return props.streaming && bi === blockIndex(props, 'text')
}
</script>

<template>
  <template v-for="(r, ri) in rows" :key="ri">
    <!-- 思考块：收起时一行实时预览（流式）或轮次摘要，点击展开全文 -->
    <div v-if="!r.stack && r.b.kind === 'think'" class="think" :class="{ open: r.b.open }">
      <button class="think-head" type="button" :title="r.b.open ? '收起思考' : '展开思考'"
              @click="r.b.open = !r.b.open">
        <span class="step-ico think-ico" :class="{ live: liveThink(r.bi, r.b) }" aria-hidden="true">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 14c.2-1 .7-1.7 1.5-2.4 1.1-1 2.5-2.4 2.5-4.6A6 6 0 0 0 6 7c0 2 1.4 3.4 2.5 4.6.8.7 1.3 1.4 1.5 2.4"/><path d="M9 18h6"/><path d="M10 22h4"/></svg>
        </span>
        <span class="think-title">{{ thinkTitle(r.b, r.bi) }}</span>
        <svg class="think-chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
      </button>
      <div v-show="r.b.open" class="think-body">
        <div class="think-text">{{ r.b.text }}</div>
      </div>
    </div>

    <!-- 一叠工具调用：收起只报次数，点开逐条列出（每条还能再展开看参数与结果） -->
    <div v-else-if="r.stack" class="tool-stack" :class="{ open: r.items[0].gopen, 'is-fail': stackFails(r.items) }">
      <button class="tool-head" type="button" :aria-expanded="r.items[0].gopen ? 'true' : 'false'"
              :title="stackTitle(r.items[0], r.items)" @click="r.items[0].gopen = !r.items[0].gopen">
        <span class="step-ico" :class="stackState(r.items)" aria-hidden="true">
          <i v-if="stackState(r.items) === 'run'" class="spin step-ring"></i>
          <svg v-else-if="stackState(r.items) === 'fail'" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/></svg>
          <svg v-else width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m12 3 9 5-9 5-9-5 9-5z"/><path d="m3 13 9 5 9-5"/></svg>
        </span>
        <span class="step-name">{{ stackLabel(r.items) }}</span>
        <svg class="think-chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
      </button>
      <div v-show="r.items[0].gopen" class="tool-stack-body">
        <ToolStep v-for="(b, k) in r.items" :key="r.keys[k]" :b="b" :own="ownTools" @toggle="b.open = !b.open" />
      </div>
    </div>

    <!-- 单次工具调用：不套折叠壳，直接摆一行 -->
    <ToolStep v-else-if="r.b.kind === 'tool'" :b="r.b" :own="ownTools" @toggle="r.b.open = !r.b.open" />

    <!-- 正文块（fileChips：行内代码里的本机文件路径渲染成可点预览 chip） -->
    <div v-else class="turn-text chat-md" @click="onTextClick"><span v-html="renderMD(r.b.text, { fileChips: true })"></span><span v-if="cursorOn(r.bi)" class="cursor"></span></div>
  </template>
</template>
