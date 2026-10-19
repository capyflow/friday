<script setup>
// 闪念：笔记 + 待办的统一时间线（简易 Blinko）。与小五工具共用一份存储：
// 笔记支持 Markdown 与 #标签，待办保留截止/提醒；置顶固定顶部，已完成待办有专门视图。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { fmtDT } from '../format.js'
import { toast } from '../store.js'
import { renderMD } from '../md.js'
import EmptyState from './art/EmptyState.vue'

const TABS = [
  { id: 'all', label: '全部' },
  { id: 'note', label: '笔记' },
  { id: 'todo', label: '待办' },
  { id: 'done', label: '已完成' },
]

const entries = ref([])
const tags = ref([])
const syncError = ref('')
const tab = ref('all')
const searchQ = ref('')
const activeTag = ref('')
const composing = ref('note')
const draft = ref('')
const draftDueAt = ref('')
const draftRemindAt = ref('')
const editingId = ref(null)
const editText = ref('')
const editDueAt = ref('')
const editRemindAt = ref('')
const submitting = ref(false)
const syncing = ref(false)
const pendingActions = reactive(new Set())
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

const countLine = computed(() => {
  if (!entries.value.length) return ''
  const notes = entries.value.filter(e => e.kind === 'note').length
  const pendingTodos = entries.value.filter(e => e.kind === 'todo' && !e.done).length
  return `${notes} 条笔记 · ${pendingTodos} 项未完成 · 共 ${entries.value.length} 条`
})

function matchFilter(e) {
  if (tab.value === 'note' && e.kind !== 'note') return false
  if (tab.value === 'todo' && (e.kind !== 'todo' || e.done)) return false
  if (tab.value === 'done' && (e.kind !== 'todo' || !e.done)) return false
  const q = searchQ.value.trim().toLowerCase()
  if (q && !e.text.toLowerCase().includes(q)) return false
  if (activeTag.value && !(e.tags || []).includes(activeTag.value)) return false
  return true
}

const filtered = computed(() => entries.value.filter(matchFilter))
const pinnedEntries = computed(() => tab.value === 'done' ? [] : filtered.value.filter(e => e.pinned))
const timelineEntries = computed(() => {
  const list = filtered.value.filter(e => tab.value === 'done' || !e.pinned)
  if (tab.value === 'done') {
    return [...list].sort((a, b) => new Date(b.doneAt || b.createdAt) - new Date(a.doneAt || a.createdAt))
  }
  return [...list].sort((a, b) => new Date(b.createdAt) - new Date(a.createdAt))
})

const WEEK = ['周日', '周一', '周二', '周三', '周四', '周五', '周六']
function dayKey(value) {
  const d = new Date(value)
  return `${d.getFullYear()}-${d.getMonth() + 1}-${d.getDate()}`
}
function dayLabel(value) {
  const d = new Date(value)
  const today = new Date(now.value)
  const key = dayKey(value)
  if (key === dayKey(today)) return '今天'
  const yesterday = new Date(now.value - 86400000)
  if (key === dayKey(yesterday)) return '昨天'
  return `${d.getMonth() + 1}月${d.getDate()}日 ${WEEK[d.getDay()]}`
}
const groups = computed(() => {
  const out = []
  if (pinnedEntries.value.length) {
    out.push({ key: 'pinned', label: '置顶', items: pinnedEntries.value })
  }
  let current = null
  for (const e of timelineEntries.value) {
    const when = tab.value === 'done' ? (e.doneAt || e.createdAt) : e.createdAt
    const key = dayKey(when)
    if (!current || current.key !== key) {
      current = { key, label: dayLabel(when), items: [] }
      out.push(current)
    }
    current.items.push(e)
  }
  return out
})

const emptyText = computed(() => {
  if (tab.value === 'done') return '还没有已完成的待办，勾选一个试试'
  if (tab.value === 'todo') return '没有未完成的待办，对小五说"记个待办：买牛奶"，或在上方新建'
  if (tab.value === 'note') return '还没有笔记，随手记点想法，支持 #标签 和 Markdown'
  return '清单是空的：想法记成笔记，要做的事记成待办，或对小五说"记一下…"'
})

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
function isOverdue(e) {
  return e.dueAt && !e.done && new Date(e.dueAt).getTime() < now.value
}

