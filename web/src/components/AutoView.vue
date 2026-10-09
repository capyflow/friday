<script setup>
// 自动化（定时任务）：单次/多次/循环/智能任务的新建与取消。
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { fmtDT, kindName } from '../format.js'
import { toast } from '../store.js'
import SelectBox from './SelectBox.vue'

const KIND_OPTIONS = [
  { value: 'once', label: '单次' },
  { value: 'repeat', label: '多次' },
  { value: 'cron', label: '循环' },
  { value: 'agent', label: '智能任务' },
]

const kind = ref('once')
const text = ref('')
const arg = ref('')
const interval = ref('')
const times = ref('')
const every = ref('')
const tasks = ref([])
const msg = reactive({ text: '', cls: '' })
let msgTimer = null

function showMsg(t, ok) {
  msg.text = t
  msg.cls = ok ? 'ok' : 'err'
  clearTimeout(msgTimer)
  msgTimer = setTimeout(() => { msg.text = '' }, 4000)
}

const show = computed(() => ({
  arg: kind.value === 'once',
  interval: kind.value === 'repeat',
  times: kind.value === 'repeat',
  every: kind.value === 'cron' || kind.value === 'agent',
}))
const PLACEHOLDERS = {
  arg: '10分钟后 / 2026-09-01T20:00:00+08:00',
  interval: '间隔，如 10分钟',
  everyCron: '每天早上8点 / 0 8 * * *',
  everyAgent: '触发时刻：每天早上8点 / 0 8 * * * / 每小时',
}
const textPlaceholder = computed(() => kind.value === 'agent'
  ? '智能任务指令，如：看看 notes.txt，把今天要做的事告诉我'
  : '任务内容，如：该敷面膜了')
const everyPlaceholder = computed(() =>
  kind.value === 'agent' ? PLACEHOLDERS.everyAgent : PLACEHOLDERS.everyCron)

function plan(t) {
  if (t.kind === 'repeat') return '每 ' + (t.interval || '') + ' ×' + (t.remaining ?? '?') + '/' + (t.times ?? '?')
  if (t.kind === 'cron') return 'cron: ' + (t.cron || '')
  if (t.kind === 'agent') return (t.heartbeat ? '心跳 · ' : 'agent · ') + 'cron: ' + (t.cron || '')
  return '—'
}

async function loadTasks() {
  try {
    tasks.value = (await api('GET', '/api/tasks')).tasks || []
  } catch (e) { toast('任务加载失败：' + e.message, true) }
}

async function submit() {
  const body = { text: text.value.trim() }
  if (kind.value === 'once') {
    const v = arg.value.trim()
    if (v.includes('T')) body.when = v; else body.in = v
  } else if (kind.value === 'repeat') {
    body.in = interval.value.trim()
    body.times = Number(times.value)
  } else if (kind.value === 'agent') {
    body.agent = true
    body.every = every.value.trim()
  } else {
    body.every = every.value.trim()
  }
  try {
    const r = await api('POST', '/api/tasks', body)
    toast(r.message || '已添加')
    text.value = arg.value = interval.value = times.value = every.value = ''
    loadTasks()
  } catch (e) { showMsg(e.message, false) }
}

async function remove(t) {
  if (!confirm('确定取消任务 ' + t.id + '？')) return
  try {
    const r = await api('DELETE', '/api/tasks/' + t.id)
    toast(r.message || '已取消')
    loadTasks()
  } catch (e) { showMsg(e.message, false) }
}

onMounted(loadTasks)
</script>

<template>
  <section class="view" id="view-auto">
    <div class="view-inner">
      <h2>自动化 <span class="count">{{ tasks.length ? tasks.length + ' 条' : '' }}</span></h2>
      <div class="panel">
        <div class="panel-title">新建任务</div>
        <div class="msg" :class="msg.cls">{{ msg.text }}</div>
        <form @submit.prevent="submit">
          <SelectBox v-model="kind" :options="KIND_OPTIONS" aria-label="任务类型" style="min-width:112px"></SelectBox>
          <input v-model="text" class="w1" :placeholder="textPlaceholder" required>
          <input v-if="show.arg" v-model="arg" :placeholder="PLACEHOLDERS.arg">
          <input v-if="show.interval" v-model="interval" :placeholder="PLACEHOLDERS.interval">
          <input v-if="show.times" v-model="times" type="number" min="2" placeholder="次数">
          <input v-if="show.every" v-model="every" :placeholder="everyPlaceholder">
          <button class="btn primary" type="submit">添加任务</button>
        </form>
        <table v-if="tasks.length">
          <thead><tr><th>类型</th><th>内容</th><th>计划</th><th>下次触发</th><th></th></tr></thead>
          <tbody>
            <tr v-for="t in tasks" :key="t.id">
              <td><span class="badge" :class="t.kind">{{ kindName[t.kind] || t.kind }}</span></td>
              <td>{{ t.text }}</td>
              <td class="meta">{{ plan(t) }}</td>
              <td class="mono">{{ fmtDT(t.next) }}</td>
              <td><button class="btn sm danger" @click="remove(t)">取消</button></td>
            </tr>
          </tbody>
        </table>
        <div v-else class="empty">当前没有待触发的任务</div>
      </div>
    </div>
  </section>
</template>
