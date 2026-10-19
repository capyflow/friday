<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { fmtDT, fmtNum, kindName } from '../format.js'
import { store, toast } from '../store.js'
import SelectBox from './SelectBox.vue'
import EmptyState from './art/EmptyState.vue'

const MODE_OPTIONS = [{ value: 'once', label: '单次' }, { value: 'repeat', label: '有限多次' }, { value: 'cron', label: '循环' }]
const CHANNEL_OPTIONS = [{ value: '', label: '全部可用通道' }, { value: 'web', label: '通知中心' },
  { value: 'dingtalk', label: '钉钉' }, { value: 'feishu', label: '飞书' }, { value: 'console', label: '本地输出' }]
const STATUS_NAME = { pending: '待处理', processing: '处理中', delivered: '已送达', failed: '失败', silent: '静默' }
const UNIT_OPTIONS = [{ value: 'minute', label: '每分钟' }, { value: 'hourly', label: '每小时' },
  { value: 'daily', label: '每天' }, { value: 'weekly', label: '每周' }, { value: 'monthly', label: '每月' },
  { value: 'yearly', label: '每年' }]
const REPEAT_UNITS = [{ value: '分钟', label: '分钟' }, { value: '小时', label: '小时' }]
// weekdays 用 0-6 且 0=周日（与后端 scheduleRule 对齐），展示按国内习惯周一在前。
const WEEKDAYS = [{ v: 1, l: '周一' }, { v: 2, l: '周二' }, { v: 3, l: '周三' }, { v: 4, l: '周四' },
  { v: 5, l: '周五' }, { v: 6, l: '周六' }, { v: 0, l: '周日' }]
const WEEKDAY_NAME = Object.fromEntries(WEEKDAYS.map(d => [d.v, d.l]))
const MONTH_DAYS = Array.from({ length: 31 }, (_, i) => ({ v: i + 1, l: String(i + 1) }))
const MONTHS = Array.from({ length: 12 }, (_, i) => ({ v: i + 1, l: (i + 1) + '月' }))
const QUICK_DELAYS = [{ label: '10分钟', minutes: 10 }, { label: '30分钟', minutes: 30 }, { label: '1小时', minutes: 60 }]
const QUICK_MOMENTS = [{ key: 'tonight', label: '今晚 20:00' }, { key: 'tomorrow', label: '明早 8:00' }]
const emptyRule = () => ({ unit: 'daily', interval: 1, hour: 8, minute: 0, weekdays: [1], monthDays: [1], months: [] })
// 表单只收确定性输入：UI 把语义换算成后端契约里的确定值（in / when / delayMinutes /
// every / scheduleRule 五选一互斥），不再传自由文本让后端猜它是时长还是时刻。
const emptyDraft = () => ({ text: '', mode: 'once', delay: '', at: '', repNum: 10, repUnit: '分钟', times: 3,
  cronVia: 'rule', rule: emptyRule(), every: '', channel: '', agent: false, goal: false, maxSteps: '', maxTokens: '', skill: '' })
const draft = reactive(emptyDraft())
const tasks = ref([])
const goals = ref({})        // 目标 id → {running, usedTokens, stepsLimit, tokensLimit}
const editing = ref(null)
const textInput = ref(null)
const message = ref('')
const saving = ref(false)
const busy = reactive(new Set())
const histories = reactive({})
const expanded = ref(null)
const historyBusy = ref(false)
const historyError = ref('')
const formOpen = ref(false)
let poll
let alive = true
let returnFocus = null
const title = computed(() => editing.value ? '编辑任务 #' + editing.value.id : '新建任务')