async function loadEntries() {
  try {
    const result = await api('GET', '/api/entries')
    if (disposed) return
    entries.value = result.entries || []
    tags.value = result.tags || []
    syncError.value = result.syncError || ''
  } catch (e) { if (!disposed) toast('闪念加载失败：' + e.message, true) }
}

async function mutate(id, fn, okText) {
  try {
    await fn()
    if (!disposed && okText) toast(okText)
  } catch (e) {
    if (!disposed) showMsg(e.message, false)
  } finally {
    pendingActions.delete(id)
    if (!disposed) await loadEntries()
  }
}

async function submitDraft() {
  const text = draft.value.trim()
  if (!text || submitting.value) return
  submitting.value = true
  try {
    const body = { kind: composing.value, text }
    if (composing.value === 'todo') {
      body.dueAt = timestamp(draftDueAt.value)
      body.remindAt = timestamp(draftRemindAt.value)
    }
    await api('POST', '/api/entries', body)
    if (disposed) return
    draft.value = draftDueAt.value = draftRemindAt.value = ''
    showMsg('已记下', true)
  } catch (e) {
    if (!disposed) showMsg(e.message, false)
  } finally {
    submitting.value = false
    if (!disposed) await loadEntries()
  }
}
function onComposerKey(e) {
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); submitDraft() }
}

function startEdit(e) {
  if (submitting.value || pendingActions.has(e.id)) return
  editingId.value = e.id
  editText.value = e.text
  editDueAt.value = localTime(e.dueAt)
  editRemindAt.value = e.done ? '' : localTime(e.remindAt)
}
function cancelEdit() { editingId.value = null }
async function saveEdit(e) {
  if (submitting.value) return
  submitting.value = true
  try {
    const body = { text: editText.value.trim() }
    if (e.kind === 'todo') {
      body.dueAt = timestamp(editDueAt.value)
      body.remindAt = timestamp(editRemindAt.value)
    }
    await api('PUT', '/api/entries/' + encodeURIComponent(e.id), body)
    if (disposed) return
    editingId.value = null
    showMsg('已保存', true)
  } catch (err) {
    if (!disposed) showMsg(err.message, false)
  } finally {
    submitting.value = false
    if (!disposed) await loadEntries()
  }
}
function toggle(e, done) {
  if (pendingActions.has(e.id)) return
  pendingActions.add(e.id)
  mutate(e.id, () => api('POST', '/api/entries/' + encodeURIComponent(e.id), { done }))
}
function togglePin(e) {
  if (pendingActions.has(e.id)) return
  pendingActions.add(e.id)
  mutate(e.id, () => api('POST', '/api/entries/' + encodeURIComponent(e.id) + '/pin', { pinned: !e.pinned }))
}
function remove(e) {
  if (pendingActions.has(e.id)) return
  if (!confirm('确定删除这条' + (e.kind === 'note' ? '笔记' : '待办') + '？')) return
  pendingActions.add(e.id)
  mutate(e.id, async () => {
    await api('DELETE', '/api/entries/' + encodeURIComponent(e.id))
    if (editingId.value === e.id) editingId.value = null
  }, '已删除')
}
async function reconcile() {
  if (syncing.value) return
  syncing.value = true
  try { await api('POST', '/api/entries/reconcile', {}); if (!disposed) toast('提醒已恢复同步') }
  catch (e) { if (!disposed) showMsg(e.message, false) }
  finally { syncing.value = false; if (!disposed) await loadEntries() }
}

onMounted(() => { loadEntries(); clockTimer = setInterval(() => { now.value = Date.now() }, 60000) })
onUnmounted(() => { disposed = true; clearTimeout(msgTimer); clearInterval(clockTimer) })
</script>

