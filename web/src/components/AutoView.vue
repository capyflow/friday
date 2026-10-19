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
const emptyDraft = () => ({ text: '', mode: 'once', arg: '', interval: '', times: 3, every: '', channel: '', agent: false, goal: false, maxSteps: '', maxTokens: '', skill: '' })
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
function duration(t) { return typeof t.interval === 'number' ? (t.interval / 1e9) + 's' : t.interval }
function plan(t) {
  if (t.cron) return 'cron: ' + t.cron
  if (t.interval) return '每 ' + duration(t) + '，剩 ' + t.remaining + '/' + t.times + ' 次'
  return '单次 · ' + fmtDT(t.when)
}
function channelName(channel) { return CHANNEL_OPTIONS.find(o => o.value === channel)?.label || channel || '全部可用通道' }
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
  Object.assign(draft, { text: t.text, mode: taskMode(t), arg: t.when || t.next || '', interval: duration(t) || '',
    times: t.times || 3, every: t.cron || '', channel: t.channel || '', agent: t.kind === 'agent',
    goal: !!t.goal, maxSteps: t.maxSteps ?? '', maxTokens: t.maxTokens ?? '', skill: t.skill || '' })
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
    const arg = draft.arg.trim()
    if (!arg) throw new Error('请填写触发时间')
    if (arg.includes('T')) body.when = arg; else body.in = arg
  } else if (draft.mode === 'repeat') {
    body.in = draft.interval.trim()
    body.times = Number(draft.times)
    if (!body.in || !Number.isInteger(body.times) || body.times < 2) throw new Error('请填写间隔及至少 2 次的整数次数')
  } else {
    body.every = draft.every.trim()
    if (!body.every) throw new Error('请填写循环规则')
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
              <label v-if="draft.mode === 'once'">触发时间<input v-model="draft.arg" aria-label="触发时间" required :disabled="saving" placeholder="10分钟 / 2026-10-11T08:00:00+08:00"></label>
              <label v-if="draft.mode === 'repeat'">间隔<input v-model="draft.interval" aria-label="间隔" required :disabled="saving" placeholder="如：10分钟"></label>
              <label v-if="draft.mode === 'repeat'">总次数<input v-model="draft.times" aria-label="总次数" type="number" min="2" step="1" required :disabled="saving"></label>
              <label v-if="draft.mode === 'cron'">循环规则<input v-model="draft.every" aria-label="循环规则" required :disabled="saving" placeholder="每天早上8点 / 0 8 * * *"></label>
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
.task-actions, .task-header, .task-details { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; }
.task-actions .btn, .dlg-btns .btn { min-height: 44px; }
.task-list { display: grid; gap: 16px; margin-top: 20px; }
.task-header strong { overflow-wrap: anywhere; flex: 1; }
.task-details { margin: 12px 0 16px; }
.task-history { border-top: 1px solid var(--border); padding-top: 16px; margin-top: 16px; }
.task-history-item + .task-history-item { margin-top: 16px; }
.task-history-item p { white-space: pre-wrap; overflow-wrap: anywhere; margin: 8px 0; }
</style>
