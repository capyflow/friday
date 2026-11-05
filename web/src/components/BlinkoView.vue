<script setup>
// 闪念：笔记 + 待办的统一时间线（简易 Blinko）。与小五工具共用一份存储：
// 右下角悬浮 + 打开编辑弹窗（正文即编辑区，标签经 # 按钮选择/新建），
// 笔记与待办正文都支持 Markdown（弹窗内一排格式按钮在选区上插入语法），
// 待办保留截止/提醒；点卡片正文弹出详情看完整内容，列表正文有高度上限、超出截断；
// 置顶固定顶部，已完成待办有专门视图。
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
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
const pendingActions = reactive(new Set())
const now = ref(Date.now())
let clockTimer = null
let disposed = false

/* ---------- 编辑弹窗（创建与编辑共用一个编辑区） ---------- */
const dialogOpen = ref(false)
const editingEntry = ref(null)
const form = reactive({ kind: 'note', text: '', dueAt: '', remindAt: '', tags: [] })
const showTimes = ref(false)
const tagPickerOpen = ref(false)
const newTag = ref('')
const submitting = ref(false)
const msg = reactive({ text: '', cls: '' })
const editorInput = ref(null)
let msgTimer = null

function showMsg(t, ok) {
  msg.text = t
  msg.cls = ok ? 'ok' : 'err'
  clearTimeout(msgTimer)
  msgTimer = setTimeout(() => { msg.text = '' }, 4000)
}