function taskMode(t) { return t.cron ? 'cron' : t.interval ? 'repeat' : 'once' }
// splitInterval 把后端存回的纳秒间隔拆成"数字 + 分钟/小时"，供回填与展示，
// 用户不必再面对 3600s 这种写法。
function splitInterval(ns) {
  const sec = Number(ns) / 1e9
  if (!Number.isFinite(sec) || sec <= 0) return { num: 10, unit: '分钟' }
  if (sec % 3600 === 0) return { num: sec / 3600, unit: '小时' }
  return { num: Math.max(1, Math.round(sec / 60)), unit: '分钟' }
}
const pad2 = n => String(n).padStart(2, '0')
function hhmm(hour, minute) { return pad2(Number(hour) || 0) + ':' + pad2(Number(minute) || 0) }
// ruleSummary 把结构化规则念回人话：卡片摘要与表单预览共用，避免"选了却看不出排成什么样"。
function ruleSummary(r) {
  const at = hhmm(r.hour, r.minute)
  const days = (list, suffix) => (list || []).map(n => n + suffix).join('、')
  switch (r.unit) {
    case 'minute': return Number(r.interval) > 1 ? '每 ' + r.interval + ' 分钟' : '每分钟'
    case 'hourly': return (Number(r.interval) > 1 ? '每 ' + r.interval + ' 小时' : '每小时') + '的第 ' + (Number(r.minute) || 0) + ' 分钟'
    case 'weekly': return '每' + (r.weekdays || []).map(n => WEEKDAY_NAME[n] || n).join('、') + ' ' + at
    case 'monthly': return '每月' + days(r.monthDays, ' 号') + ' ' + at
    case 'yearly': return '每年' + days(r.months, ' 月') + ' ' + days(r.monthDays, ' 号') + ' ' + at
    default: return '每天 ' + at
  }
}
function plan(t) {
  if (t.cron) return t.scheduleRule ? ruleSummary(t.scheduleRule) : 'cron: ' + t.cron
  if (t.interval) { const r = splitInterval(t.interval); return '每 ' + r.num + r.unit + '，剩 ' + t.remaining + '/' + t.times + ' 次' }
  return '单次 · ' + fmtDT(t.when)
}
function channelName(channel) { return CHANNEL_OPTIONS.find(o => o.value === channel)?.label || channel || '全部可用通道' }
// toLocalInput 把任意可解析的时刻转成 datetime-local 认的"本机时区 YYYY-MM-DDTHH:mm"。
// 提交时反过来交给 Date 解析、toISOString 成 UTC 带 Z 的 RFC3339：
// 前端不手拼时区 offset，夏令时与半时区交给 Date 和后端 time.Parse 才不会算错。
function toLocalInput(value) {
  if (!value) return ''
  const d = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(d.getTime())) return ''
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) +
    'T' + pad2(d.getHours()) + ':' + pad2(d.getMinutes())
}
// quickMoment 是"今晚 20:00 / 明早 8:00"的本机等价写法：填进选择器，提交时按 when 换算。
function quickMoment(key) {
  const now = new Date()
  const d = key === 'tonight'
    ? new Date(now.getFullYear(), now.getMonth(), now.getDate(), 20, 0, 0)
    : new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1, 8, 0, 0)
  return toLocalInput(d)
}
function isQuickMoment(key) { return !draft.delay && !!draft.at && draft.at === quickMoment(key) }
function pickDelay(minutes) {
  if (saving.value) return
  draft.delay = minutes
  draft.at = ''
}
function pickMoment(key) {
  if (saving.value) return
  draft.delay = ''
  draft.at = quickMoment(key)
}
function setAt(value) { draft.delay = ''; draft.at = value }
// oncePast：算出的时刻不晚于现在就行内说清楚，并在提交时拦住。
const oncePast = computed(() => {
  if (draft.mode !== 'once' || draft.delay || !draft.at) return false
  const ts = new Date(draft.at).getTime()
  return !Number.isNaN(ts) && ts <= Date.now()
})
function resetForm() { editing.value = null; Object.assign(draft, emptyDraft()); message.value = '' }
// 取消/关闭/Escape/保存成功都走这里：清空草稿，下次打开总是干净表单，焦点交回打开它的按钮。
function closeForm() {
  formOpen.value = false
  resetForm()
  if (returnFocus?.isConnected) returnFocus.focus()
  returnFocus = null
}
function cancel() { if (!saving.value) closeForm() }
async function openCreate(e) {
  if (saving.value) return
  resetForm()
  returnFocus = e?.currentTarget || null
  formOpen.value = true
  await nextTick()
  textInput.value?.focus()
}
async function edit(t, e) {
  if (saving.value || busy.has(t.id)) return
  editing.value = t
  const mode = taskMode(t)
  Object.assign(draft, emptyDraft(), { text: t.text, mode, channel: t.channel || '', agent: t.kind === 'agent',
    goal: !!t.goal, maxSteps: t.maxSteps ?? '', maxTokens: t.maxTokens ?? '', skill: t.skill || '' })
  if (mode === 'once') draft.at = toLocalInput(t.when || t.next)
  if (mode === 'repeat') {
    const r = splitInterval(t.interval)
    draft.repNum = r.num
    draft.repUnit = r.unit
    draft.times = t.times || 3
  }
  if (mode === 'cron') {
    // 有结构化规则就回填控件；只有 cron 表达式的存量任务（或手写的）落到"直接写表达式"。
    // 不反解表达式：同一条 cron 有多种等价写法，猜错语义比不猜更糟。
    if (t.scheduleRule) {
      const r = t.scheduleRule
      draft.cronVia = 'rule'
      draft.rule = { unit: UNIT_OPTIONS.some(o => o.value === r.unit) ? r.unit : 'daily',
        interval: r.interval || 1, hour: r.hour ?? 8, minute: r.minute ?? 0,
        weekdays: [...(r.weekdays || [])], monthDays: [...(r.monthDays || [])], months: [...(r.months || [])] }
    } else {
      draft.cronVia = 'expr'
      draft.every = t.cron || ''
    }
  }
  message.value = ''
  returnFocus = e?.currentTarget || null
  formOpen.value = true
  await nextTick()
  textInput.value?.focus()
}
async function loadGoals() {
  try {
    const r = await api('GET', '/api/goals')
    const map = {}
    for (const g of r.goals || []) map[g.id] = g
    if (alive) goals.value = map
  } catch (e) { /* 目标进度拉不到不影响任务管理，下次轮询再试 */ }
}

