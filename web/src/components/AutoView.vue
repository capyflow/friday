<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { fmtDT, kindName } from '../format.js'
import { toast } from '../store.js'
import SelectBox from './SelectBox.vue'

const MODE_OPTIONS = [{ value: 'once', label: '单次' }, { value: 'repeat', label: '有限多次' }, { value: 'cron', label: '循环' }]
const CHANNEL_OPTIONS = [{ value: '', label: '全部可用通道' }, { value: 'web', label: '通知中心' },
  { value: 'dingtalk', label: '钉钉' }, { value: 'feishu', label: '飞书' }, { value: 'console', label: '本地输出' }]
const STATUS_NAME = { pending: '待处理', processing: '处理中', delivered: '已送达', failed: '失败', silent: '静默' }
const emptyDraft = () => ({ text: '', mode: 'once', arg: '', interval: '', times: 3, every: '', channel: '', agent: false })
const draft = reactive(emptyDraft())
const tasks = ref([])
const editing = ref(null)
const textInput = ref(null)
const message = ref('')
const saving = ref(false)
const busy = reactive(new Set())
const histories = reactive({})
const expanded = ref(null)
const historyBusy = ref(false)
const historyError = ref('')
let poll
let alive = true
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
async function edit(t) {
  if (saving.value || busy.has(t.id)) return
  editing.value = t
  Object.assign(draft, { text: t.text, mode: taskMode(t), arg: t.when || t.next || '', interval: duration(t) || '',
    times: t.times || 3, every: t.cron || '', channel: t.channel || '', agent: t.kind === 'agent' })
  message.value = ''
  await nextTick()
  textInput.value?.focus()
}
async function loadTasks() {
  try {
    const r = await api('GET', '/api/tasks')
    if (alive) tasks.value = r.tasks || []
  } catch (e) { if (alive) toast('任务加载失败：' + e.message, true) }
}
function formBody() {
  const body = { text: draft.text.trim(), agent: draft.agent, channel: draft.channel }
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
    resetForm()
    await loadTasks()
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
    await loadTasks()
  } catch (e) { if (alive) toast('操作失败：' + e.message, true) } finally { busy.delete(t.id) }
}
async function remove(t) {
  if (busy.has(t.id) || saving.value && editing.value?.id === t.id || !confirm('取消任务 ' + t.id + ' 的未来安排？已经入队的执行会保留。')) return
  busy.add(t.id)
  try {
    const r = await api('DELETE', '/api/tasks/' + encodeURIComponent(t.id))
    if (!alive) return
    toast(r.message || '未来安排已取消')
    if (editing.value?.id === t.id) resetForm()
    await loadTasks()
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
onMounted(() => { loadTasks(); poll = setInterval(loadTasks, 5000) })
onBeforeUnmount(() => { alive = false; clearInterval(poll) })
</script>

<template>
  <section class="view" id="view-auto">
    <div class="view-inner">
      <h2>自动化 <span class="count">{{ tasks.length ? tasks.length + ' 条' : '' }}</span></h2>
      <div class="panel">
        <div class="panel-title">{{ title }}</div>
        <p class="meta">编辑、暂停和取消只影响未来安排，已经入队或处理中的执行会保留。</p>
        <form class="task-form" :aria-busy="saving" @submit.prevent="submit">
          <label class="task-wide">任务内容<input ref="textInput" v-model="draft.text" aria-label="任务内容" required :disabled="saving" placeholder="如：提醒我喝水"></label>
          <label>定时方式<SelectBox v-model="draft.mode" :options="MODE_OPTIONS" aria-label="定时方式" :disabled="saving" /></label>
          <label v-if="draft.mode === 'once'">触发时间<input v-model="draft.arg" aria-label="触发时间" required :disabled="saving" placeholder="10分钟 / 2026-10-11T08:00:00+08:00"></label>
          <label v-if="draft.mode === 'repeat'">间隔<input v-model="draft.interval" aria-label="间隔" required :disabled="saving" placeholder="如：10分钟"></label>
          <label v-if="draft.mode === 'repeat'">总次数<input v-model="draft.times" aria-label="总次数" type="number" min="2" step="1" required :disabled="saving"></label>
          <label v-if="draft.mode === 'cron'">循环规则<input v-model="draft.every" aria-label="循环规则" required :disabled="saving" placeholder="每天早上8点 / 0 8 * * *"></label>
          <label>通知通道<SelectBox v-model="draft.channel" :options="CHANNEL_OPTIONS" aria-label="通知通道" :disabled="saving" /></label>
          <label class="task-check"><input v-model="draft.agent" type="checkbox" :disabled="saving">智能任务：到点执行指令</label>
          <p v-if="editing" class="meta task-wide">当前{{ editing.status === 'paused' ? '已暂停' : '启用' }}，原计划下次 {{ fmtDT(editing.next) }}。修改定时规则后重新计算计划及总次数；正文或通道修改保留剩余次数。</p>
          <p v-if="message" class="msg err task-wide" role="alert">{{ message }}</p>
          <div class="task-actions task-wide">
            <button class="btn primary" type="submit" :disabled="saving">{{ saving ? '保存中…' : editing ? '保存未来安排' : '添加任务' }}</button>
            <button v-if="editing" class="btn" type="button" :disabled="saving" @click="resetForm">放弃编辑</button>
          </div>
        </form>
      </div>
      <div class="task-list">
        <article v-for="t in tasks" :key="t.id" class="panel task-card" :data-task-id="t.id">
          <div class="task-header"><span class="badge" :class="t.kind">{{ kindName[t.kind] || t.kind }}</span><strong>{{ t.text }}</strong><span class="meta">{{ t.status === 'paused' ? '已暂停' : '启用' }}</span></div>
          <div class="meta task-details"><span>{{ plan(t) }}</span><span>{{ channelName(t.channel) }}</span><span>{{ t.status === 'paused' ? '原计划下次' : '下次触发' }} {{ fmtDT(t.next) }}</span></div>
          <p v-if="t.todoId" class="meta">关联待办提醒，请在待办页面管理。</p>
          <div class="task-actions" :aria-busy="busy.has(t.id)">
            <template v-if="!t.todoId">
              <button class="btn" :disabled="busy.has(t.id) || saving" @click="edit(t)">编辑</button>
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
        <div v-if="!tasks.length" class="panel empty">当前没有未来安排，已触发任务的执行结果可在通知中心查看。</div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.task-form { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(240px, 100%), 1fr)); gap: 16px; }
.task-form label { display: flex; flex-direction: column; gap: 8px; font-size: 14px; min-width: 0; }
.task-form input { width: 100%; box-sizing: border-box; }
.task-form .task-check { flex-direction: row; align-items: center; }
.task-check input { width: auto; }
.task-wide { grid-column: 1 / -1; }
.task-actions, .task-header, .task-details { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; }
.task-actions .btn { min-height: 44px; }
.task-list { display: grid; gap: 16px; margin-top: 20px; }
.task-header strong { overflow-wrap: anywhere; flex: 1; }
.task-details { margin: 12px 0 16px; }
.task-history { border-top: 1px solid var(--border); padding-top: 16px; margin-top: 16px; }
.task-history-item + .task-history-item { margin-top: 16px; }
.task-history-item p { white-space: pre-wrap; overflow-wrap: anywhere; margin: 8px 0; }
</style>
