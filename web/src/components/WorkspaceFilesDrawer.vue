<script setup>
// WorkspaceFilesDrawer 是工作区的文件/改动/项目记忆/验证抽屉：
//   - 文件：按目录懒加载的文件树 + 只读预览（复用 /api/files/preview）；
//   - 本轮改动：按轮列出 agent 改过的文件、逐文件 diff 与“还原本轮”；
//   - 项目记忆：同一工作区共享的 wiki 页，下一轮/重连时随工作区说明注入给 agent；
//   - 验证：时间线里的本地预览地址（内嵌或新窗口）+ 一键跑检查命令，看退出码与输出尾部。
// 只读优先：没有快照时明说“不能还原”，不把“看得到”伪装成“退得回”；验证命令同样
// 要先过审批卡，Friday 不承诺沙箱。
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { api } from '../api.js'
import { renderMD } from '../md.js'

const props = defineProps({
  thread: { type: Object, default: null },
  messages: { type: Array, default: () => [] },
  workspaceId: { type: String, default: '' },
})
const emit = defineEmits(['reverted'])

const openState = ref(false)
const tab = ref('files')
const closeBtn = ref(null)
let lastFocus = null

const root = ref('')
const nodes = reactive({})
let treeGen = 0
const selected = ref(null)
const preview = reactive({ loading: false, error: '', kind: '', name: '', path: '', size: 0,
  modTime: '', content: '', dataUrl: '', truncated: false, message: '' })

const changeTurns = computed(() => {
  const out = []
  for (const m of props.messages || []) {
    if (m.role === 'bot' && m.changes && m.changes.turnId) out.push(m.changes)
  }
  return out.reverse()
})
const latestChangeCount = computed(() => changeTurns.value[0]?.files?.length || 0)
const activeTurnId = ref('')
const changeLoading = ref(false)
const changeError = ref('')
const changeDetail = ref(null)
const currentFile = ref('')
const revertArm = ref(false)
const reverting = ref(false)
let revertTimer = null

const mem = reactive({ pages: [], loading: false, error: '', saved: '', editing: null,
  draft: { id: '', title: '', body: '' }, saving: false, confirmDelete: '' })
let memGen = 0
let deleteTimer = null