function viewRuns(session) {
  store.traceFilter = session
  store.settingsSection = 'trace'
  store.settingsOpen = true
}

async function stopGoal(t) {
  if (!confirm('停止「' + t.text + '」当前这次推进？已完成的步骤结果仍会送达。')) return
  try {
    const r = await api('POST', '/api/goals/' + encodeURIComponent(t.id) + '/stop', {})
    message.value = ''
    toast(r.message || '已请求停止')
    await loadGoals()
  } catch (e) { toast(e.message, true) }
}

async function loadTasks() {
  try {
    const r = await api('GET', '/api/tasks')
    if (alive) tasks.value = r.tasks || []
  } catch (e) { if (alive) toast('任务加载失败：' + e.message, true) }
}
// onAgentToggle 取消"智能任务"时一并取消"目标"：目标必须是智能任务。
function onAgentToggle() {
  if (!draft.agent) { draft.goal = false; draft.maxSteps = ''; draft.maxTokens = ''; draft.skill = '' }
}

function togglePick(list, value) {
  const i = list.indexOf(value)
  if (i >= 0) list.splice(i, 1)
  else list.push(value)
}
function intRange(value, label, min, max) {
  const n = Number(value)
  if (!Number.isInteger(n) || n < min || n > max) throw new Error(label + '须在 ' + min + '-' + max + ' 之间')
  return n
}
function pickList(list, label, min, max) {
  if (!list.length) throw new Error('请至少选择一个' + label)
  const bad = list.find(n => !Number.isInteger(n) || n < min || n > max)
  if (bad !== undefined) throw new Error(label + '须在 ' + min + '-' + max + ' 之间')
  return [...list].sort((a, b) => a - b)
}
// rulePayload 按单位只提交该单位用得上的字段：
// 后端会明确拒绝 daily 及以上的 N>1（cron 的 */N 在每月 1 号重置，需要锚点），
// 所以切到这些单位时不带 interval，而不是填了再指望它被忽略；
// hourly 同理——cron 的时位是 */N，小时数不参与排程，就不提交 hour。
function rulePayload() {
  const r = draft.rule
  const at = () => ({ hour: intRange(r.hour, '小时', 0, 23), minute: intRange(r.minute, '分钟', 0, 59) })
  switch (r.unit) {
    case 'minute':
      return { unit: 'minute', interval: intRange(r.interval, '每 N 分钟的 N', 1, 59) }
    case 'hourly':
      return { unit: 'hourly', interval: intRange(r.interval, '每 N 小时的 N', 1, 23), minute: intRange(r.minute, '触发分钟', 0, 59) }
    case 'weekly':
      return { unit: 'weekly', ...at(), weekdays: pickList(r.weekdays, '星期几', 0, 6) }
    case 'monthly':
      return { unit: 'monthly', ...at(), monthDays: pickList(r.monthDays, '每月几号', 1, 31) }
    case 'yearly':
      return { unit: 'yearly', ...at(), monthDays: pickList(r.monthDays, '每月几号', 1, 31), months: pickList(r.months, '月份', 1, 12) }
    default:
      return { unit: 'daily', ...at() }
  }
}
// rulePreview 只把当前选择念出来给用户确认；不合法时留空，交给提交时的具体报错说话。
const rulePreview = computed(() => {
  try { return ruleSummary(rulePayload()) } catch (e) { return '' }
})

