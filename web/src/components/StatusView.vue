<script setup>
// 运行状态：服务状态卡片 + 成本态势横幅 + Token 用量账本（按天/模型/来源/会话/运行）。
import { computed, nextTick, onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { fmtNum, upStr } from '../format.js'
import { toast } from '../store.js'
import { runCountUp } from '../anim.js'

const status = ref(null)
const usage = reactive({
  days: [], total: {}, currency: 'CNY', err: '',
  byModel: [], byOrigin: [], bySession: [], byRun: [],
})

const cards = computed(() => {
  const st = status.value
  if (!st) return []
  return [
    ['LLM 模型', st.provider.model + (st.apiKeyConfigured ? '' : '（未配密钥）'), 'is-orange'],
    ['待触发任务', st.tasks.total, ''],
    ['待办未完成', st.todos ? st.todos.pending : 0, 'is-mint'],
    ['记忆页面', st.memories, 'is-sky'],
    ['已运行', upStr(st.uptimeSeconds), ''],
    ['服务地址', location.host, ''],
  ]
})

const todayCards = computed(() => {
  const localToday = new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 10)
  const todayRow = usage.days.find(d => d.date === localToday) || { calls: 0, in: 0, out: 0 }
  const total = usage.total
  return [
    ['今日调用', todayRow.calls, ''],
    ['今日 IN', fmtNum(todayRow.in), ''],
    ['今日 OUT', fmtNum(todayRow.out), ''],
    ['累计 IN', fmtNum(total.in || 0), ''],
    ['累计 OUT', fmtNum(total.out || 0), ''],
    ['累计合计', fmtNum((total.in || 0) + (total.out || 0)), 'is-yellow'],
  ]
})

const DIMS = [
  { key: 'model', label: '按模型', rows: () => usage.byModel, hasCost: true },
  { key: 'origin', label: '按来源', rows: () => usage.byOrigin, hasCost: false },
  { key: 'session', label: '按会话', rows: () => usage.bySession, hasCost: false },
  { key: 'run', label: '按运行', rows: () => usage.byRun, hasCost: false },
]
const dim = ref('model')
const curDim = computed(() => DIMS.find(d => d.key === dim.value) || DIMS[0])
const dimRows = computed(() => curDim.value.rows() || [])
const hasDimRows = computed(() => dimRows.value.length > 0)
const unpricedCount = computed(() => usage.byModel.filter(r => !r.priced).length)

const cost = computed(() => (status.value && status.value.cost) || null)
const costClass = computed(() => {
  const s = cost.value && cost.value.state
  return s === 'over' ? 'over' : s === 'warn' ? 'warn' : ''
})
const costLabel = computed(() => {
  const s = cost.value && cost.value.state
  return s === 'over' ? '已超预算' : s === 'warn' ? '接近预算' : '预算正常'
})

onMounted(async () => {
  try {
    status.value = await api('GET', '/api/status')
  } catch (e) { toast('状态加载失败：' + e.message, true); return }
  try {
    const u = await api('GET', '/api/usage')
    usage.days = u.days || []
    usage.total = u.total || {}
    usage.currency = u.currency || 'CNY'
    usage.byModel = u.byModel || []
    usage.byOrigin = u.byOrigin || []
    usage.bySession = u.bySession || []
    usage.byRun = u.byRun || []
  } catch (e) { usage.err = e.message }
  nextTick(() => runCountUp(document.getElementById('view-status')))
})
</script>