function resetForm(kind = 'note') {
  form.kind = kind
  form.text = ''
  form.dueAt = ''
  form.remindAt = ''
  form.tags = []
  showTimes.value = false
  tagPickerOpen.value = false
  newTag.value = ''
}
async function openCreate() {
  editingEntry.value = null
  resetForm()
  msg.text = ''
  dialogOpen.value = true
  await nextTick()
  editorInput.value?.focus()
}
async function openEdit(e) {
  if (submitting.value || pendingActions.has(e.id)) return
  editingEntry.value = e
  resetForm(e.kind)
  form.text = e.text
  form.dueAt = localTime(e.dueAt)
  form.remindAt = e.done ? '' : localTime(e.remindAt)
  form.tags = [...(e.tags || [])]
  showTimes.value = e.kind === 'todo' && !!(e.dueAt || e.remindAt)
  msg.text = ''
  dialogOpen.value = true
  await nextTick()
  editorInput.value?.focus()
}
function closeDialog() {
  if (submitting.value) return
  dialogOpen.value = false
  tagPickerOpen.value = false
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

/* 标签与正文的同步：标签以正文里的 #token 为源（后端从文本派生），
   保存时移除已取消的旧标签 token、把选中的标签规整到正文末尾。 */
function tagStripRe(tag) { return new RegExp('#' + tag + '(?=$|[^\\p{L}\\p{N}_/-])', 'gu') }
function hasTagToken(text, tag) { return new RegExp('#' + tag + '(?=$|[^\\p{L}\\p{N}_/-])', 'u').test(text) }
function applyTags() {
  let text = form.text.replace(/\s+$/, '')
  for (const t of editingEntry.value?.tags || []) {
    if (!form.tags.includes(t)) text = text.replace(tagStripRe(t), '')
  }
  for (const t of form.tags) {
    if (!hasTagToken(text, t)) text = (text.trimEnd() + ' #' + t)
  }
  return text.trim()
}
function toggleTag(t) {
  const i = form.tags.indexOf(t)
  if (i >= 0) form.tags.splice(i, 1)
  else form.tags.push(t)
}
function addNewTag() {
  const value = newTag.value.trim().replace(/^#/, '')
  if (value && !form.tags.includes(value)) form.tags.push(value)
  newTag.value = ''
}

/* ---------- Markdown 格式工具栏 ----------
   只在 textarea 选区上做纯文本增删：正文仍是 Markdown 源文，
   后端 parseTags 与小五读到的文本不受影响。 */
const MD_TOOLS = [
  { id: 'bold', text: 'B', cls: 't-b', hint: '加粗' },
  { id: 'italic', text: 'I', cls: 't-i', hint: '斜体' },
  { id: 'strike', text: 'S', cls: 't-s', hint: '删除线' },
  { id: 'heading', text: 'H', cls: '', hint: '标题' },
  { id: 'bullet', text: '•', cls: '', hint: '列表' },
  { id: 'code', text: '</>', cls: 't-mono', hint: '代码' },
  { id: 'link', text: '链接', cls: '', hint: '链接' },
]
function runTool(id) {
  if (id === 'bold') applyToText((src, s, e) => wrapAt(src, s, e, '**'))
  if (id === 'italic') applyToText((src, s, e) => wrapAt(src, s, e, '*'))
  if (id === 'strike') applyToText((src, s, e) => wrapAt(src, s, e, '~~'))
  if (id === 'heading') applyToText(lineToggle('## '))
  if (id === 'bullet') applyToText(lineToggle('- '))
  if (id === 'code') applyToText((src, s, e) => (src.slice(s, e).includes('\n') ? fenceAt(src, s, e) : wrapAt(src, s, e, '`')))
  if (id === 'link') applyToText(linkAt)
}

function applyToText(fn) {
  const el = editorInput.value
  if (!el) return
  const { text, start, end } = fn(form.text, el.selectionStart, el.selectionEnd)
  form.text = text
  nextTick(() => { el.focus(); el.setSelectionRange(start, end) })
}
function splice(src, from, to, out, start, end) {
  return { text: src.slice(0, from) + out + src.slice(to), start, end }
}
/* 包裹/取消包裹：选区保持覆盖正文，所以取消要认"选区两侧已有标记"，工具栏可原地再点一次还原 */
function wrapAt(src, s, e, mark) {
  const n = mark.length
  const sel = src.slice(s, e)
  if (sel.length > n * 2 && sel.startsWith(mark) && sel.endsWith(mark)) {
    const body = sel.slice(n, -n)
    return splice(src, s, e, body, s, s + body.length)
  }
  if (s >= n && src.slice(s - n, s) === mark && src.slice(e, e + n) === mark) {
    return { text: src.slice(0, s - n) + sel + src.slice(e + n), start: s - n, end: e - n }
  }
  return splice(src, s, e, mark + sel + mark, s + n, s + n + sel.length)
}
function fenceAt(src, s, e) {
  const sel = src.slice(s, e)
  if (sel.startsWith('```') && sel.endsWith('```') && sel.length > 6) {
    const body = sel.slice(3, -3).replace(/^\n/, '').replace(/\n$/, '')
    return splice(src, s, e, body, s, s + body.length)
  }
  if (s >= 4 && src.slice(s - 4, s) === '```\n' && src.slice(e, e + 4) === '\n```') {
    return { text: src.slice(0, s - 4) + sel + src.slice(e + 4), start: s - 4, end: e - 4 }
  }
  const out = '```\n' + sel + '\n```'
  return splice(src, s, e, out, s + 4, s + 4 + sel.length)
}
function lineToggle(mark) {
  return (src, s, e) => {
    const from = src.lastIndexOf('\n', s - 1) + 1
    const to = src.indexOf('\n', e) < 0 ? src.length : src.indexOf('\n', e)
    const lines = src.slice(from, to).split('\n')
    const on = lines.every(l => !l.trim() || l.startsWith(mark))
    const body = lines.map(l => (!l.trim() ? l : (on ? l.slice(mark.length) : mark + l))).join('\n')
    return splice(src, from, to, body, from, from + body.length)
  }
}
function linkAt(src, s, e) {
  const label = src.slice(s, e) || '链接文字'
  const out = `[${label}](https://)`
  const url = s + label.length + 3
  return splice(src, s, e, out, url, url + 'https://'.length)
}

async function saveForm() {
  const raw = form.text.trim()
  if (!raw || submitting.value) return
  submitting.value = true
  tagPickerOpen.value = false
  try {
    const text = applyTags()
    const isEdit = !!editingEntry.value
    let body
    if (isEdit) {
      body = { text }
      if (form.kind === 'todo') {
        body.dueAt = timestamp(form.dueAt)
        body.remindAt = timestamp(form.remindAt)
      }
      await api('PUT', '/api/entries/' + encodeURIComponent(editingEntry.value.id), body)
    } else {
      body = { kind: form.kind, text }
      if (form.kind === 'todo') {
        body.dueAt = timestamp(form.dueAt)
        body.remindAt = timestamp(form.remindAt)
      }
      await api('POST', '/api/entries', body)
    }
    if (disposed) return
    dialogOpen.value = false
    toast(isEdit ? '已保存' : '已记下')
  } catch (e) {
    if (!disposed) showMsg(e.message, false)
  } finally {
    submitting.value = false
    if (!disposed) await loadEntries()
  }
}
function onComposerKey(e) {
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); saveForm() }
  if (e.key === 'Escape' && e.target === editorInput.value) { e.preventDefault(); closeDialog() }
}