function formBody() {
  const goal = !!(draft.agent && draft.goal)
  const body = { text: draft.text.trim(), agent: draft.agent, channel: draft.channel }
  if (goal) {
    body.goal = true
    const steps = String(draft.maxSteps ?? '').trim()
    const tokens = String(draft.maxTokens ?? '').trim()
    if (steps) {
      const n = Number(steps)
      if (!Number.isInteger(n) || n < 1) throw new Error('目标步数至少为 1 的整数')
      body.maxSteps = n
    }
    if (tokens) {
      const n = Number(tokens)
      if (!Number.isInteger(n) || n < 1) throw new Error('目标 token 预算至少为 1 的整数')
      body.maxTokens = n
    }
    if (draft.skill.trim()) body.skill = draft.skill.trim()
  }
  if (!body.text) throw new Error('请填写任务内容')
  if (draft.mode === 'once') {
    if (draft.delay) body.delayMinutes = draft.delay
    else {
      const at = draft.at.trim()
      if (!at) throw new Error('请填写触发时间')
      const ts = new Date(at).getTime()
      if (Number.isNaN(ts)) throw new Error('请选择有效的日期与时间')
      if (ts <= Date.now()) throw new Error('该时刻已过，请重选')
      body.when = new Date(ts).toISOString()
    }
  } else if (draft.mode === 'repeat') {
    const n = Number(draft.repNum)
    if (!Number.isInteger(n) || n < 1) throw new Error('请填写 1 以上的整数间隔')
    const times = Number(draft.times)
    if (!Number.isInteger(times) || times < 2) throw new Error('请填写间隔及至少 2 次的整数次数')
    body.in = n + draft.repUnit
    body.times = times
  } else if (draft.cronVia === 'expr') {
    body.every = draft.every.trim()
    if (!body.every) throw new Error('请填写循环规则')
  } else {
    body.scheduleRule = rulePayload()
  }
  return body
}
async function submit() {
  if (saving.value) return
  let body
  try { body = formBody() } catch (e) { message.value = e.message; return }
  saving.value = true
  message.value = ''
  const id = editing.value?.id
  try {
    const r = await api(id ? 'PUT' : 'POST', id ? '/api/tasks/' + encodeURIComponent(id) : '/api/tasks', body)
    if (!alive) return
    toast(r.message || (id ? '未来安排已更新' : '任务已添加'))
    closeForm()
    await refreshAll()
  } catch (e) { if (alive) message.value = e.message } finally { saving.value = false }
}
async function changeStatus(t) {
  if (busy.has(t.id) || saving.value && editing.value?.id === t.id) return
  busy.add(t.id)
  const action = t.status === 'paused' ? 'resume' : 'pause'
  try {
    const r = await api('POST', '/api/tasks/' + encodeURIComponent(t.id) + '/' + action, {})
    if (!alive) return
    toast(r.message || (action === 'pause' ? '未来触发已暂停' : '任务已恢复'))
    await refreshAll()
  } catch (e) { if (alive) toast('操作失败：' + e.message, true) } finally { busy.delete(t.id) }
}
async function remove(t) {
  if (busy.has(t.id) || saving.value && editing.value?.id === t.id || !confirm('取消任务 ' + t.id + ' 的未来安排？已经入队的执行会保留。')) return
  busy.add(t.id)
  try {
    const r = await api('DELETE', '/api/tasks/' + encodeURIComponent(t.id))
    if (!alive) return
    toast(r.message || '未来安排已取消')
    if (editing.value?.id === t.id) closeForm()
    await refreshAll()
  } catch (e) { if (alive) toast('取消失败：' + e.message, true) } finally { busy.delete(t.id) }
}
async function history(t) {
  if (historyBusy.value) return
  if (expanded.value === t.id) { expanded.value = null; return }
  expanded.value = t.id
  historyError.value = ''
  historyBusy.value = true
  try {
    const r = await api('GET', '/api/notifications?taskId=' + encodeURIComponent(t.id))
    if (alive) histories[t.id] = (r.notifications || []).filter(n => n.taskId === t.id)
  } catch (e) { if (alive) historyError.value = '执行历史加载失败：' + e.message } finally { historyBusy.value = false }
}
function refreshAll() { return Promise.all([loadTasks(), loadGoals()]) }

