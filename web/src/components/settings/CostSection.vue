<script setup>
// 成本与预算：价格表编辑（热生效）+ 预算态势与主动任务开关。
// 预算额度本身在 config.yaml 的 cost 段配置（重启生效），这里只读展示。
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'

const EMPTY_PRICE = { model: '', inputPerMTok: '', outputPerMTok: '', cacheReadPerMTok: '' }

const view = ref(null)           // /api/cost 的 cost 视图
const rows = reactive([])        // 价格表编辑行
const unknowns = ref([])
const currency = ref('CNY')
const msg = reactive({ text: '', cls: '' })
let timer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 4000)
}

const budget = computed(() => (view.value && view.value.budget) || {})
const stateClass = computed(() => {
  const s = view.value && view.value.state
  return s === 'over' ? 'bad' : s === 'warn' ? 'warn' : 'ok'
})
const stateText = computed(() => {
  const s = view.value && view.value.state
  return s === 'over' ? '已超预算' : s === 'warn' ? '接近预算' : (s === 'ok' ? '预算正常' : '—')
})

function num(v) {
  if (v === '' || v === null || v === undefined) return 0
  const n = Number(v)
  return Number.isFinite(n) && n > 0 ? n : 0
}

async function loadCost() {
  try {
    view.value = (await api('GET', '/api/cost')).cost
  } catch (e) { show('预算状态加载失败：' + e.message, false) }
}

async function loadPricing() {
  try {
    const r = await api('GET', '/api/cost/pricing')
    currency.value = r.currency || 'CNY'
    rows.splice(0, rows.length, ...(r.prices || []).map(p => ({
      model: p.model || '',
      inputPerMTok: p.inputPerMTok ?? '',
      outputPerMTok: p.outputPerMTok ?? '',
      cacheReadPerMTok: p.cacheReadPerMTok ?? '',
    })))
    unknowns.value = r.unknownModels || []
  } catch (e) { show('价格表加载失败：' + e.message, false) }
}

function addRow() { rows.push({ ...EMPTY_PRICE }) }
function removeRow(i) { rows.splice(i, 1) }
function addUnknown(name) {
  if (rows.some(r => r.model.trim() === name)) return
  rows.push({ ...EMPTY_PRICE, model: name })
  unknowns.value = unknowns.value.filter(m => m !== name)
}

async function save() {
  const prices = rows.filter(r => r.model.trim()).map(r => ({
    model: r.model.trim(),
    inputPerMTok: num(r.inputPerMTok),
    outputPerMTok: num(r.outputPerMTok),
    cacheReadPerMTok: num(r.cacheReadPerMTok),
  }))
  try {
    const r = await api('POST', '/api/cost/pricing', { currency: currency.value, prices })
    show(r.message || '价格表已保存', true)
    await Promise.all([loadPricing(), loadCost()])
  } catch (e) { show(e.message, false) }
}

const pausing = ref(false)
async function togglePause() {
  const on = !(view.value && view.value.manualPaused)
  pausing.value = true
  try {
    const r = await api('POST', on ? '/api/cost/pause' : '/api/cost/resume', {})
    show(r.message || (on ? '已暂停主动任务' : '已恢复主动任务'), true)
    await loadCost()
  } catch (e) { show(e.message, false) } finally { pausing.value = false }
}

onMounted(async () => { await Promise.all([loadCost(), loadPricing()]) })
</script>