/* ---------- 详情弹窗：点卡片正文查看完整内容（不做截断） ---------- */
const detailEntry = ref(null)
const detailBox = ref(null)
const viewRoot = ref(null)
function openCardDetail(ev, e) {
  if (ev.target.closest('a')) return // 正文里的链接保持默认跳转
  detailEntry.value = e
  nextTick(() => detailBox.value?.focus())
}
function closeDetail() { detailEntry.value = null }
function editFromDetail() {
  const e = detailEntry.value
  closeDetail()
  if (e) openEdit(e)
}
function tagFromDetail(t) {
  activeTag.value = activeTag.value === t ? '' : t
  closeDetail()
}

/* ---------- 列表加载、过滤与时间线分组 ---------- */
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
    if (!disposed) toast(e.message, true)
  } finally {
    pendingActions.delete(id)
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
  mutate(e.id, () => api('DELETE', '/api/entries/' + encodeURIComponent(e.id)), '已删除')
}
async function reconcile() {
  try { await api('POST', '/api/entries/reconcile', {}); if (!disposed) toast('提醒已恢复同步') }
  catch (e) { if (!disposed) toast(e.message, true) }
  finally { if (!disposed) await loadEntries() }
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

/* 列表正文超出高度上限截断：量出真被截断的卡片，只给它们加底部渐隐 */
async function markClipped() {
  await nextTick()
  if (disposed || !viewRoot.value) return
  for (const el of viewRoot.value.querySelectorAll('.entry-clamp')) {
    el.classList.toggle('clipped', el.scrollHeight > el.clientHeight + 1)
  }
}
watch(filtered, markClipped)
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
  const key = dayKey(value)
  if (key === dayKey(new Date(now.value))) return '今天'
  if (key === dayKey(new Date(now.value - 86400000))) return '昨天'
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
  if (tab.value === 'todo') return '没有未完成的待办，对小五说"记个待办：买牛奶"，或点右下角 + 新建'
  if (tab.value === 'note') return '还没有笔记，点右下角 + 随手记点想法，支持 #标签 和 Markdown'
  return '清单是空的：点右下角 + 记一条，或对小五说"记一下…"'
})

function isOverdue(e) {
  return e.dueAt && !e.done && new Date(e.dueAt).getTime() < now.value
}

onMounted(() => {
  loadEntries()
  clockTimer = setInterval(() => { now.value = Date.now() }, 60000)
  window.addEventListener('resize', markClipped)
})
onUnmounted(() => {
  disposed = true
  clearTimeout(msgTimer); clearInterval(clockTimer)
  window.removeEventListener('resize', markClipped)
})
</script>