onMounted(() => { refreshAll(); poll = setInterval(refreshAll, 5000) })
onBeforeUnmount(() => { alive = false; clearInterval(poll) })
</script>

<template>
  <section class="view" id="view-auto">
    <div class="view-inner">
      <h2>自动化 <span class="count">{{ tasks.length ? tasks.length + ' 条' : '' }}</span></h2>
      <div class="panel task-new">
        <div class="tn-txt">
          <div class="panel-title">新建任务</div>
          <p class="meta">编辑、暂停和取消只影响未来安排，已经入队或处理中的执行会保留。</p>
        </div>
        <button class="btn primary" type="button" :disabled="saving" @click="openCreate($event)">添加任务</button>
      </div>
      <div class="task-list">
        <article v-for="t in tasks" :key="t.id" class="panel task-card" :data-task-id="t.id">
          <div class="task-header"><span class="badge" :class="t.kind">{{ kindName[t.kind] || t.kind }}</span><span v-if="t.goal" class="badge goal">目标</span><strong>{{ t.text }}</strong><span class="meta">{{ t.status === 'paused' ? '已暂停' : '启用' }}</span><span v-if="goals[t.id]?.running" class="meta"><i class="spin"></i>推进中</span></div>
          <div class="meta task-details"><span>{{ plan(t) }}</span><span>{{ channelName(t.channel) }}</span><span>{{ t.status === 'paused' ? '原计划下次' : '下次触发' }} {{ fmtDT(t.next) }}</span>
            <span v-if="t.goal">{{ fmtNum((goals[t.id]?.usedTokens) || 0) }} / {{ fmtNum(goals[t.id]?.tokensLimit || t.maxTokens || 0) }} tokens</span>
            <span v-if="t.goal && t.skill">技能 {{ t.skill }}</span></div>
          <p v-if="t.todoId" class="meta">关联待办提醒，请在待办页面管理。</p>
          <div class="task-actions" :aria-busy="busy.has(t.id)">
            <button v-if="t.goal" class="btn" type="button" @click="viewRuns('goal-' + t.id)">查看执行轮次</button>
            <button v-if="t.goal && goals[t.id]?.running" class="btn" type="button" @click="stopGoal(t)">停止这次推进</button>
            <template v-if="!t.todoId">
              <button class="btn" :disabled="busy.has(t.id) || saving" @click="edit(t, $event)">编辑</button>
              <button class="btn" :disabled="busy.has(t.id) || saving && editing?.id === t.id" @click="changeStatus(t)">{{ t.status === 'paused' ? '恢复' : '暂停' }}</button>
              <button class="btn danger" :disabled="busy.has(t.id) || saving && editing?.id === t.id" @click="remove(t)">取消</button>
            </template>
            <button class="btn" :disabled="historyBusy" :aria-expanded="expanded === t.id" @click="history(t)">执行历史</button>
          </div>
          <div v-if="expanded === t.id" class="task-history" aria-live="polite">
            <p v-if="historyBusy" class="meta">正在加载执行历史…</p>
            <p v-else-if="historyError" class="msg err" role="alert">{{ historyError }}</p>
            <template v-else>
              <p v-if="!histories[t.id]?.length" class="meta">暂无执行记录</p>
              <div v-for="n in histories[t.id] || []" :key="n.id" class="task-history-item">
                <div class="meta">{{ fmtDT(n.createdAt) }} · {{ STATUS_NAME[n.status] || n.status }} · 已尝试 {{ n.attempts }} 次</div>
                <p>{{ n.text }}</p><p v-if="n.lastError" class="msg err">{{ n.lastError }}</p>
              </div>
            </template>
          </div>
        </article>
        <EmptyState v-if="!tasks.length" class="panel" kind="schedule" :size="96">当前没有未来安排，已触发任务的执行结果可在通知中心查看。</EmptyState>
      </div>
    </div>
    <Teleport to="body">
      <div v-if="formOpen" class="dlg-mask" @click.self="cancel">
        <div class="dlg" role="dialog" aria-modal="true" aria-labelledby="taskDialogTitle" @keydown.esc="cancel">
          <form class="dlg-form" :aria-busy="saving" @submit.prevent="submit">
            <header class="dlg-head">
              <h3 id="taskDialogTitle" class="dlg-title">{{ title }}</h3>
              <button type="button" class="set-close" aria-label="关闭任务表单" @click="cancel">
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
                     stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
              </button>
            </header>
            <div class="dlg-body task-form">
              <label class="task-wide">任务内容<input ref="textInput" v-model="draft.text" aria-label="任务内容" required :disabled="saving" placeholder="如：提醒我喝水"></label>
              <label>定时方式<SelectBox v-model="draft.mode" :options="MODE_OPTIONS" aria-label="定时方式" :disabled="saving" /></label>
              <!-- 单次：快捷项换算成 delayMinutes，或填进时刻选择器按 when 提交绝对时刻 -->
              <template v-if="draft.mode === 'once'">
                <div class="task-wide quick" role="group" aria-label="快捷触发时间">
                  <button v-for="q in QUICK_DELAYS" :key="q.minutes" type="button" class="btn sm"
                          :class="{ on: draft.delay === q.minutes }" :aria-pressed="draft.delay === q.minutes"
                          :disabled="saving" @click="pickDelay(q.minutes)">{{ q.label }}</button>
                  <button v-for="q in QUICK_MOMENTS" :key="q.key" type="button" class="btn sm"
                          :class="{ on: isQuickMoment(q.key) }" :aria-pressed="isQuickMoment(q.key)"
                          :disabled="saving" @click="pickMoment(q.key)">{{ q.label }}</button>
                </div>
                <label class="task-wide">触发时刻<input :value="draft.at" type="datetime-local" aria-label="触发时刻"
                        :disabled="saving" @input="setAt($event.target.value)">
                  <span v-if="oncePast" class="msg err hint-past" role="status">该时刻已过，请重选</span>
                </label>
              </template>

              <!-- 有限多次：数字 + 单位，提交时拼成 in -->
              <label v-if="draft.mode === 'repeat'">间隔<span class="num-unit"><input v-model="draft.repNum" type="number" min="1" step="1" aria-label="间隔" required :disabled="saving"><SelectBox v-model="draft.repUnit" :options="REPEAT_UNITS" aria-label="间隔单位" :disabled="saving" /></span></label>
              <label v-if="draft.mode === 'repeat'">总次数<input v-model="draft.times" type="number" min="2" step="1" aria-label="总次数" required :disabled="saving"></label>
              <!-- 循环：结构化规则为主，保留"直接写表达式"给高级用户与存量任务 -->
              <template v-if="draft.mode === 'cron'">
                <div class="task-wide cron-switch" role="tablist" aria-label="循环规则写法">
                  <button type="button" role="tab" :aria-selected="draft.cronVia === 'rule'" :class="{ active: draft.cronVia === 'rule' }" :disabled="saving" @click="draft.cronVia = 'rule'">结构化规则</button>
                  <button type="button" role="tab" :aria-selected="draft.cronVia === 'expr'" :class="{ active: draft.cronVia === 'expr' }" :disabled="saving" @click="draft.cronVia = 'expr'">直接写 cron 表达式</button>
                </div>
                <template v-if="draft.cronVia === 'rule'">
                  <label>循环方式<SelectBox v-model="draft.rule.unit" :options="UNIT_OPTIONS" aria-label="循环方式" :disabled="saving" /></label>
                  <label v-if="draft.rule.unit === 'minute' || draft.rule.unit === 'hourly'">{{ draft.rule.unit === 'minute' ? '每 N 分钟' : '每 N 小时' }}<input v-model="draft.rule.interval" type="number" min="1" :max="draft.rule.unit === 'minute' ? 59 : 23" step="1" aria-label="循环间隔" required :disabled="saving"></label>
                  <label v-if="draft.rule.unit !== 'minute'">{{ draft.rule.unit === 'hourly' ? '每小时第几分钟' : '触发分钟' }}<input v-model="draft.rule.minute" type="number" min="0" max="59" step="1" aria-label="触发分钟" required :disabled="saving"></label>
                  <label v-if="draft.rule.unit !== 'minute' && draft.rule.unit !== 'hourly'">触发小时<input v-model="draft.rule.hour" type="number" min="0" max="23" step="1" aria-label="触发小时" required :disabled="saving"></label>
                  <div v-if="draft.rule.unit === 'weekly'" class="task-wide pick" role="group" aria-label="星期几">
                    <span class="pick-title meta">星期几</span>
                    <button v-for="d in WEEKDAYS" :key="d.v" type="button" class="chip" :title="d.l"
                            :class="{ on: draft.rule.weekdays.includes(d.v) }" :aria-pressed="draft.rule.weekdays.includes(d.v)"
                            :disabled="saving" @click="togglePick(draft.rule.weekdays, d.v)">{{ d.l }}</button>
                  </div>
                  <div v-if="draft.rule.unit === 'monthly' || draft.rule.unit === 'yearly'" class="task-wide pick" role="group" aria-label="每月几号">
                    <span class="pick-title meta">每月几号</span>
                    <button v-for="d in MONTH_DAYS" :key="d.v" type="button" class="chip" :title="'每月 ' + d.v + ' 号'"
                            :class="{ on: draft.rule.monthDays.includes(d.v) }" :aria-pressed="draft.rule.monthDays.includes(d.v)"
                            :disabled="saving" @click="togglePick(draft.rule.monthDays, d.v)">{{ d.l }}</button>
                  </div>
                  <div v-if="draft.rule.unit === 'yearly'" class="task-wide pick" role="group" aria-label="月份">
                    <span class="pick-title meta">月份</span>
                    <button v-for="m in MONTHS" :key="m.v" type="button" class="chip" :title="m.l"
                            :class="{ on: draft.rule.months.includes(m.v) }" :aria-pressed="draft.rule.months.includes(m.v)"
                            :disabled="saving" @click="togglePick(draft.rule.months, m.v)">{{ m.l }}</button>
                  </div>
                  <p v-if="rulePreview" class="meta task-wide">将按「{{ rulePreview }}」触发。</p>
                </template>
                <label v-else class="task-wide">循环规则<input v-model="draft.every" aria-label="循环规则" required :disabled="saving" placeholder="0 8 * * *（分 时 日 月 周）"></label>
              </template>
              <label>通知通道<SelectBox v-model="draft.channel" :options="CHANNEL_OPTIONS" aria-label="通知通道" :disabled="saving" /></label>
              <label class="task-check"><input v-model="draft.agent" type="checkbox" :disabled="saving" @change="onAgentToggle">智能任务：到点执行指令</label>
              <label v-if="draft.agent" class="task-check"><input v-model="draft.goal" type="checkbox" :disabled="saving">目标：多轮自主推进直到办成</label>
              <template v-if="draft.agent && draft.goal">
                <label>步数上限<input v-model="draft.maxSteps" type="number" min="1" step="1" aria-label="目标步数上限" :disabled="saving" placeholder="默认按配置"></label>
                <label>Token 预算<input v-model="draft.maxTokens" type="number" min="1" step="1000" aria-label="目标 token 预算" :disabled="saving" placeholder="默认按配置"></label>
                <label>技能（可留空）<input v-model="draft.skill" aria-label="目标钉住的技能" :disabled="saving" placeholder="见设置 → 技能"></label>
              </template>
              <p v-if="editing" class="meta task-wide">当前{{ editing.status === 'paused' ? '已暂停' : '启用' }}，原计划下次 {{ fmtDT(editing.next) }}。修改定时规则后重新计算计划及总次数；正文或通道修改保留剩余次数。</p>
            </div>
            <footer class="dlg-foot">
              <p v-if="message" class="msg err" role="alert">{{ message }}</p>
              <div class="dlg-btns">
                <button class="btn" type="button" :disabled="saving" @click="cancel">取消</button>
                <button class="btn primary" type="submit" :disabled="saving">{{ saving ? '保存中…' : '确认' }}</button>
              </div>
            </footer>
          </form>
        </div>
      </div>
    </Teleport>
  </section>
