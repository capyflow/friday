<script setup>
// TimelineBlocks 是聊天时间线内容块的共享渲染组件（ChatView 与 WorkspaceView 共用，
// 样式在全局 style.css）：思考块（可折叠，流式一行预览）、工具调用块与正文块
// 按到达顺序排列。连续多次工具调用会归成一叠（「执行工具 N 次」）；中间没有正文
// 隔开的连续思考/工具行再并成一个过程行，收起只报「N 次思考 · M 次工具调用」，
// 点开在限高滚动的展开区里列全程。块对象是父组件的 reactive 数组元素，这里的
// 开合切换直接改写它们。
import { computed } from 'vue'
import { renderMD, localFileHref, joinFilePath } from '../md.js'
import { copyText } from '../clipboard.js'
import { fmtRate } from '../speed.js'
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
  // 会话主目录：正文里的相对文件链接（[文案](docs/a.md)）按它拼绝对路径再预览。
  // 内置聊天没有 cwd，不传时相对链接不拦（绝对路径链接不受影响）。
  baseDir: { type: String, default: '' },
  // 当前输出速度（tok/s，前端按字符估算），跟着正在写的正文光标一起显示。
  rate: { type: Number, default: 0 },
})

// 复制按钮反馈：直接改文字，1.2s 后还原。流式重渲染可能中途换掉节点，
// 那时反馈自然消失，不额外追踪。
function flashCopy(btn, msg) {
  if (!btn.isConnected) return
  btn.textContent = msg
  setTimeout(() => { if (btn.isConnected) btn.textContent = '复制' }, 1200)
}

// 正文里的文件路径 chip 与文件链接都转后端只读预览（v-html 内的点击走委托）。
// 代码块右上角的复制按钮也在这里接：按文本取相邻 pre 的 textContent。
// http(s)/mailto 外链不在这拦：桌面壳由 App.vue 的全局委托转系统浏览器，浏览器
// 直连保持原生行为（中键/右键不受影响）。
function onTextClick(e) {
  const copyBtn = e.target.closest?.('.md-copy')
  if (copyBtn) {
    const pre = copyBtn.closest('.md-code')?.querySelector('pre')
    const text = pre ? pre.textContent : ''
    if (text) copyText(text).then(() => flashCopy(copyBtn, '已复制'), () => flashCopy(copyBtn, '复制失败'))
    return
  }
  const chip = e.target.closest?.('.file-chip')
  if (chip?.dataset.path) { emit('preview', chip.dataset.path); return }
  const a = e.target.closest?.('a[href]')
  if (!a) return
  const href = localFileHref(a.getAttribute('href'))
  if (href === null) return
  const abs = href.startsWith('/') ? href : joinFilePath(props.baseDir, href)
  if (!abs) return // 没有基准目录拼不出绝对路径：不拦，回退默认行为
  e.preventDefault()
  emit('preview', abs)
}