<template>
  <section ref="viewRoot" class="view" id="view-blinko">
    <div class="view-inner">
      <h2>闪念 <span class="count">{{ countLine }}</span></h2>

      <div v-if="syncError" class="panel sync-banner">
        <div class="msg err" role="alert">
          条目已保存，提醒同步尚未完成：{{ syncError }}
          <button class="btn sm" @click="reconcile">重试同步</button>
        </div>
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
            <div class="entry-content" title="查看详情" @click="openCardDetail($event, e)">
              <div v-if="e.kind === 'note'" class="entry-md entry-clamp" v-html="renderMD(e.text)"></div>
              <div v-else class="entry-md" :class="{ done: e.done }" v-html="renderMD(e.text)"></div>
              <div class="entry-meta">
                <span v-if="isOverdue(e)" class="badge warn">已逾期</span>
                <span v-else-if="e.dueAt && !e.done" class="badge repeat">截止 {{ fmtDT(e.dueAt) }}</span>
                <span v-if="e.remindAt && !e.done" class="badge repeat">提醒 {{ fmtDT(e.remindAt) }}</span>
                <span v-if="e.doneAt" class="meta">完成于 {{ fmtDT(e.doneAt) }}</span>
                <button v-for="t in e.tags || []" :key="t" class="chip" type="button" @click.stop="activeTag = activeTag === t ? '' : t">#{{ t }}</button>
              </div>
            </div>
          </div>
          <div class="entry-actions">
            <button class="btn sm icon" :class="{ on: e.pinned }" :disabled="pendingActions.has(e.id)"
                    :aria-label="e.pinned ? '取消置顶' : '置顶'" :title="e.pinned ? '取消置顶' : '置顶'" @click="togglePin(e)">★</button>
            <button class="btn sm" :disabled="pendingActions.has(e.id)" @click="openEdit(e)">编辑</button>
            <button class="btn sm danger" :disabled="pendingActions.has(e.id)" @click="remove(e)">删除</button>
          </div>
        </article>
      </div>

      <EmptyState v-if="!filtered.length" kind="todo" :size="100">{{ emptyText }}</EmptyState>
    </div>

    <!-- 悬浮新建按钮 -->
    <button class="blinko-fab" aria-label="新建闪念" title="记一笔（笔记 / 待办）" @click="openCreate">
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
           stroke-linecap="round" aria-hidden="true"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
    </button>

    <!-- 编辑弹窗：创建与编辑共用；正文即编辑区，标签走独立 # 按钮 -->
    <div v-if="dialogOpen" class="blinko-mask" @click.self="closeDialog">
      <div class="blinko-dialog" role="dialog" aria-modal="true" :aria-label="editingEntry ? '编辑闪念' : '新建闪念'">
        <div v-if="!editingEntry" class="composer-tabs" role="tablist" aria-label="条目类型">
          <button type="button" role="tab" :aria-selected="form.kind === 'note'" :class="{ active: form.kind === 'note' }" @click="form.kind = 'note'">笔记</button>
          <button type="button" role="tab" :aria-selected="form.kind === 'todo'" :class="{ active: form.kind === 'todo' }" @click="form.kind = 'todo'">待办</button>
        </div>
        <div v-else class="dialog-kind meta">{{ form.kind === 'note' ? '笔记' : '待办' }} · 编辑</div>

        <div class="editor-box">
          <div class="md-toolbar" role="group" aria-label="Markdown 格式">
            <button v-for="tool in MD_TOOLS" :key="tool.id" type="button" :class="tool.cls"
                    :aria-label="tool.hint" :title="tool.hint" :disabled="submitting"
                    @click="runTool(tool.id)">{{ tool.text }}</button>
          </div>
          <textarea ref="editorInput" v-model="form.text" rows="6"
                    :aria-label="form.kind === 'note' ? '笔记内容' : '待办内容'"
                    :placeholder="form.kind === 'note' ? '记个想法… 支持 Markdown' : '要做什么？支持 Markdown'"
                    :disabled="submitting" @keydown="onComposerKey"></textarea>
        </div>

        <div v-if="form.kind === 'todo' && showTimes" class="composer-times">
          <label><span class="meta">截止（可选）</span><input v-model="form.dueAt" type="datetime-local" aria-label="截止时间" :disabled="submitting"></label>
          <label><span class="meta">提醒（可选）</span><input v-model="form.remindAt" type="datetime-local" aria-label="提醒时间" :disabled="submitting"></label>
        </div>

        <div v-if="tagPickerOpen" class="tag-picker">
          <input v-model="newTag" aria-label="新建标签" placeholder="新建标签，回车添加" :disabled="submitting" @keydown.enter.prevent="addNewTag">
          <div v-if="tags.length" class="tag-list">
            <button v-for="t in tags" :key="t" type="button" class="chip" :class="{ on: form.tags.includes(t) }"
                    @click="toggleTag(t)">#{{ t }}</button>
          </div>
          <p v-else class="meta">还没有标签，输入后回车创建第一个</p>
        </div>

        <div class="dialog-foot">
          <div class="foot-left">
            <button type="button" class="btn sm icon tag-btn" :class="{ on: tagPickerOpen || form.tags.length }"
                    aria-label="标签" :title="form.tags.length ? '标签（已选 ' + form.tags.length + ' 个）' : '标签'"
                    :disabled="submitting" @click="tagPickerOpen = !tagPickerOpen">#</button>
            <button v-for="t in form.tags" :key="t" type="button" class="chip on" :aria-label="'移除标签 ' + t"
                    :title="'移除 #' + t" @click="toggleTag(t)">#{{ t }}</button>
            <button v-if="form.kind === 'todo'" type="button" class="btn sm" :class="{ on: showTimes }"
                    @click="showTimes = !showTimes">{{ showTimes ? '收起时间' : '截止 / 提醒' }}</button>
          </div>
          <div class="foot-right">
            <span class="meta shortcut">⌘/Ctrl + Enter 保存</span>
            <button class="btn sm" type="button" :disabled="submitting" @click="closeDialog">取消</button>
            <button class="btn primary sm" type="button" :disabled="submitting || !form.text.trim()"
                    @click="saveForm">{{ submitting ? '保存中…' : editingEntry ? '保存' : '记下' }}</button>
          </div>
        </div>

        <div class="msg" :class="msg.cls" role="status">{{ msg.text }}</div>
      </div>
    </div>

    <!-- 详情弹窗：完整正文 + 全部元信息 -->
    <div v-if="detailEntry" class="blinko-mask" @click.self="closeDetail">
      <div ref="detailBox" class="blinko-dialog detail-dialog" role="dialog" aria-modal="true" aria-label="闪念详情"
           tabindex="-1" @keydown.esc.prevent="closeDetail">
        <div class="dialog-kind meta">
          {{ detailEntry.kind === 'note' ? '笔记' : '待办' }}
          <span v-if="detailEntry.pinned" class="detail-pin" title="已置顶">★</span>
        </div>
        <div class="entry-md detail-body" :class="{ done: detailEntry.done }" v-html="renderMD(detailEntry.text)"></div>
        <div class="entry-meta">
          <span v-if="isOverdue(detailEntry)" class="badge warn">已逾期</span>
          <span v-if="detailEntry.dueAt" class="badge repeat">截止 {{ fmtDT(detailEntry.dueAt) }}</span>
          <span v-if="detailEntry.remindAt" class="badge repeat">提醒 {{ fmtDT(detailEntry.remindAt) }}</span>
          <span v-if="detailEntry.doneAt" class="meta">完成于 {{ fmtDT(detailEntry.doneAt) }}</span>
          <button v-for="t in detailEntry.tags || []" :key="t" class="chip" type="button"
                  :title="'筛选 #' + t" @click="tagFromDetail(t)">#{{ t }}</button>
        </div>
        <div class="meta detail-time">创建于 {{ fmtDT(detailEntry.createdAt) }}</div>
        <div class="dialog-foot">
          <div class="foot-left"></div>
          <div class="foot-right">
            <button class="btn sm" type="button" @click="editFromDetail">编辑</button>
            <button class="btn primary sm" type="button" @click="closeDetail">关闭</button>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.sync-banner { margin-bottom: 12px; }