<template>
  <section class="view" id="view-blinko">
    <div class="view-inner">
      <h2>闪念 <span class="count">{{ countLine }}</span></h2>

      <!-- 记一笔：笔记 / 待办 -->
      <div class="panel blinko-composer">
        <div class="composer-tabs" role="tablist" aria-label="条目类型">
          <button type="button" role="tab" :aria-selected="composing === 'note'" :class="{ active: composing === 'note' }" @click="composing = 'note'">笔记</button>
          <button type="button" role="tab" :aria-selected="composing === 'todo'" :class="{ active: composing === 'todo' }" @click="composing = 'todo'">待办</button>
        </div>
        <textarea v-model="draft" rows="2" :aria-label="composing === 'note' ? '笔记内容' : '待办内容'"
                  :placeholder="composing === 'note' ? '记个想法… 支持 #标签 和 Markdown' : '要做什么？可设截止与提醒'"
                  :disabled="submitting" @keydown="onComposerKey"></textarea>
        <div v-if="composing === 'todo'" class="composer-times">
          <label><span class="meta">截止（可选）</span><input v-model="draftDueAt" type="datetime-local" aria-label="截止时间" :disabled="submitting"></label>
          <label><span class="meta">提醒（可选）</span><input v-model="draftRemindAt" type="datetime-local" aria-label="提醒时间" :disabled="submitting"></label>
        </div>
        <div class="composer-foot">
          <span class="meta">⌘/Ctrl + Enter 保存；#标签 可在下方筛选</span>
          <button class="btn primary sm" type="button" :disabled="submitting || !draft.trim()" @click="submitDraft">{{ submitting ? '保存中…' : '记下' }}</button>
        </div>
        <div v-if="syncError" class="msg err" role="alert">
          条目已保存，提醒同步尚未完成：{{ syncError }}
          <button class="btn sm" :disabled="syncing" @click="reconcile">{{ syncing ? '同步中…' : '重试同步' }}</button>
        </div>
        <div class="msg" :class="msg.cls" role="status">{{ msg.text }}</div>
      </div>

      <!-- 过滤：tabs + 搜索 + 标签 -->
      <div class="blinko-toolbar">
        <div class="blinko-tabs" role="tablist" aria-label="类型过滤">
          <button v-for="tItem in TABS" :key="tItem.id" type="button" role="tab"
                  :aria-selected="tab === tItem.id" :class="{ active: tab === tItem.id }"
                  @click="tab = tItem.id">{{ tItem.label }}</button>
        </div>
        <input v-model="searchQ" class="blinko-search" type="search" aria-label="搜索闪念" placeholder="搜索…">
      </div>
      <div v-if="tags.length" class="blinko-tagbar">
        <button class="chip" :class="{ on: !activeTag }" type="button" @click="activeTag = ''">全部</button>
        <button v-for="t in tags" :key="t" class="chip" :class="{ on: activeTag === t }" type="button" @click="activeTag = activeTag === t ? '' : t">#{{ t }}</button>
      </div>

      <!-- 时间线：置顶组 + 按天分组 -->
      <div v-for="g in groups" :key="g.key" class="blinko-group">
        <div class="group-head meta">{{ g.label }}</div>
        <article v-for="e in g.items" :key="e.id" class="entry-card" :class="{ pinned: g.key === 'pinned', 'is-done': e.done }">
          <div class="entry-main">
            <input v-if="e.kind === 'todo'" type="checkbox" :checked="e.done" :disabled="pendingActions.has(e.id)"
                   :aria-label="'标记完成 ' + e.text" @change="toggle(e, $event.target.checked)">
            <template v-if="editingId === e.id">
              <div class="entry-editor">
                <textarea v-model="editText" rows="3" aria-label="编辑内容"></textarea>
                <div v-if="e.kind === 'todo'" class="composer-times">
                  <label><span class="meta">截止</span><input v-model="editDueAt" type="datetime-local" aria-label="截止时间"></label>
                  <label><span class="meta">提醒</span><input v-model="editRemindAt" type="datetime-local" aria-label="提醒时间"></label>
                </div>
                <div class="editor-actions">
                  <button class="btn sm primary" :disabled="submitting" @click="saveEdit(e)">保存</button>
                  <button class="btn sm" :disabled="submitting" @click="cancelEdit">取消</button>
                </div>
              </div>
            </template>
            <template v-else>
              <div v-if="e.kind === 'note'" class="entry-md" v-html="renderMD(e.text)"></div>
              <div v-else class="entry-text" :class="{ done: e.done }">{{ e.text }}</div>
              <div class="entry-meta">
                <span v-if="isOverdue(e)" class="badge warn">已逾期</span>
                <span v-else-if="e.dueAt && !e.done" class="badge repeat">截止 {{ fmtDT(e.dueAt) }}</span>
                <span v-if="e.remindAt && !e.done" class="badge repeat">提醒 {{ fmtDT(e.remindAt) }}</span>
                <span v-if="e.doneAt" class="meta">完成于 {{ fmtDT(e.doneAt) }}</span>
                <button v-for="t in e.tags || []" :key="t" class="chip" type="button" @click="activeTag = activeTag === t ? '' : t">#{{ t }}</button>
              </div>
            </template>
          </div>
          <div class="entry-actions">
            <button class="btn sm icon" :class="{ on: e.pinned }" :disabled="pendingActions.has(e.id) || submitting"
                    :aria-label="e.pinned ? '取消置顶' : '置顶'" :title="e.pinned ? '取消置顶' : '置顶'" @click="togglePin(e)">★</button>
            <button class="btn sm" :disabled="pendingActions.has(e.id) || submitting" @click="editingId === e.id ? cancelEdit() : startEdit(e)">编辑</button>
            <button class="btn sm danger" :disabled="pendingActions.has(e.id)" @click="remove(e)">删除</button>
          </div>
        </article>
      </div>

      <EmptyState v-if="!filtered.length" kind="todo" :size="100">{{ emptyText }}</EmptyState>
    </div>
  </section>