// 相邻的工具调用并成一叠；只有一次的退回普通行（多套一层折叠只会让人多点一下）。
// 连续的思考/工具行（中间没有正文隔开）再并成一个过程行：摘要一条 + 限高滚动的
// 展开容器，大批量思考/工具全展开也不会把回答顶出一屏。只有一行的过程不收
// （一行内容不值得多套一层折叠），原样摆出。叠与过程行的开合都记在各自第一块
// （gopen）上，后续到达的块不会把它们挤掉。
const rows = computed(() => {
  const flat = []
  props.blocks.forEach((b, bi) => {
    const last = flat[flat.length - 1]
    if (b.kind === 'tool' && last && last.stack) { last.items.push(b); last.keys.push(bi); return }
    if (b.kind === 'tool') { flat.push({ stack: true, items: [b], keys: [bi] }); return }
    flat.push({ stack: false, b, bi })
  })
  const base = flat.map(r => (r.stack && r.items.length > 1 ? r : { ...r, stack: false, b: r.b ?? r.items[0], bi: r.bi ?? r.keys[0] }))
  const out = []
  let run = null
  // 正文一出现就断开过程：下一轮的思考/工具另起一个过程行。
  const flush = () => {
    if (!run) return
    if (run.length >= 2) {
      const first = run[0]
      const head = first.stack ? first.items[0] : first.b
      out.push({ proc: true, head, rows: run })
    } else {
      out.push(run[0])
    }
    run = null
  }
  base.forEach(r => {
    if (r.stack || r.b.kind === 'think' || r.b.kind === 'tool') {
      if (!run) run = []
      run.push(r)
      return
    }
    flush()
    out.push(r)
  })
  flush()
  return out
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
// 过程行的汇总：思考几条、工具几次、失败几次（叠里的每条也算一次）。
function procCounts(g) {
  let th = 0, tl = 0, f = 0
  g.rows.forEach(r => (r.stack ? r.items : [r.b]).forEach(b => {
    if (b.kind === 'tool') { tl += 1; if (b.status === 'fail') f += 1 }
    else th += 1
  }))
  return { th, tl, f }
}
// 流式期间正在写的思考在这个过程行里：摘要标题实时换成思考预览，收着也看得见
// agent 在想什么（与裸思考行的 liveThink 同一套判定）。
function procLive(g) {
  if (!props.streaming || props.paused) return null
  const li = blockIndex(props, 'think')
  for (const r of g.rows) {
    if (!r.stack && r.b.kind === 'think' && r.bi === li && !r.b.meta)
      return thinkPreview(r.b.text) || '思考中…'
  }
  return null
}
function procLabel(g) {
  const live = procLive(g)
  if (live) return live
  const { th, tl, f } = procCounts(g)
  const s = [th ? `${th} 次思考` : '', tl ? `${tl} 次工具调用` : ''].filter(Boolean).join(' · ')
  return f ? `${s} · ${f} 次出错` : s
}
function procTitle(g) {
  const { th, tl, f } = procCounts(g)
  const verb = g.head.gopen ? '收起' : '展开'
  return `${verb} ${th} 次思考、${tl} 次工具调用${f ? `（${f} 次出错）` : ''}`
}
// 过程行前导：失败 > 运行 > 实时思考（灯泡呼吸）> 完成，一眼读出全程进展。
function procState(g) {
  if (procCounts(g).f) return 'fail'
  if (g.rows.some(r => (r.stack ? r.items : [r.b]).some(b => b.status === 'run'))) return 'run'
  return procLive(g) ? 'live' : 'ok'
}
// 光标跟着本轮正在写的正文走（思考分片插进来时不该跑到思考行下面）。
function cursorOn(bi) {
  return props.streaming && bi === blockIndex(props, 'text')
}
</script>

<template>
  <template v-for="(r, ri) in rows" :key="ri">
    <!-- 过程行：摘要一条（开合箭头与失败配色借用 .tool-stack），点开才见全程。
         成员行收进下面的 .proc-body 限高滚动容器；流式期间标题实时跟随思考预览。 -->
    <template v-if="r.proc">
      <div class="tool-stack proc" :class="{ open: r.head.gopen, 'is-fail': procCounts(r).f > 0 }">
        <button class="tool-head" type="button" :aria-expanded="r.head.gopen ? 'true' : 'false'"
                :title="procTitle(r)" @click="r.head.gopen = !r.head.gopen">
          <span class="step-ico think-ico" :class="{ live: procState(r) === 'live', fail: procState(r) === 'fail' }" aria-hidden="true">
            <i v-if="procState(r) === 'run'" class="spin step-ring"></i>
            <svg v-else-if="procState(r) === 'fail'" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/></svg>
            <svg v-else width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 14c.2-1 .7-1.7 1.5-2.4 1.1-1 2.5-2.4 2.5-4.6A6 6 0 0 0 6 7c0 2 1.4 3.4 2.5 4.6.8.7 1.3 1.4 1.5 2.4"/><path d="M9 18h6"/><path d="M10 22h4"/></svg>
          </span>
          <span class="step-name">{{ procLabel(r) }}</span>
          <svg class="think-chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
        </button>
      </div>
      <!-- 展开容器：与 .think-body / .tool-out 一样靠自身滚动兜住高度，
           成员行的展开状态记在各自块上，跟着摘要头整开整收。 -->
      <div v-show="r.head.gopen" class="proc-body">
        <template v-for="(sr, si) in r.rows" :key="si">
          <!-- 成员思考行：与顶层思考行同款，展开状态记在块上 -->
          <div v-if="!sr.stack && sr.b.kind === 'think'" class="think" :class="{ open: sr.b.open }">
            <button class="think-head" type="button" :title="sr.b.open ? '收起思考' : '展开思考'"
                    @click="sr.b.open = !sr.b.open">
              <span class="step-ico think-ico" :class="{ live: liveThink(sr.bi, sr.b) }" aria-hidden="true">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 14c.2-1 .7-1.7 1.5-2.4 1.1-1 2.5-2.4 2.5-4.6A6 6 0 0 0 6 7c0 2 1.4 3.4 2.5 4.6.8.7 1.3 1.4 1.5 2.4"/><path d="M9 18h6"/><path d="M10 22h4"/></svg>
              </span>
              <span class="think-title">{{ thinkTitle(sr.b, sr.bi) }}</span>
              <svg class="think-chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
            </button>
            <div v-show="sr.b.open" class="think-body">
              <div class="think-text">{{ sr.b.text }}</div>
            </div>
          </div>

          <!-- 成员工具调用：过程行外面已经汇总过「N 次工具调用」，里面不再
               套一层「执行工具 N 次」的折叠——重复折叠两层，点开还是得再点一次。
               每条调用平铺一行，逐条可再展开看参数与结果。 -->
          <template v-else-if="sr.stack">
            <ToolStep v-for="(b, k) in sr.items" :key="sr.keys[k]" :b="b" :own="ownTools" @toggle="b.open = !b.open" />
          </template>

          <!-- 成员单次工具调用：容器已是包裹层，直接摆一行 -->
          <ToolStep v-else-if="sr.b.kind === 'tool'" :b="sr.b" :own="ownTools" @toggle="sr.b.open = !sr.b.open" />
        </template>
      </div>
    </template>

    <!-- 思考块：收起时一行实时预览（流式）或轮次摘要，点击展开全文 -->
    <div v-else-if="!r.stack && r.b.kind === 'think'" class="think" :class="{ open: r.b.open }">
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
    <div v-else class="turn-text chat-md" @click="onTextClick"><span v-html="renderMD(r.b.text, { fileChips: true, codeCopy: true })"></span><span v-if="cursorOn(r.bi)" class="cursor"></span><span v-if="cursorOn(r.bi) && rate > 0" class="tok-rate">↑{{ fmtRate(rate) }} tok/s</span></div>
  </template>
</template>