.sync-banner .msg { margin: 0; }

/* 悬浮新建按钮 */
.blinko-fab { position: fixed; right: 34px; bottom: 34px; width: 52px; height: 52px; border-radius: 50%;
  border: 0; background: var(--primary); color: var(--on-primary); cursor: pointer;
  display: flex; align-items: center; justify-content: center;
  box-shadow: 0 10px 26px rgba(0, 0, 0, .28); transition: transform .16s ease, box-shadow .16s ease; z-index: 40; }
.blinko-fab:hover { transform: translateY(-2px) scale(1.05); box-shadow: 0 14px 30px rgba(0, 0, 0, .34); }
.blinko-fab:active { transform: scale(.96); }

/* 编辑弹窗 */
.blinko-mask { position: fixed; inset: 0; background: rgba(24, 24, 27, .45); z-index: 60;
  display: flex; align-items: flex-start; justify-content: center; padding: 12vh 16px 16px; }
html[data-theme="dark"] .blinko-mask { background: rgba(0, 0, 0, .6); }
.blinko-dialog { width: min(620px, 100%); background: var(--card); border: 1px solid var(--border);
  border-radius: 14px; padding: 14px 16px 12px; display: flex; flex-direction: column; gap: 10px;
  box-shadow: var(--shadow-overlay); }
