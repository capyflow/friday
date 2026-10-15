<script setup>
// 成本（Token）：预算态势与每日 token 热力图的只读展示（额度配置在 config.yaml
// 的 cost 段，暂停等操作不在这里）。只统计 token，不再折算金额。
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'
import { fmtTokens } from '../../format.js'

const view = ref(null)           // /api/cost 的 cost 视图
const days = ref([])             // /api/usage 的按天账本（倒序）
const usageErr = ref('')
const msg = reactive({ text: '', cls: '' })
let timer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 4000)
}

const stateClass = computed(() => {
  const s = view.value && view.value.state
  return s === 'over' ? 'bad' : s === 'warn' ? 'warn' : 'ok'
})
const stateText = computed(() => {
  const s = view.value && view.value.state
  return s === 'over' ? '已超预算' : s === 'warn' ? '接近预算' : (s === 'ok' ? '预算正常' : '—')
})

// 热力图列数随容器宽度自适应（GitHub 式撑满横向）：13px 方格 + 4px 间距，
// 排不下更多周就少排，量不到宽度（首帧 / jsdom）时退回 12 周最小窗口。
const HEAT_CELL = 13
const HEAT_GAP = 4
const HEAT_MIN_WEEKS = 12
const HEAT_MAX_WEEKS = 52
const heatMain = ref(null)
const heatWeeks = ref(HEAT_MIN_WEEKS)
let heatRO = null
function measureHeat() {
  const w = heatMain.value && heatMain.value.clientWidth
  if (!w) return
  const n = Math.floor((w + HEAT_GAP) / (HEAT_CELL + HEAT_GAP))
  heatWeeks.value = Math.min(HEAT_MAX_WEEKS, Math.max(HEAT_MIN_WEEKS, n))
}

function localDate(d) {
  const p = n => (n < 10 ? '0' : '') + n
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate())
}

function levelOf(v, max) {
  if (v <= 0) return 0
  const q = max / 4
  if (v <= q) return 1
  if (v <= 2 * q) return 2
  if (v <= 3 * q) return 3
  return 4
}

// heat 生成近 N 周（周一起始，N 随容器宽度自适应）的日历格子；未来格子标记 future 占位不显示。
const heat = computed(() => {
  const totals = new Map(days.value.map(d => [d.date, (d.in || 0) + (d.out || 0)]))
  const today = new Date()
  const monday = new Date(today.getFullYear(), today.getMonth(), today.getDate())
  monday.setDate(monday.getDate() - ((monday.getDay() + 6) % 7))
  const cursor = new Date(monday)
  cursor.setDate(cursor.getDate() - (heatWeeks.value - 1) * 7)
  const max = days.value.reduce((m, d) => Math.max(m, (d.in || 0) + (d.out || 0)), 0)
  const weeks = []
  let labeledMonth = -1
  for (let w = 0; w < heatWeeks.value; w++) {
    const colMonday = new Date(cursor)
    const label = colMonday.getMonth() === labeledMonth ? '' : (colMonday.getMonth() + 1) + '月'
    if (label) labeledMonth = colMonday.getMonth()
    const cells = []
    for (let r = 0; r < 7; r++) {
      const date = localDate(cursor)
      const v = totals.get(date) || 0
      cells.push({ date, v, future: cursor > today, level: levelOf(v, max),
        tip: date + ' · ' + (v > 0 ? fmtTokens(v) + ' tokens' : '无消耗') })
      cursor.setDate(cursor.getDate() + 1)
    }
    weeks.push({ label, cells })
  }
  return weeks
})

// heatTotal 只统计热力图窗口（近 N 周）内的天数。
const heatTotal = computed(() => {
  const start = heat.value.length ? heat.value[0].cells[0].date : ''
  return days.value.filter(d => d.date >= start)
    .reduce((s, d) => s + (d.in || 0) + (d.out || 0), 0)
})

async function loadCost() {
  try {
    view.value = (await api('GET', '/api/cost')).cost
  } catch (e) { show('预算状态加载失败：' + e.message, false) }
}

async function loadUsage() {
  try {
    const r = await api('GET', '/api/usage')
    days.value = r.days || []
    usageErr.value = ''
  } catch (e) { usageErr.value = e.message }
}

onMounted(async () => {
  if (typeof window.ResizeObserver !== 'undefined' && heatMain.value) {
    heatRO = new window.ResizeObserver(measureHeat)
    heatRO.observe(heatMain.value)
  }
  await Promise.all([loadCost(), loadUsage()])
})
onBeforeUnmount(() => {
  if (heatRO) heatRO.disconnect()
  clearTimeout(timer)
})
</script>

<template>
  <div v-if="view" class="cost-state" :class="stateClass">
    <div class="cs-head">
      <span class="dot" :class="stateClass"></span>
      <b>{{ stateText }}</b>
      <span class="meta">最紧一档 {{ Math.round((view.ratio || 0) * 100) }}%（{{ view.tightest || '未设额度' }}）</span>
    </div>
    <div class="cs-grid two">
      <div><span class="meta">今日 token</span><div class="cs-v">{{ fmtTokens(view.todayTokens) }}</div></div>
      <div><span class="meta">本月 token</span><div class="cs-v">{{ fmtTokens(view.monthlyTokens) }}</div></div>
    </div>
    <div v-if="view.proactivePaused" class="cs-note">
      主动任务（心跳巡检 / 智能任务 / 目标启动）当前被暂停；对话与定时提醒不受影响。
    </div>
    <div v-if="view.zeroCalls" class="cs-note">
      有 {{ view.zeroCalls }} 次调用未回传用量——多为网关不接受 stream_options，
      可在 provider 段关闭 streamUsage 后再观察。
    </div>
    <div class="msg" :class="msg.cls">{{ msg.text }}</div>
  </div>

  <h3 class="set-sub">每日 token 热力图 <span class="meta">近 {{ heatWeeks }} 周（IN + OUT 合计）</span></h3>
  <div v-if="usageErr" class="cs-note">用量账本加载失败：{{ usageErr }}</div>
  <div v-else class="heat">
    <div class="heat-inner">
      <div class="heat-side" aria-hidden="true">
        <span v-for="(n, i) in ['一', '', '三', '', '五', '', '']" :key="i">{{ n }}</span>
      </div>
      <div ref="heatMain" class="heat-main">
        <div class="heat-months" :style="{ gridTemplateColumns: 'repeat(' + heatWeeks + ', 13px)' }">
          <span v-for="(wk, i) in heat" :key="i">{{ wk.label }}</span>
        </div>
        <div class="heat-cols" :style="{ gridTemplateColumns: 'repeat(' + heatWeeks + ', 13px)' }">
          <div v-for="(wk, i) in heat" :key="i" class="heat-col">
            <span v-for="c in wk.cells" :key="c.date" class="hc" :class="['l' + c.level, { future: c.future }]"
                  :title="c.tip"></span>
          </div>
        </div>
      </div>
    </div>
    <div class="heat-legend">
      <span class="meta heat-total">近 {{ heatWeeks }} 周合计 {{ fmtTokens(heatTotal) }} tokens</span>
    </div>
  </div>

  <!-- 预算额度（只读）暂时隐藏：额度在 config.yaml 的 cost.budget 段配置，
       需要恢复展示时从 git 历史找回 budget-list 块 -->
</template>
