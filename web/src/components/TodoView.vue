<script setup>
// 待办清单：增/勾选完成/删除，与 agent 工具共用一份存储。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { fmtDT } from '../format.js'
import { toast } from '../store.js'
import EmptyState from './art/EmptyState.vue'

const text = ref('')
const dueAt = ref('')
const remindAt = ref('')
const editingID = ref(null)
const submitting = ref(false)
const syncing = ref(false)
const syncError = ref('')
const pendingActions = reactive(new Set())
const todos = ref([])
const msg = reactive({ text: '', cls: '' })
const now = ref(Date.now())
let msgTimer = null
let clockTimer = null
let disposed = false

function showMsg(t, ok) {
  msg.text = t
  msg.cls = ok ? 'ok' : 'err'
  clearTimeout(msgTimer)
  msgTimer = setTimeout(() => { msg.text = '' }, 4000)
}

const pending = computed(() => todos.value.filter(t => !t.done).length)
const countLine = computed(() => todos.value.length ? pending.value + ' 项待完成 / 共 ' + todos.value.length + ' 条' : '')

function status(t) {
  if (t.done) return ['已完成', 'teal']
  if (t.dueAt && new Date(t.dueAt).getTime() < now.value) return ['已逾期', 'warn']
  return ['待完成', 'blue']
}
function localTime(value) {
  if (!value) return ''
  const date = new Date(value)
  const pad = number => String(number).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}
function timestamp(value) {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) throw new Error('请输入有效的日期与时间')
  return date.toISOString()
}
function resetForm() {
  text.value = dueAt.value = remindAt.value = ''
  editingID.value = null
}
function edit(t) {
  if (submitting.value || pendingActions.has(t.id)) return
  editingID.value = t.id
  text.value = t.text
  dueAt.value = localTime(t.dueAt)
  remindAt.value = t.done ? '' : localTime(t.remindAt)
}

async function loadTodos() {
  try {
    const result = await api('GET', '/api/todos')
    if (disposed) return
    todos.value = result.todos || []
    syncError.value = result.syncError || ''
  } catch (e) { if (!disposed) toast('待办加载失败：' + e.message, true) }
}

async function submit() {
  if (submitting.value) return
  submitting.value = true
  try {
    const body = { text: text.value.trim(), dueAt: timestamp(dueAt.value), remindAt: timestamp(remindAt.value) }
    await api(editingID.value ? 'PUT' : 'POST', editingID.value ? '/api/todos/' + encodeURIComponent(editingID.value) : '/api/todos', body)
    if (disposed) return
    resetForm()
    showMsg('待办已保存', true)
  } catch (e) {
    if (!disposed) {
      // 后端明确说明源事实已经提交时不能再次创建，改为恢复提醒同步。
      if (e.message.includes('待办已保存')) resetForm()
      showMsg(e.message, false)
    }
  } finally {
    submitting.value = false
    if (!disposed) await loadTodos()
  }
}

async function toggle(t, done) {
  if (pendingActions.has(t.id)) return
  pendingActions.add(t.id)
  try { await api('POST', '/api/todos/' + encodeURIComponent(t.id), { done }) }
  catch (e) { if (!disposed) showMsg(e.message, false) }
  finally { pendingActions.delete(t.id); if (!disposed) await loadTodos() }
}

async function remove(t) {
  if (pendingActions.has(t.id)) return
  if (!confirm('确定删除待办 ' + t.id + '？')) return
  pendingActions.add(t.id)
  try {
    await api('DELETE', '/api/todos/' + encodeURIComponent(t.id))
    if (disposed) return
    if (editingID.value === t.id) resetForm()
    toast('已删除')
  } catch (e) { if (!disposed) showMsg(e.message, false) }
  finally { pendingActions.delete(t.id); if (!disposed) await loadTodos() }
}

async function reconcile() {
  if (syncing.value) return
  syncing.value = true
  try { await api('POST', '/api/todos/reconcile', {}); if (!disposed) toast('提醒已恢复同步') }
  catch (e) { if (!disposed) showMsg(e.message, false) }
  finally { syncing.value = false; if (!disposed) await loadTodos() }
}

onMounted(() => { loadTodos(); clockTimer = setInterval(() => { now.value = Date.now() }, 60000) })
onUnmounted(() => { disposed = true; clearTimeout(msgTimer); clearInterval(clockTimer) })
</script>

<template>
  <section class="view" id="view-todo">
    <div class="view-inner">
      <h2>待办 <span class="count">{{ countLine }}</span></h2>
      <div class="panel">
        <div class="panel-title">{{ editingID ? '编辑待办' : '新增待办' }}</div>
        <div v-if="syncError" class="msg err" role="alert">
          待办已保存，提醒同步尚未完成：{{ syncError }}
          <button class="btn sm" :disabled="syncing" @click="reconcile">{{ syncing ? '同步中…' : '重试同步' }}</button>
        </div>
        <div class="msg" :class="msg.cls" role="status">{{ msg.text }}</div>
        <form @submit.prevent="submit" :aria-busy="submitting">
          <label>
            <span class="meta">内容</span>
            <input v-model="text" aria-label="待办内容" placeholder="新增待办，如：交房租" :disabled="submitting" required>
          </label>
          <label>
            <span class="meta">截止时间（本地，可选）</span>
            <input v-model="dueAt" type="datetime-local" aria-label="截止时间" :disabled="submitting">
          </label>
          <label>
            <span class="meta">提醒时间（本地，可选）</span>
            <input v-model="remindAt" type="datetime-local" aria-label="提醒时间" :disabled="submitting">
          </label>
          <button class="btn primary" type="submit" :disabled="submitting">{{ submitting ? '保存中…' : editingID ? '保存' : '添加' }}</button>
          <button v-if="editingID" class="btn" type="button" :disabled="submitting" @click="resetForm">取消编辑</button>
        </form>
        <p class="meta">截止时间用于标记逾期；填写提醒时间才会创建提醒。完成或删除待办会取消未来提醒。</p>
        <div v-if="todos.length" style="overflow-x:auto">
        <table>
          <thead><tr><th style="width:36px"></th><th>内容</th><th>状态</th><th>截止 / 提醒</th><th></th></tr></thead>
          <tbody>
            <tr v-for="t in todos" :key="t.id">
              <td><input type="checkbox" :checked="t.done" :disabled="pendingActions.has(t.id)" :aria-label="'标记完成 ' + t.text" @change="toggle(t, $event.target.checked)"></td>
              <td :class="{ done: t.done }">{{ t.text }}</td>
              <td><span class="badge" :class="status(t)[1]">{{ status(t)[0] }}</span></td>
              <td class="meta">
                <div>{{ t.dueAt ? '截止 ' + fmtDT(t.dueAt) : '无截止时间' }}</div>
                <div v-if="t.remindAt && !t.done">提醒 {{ fmtDT(t.remindAt) }}</div>
              </td>
              <td><div style="display:flex;gap:6px">
                <button class="btn sm" :disabled="pendingActions.has(t.id) || submitting" @click="edit(t)">编辑</button>
                <button class="btn sm danger" :disabled="pendingActions.has(t.id)" @click="remove(t)">删除</button>
              </div></td>
            </tr>
          </tbody>
        </table>
        </div>
        <EmptyState v-else kind="todo" :size="100">待办清单是空的，对小五说"记个待办：买牛奶"，或直接在上方添加</EmptyState>
      </div>
    </div>
  </section>
</template>
