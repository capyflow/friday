<script setup>
// 全局调用轨迹（一条事件流，跟随对话历史）。5s 轮询，内容无变化不重渲染；
// 监听聊天页发出的 friday:trace-refresh 事件在回复完成时立即刷新。
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { api, traceAPI } from '../api.js'
import { EV_TITLE, fmtClock, fmtNum, prettyJSON } from '../format.js'
import { store } from '../store.js'

const events = ref([])
const stats = reactive({ requests: 0, in: 0, out: 0 })
const model = ref('')
const auto = ref(true)
let lastJSON = ''
let timer = null

const statsLine = computed(() =>
  '共 ' + fmtNum(stats.requests) + ' 次调用 · IN ' + fmtNum(stats.in)
  + ' · OUT ' + fmtNum(stats.out) + (model.value ? ' · ' + model.value : ''))

function summary(e) {
  if (e.kind === 'llm_in' || e.kind === 'llm_out') return e.input || ''
  if (e.kind === 'tool_req') { const m = e.text.match(/工具 (\S+?)，/); return m ? m[1] : e.text.slice(0, 60) }
  return (e.text || '').split('\n')[0].slice(0, 80)
}
function usageLine(e) {
  const parts = []
  if (e.in) parts.push('IN ' + fmtNum(e.in))
  if (e.out) parts.push('OUT ' + fmtNum(e.out))
  if (e.ms) parts.push((e.ms / 1000).toFixed(1) + 's')
  return parts.join(' · ')
}
function bodyOf(e) {
  return (e.kind === 'llm_in' || e.kind === 'llm_out') ? prettyJSON(e.text) : e.text
}
function toggle(e) { e._open = !e._open }

// filtered 是"只看某一次多步运行"（计划 plan-<id> / 目标 goal-<id>）的开关。
const filtered = computed(() => store.traceFilter || '')
function clearFilter() { store.traceFilter = '' }

async function refresh() {
  try {
    const query = filtered.value
      ? '/api/traces?session=' + encodeURIComponent(filtered.value) : '/api/traces'
    const body = await traceAPI(query)
    const j = JSON.stringify(body)
    if (j === lastJSON) return
    lastJSON = j
    events.value = (body.events || []).map(e => ({ ...e, _open: false }))
    Object.assign(stats, body.stats || { requests: 0, in: 0, out: 0 })
  } catch (e) { /* 静默 */ }
}

onMounted(async () => {
  try { model.value = (await api('GET', '/api/status')).provider.model || '' } catch (e) {}
  refresh()
  timer = setInterval(() => { if (auto.value) refresh() }, 5000)
  window.addEventListener('friday:trace-refresh', refresh)
  watch(filtered, () => { lastJSON = ''; refresh() })
})
onUnmounted(() => {
  clearInterval(timer)
  window.removeEventListener('friday:trace-refresh', refresh)
})
</script>

<template>
  <section class="view" id="view-trace">
    <div class="view-inner">
      <h2>调用轨迹
        <span v-if="filtered" class="trace-filter">
          只看 {{ filtered }}
          <button type="button" aria-label="清除轨迹过滤" @click="clearFilter">×</button>
        </span>
        <span v-else class="count">全局一条调用流 · 跟随对话历史（清除上下文时一并清除）</span>
      </h2>
      <div class="trace-bar">
        <div id="traceStats">{{ statsLine }}</div>
        <label><input v-model="auto" type="checkbox"> 自动刷新</label>
        <button class="btn" type="button" @click="refresh">刷新</button>
      </div>
      <template v-if="events.length">
        <div v-for="(e, i) in events" :key="i" class="ev" :class="e.kind">
          <div class="ev-head" @click="toggle(e)">
            <span class="chip" :class="'c-' + e.kind">{{ EV_TITLE[e.kind] || e.kind }}</span>
            <span class="ev-summary">{{ summary(e) }}</span>
            <span class="grow"></span>
            <span v-if="usageLine(e)" class="mono">{{ usageLine(e) }}</span>
            <span class="meta">{{ fmtClock(e.time) }}</span>
          </div>
          <div v-show="e._open" class="ev-body">{{ bodyOf(e) }}</div>
        </div>
      </template>
      <div v-else class="empty">还没有调用记录<br>和小五说句话试试</div>
    </div>
  </section>
</template>