<template>
  <section class="view" id="view-status">
    <div class="view-inner">
      <h2>运行状态 <span class="count"><span class="dot ok"></span>在线</span></h2>

      <div v-if="cost" class="cost-banner" :class="costClass">
        <span class="dot" :class="cost.state === 'over' ? 'bad' : cost.state === 'warn' ? 'run' : 'ok'"></span>
        <b>{{ costLabel }}</b>
        <span class="cb-sep">·</span>
        <span>今日 {{ fmtNum(cost.todayTokens) }} tokens</span>
        <template v-if="cost.todayCost">
          <span class="cb-sep">·</span>
          <span>{{ cost.currency }} {{ cost.todayCost.toFixed(4) }}</span>
        </template>
        <template v-if="cost.ratio">
          <span class="cb-sep">·</span>
          <span class="meta">最紧一档 {{ Math.round(cost.ratio * 100) }}%（{{ cost.tightest }}）</span>
        </template>
        <template v-if="cost.proactivePaused">
          <span class="cb-sep">·</span>
          <span class="meta">主动任务已暂停（对话与提醒不受影响）</span>
        </template>
        <template v-if="cost.unknownModels && cost.unknownModels.length">
          <span class="cb-sep">·</span>
          <span class="meta">{{ cost.unknownModels.length }} 个模型未配价，金额未计入</span>
        </template>
        <template v-if="cost.zeroCalls">
          <span class="cb-sep">·</span>
          <span class="meta">{{ cost.zeroCalls }} 次调用未回传用量</span>
        </template>
      </div>

      <div class="cards" v-if="status">
        <div v-for="([k, v, cls]) in cards" :key="k" class="card" :class="cls">
          <div class="k">{{ k }}</div>
          <div class="v" v-if="typeof v === 'number'" :data-count="v">0</div>
          <div class="v" v-else style="font-size:15px">{{ v }}</div>
        </div>
      </div>

      <h2 class="mt22">Token 用量 <span class="count">按天累计（本地时区）</span></h2>
      <div class="cards">
        <div v-for="([k, v, cls]) in todayCards" :key="k" class="card" :class="cls">
          <div class="k">{{ k }}</div>
          <div class="v" v-if="typeof v === 'number'" :data-count="v">0</div>
          <div class="v" v-else style="font-size:15px">{{ v }}</div>
        </div>
      </div>

      <div class="panel" style="margin-top:14px" v-if="usage.days.length">
        <table>
          <thead><tr><th>日期</th><th>调用次数</th><th>IN</th><th>OUT</th><th>合计</th></tr></thead>
          <tbody>
            <tr v-for="d in usage.days.slice(0, 30)" :key="d.date">
              <td>{{ d.date }}</td>
              <td>{{ fmtNum(d.calls) }}</td>
              <td class="mono">{{ fmtNum(d.in) }}</td>
              <td class="mono">{{ fmtNum(d.out) }}</td>
              <td class="mono">{{ fmtNum(d.in + d.out) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="dim-tabs" role="tablist" aria-label="用量维度">
        <button class="dim-tab" type="button" role="tab" :class="{ active: dim === d.key }"
                :aria-selected="dim === d.key" v-for="d in DIMS" :key="d.key" @click="dim = d.key">
          {{ d.label }}
        </button>
      </div>
      <div class="panel dim-panel" v-if="hasDimRows">
        <table>
          <thead>
            <tr>
              <th>{{ curDim.label.replace('按', '') }}</th>
              <th>调用次数</th><th>IN</th><th>OUT</th><th>合计</th>
              <th v-if="curDim.hasCost">金额（{{ usage.currency }}）</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in dimRows" :key="r.key">
              <td>{{ r.key }}</td>
              <td>{{ fmtNum(r.calls) }}</td>
              <td class="mono">{{ fmtNum(r.in) }}</td>
              <td class="mono">{{ fmtNum(r.out) }}</td>
              <td class="mono">
                {{ fmtNum(r.in + r.out) }}
                <span v-if="r.zeroCalls" class="meta">（{{ r.zeroCalls }} 次未回传）</span>
              </td>
              <td v-if="curDim.hasCost" class="mono">
                <template v-if="r.priced">{{ r.cost.toFixed(4) }}</template>
                <span v-else class="meta">价格未配置</span>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-if="dim === 'model' && unpricedCount" class="meta" style="padding:8px 12px">
          {{ unpricedCount }} 个模型未配价：到「设置 → 成本与预算」补单价后才会算出金额
        </div>
      </div>
      <div v-else-if="!usage.days.length && usage.err" class="empty">用量加载失败：{{ usage.err }}</div>
      <div v-else-if="!hasDimRows" class="empty">还没有这个维度的用量记录，和小五聊一句就有了</div>
    </div>
  </section>
</template>