// 验证面板：预览地址来自时间线 + 手填（只允许本地），检查命令走后端审批闸门。
const chk = reactive({
  runs: [], suggestions: [], loading: false, error: '', notice: '', busy: false,
  selectedId: '', detail: null, command: '', timeoutSec: 300,
  pane: 'output', frameUrl: '', custom: [], addUrl: '', addError: '',
})
let checkTimer = null
let checkGen = 0
const CHECK_ACTIVE = ['awaiting', 'running']
const LOCAL_URL_RE = /https?:\/\/(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])(?::\d{2,5})?(?:\/[^\s"'`）)】<>]*)?/gi
const LOCAL_PREFIX_RE = /^https?:\/\//i
const LOCAL_HOST_RE = /^https?:\/\/(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])(?::\d{2,5})?(?:[/?#].*)?$/i

const checkActiveCount = computed(() => chk.runs.filter(r => CHECK_ACTIVE.includes(r.state)).length)

const previews = computed(() => {
  const out = []
  const seen = new Set()
  const push = (url, source) => {
    const clean = String(url || '').replace(/[.,;:!?]+$/, '')
    if (!clean || seen.has(clean)) return
    seen.add(clean)
    out.push({ url: clean, source })
  }
  for (const m of props.messages || []) {
    if (m.role !== 'bot' || !m.text) continue
    for (const hit of String(m.text).match(LOCAL_URL_RE) || []) push(hit, '时间线')
  }
  for (const u of chk.custom) push(u, '手动添加')
  return out
})

const checkLines = computed(() => (chk.detail?.output || '').split('\n').map(text => ({
  text,
  bad: text.trim() !== '' && /(FAIL|FAILED|failed|✗|not ok|panic:|--- FAIL|Error:|错误)/.test(text),
})))

// checkNote：退出码已经在头部标过，就不再来一条同义的提示框；
// 超时/停止/未批准/没跑起来这些原因才值得单独说一句。
const checkNote = computed(() => {
  const note = chk.detail?.note || ''
  if (!note) return ''
  if ((chk.detail.state === 'failed' || chk.detail.state === 'passed') && note.startsWith('退出码')) return ''
  return note
})

async function loadChecks() {
  const th = props.thread
  if (!th) return
  const gen = ++checkGen
  chk.loading = true
  chk.error = ''
  try {
    const r = await api('GET', `/api/acp/threads/${th.id}/checks`)
    if (gen !== checkGen) return
    chk.runs = r.runs || []
    chk.suggestions = r.suggestions || []
    // 选中项：还指向原来的那条就跟着它刷新详情（跑完的那一刻输出才拿得到），
    // 记录被裁掉或还没选过就落到最新一条。
    const isLive = s => CHECK_ACTIVE.includes(s)
    const sel = chk.runs.find(x => x.id === chk.selectedId) || chk.runs[0] || null
    if (!sel) {
      chk.selectedId = ''
      chk.detail = null
    } else {
      chk.selectedId = sel.id
      if (!chk.detail || chk.detail.id !== sel.id || (isLive(chk.detail.state) && !isLive(sel.state))) {
        loadCheckDetail()
      }
    }
    // 跑完就把「等你批准」那句撤掉，不留过期提示
    if (!checkActiveCount.value) chk.notice = ''
    ensureCheckPolling()
  } catch (e) {
    if (gen === checkGen) chk.error = e.message || String(e)
  } finally {
    if (gen === checkGen) chk.loading = false
  }
}

// ensureCheckPolling：有待批/在跑时才轮询，结束就停，不空转网络。
function ensureCheckPolling() {
  if (checkActiveCount.value > 0 && tab.value === 'verify' && openState.value) {
    if (!checkTimer) checkTimer = setInterval(loadChecks, 1500)
    return
  }
  stopCheckPolling()
}

function stopCheckPolling() {
  if (checkTimer) {
    clearInterval(checkTimer)
    checkTimer = null
  }
}

async function selectCheckRun(run) {
  chk.selectedId = run.id
  chk.pane = 'output'
  chk.frameUrl = ''
  await loadCheckDetail()
}

async function loadCheckDetail() {
  const th = props.thread
  if (!th || !chk.selectedId) {
    chk.detail = null
    return
  }
  try {
    const r = await api('GET', `/api/acp/threads/${th.id}/checks/${encodeURIComponent(chk.selectedId)}`)
    chk.detail = r.run || null
  } catch (e) {
    chk.detail = null
    chk.error = e.message || String(e)
  }
}

async function runCheck(command) {
  const th = props.thread
  const cmd = String(command ?? chk.command).trim()
  if (!th || !cmd || chk.busy) return
  chk.busy = true
  chk.error = ''
  chk.notice = ''
  try {
    const r = await api('POST', `/api/acp/threads/${th.id}/checks`,
      { command: cmd, timeoutSec: Number(chk.timeoutSec) || 300 })
    chk.notice = r.message || '已提交，等你在审批卡上批准后执行'
    chk.selectedId = r.run?.id || chk.selectedId
    chk.pane = 'output'
    chk.frameUrl = ''
    chk.command = ''
    await loadChecks()
    await loadCheckDetail()
  } catch (e) {
    chk.error = e.message || String(e)
  } finally {
    chk.busy = false
  }
}

async function stopCheckRun(run) {
  const th = props.thread
  if (!th || !run) return
  chk.error = ''
  try {
    await api('POST', `/api/acp/threads/${th.id}/checks/${encodeURIComponent(run.id)}/stop`)
    await loadChecks()
    await loadCheckDetail()
  } catch (e) {
    chk.error = e.message || String(e)
  }
}

function useSuggestion(s) {
  chk.command = s.command
  runCheck(s.command)
}

function openPreview(p) {
  chk.pane = 'preview'
  chk.frameUrl = p.url
}

function openPreviewExternal(p) {
  window.open(p.url, '_blank', 'noopener')
}

function addPreviewUrl() {
  const raw = chk.addUrl.trim()
  chk.addError = ''
  if (!raw) return
  // 不用 new URL 解析：这里只要判「是不是本机地址」，正则更直白，也不依赖宿主实现。
  if (!LOCAL_PREFIX_RE.test(raw)) {
    chk.addError = '地址要带 http:// 前缀，例如 http://localhost:5173'
    return
  }
  if (!LOCAL_HOST_RE.test(raw)) {
    chk.addError = '只能预览本机地址（localhost / 127.0.0.1）'
    return
  }
  chk.custom.push(raw)
  chk.addUrl = ''
  openPreview({ url: raw })
}

function removePreview(url) {
  chk.custom = chk.custom.filter(u => u !== url)
  if (chk.frameUrl === url) {
    chk.pane = 'output'
    chk.frameUrl = ''
  }
}

function checkStateLabel(s) {
  return ({ awaiting: '待批准', running: '跑着', passed: '通过', failed: '失败',
    timeout: '超时', killed: '已停止', denied: '未批准', error: '没跑起来' })[s] || s
}

function fmtDuration(ms) {
  if (!Number.isFinite(ms) || ms <= 0) return ''
  if (ms < 1000) return ms + ' 毫秒'
  if (ms < 60000) return (ms / 1000).toFixed(1) + ' 秒'
  return Math.floor(ms / 60000) + ' 分 ' + Math.round((ms % 60000) / 1000) + ' 秒'
}

const selectedChange = computed(() =>
  (changeDetail.value?.files || []).find(f => f.path === currentFile.value) || null)

const diffRows = computed(() => parseDiff(selectedChange.value?.diff || ''))

const rows = computed(() => {
  const out = []
  const walk = (rel, depth) => {
    const st = nodes[rel]
    if (!st) return
    for (const e of st.entries || []) {
      out.push({ entry: e, depth, expanded: !!nodes[e.path]?.expanded })
      if (e.dir && nodes[e.path]?.expanded) walk(e.path, depth + 1)
    }
  }
  walk('', 0)
  return out
})

function open(which = 'files', turnId = '') {
  if (!props.thread) return
  lastFocus = document.activeElement
  openState.value = true
  tab.value = which === 'changes' ? 'changes' : which === 'memory' ? 'memory'
    : which === 'verify' ? 'verify' : 'files'
  if (props.thread) {
    if (root.value !== props.thread.cwd) resetTree()
    else if (tab.value === 'files' && !nodes['']) loadDir('')
  }
  if (tab.value === 'changes') loadChanges(turnId)
  if (tab.value === 'memory') loadMemory()
  if (tab.value === 'verify') loadChecks()
  nextTick(() => closeBtn.value?.focus())
}

function openMemory(draft = {}) {
  mem.draft = { id: '', title: draft.title || '项目约定', body: draft.body || '' }
  mem.editing = 'new'
  open('memory')
}

function close() {
  openState.value = false
  revertArm.value = false
  stopCheckPolling()
  if (lastFocus && typeof lastFocus.focus === 'function') lastFocus.focus()
}

function switchTab(which) {
  tab.value = which
  if (which !== 'verify') stopCheckPolling()
  if (which === 'files') {
    if (!nodes['']) loadDir('')
  } else if (which === 'memory') {
    loadMemory()
  } else if (which === 'verify') {
    loadChecks()
  } else if (!changeDetail.value || changeDetail.value.turnId !== activeTurnId.value) {
    loadChanges(activeTurnId.value || '')
  }
}

async function loadMemory() {
  if (!props.workspaceId) {
    mem.pages = []
    mem.error = '这条会话还没有工作区，项目记忆暂不可用。'
    return
  }
  const gen = ++memGen
  mem.loading = true
  mem.error = ''
  try {
    const r = await api('GET', `/api/acp/workspaces/${props.workspaceId}/memory`)
    if (gen !== memGen) return
    mem.pages = r.pages || []
  } catch (e) {
    if (gen === memGen) mem.error = e.message || String(e)
  } finally {
    if (gen === memGen) mem.loading = false
  }
}

async function editMemory(page) {
  mem.error = ''
  mem.saved = ''
  try {
    const r = await api('GET', `/api/acp/workspaces/${props.workspaceId}/memory/${encodeURIComponent(page.id)}`)
    mem.editing = page.id
    mem.draft = { id: page.id, title: r.page.title, body: r.page.body }
  } catch (e) {
    mem.error = e.message || String(e)
  }
}

function newMemory() {
  mem.editing = 'new'
  mem.saved = ''
  mem.error = ''
  mem.draft = { id: '', title: '', body: '' }
}

function cancelMemory() {
  mem.editing = null
  mem.draft = { id: '', title: '', body: '' }
  mem.saved = ''
}

async function saveMemory() {
  if (!props.workspaceId || mem.saving) return
  mem.saving = true
  mem.error = ''
  mem.saved = ''
  try {
    const r = await api('POST', `/api/acp/workspaces/${props.workspaceId}/memory`, {
      id: mem.draft.id || undefined, title: mem.draft.title, body: mem.draft.body,
    })
    mem.saved = r.message || '项目记忆已保存'
    mem.editing = r.page?.id || mem.editing
    mem.draft.id = r.page?.id || mem.draft.id
    await loadMemory()
  } catch (e) {
    mem.error = e.message || String(e)
  } finally {
    mem.saving = false
  }
}

function armDeleteMemory(page) {
  if (mem.confirmDelete !== page.id) {
    mem.confirmDelete = page.id
    clearTimeout(deleteTimer)
    deleteTimer = setTimeout(() => { mem.confirmDelete = '' }, 5000)
    return
  }
  mem.confirmDelete = ''
  deleteMemory(page.id)
}

async function deleteMemory(id) {
  if (!props.workspaceId) return
  mem.error = ''
  try {
    await api('DELETE', `/api/acp/workspaces/${props.workspaceId}/memory/${encodeURIComponent(id)}`)
    if (mem.editing === id) cancelMemory()
    await loadMemory()
  } catch (e) {
    mem.error = e.message || String(e)
  }
}

function resetTree() {
  memGen++
  treeGen++
  checkGen++
  stopCheckPolling()
  Object.assign(chk, { runs: [], suggestions: [], loading: false, error: '', notice: '',
    busy: false, selectedId: '', detail: null, command: '', frameUrl: '', custom: [],
    pane: 'output', addUrl: '', addError: '' })
  for (const k of Object.keys(nodes)) delete nodes[k]
  root.value = props.thread?.cwd || ''
  selected.value = null
  Object.assign(preview, { loading: false, error: '', kind: '', name: '', path: '', size: 0,
    modTime: '', content: '', dataUrl: '', truncated: false, message: '' })
  if (root.value) loadDir('')
}

async function loadDir(rel) {
  const th = props.thread
  if (!th) return
  const gen = treeGen
  if (!nodes[rel]) nodes[rel] = { entries: [], loading: false, error: '', expanded: true, truncated: false }
  const st = nodes[rel]
  st.loading = true
  st.error = ''
  try {
    const r = await api('GET', `/api/acp/threads/${th.id}/files?path=${encodeURIComponent(rel)}`)
    if (gen !== treeGen) return
    st.entries = r.entries || []
    st.truncated = !!r.truncated
  } catch (e) {
    if (gen === treeGen) st.error = e.message || String(e)
  } finally {
    if (gen === treeGen) st.loading = false
  }
}

function onRow(row) {
  const e = row.entry
  if (e.dir) {
    const st = nodes[e.path]
    if (!st) {
      nodes[e.path] = { entries: [], loading: false, error: '', expanded: true, truncated: false }
      loadDir(e.path)
    } else {
      st.expanded = !st.expanded
      if (st.expanded && !st.entries.length && !st.loading) loadDir(e.path)
    }
    selected.value = null
    return
  }
  selected.value = e
  loadPreview(e)
}

async function loadPreview(e) {
  const abs = joinPath(root.value, e.path)
  Object.assign(preview, { loading: true, error: '', kind: '', name: e.name, path: abs,
    size: e.size || 0, modTime: e.modTime || '', content: '', dataUrl: '', truncated: false, message: '' })
  try {
    const r = await api('GET', '/api/files/preview?path=' + encodeURIComponent(abs))
    Object.assign(preview, r)
  } catch (err) {
    preview.error = err.message || String(err)
  } finally {
    preview.loading = false
  }
}

async function loadChanges(turnId) {
  const th = props.thread
  if (!th) return
  changeError.value = ''
  changeLoading.value = true
  revertArm.value = false
  try {
    const id = turnId || activeTurnId.value || changeTurns.value[0]?.turnId || ''
    if (!id) {
      changeDetail.value = null
      activeTurnId.value = ''
      currentFile.value = ''
      return
    }
    activeTurnId.value = id
    const r = await api('GET', `/api/acp/threads/${th.id}/changes?turn=${encodeURIComponent(id)}`)
    changeDetail.value = r.changes || null
    const files = changeDetail.value?.files || []
    const first = files.find(f => !f.binary && f.diff) || files[0]
    currentFile.value = first?.path || ''
  } catch (e) {
    changeDetail.value = null
    changeError.value = e.message || String(e)
  } finally {
    changeLoading.value = false
  }
}

function selectTurn() {
  loadChanges(activeTurnId.value)
}

function selectChangeFile(f) {
  currentFile.value = f.path
}

function armRevert() {
  if (reverting.value || changeDetail.value?.revertedAt || changeDetail.value?.snapshot === false) return
  if (!revertArm.value) {
    revertArm.value = true
    clearTimeout(revertTimer)
    revertTimer = setTimeout(() => { revertArm.value = false }, 5000)
    return
  }
  doRevert()
}

async function doRevert() {
  const th = props.thread
  const turn = changeDetail.value?.turnId
  if (!th || !turn) return
  reverting.value = true
  changeError.value = ''
  try {
    const r = await api('POST', `/api/acp/threads/${th.id}/changes/revert`, { turn })
    if (changeDetail.value) {
      changeDetail.value.revertedAt = r.changes?.revertedAt || new Date().toISOString()
    }
    revertArm.value = false
    emit('reverted', turn)
  } catch (e) {
    changeError.value = e.message || String(e)
    revertArm.value = false
  } finally {
    reverting.value = false
  }
}

function statusLetter(s) {
  return s === 'added' ? 'A' : s === 'deleted' ? 'D' : 'M'
}
function statusLabel(s) {
  return s === 'added' ? '新增' : s === 'deleted' ? '删除' : '修改'
}
function fmtSize(n) {
  if (!Number.isFinite(n) || n <= 0) return ''
  if (n < 1024) return n + ' B'
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KB'
  return (n / 1048576).toFixed(1) + ' MB'
}
function joinPath(base, rel) {
  if (!base) return rel
  return base.replace(/\/+$/, '') + '/' + rel
}
function shortPath(p) {
  if (!p) return ''
  const parts = p.split('/').filter(Boolean)
  return parts.length > 3 ? '…/' + parts.slice(-2).join('/') : p
}
function turnLabel(c, idx) {
  const when = c.at ? new Date(c.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : ''
  const files = c.files?.length || 0
  const which = idx === 0 ? '最近一轮' : '第 ' + (idx + 1) + ' 轮'
  return `${which} · ${files} 个文件${when ? ' · ' + when : ''}`
}

function parseDiff(diff) {
  if (!diff) return []
  const out = []
  let oldNo = 0
  let newNo = 0
  for (const line of diff.split('\n')) {
    const hunk = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/)
    if (hunk) {
      oldNo = Number(hunk[1])
      newNo = Number(hunk[2])
      out.push({ kind: 'hunk', text: line })
      continue
    }
    if (line.startsWith('+++') || line.startsWith('---')) continue
    if (line.startsWith('\\')) { out.push({ kind: 'meta', text: line }); continue }
    if (line.startsWith('+')) {
      out.push({ kind: 'add', old: '', new: newNo++, text: line.slice(1) })
    } else if (line.startsWith('-')) {
      out.push({ kind: 'del', old: oldNo++, new: '', text: line.slice(1) })
    } else {
      out.push({ kind: 'ctx', old: oldNo++, new: newNo++, text: line.startsWith(' ') ? line.slice(1) : line })
    }
  }
  return out
}

function onKeydown(e) {
  if (!openState.value) return
  if (e.key === 'Escape') {
    e.preventDefault()
    close()
  }
}

onMounted(() => document.addEventListener('keydown', onKeydown))
onUnmounted(() => {
  document.removeEventListener('keydown', onKeydown)
  clearTimeout(revertTimer)
  clearTimeout(deleteTimer)
  stopCheckPolling()
})

watch(() => [props.thread?.id, props.workspaceId], () => {
  if (!openState.value) return
  if (!props.thread) { close(); return }
  resetTree()
  changeDetail.value = null
  activeTurnId.value = ''
  currentFile.value = ''
  mem.editing = null
  mem.saved = ''
  if (tab.value === 'changes') loadChanges('')
  if (tab.value === 'memory') loadMemory()
  if (tab.value === 'verify') loadChecks()
})

defineExpose({ open, close, openMemory })
</script>

<template>
  <div v-if="openState">
    <div class="wfd-mask" @click="close"></div>
    <aside class="wfd-panel" role="dialog" aria-modal="true" aria-label="工作区面板">
      <header class="wfd-head">
        <div class="wfd-title">
          <span class="wfd-name">工作区</span>
          <span class="wfd-meta" :title="thread?.cwd || ''">
            {{ thread?.agent || '' }} · {{ shortPath(thread?.cwd) }}
          </span>
        </div>
        <div class="wfd-tabs" role="tablist" aria-label="工作区视图">
          <button type="button" role="tab" :aria-selected="tab === 'files'"
                  :class="{ on: tab === 'files' }" @click="switchTab('files')">文件</button>
          <button type="button" role="tab" :aria-selected="tab === 'changes'"
                  :class="{ on: tab === 'changes' }" @click="switchTab('changes')">
            本轮改动
            <span v-if="latestChangeCount" class="wfd-tab-n">{{ latestChangeCount }}</span>
          </button>
          <button type="button" role="tab" :aria-selected="tab === 'memory'"
                  :class="{ on: tab === 'memory' }" @click="switchTab('memory')">
            项目记忆
            <span v-if="mem.pages.length" class="wfd-tab-n">{{ mem.pages.length }}</span>
          </button>
          <button type="button" role="tab" :aria-selected="tab === 'verify'"
                  :class="{ on: tab === 'verify' }" @click="switchTab('verify')">
            验证
            <span v-if="checkActiveCount" class="wfd-tab-n">{{ checkActiveCount }}</span>
          </button>
        </div>
        <button ref="closeBtn" class="btn icon wfd-close" type="button" aria-label="关闭工作区文件"
                title="关闭（Esc）" @click="close">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
               stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
        </button>
      </header>

      <div v-if="tab === 'files'" class="wfd-body wfd-files">
        <div class="wfd-tree" aria-label="工作区文件树">
          <div class="wfd-root" :title="root">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"
                 stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>
            <span>{{ root.split('/').filter(Boolean).pop() || '工作区' }}</span>
          </div>
          <div v-if="nodes['']?.loading && !nodes['']?.entries.length" class="wfd-status">读取中…</div>
          <div v-else-if="nodes['']?.error" class="wfd-status is-error">{{ nodes[''].error }}</div>
          <template v-else>
            <button v-for="row in rows" :key="row.entry.path" type="button"
                    class="wfd-row" :class="{ dir: row.entry.dir, active: selected?.path === row.entry.path }"
                    :style="{ paddingLeft: (12 + row.depth * 14) + 'px' }"
                    :aria-expanded="row.entry.dir ? row.expanded : undefined"
                    @click="onRow(row)">
              <svg v-if="row.entry.dir" class="wfd-chev" :class="{ open: row.expanded }" width="12" height="12"
                   viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
                   stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg>
              <svg v-else class="wfd-file-ico" width="13" height="13" viewBox="0 0 24 24" fill="none"
                   stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/>
              </svg>
              <span class="wfd-row-name">{{ row.entry.name }}</span>
              <span v-if="!row.entry.dir" class="wfd-size">{{ fmtSize(row.entry.size) }}</span>
            </button>
            <div v-if="!rows.length" class="wfd-status">这个目录是空的。</div>
            <div v-if="nodes['']?.truncated" class="wfd-status">目录项过多，只显示前 1000 个。</div>
          </template>
          <div v-if="nodes['']?.entries.length" class="wfd-tree-note">点文件查看内容；点目录展开。</div>
        </div>

        <div class="wfd-detail">
          <div v-if="!selected" class="wfd-empty">
            <div class="wfd-empty-art" aria-hidden="true">
              <svg width="34" height="34" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.4"
                   stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M8 13h8M8 17h5"/></svg>
            </div>
            <b>选择左侧文件查看内容</b>
            <p class="meta">agent 改了什么，去「本轮改动」看 diff 并还原。</p>
          </div>
          <template v-else>
            <div class="wfd-detail-head">
              <div class="wfd-detail-title">
                <b :title="preview.path">{{ preview.name || selected.name }}</b>
                <span class="meta">{{ fmtSize(preview.size) }}<template v-if="preview.modTime"> · {{ new Date(preview.modTime).toLocaleString() }}</template></span>
              </div>
            </div>
            <div v-if="preview.loading" class="wfd-status">读取中…</div>
            <div v-else-if="preview.error" class="wfd-status is-error">{{ preview.error }}</div>
            <div v-else-if="preview.kind === 'image'" class="wfd-preview-body">
              <img class="wfd-img" :src="preview.dataUrl" :alt="preview.name">
            </div>
            <div v-else-if="preview.kind === 'text'" class="wfd-preview-body">
              <div v-if="preview.truncated" class="wfd-notice">文件较大，仅显示前 512 KB</div>
              <div v-if="/\.(md|markdown)$/i.test(preview.name)" class="wfd-md chat-md" v-html="renderMD(preview.content)"></div>
              <pre v-else class="wfd-code">{{ preview.content }}</pre>
            </div>
            <div v-else class="wfd-status">{{ preview.message || '暂不支持预览' }}</div>
          </template>
        </div>
      </div>

      <div v-else-if="tab === 'changes'" class="wfd-body wfd-changes">
        <div class="wfd-change-list">
          <div class="wfd-change-top">
            <select v-if="changeTurns.length > 1" v-model="activeTurnId" class="wfd-turn-select"
                    aria-label="选择轮次" @change="selectTurn">
              <option v-for="(c, i) in changeTurns" :key="c.turnId" :value="c.turnId">{{ turnLabel(c, i) }}</option>
            </select>
            <span v-else class="wfd-turn-label">{{ changeTurns.length ? turnLabel(changeTurns[0], 0) : '本轮改动' }}</span>
            <span v-if="changeDetail?.revertedAt" class="wfd-reverted">已还原</span>
          </div>
          <div v-if="changeLoading" class="wfd-status">读取改动…</div>
          <div v-else-if="changeError" class="wfd-status is-error">{{ changeError }}</div>
          <div v-else-if="!changeDetail" class="wfd-empty wfd-change-empty">
            <b>还没有可查看的改动</b>
            <p class="meta">agent 跑完一轮后，这里会列出它改过的文件。</p>
          </div>
          <template v-else>
            <div v-if="changeDetail.note" class="wfd-notice">{{ changeDetail.note }}</div>
            <div v-if="!changeDetail.files.length" class="wfd-status">本轮没有改动文件。</div>
            <button v-for="f in changeDetail.files" :key="f.path" type="button"
                    class="wfd-change-row" :class="{ active: currentFile === f.path }" @click="selectChangeFile(f)">
              <span class="wfd-status-dot" :class="f.status" :aria-label="statusLabel(f.status)">{{ statusLetter(f.status) }}</span>
              <span class="wfd-change-path" :title="f.path">{{ f.path }}</span>
              <span class="wfd-change-stat"><b v-if="f.additions">+{{ f.additions }}</b><i v-if="f.deletions">-{{ f.deletions }}</i></span>
            </button>
            <div class="wfd-revert">
              <button type="button" class="btn sm" :class="{ danger: revertArm }"
                      :disabled="reverting || !!changeDetail.revertedAt || changeDetail.snapshot === false"
                      @click="armRevert">
                {{ changeDetail.revertedAt ? '本轮已还原' : reverting ? '还原中…' : revertArm ? '确认还原 ' + changeDetail.files.length + ' 个文件' : '还原本轮改动' }}
              </button>
              <p class="wfd-hint">
                <template v-if="changeDetail.revertedAt">文件已恢复到本轮开始前；之后的新改动不受影响。</template>
                <template v-else-if="changeDetail.snapshot === false">本轮没有可用快照，只能查看不能还原。</template>
                <template v-else>只恢复本轮改过的文件；文件在本轮后又被改过会拒绝还原。</template>
              </p>
            </div>
          </template>
        </div>

        <div class="wfd-detail wfd-diff-pane">
          <div v-if="!selectedChange" class="wfd-empty">
            <b>选择左侧文件查看 diff</b>
            <p class="meta">绿色是新增，红色是删除；行号左边是旧文件，右边是新文件。</p>
          </div>
          <template v-else>
            <div class="wfd-detail-head">
              <div class="wfd-detail-title">
                <b :title="selectedChange.path">{{ selectedChange.path }}</b>
                <span class="wfd-diff-meta">
                  <span class="wfd-status-text" :class="selectedChange.status">{{ statusLabel(selectedChange.status) }}</span>
                  <b v-if="selectedChange.additions" class="add">+{{ selectedChange.additions }}</b>
                  <i v-if="selectedChange.deletions" class="del">-{{ selectedChange.deletions }}</i>
                  <span v-if="selectedChange.binary" class="meta">二进制</span>
                </span>
              </div>
            </div>
            <div v-if="selectedChange.error && !diffRows.length" class="wfd-status">{{ selectedChange.error }}</div>
            <div v-else-if="!diffRows.length" class="wfd-status">这个文件没有文本 diff。</div>
            <div v-else class="wfd-diff" role="region" :aria-label="selectedChange.path + ' 的差异'">
              <div v-for="(row, i) in diffRows" :key="i" class="wfd-diff-row" :class="row.kind">
                <template v-if="row.kind === 'hunk'">
                  <span class="wfd-diff-hunk">{{ row.text }}</span>
                </template>
                <template v-else-if="row.kind === 'meta'">
                  <span class="wfd-diff-hunk">{{ row.text }}</span>
                </template>
                <template v-else>
                  <span class="wfd-ln">{{ row.old }}</span>
                  <span class="wfd-ln">{{ row.new }}</span>
                  <span class="wfd-diff-code">{{ row.text }}</span>
                </template>
              </div>
            </div>
            <div v-if="selectedChange.truncated" class="wfd-notice">diff 过长，已截断；还原仍会处理完整文件。</div>
          </template>
        </div>
      </div>

      <div v-else-if="tab === 'memory'" class="wfd-body wfd-memory">
        <div class="wfd-memory-list">
          <div class="wfd-change-top">
            <span class="wfd-turn-label">项目记忆 {{ mem.pages.length ? mem.pages.length + ' 页' : '' }}</span>
            <button class="btn sm" type="button" @click="newMemory">新建</button>
          </div>
          <div v-if="mem.loading" class="wfd-status">读取中…</div>
          <div v-else-if="mem.error && !mem.pages.length" class="wfd-status is-error">{{ mem.error }}</div>
          <div v-else-if="!mem.pages.length" class="wfd-empty wfd-change-empty">
            <b>还没有项目记忆</b>
            <p class="meta">把约定、目录结构、验收口径记在这里；同一工作区的 agent 下一轮都会读到。</p>
          </div>
          <button v-for="page in mem.pages" :key="page.id" type="button"
                  class="wfd-change-row" :class="{ active: mem.editing === page.id }" @click="editMemory(page)">
            <span class="wfd-change-path" :title="page.title">{{ page.title }}</span>
            <span class="wfd-change-stat meta">{{ new Date(page.updatedAt).toLocaleDateString() }}</span>
          </button>
        </div>
        <div class="wfd-detail wfd-memory-detail">
          <div v-if="!mem.editing" class="wfd-empty">
            <b>选择一页项目记忆</b>
            <p class="meta">或点「新建」写一条约定。项目记忆按工作区共享，不写进用户仓库。</p>
          </div>
          <template v-else>
            <div class="wfd-detail-head">
              <div class="wfd-detail-title">
                <b>{{ mem.draft.id ? '编辑项目记忆' : '新建项目记忆' }}</b>
                <span class="meta">下一轮或重连时注入给 agent</span>
              </div>
            </div>
            <div class="wfd-memory-form">
              <label class="meta" for="memTitle">标题</label>
              <input id="memTitle" v-model="mem.draft.title" aria-label="项目记忆标题" placeholder="如：项目约定">
              <label class="meta" for="memBody">正文（markdown）</label>
              <textarea id="memBody" v-model="mem.draft.body" aria-label="项目记忆正文" rows="12"
                        placeholder="这个仓库用 pnpm，不要用 npm；验收要跑 npm run check。"></textarea>
              <p v-if="mem.error" class="wfd-status is-error" role="alert">{{ mem.error }}</p>
              <p v-if="mem.saved" class="wfd-status" role="status">{{ mem.saved }}</p>
              <div class="wfd-memory-actions">
                <button class="btn sm primary" type="button" :disabled="mem.saving || !mem.draft.title.trim() || !mem.draft.body.trim()"
                        @click="saveMemory">{{ mem.saving ? '保存中…' : '保存' }}</button>
                <button v-if="mem.draft.id" class="btn sm danger" type="button" @click="armDeleteMemory({ id: mem.draft.id })">
                  {{ mem.confirmDelete === mem.draft.id ? '确认删除' : '删除' }}
                </button>
                <button class="btn sm" type="button" @click="cancelMemory">关闭</button>
              </div>
            </div>
          </template>
        </div>
      </div>
      <div v-else class="wfd-body wfd-check">
        <div class="wfd-check-list">
          <div class="wfd-change-top">
            <span class="wfd-turn-label">本地预览</span>
          </div>
          <div v-if="!previews.length" class="wfd-status">时间线里还没有本地地址；agent 起服务后会自动出现在这里。</div>
          <div v-for="p in previews" :key="p.url" class="wfd-check-row"
               :class="{ active: chk.pane === 'preview' && chk.frameUrl === p.url }">
            <button type="button" class="wfd-check-main" :title="p.url" @click="openPreview(p)">{{ p.url }}</button>
            <span class="wfd-check-acts">
              <button type="button" class="btn sm" @click="openPreviewExternal(p)">新窗口</button>
              <button v-if="p.source === '手动添加'" type="button" class="btn sm"
                      @click="removePreview(p.url)">移除</button>
            </span>
          </div>
          <div class="wfd-check-add">
            <input v-model="chk.addUrl" aria-label="添加本地预览地址" placeholder="http://localhost:5173"
                   @keyup.enter="addPreviewUrl">
            <button type="button" class="btn sm" @click="addPreviewUrl">添加</button>
          </div>
          <p v-if="chk.addError" class="wfd-status is-error" role="alert">{{ chk.addError }}</p>

          <div class="wfd-change-top wfd-check-top-cmd">
            <span class="wfd-turn-label">检查命令</span>
            <span class="meta">先批准，再执行</span>
          </div>
          <div v-if="chk.suggestions.length" class="wfd-sugs">
            <button v-for="s in chk.suggestions" :key="s.command" type="button" class="wfd-sug"
                    :title="s.source" :disabled="chk.busy || checkActiveCount > 0"
                    @click="useSuggestion(s)">{{ s.label }}</button>
          </div>
          <div v-else class="wfd-status">这个目录里没认出常用检查命令，可以手填一条。</div>
          <div class="wfd-check-run">
            <input v-model="chk.command" aria-label="验证命令" placeholder="npm run check"
                   :disabled="chk.busy" @keyup.enter="runCheck()">
            <select v-model.number="chk.timeoutSec" class="wfd-turn-select" aria-label="执行超时">
              <option :value="120">2 分钟</option>
              <option :value="300">5 分钟</option>
              <option :value="900">15 分钟</option>
              <option :value="1800">30 分钟</option>
            </select>
            <button type="button" class="btn sm primary"
                    :disabled="!chk.command.trim() || chk.busy || checkActiveCount > 0"
                    @click="runCheck()">{{ chk.busy ? '提交中…' : '运行' }}</button>
          </div>
          <p v-if="chk.notice" class="wfd-status" role="status">{{ chk.notice }}</p>
          <p v-if="chk.error" class="wfd-status is-error" role="alert">{{ chk.error }}</p>

          <div class="wfd-change-top">
            <span class="wfd-turn-label">运行记录</span>
            <span v-if="chk.loading" class="meta">读取中…</span>
          </div>
          <div v-if="!chk.runs.length" class="wfd-status">还没有跑过验证命令。</div>
          <button v-for="r in chk.runs" :key="r.id" type="button" class="wfd-change-row"
                  :class="{ active: chk.pane === 'output' && chk.selectedId === r.id }"
                  @click="selectCheckRun(r)">
            <span class="wfd-change-path" :title="r.command">{{ r.command }}</span>
            <span class="wfd-change-stat wfd-run-state" :class="'st-' + r.state">{{ checkStateLabel(r.state) }}</span>
          </button>
        </div>

        <div class="wfd-detail wfd-check-detail">
          <template v-if="chk.pane === 'preview' && chk.frameUrl">
            <div class="wfd-detail-head">
              <div class="wfd-detail-title">
                <b :title="chk.frameUrl">{{ chk.frameUrl }}</b>
                <span class="meta">内嵌预览读不到 Friday 的登录态</span>
              </div>
              <div class="wfd-detail-acts">
                <button type="button" class="btn sm" @click="openPreviewExternal({ url: chk.frameUrl })">新窗口</button>
                <button type="button" class="btn sm" @click="chk.pane = 'output'; chk.frameUrl = ''">关闭</button>
              </div>
            </div>
            <iframe class="wfd-frame" :src="chk.frameUrl" :title="'预览 ' + chk.frameUrl"
                    sandbox="allow-scripts allow-forms allow-modals allow-popups" loading="lazy"></iframe>
            <p class="wfd-hint wfd-frame-hint">页面空白通常是它拒绝内嵌（X-Frame-Options）或需要完整本地存储，改用「新窗口」。</p>
          </template>
          <template v-else-if="chk.detail">
            <div class="wfd-detail-head">
              <div class="wfd-detail-title">
                <b :title="chk.detail.command">{{ chk.detail.command }}</b>
                <span class="wfd-diff-meta">
                  <span class="wfd-status-text" :class="'st-' + chk.detail.state">{{ checkStateLabel(chk.detail.state) }}</span>
                  <span v-if="chk.detail.exitCode >= 0 && chk.detail.state !== 'passed'" class="meta">退出码 {{ chk.detail.exitCode }}</span>
                  <span v-if="chk.detail.durationMs" class="meta">{{ fmtDuration(chk.detail.durationMs) }}</span>
                  <span class="meta">{{ new Date(chk.detail.startedAt).toLocaleTimeString() }}</span>
                </span>
              </div>
              <div class="wfd-detail-acts">
                <button v-if="chk.detail.state === 'awaiting' || chk.detail.state === 'running'"
                        type="button" class="btn sm danger" @click="stopCheckRun(chk.detail)">停止</button>
              </div>
            </div>
            <div v-if="checkNote" class="wfd-notice">{{ checkNote }}</div>
            <div v-if="chk.detail.state === 'awaiting'" class="wfd-status">
              等你在审批卡上批准后开始执行（运行页也能看到这张卡）。
            </div>
            <div v-else-if="checkLines.length" class="wfd-out" role="region" aria-label="验证命令输出">
              <div v-for="(l, i) in checkLines" :key="i" class="wfd-out-line" :class="{ bad: l.bad }">{{ l.text || ' ' }}</div>
            </div>
            <div v-else class="wfd-status">这条命令没有输出。</div>
            <div v-if="chk.detail.truncated" class="wfd-notice">输出很长，只显示尾部；已按 64 KB 截断。</div>
          </template>
          <div v-else class="wfd-empty">
            <b>选择一条运行记录看输出</b>
            <p class="meta">或点上面的预览地址看页面。命令都要先在审批卡上批准，超时会被自动终止。</p>
          </div>
        </div>
      </div>
    </aside>
  </div>
</template>