/* --card 在深色下是半透明白，浮层必须换成不透明底才不会透出下面的时间线 */
html[data-theme="dark"] .blinko-dialog {
  background: rgba(24,24,24,.94);
  backdrop-filter: blur(28px) saturate(1.4); -webkit-backdrop-filter: blur(28px) saturate(1.4);
  border-color: rgba(255,255,255,.1); }
.dialog-kind { font-weight: 600; color: var(--fg); }
.blinko-dialog textarea { resize: vertical; min-height: 120px; font-size: 14px; line-height: 1.7; }
/* 编辑区：外层提供边框与聚焦效果，内部 textarea 背景透明 */
.editor-box { display: flex; flex-direction: column; border: 1px solid var(--border);
  border-radius: var(--radius-sm); background: var(--card);
  transition: border-color var(--tr), box-shadow var(--tr); }
.editor-box:focus-within { border-color: var(--border-strong); box-shadow: 0 0 0 3px rgba(24,24,27,.06); }
.editor-box textarea { border: 0; background: transparent; border-radius: 0; width: 100%; }
.editor-box textarea:focus { box-shadow: none; }
html[data-theme="dark"] .editor-box { background: rgba(255,255,255,.045); border-color: rgba(255,255,255,.10); }
html[data-theme="dark"] .editor-box:focus-within { border-color: rgba(110,168,242,.45);
  box-shadow: 0 0 0 3px rgba(110,168,242,.14); }
.md-toolbar { display: flex; gap: 2px; flex-wrap: wrap; padding: 4px 5px; border-bottom: 1px solid var(--border); }
.md-toolbar button { min-width: 27px; height: 24px; padding: 0 7px; border: 0; border-radius: 6px;
  background: transparent; color: var(--muted-fg); font: inherit; font-size: 12.5px; cursor: pointer;
  transition: background var(--tr), color var(--tr); }
.md-toolbar button:hover:not(:disabled) { background: var(--bg-subtle); color: var(--fg); }
.md-toolbar button:disabled { opacity: .55; cursor: not-allowed; }
.md-toolbar .t-b { font-weight: 700; }
.md-toolbar .t-i { font-style: italic; }
.md-toolbar .t-s { text-decoration: line-through; }
.md-toolbar .t-mono { font-family: var(--mono, ui-monospace, monospace); font-size: 11px; }
.composer-tabs { display: inline-flex; gap: 4px; width: max-content; padding: 3px; border-radius: 999px; background: var(--bg); }
.composer-tabs button { border: 0; background: transparent; color: var(--muted-fg); font: inherit; font-size: 12.5px;
  padding: 4px 14px; border-radius: 999px; cursor: pointer; }
.composer-tabs button.active { background: var(--card); color: var(--fg); font-weight: 600; box-shadow: 0 1px 4px rgba(0,0,0,.12); }
.composer-times { display: flex; gap: 14px; flex-wrap: wrap; }
.composer-times label { display: flex; align-items: center; gap: 8px; font-size: 12.5px; }
.composer-times input { width: auto; }