</template>

<style scoped>
.blinko-composer { display: flex; flex-direction: column; gap: 10px; }
.composer-tabs { display: inline-flex; gap: 4px; width: max-content; padding: 3px; border-radius: 999px; background: var(--bg); }
.composer-tabs button { border: 0; background: transparent; color: var(--muted-fg); font: inherit; font-size: 12.5px;
  padding: 4px 14px; border-radius: 999px; cursor: pointer; }
.composer-tabs button.active { background: var(--card); color: var(--fg); font-weight: 600; box-shadow: var(--shadow-sm); }
.blinko-composer textarea { resize: vertical; min-height: 56px; }
.composer-times { display: flex; gap: 14px; flex-wrap: wrap; }
.composer-times label { display: flex; align-items: center; gap: 8px; font-size: 12.5px; }
.composer-times input { width: auto; }
.composer-foot { display: flex; align-items: center; justify-content: space-between; gap: 10px; }

.blinko-toolbar { display: flex; align-items: center; gap: 10px; margin: 16px 0 8px; }
.blinko-tabs { display: inline-flex; gap: 2px; padding: 3px; border-radius: 999px; background: var(--card); border: 1px solid var(--border); }
.blinko-tabs button { border: 0; background: transparent; color: var(--muted-fg); font: inherit; font-size: 12.5px;
  padding: 4px 13px; border-radius: 999px; cursor: pointer; }
.blinko-tabs button.active { background: var(--primary); color: var(--primary-fg, #fff); font-weight: 600; }
.blinko-search { max-width: 220px; margin-left: auto; }
.blinko-tagbar { display: flex; gap: 6px; flex-wrap: wrap; margin-bottom: 8px; }
.blinko-tagbar .chip { cursor: pointer; border: 1px solid var(--border); background: var(--card); }
.blinko-tagbar .chip.on { border-color: var(--primary); color: var(--primary); }

.blinko-group { margin-top: 14px; }
.group-head { padding: 0 2px 6px; }
.entry-card { display: flex; align-items: flex-start; gap: 10px; background: var(--card); border: 1px solid var(--border);
  border-radius: var(--radius, 10px); padding: 10px 12px; margin-bottom: 8px; }
.entry-card.pinned { border-color: var(--accent); }
.entry-card.is-done { opacity: .68; }
.entry-main { flex: 1; min-width: 0; display: flex; gap: 9px; align-items: flex-start; }
.entry-main > input[type="checkbox"] { margin-top: 4px; }
.entry-text { font-size: 13.5px; line-height: 1.6; white-space: pre-wrap; word-break: break-word; }
.entry-text.done { text-decoration: line-through; color: var(--muted-fg); }
.entry-md { font-size: 13.5px; line-height: 1.65; min-width: 0; word-break: break-word; }
.entry-md :deep(p) { margin: 0 0 6px; } .entry-md :deep(p:last-child) { margin-bottom: 0; }
.entry-md :deep(pre) { margin: 6px 0; }
.entry-md :deep(ul), .entry-md :deep(ol) { margin: 6px 0; padding-left: 20px; }
.entry-meta { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; margin-top: 6px; }
.entry-meta .chip { cursor: pointer; font-size: 11px; }
.entry-actions { display: flex; gap: 4px; flex-shrink: 0; }
.entry-actions .icon { color: var(--faint-fg); }
.entry-actions .icon.on { color: var(--accent); }
.entry-editor { flex: 1; display: flex; flex-direction: column; gap: 8px; }
.entry-editor textarea { resize: vertical; }
.editor-actions { display: flex; gap: 6px; }
</style>