</template>

<style scoped>
.task-new { display: flex; align-items: center; justify-content: space-between; gap: 16px; flex-wrap: wrap; }
.task-new .panel-title { margin-bottom: 4px; }
.task-new .meta { margin: 0; }
.task-form { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(240px, 100%), 1fr)); gap: 16px; }
.task-form label { display: flex; flex-direction: column; gap: 8px; font-size: 14px; min-width: 0; }
.task-form input { width: 100%; box-sizing: border-box; }
.task-form .task-check { flex-direction: row; align-items: center; }
.task-check input { width: auto; }
.task-wide { grid-column: 1 / -1; }
/* 快捷时间：一行按钮，窄屏自动换行；选中态用描边反色，深色主题下同样可读 */
.quick { display: flex; flex-wrap: wrap; gap: 8px; }
.quick .btn { flex: 0 1 auto; min-height: 36px; }
.quick .btn.on { border-color: var(--primary); color: var(--primary); font-weight: 600; }
.num-unit { display: flex; gap: 8px; align-items: stretch; min-width: 0; }
.num-unit input { flex: 1 1 90px; min-width: 0; }
.num-unit .selbox { flex: 0 0 88px; }
.cron-switch { display: inline-flex; gap: 4px; flex-wrap: wrap; width: max-content; max-width: 100%;
  padding: 3px; border-radius: 999px; background: var(--bg-subtle); border: 1px solid var(--border); }