.dialog-foot { display: flex; align-items: center; justify-content: space-between; gap: 10px; flex-wrap: wrap; }
.foot-left, .foot-right { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.shortcut { margin-right: 6px; }
.tag-btn { font-weight: 700; }
.tag-btn.on { color: var(--accent); border-color: var(--accent); }
.foot-left .chip { cursor: pointer; }

.tag-picker { border: 1px solid var(--border); border-radius: 10px; padding: 10px; display: flex; flex-direction: column; gap: 8px; background: var(--bg); }
.tag-picker input { width: 100%; }
.tag-list { display: flex; gap: 6px; flex-wrap: wrap; }
.tag-list .chip { cursor: pointer; border: 1px solid var(--border); background: var(--card); }
.tag-list .chip.on { border-color: var(--primary); color: var(--primary); }

/* 工具栏与时间线 */
.blinko-toolbar { display: flex; align-items: center; gap: 10px; margin: 16px 0 8px; }
.blinko-tabs { display: inline-flex; gap: 2px; padding: 3px; border-radius: 999px; background: var(--card); border: 1px solid var(--border); }
.blinko-tabs button { border: 0; background: transparent; color: var(--muted-fg); font: inherit; font-size: 12.5px;
  padding: 4px 13px; border-radius: 999px; cursor: pointer; }
.blinko-tabs button.active { background: var(--primary); color: var(--on-primary); font-weight: 600; }
.blinko-search { max-width: 220px; margin-left: auto; }
.blinko-tagbar { display: flex; gap: 6px; flex-wrap: wrap; margin-bottom: 8px; }
.blinko-tagbar .chip { cursor: pointer; border: 1px solid var(--border); background: var(--card); }
.blinko-tagbar .chip.on { border-color: var(--primary); color: var(--primary); }

.blinko-group { margin-top: 14px; }
.group-head { padding: 0 2px 6px; }
.entry-card { display: flex; align-items: flex-start; gap: 10px; background: var(--card); border: 1px solid var(--border);
  border-radius: 10px; padding: 10px 12px; margin-bottom: 8px; }
.entry-card.pinned { border-color: var(--accent); }
.entry-card.is-done { opacity: .68; }
.entry-main { flex: 1; min-width: 0; display: flex; gap: 9px; align-items: flex-start; }
.entry-main > input[type="checkbox"] { margin-top: 4px; }
.entry-content { flex: 1; min-width: 0; cursor: pointer; }
.entry-md { font-size: 13.5px; line-height: 1.65; min-width: 0; word-break: break-word; }
.entry-md.done { text-decoration: line-through; color: var(--muted-fg); }
.entry-md :deep(p) { margin: 0 0 6px; } .entry-md :deep(p:last-child) { margin-bottom: 0; }
.entry-md :deep(pre) { margin: 6px 0; }
.entry-md :deep(ul), .entry-md :deep(ol) { margin: 6px 0; padding-left: 20px; }
/* 列表正文高度上限：超出截断，真被截断的卡片底部渐隐提示可点开详情 */
.entry-clamp { max-height: 132px; overflow: hidden; }
.entry-clamp.clipped { position: relative; }
.entry-clamp.clipped::after { content: ''; position: absolute; left: 0; right: 0; bottom: 0; height: 30px;
  background: linear-gradient(180deg, transparent, var(--card)); pointer-events: none; }

/* 详情弹窗：正文完整展示不截断 */
.detail-dialog { width: min(560px, 100%); max-height: 74vh; overflow-y: auto; }
.detail-dialog:focus { outline: none; }
.detail-body { font-size: 14px; line-height: 1.7; }
.detail-pin { margin-left: 6px; color: var(--accent); }
.detail-time { margin-top: -4px; }
.entry-meta { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; margin-top: 6px; }
.entry-meta .chip { cursor: pointer; font-size: 11px; }
.entry-actions { display: flex; gap: 4px; flex-shrink: 0; }
.entry-actions .icon { color: var(--faint-fg); }
.entry-actions .icon.on { color: var(--accent); }
</style>