<template>
  <div v-if="view" class="cost-state" :class="stateClass">
    <div class="cs-head">
      <span class="dot" :class="stateClass"></span>
      <b>{{ stateText }}</b>
      <span class="meta">最紧一档 {{ Math.round((view.ratio || 0) * 100) }}%（{{ view.tightest || '未设额度' }}）</span>
    </div>
    <div class="cs-grid">
      <div><span class="meta">今日 token</span><div class="cs-v">{{ view.todayTokens }}</div></div>
      <div><span class="meta">今日金额</span><div class="cs-v">{{ view.currency }} {{ (view.todayCost || 0).toFixed(4) }}</div></div>
      <div><span class="meta">本月 token</span><div class="cs-v">{{ view.monthlyTokens }}</div></div>
      <div><span class="meta">本月金额</span><div class="cs-v">{{ view.currency }} {{ (view.monthlyCost || 0).toFixed(4) }}</div></div>
    </div>
    <div v-if="view.proactivePaused" class="cs-note">
      主动任务（心跳巡检 / 智能任务 / 目标启动）当前被暂停；对话与定时提醒不受影响。
    </div>
    <div v-if="view.zeroCalls" class="cs-note">
      有 {{ view.zeroCalls }} 次调用未回传用量——多为网关不接受 stream_options，
      可在 provider 段关闭 streamUsage 后再观察。
    </div>
    <div class="row" style="margin-top:10px">
      <button class="btn" type="button" :disabled="pausing" @click="togglePause">
        {{ view.manualPaused ? '恢复主动任务' : '暂停主动任务' }}
      </button>
      <span class="meta">手动开关只影响当前进程，重启后回到配置决定的状态</span>
    </div>
    <div class="msg" :class="msg.cls">{{ msg.text }}</div>
  </div>

  <h3 class="set-sub">模型价格表（每 {{ currency }} / 百万 token）</h3>
  <div v-if="rows.length" class="price-list">
    <div class="pr-head">
      <span>模型</span><span>输入</span><span>输出</span><span>缓存读</span><span></span>
    </div>
    <div v-for="(r, i) in rows" :key="i" class="pr-row">
      <input v-model="r.model" class="pr-model" placeholder="如 deepseek-chat" aria-label="模型名">
      <input v-model="r.inputPerMTok" type="number" min="0" step="0.01" aria-label="输入单价">
      <input v-model="r.outputPerMTok" type="number" min="0" step="0.01" aria-label="输出单价">
      <input v-model="r.cacheReadPerMTok" type="number" min="0" step="0.01" aria-label="缓存读单价">
      <button class="btn sm danger" type="button" @click="removeRow(i)">删除</button>
    </div>
  </div>
  <div v-else class="empty">还没有配价——配价后「运行状态」才会显示金额，未配价的模型只统计 token</div>

  <div v-if="unknowns.length" class="unknown-box">
    <span class="meta">有用量但未配价：</span>
    <button v-for="m in unknowns" :key="m" class="btn sm" type="button" @click="addUnknown(m)">＋ {{ m }}</button>
  </div>

  <div class="row" style="margin-top:10px">
    <button class="btn" type="button" @click="addRow">＋ 添加模型</button>
    <button class="btn primary" type="button" @click="save">保存并热生效</button>
  </div>

  <h3 class="set-sub">预算额度（只读）</h3>
  <div class="budget-list">
    <div><span class="meta">日 token</span><b>{{ budget.dailyTokens || '不限' }}</b></div>
    <div><span class="meta">月 token</span><b>{{ budget.monthlyTokens || '不限' }}</b></div>
    <div><span class="meta">日金额</span><b>{{ budget.dailyCost || '不限' }}</b></div>
    <div><span class="meta">月金额</span><b>{{ budget.monthlyCost || '不限' }}</b></div>
    <div><span class="meta">预警线</span><b>{{ budget.softRatio }}</b></div>
    <div><span class="meta">单计划 token</span><b>{{ budget.perPlanTokens || '不限' }}</b></div>
    <div><span class="meta">单目标 token / 步数</span><b>{{ budget.perGoalTokens || '不限' }} / {{ budget.perGoalSteps || '不限' }}</b></div>
  </div>
  <div class="set-note">额度在 config.yaml 的 <b>cost.budget</b> 段配置（改动重启生效）。
    超线是<b>软限额</b>：停的是心跳巡检、智能任务与目标启动这类主动工作，
    <b>不会打断你正在进行的对话</b>，定时提醒照常送达。</div>
</template>