.cron-switch button { border: 0; background: transparent; color: var(--muted-fg); font: inherit; font-size: 12.5px;
  padding: 5px 14px; border-radius: 999px; cursor: pointer; }
.cron-switch button.active { background: var(--card); color: var(--fg); font-weight: 600; box-shadow: 0 1px 4px rgba(0,0,0,.12); }
.cron-switch button:disabled { opacity: .55; cursor: not-allowed; }
/* 星期几 / 几号 / 月份多选：chip 开关（不用 checkbox，避免与表单其它勾选混在一起） */
.pick { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.pick-title { flex: 1 0 100%; }
.pick .chip { cursor: pointer; border: 1px solid var(--border); background: var(--card); color: var(--muted-fg);
  font-size: 12px; padding: 4px 10px; border-radius: 999px; }
.pick .chip:hover { border-color: var(--border-strong); color: var(--fg); }
.pick .chip.on { border-color: var(--primary); color: var(--primary); font-weight: 600; }
.pick .chip:disabled { opacity: .55; cursor: not-allowed; }
.task-actions, .task-header, .task-details { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; }
.task-actions .btn, .dlg-btns .btn { min-height: 44px; }
.task-list { display: grid; gap: 16px; margin-top: 20px; }
.task-header strong { overflow-wrap: anywhere; flex: 1; }
.task-details { margin: 12px 0 16px; }
.task-history { border-top: 1px solid var(--border); padding-top: 16px; margin-top: 16px; }
.task-history-item + .task-history-item { margin-top: 16px; }
.task-history-item p { white-space: pre-wrap; overflow-wrap: anywhere; margin: 8px 0; }
</style>
