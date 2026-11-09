<script setup>
// WorkspaceTools 是工作区右侧的统一工具面板：终端、文件、改动、Git 管理、
// 项目记忆都收在这一列里（参考 ZCode 的右侧面板：一列面板 + 若干标签，
// 而不是每个工具各开一个抽屉）。切换只走外层图标条——面板里不再重复一排文字标签。
//
// 三种形态（都只发生在工作区内部，不动左侧会话列表）：
//   rail  —— 默认：只剩一条窄图标条，对话占满剩余宽度；
//   panel —— 点图标展开：对话在左、面板在右，分隔线可拖宽；
//   focus —— 点右上角展开按钮：工作区一分为二，左边收窄的会话、右边占满的工具视图。
//
// 面板用 v-show 常驻：终端的 WebSocket、xterm 实例与内容都留着，收起或切标签
// 都不打断连接（与旧版终端面板同一套语义）。
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { getPref, setPref } from '../prefs.js'
import WorkspaceTerminal from './WorkspaceTerminal.vue'
import WorkspaceFilesDrawer from './WorkspaceFilesDrawer.vue'
import WorkspaceGit from './WorkspaceGit.vue'

const props = defineProps({
  thread: { type: Object, default: null },
  threads: { type: Array, default: () => [] },
  messages: { type: Array, default: () => [] },
  workspaceId: { type: String, default: '' },
})
const emit = defineEmits(['reverted', 'layout'])

// 标签清单：工具视图的入口都在这里排成一行，后面加新工具只改这一处。
const TABS = [
  { id: 'terminal', label: '终端', icon: 'i-terminal' },
  { id: 'files', label: '文件', icon: 'i-files' },
  { id: 'changes', label: '改动', icon: 'i-diff' },
  { id: 'git', label: 'Git 管理', icon: 'i-git' },
  { id: 'memory', label: '项目记忆', icon: 'i-mem' },
]
const TAB_LABEL = TABS.reduce((m, t) => (m[t.id] = t.label, m), {})

// 抽屉那三个标签（文件/改动/记忆）由 WorkspaceFilesDrawer 自己实现，
// 这里只做转发：面板切到哪个标签，就把哪个标签交给抽屉渲染。
const DRAWER_TABS = ['files', 'changes', 'memory']

const TAB_KEY = 'friday-workspace-tools-tab'
const tab = ref(readTab())
function readTab() {
  const v = getPref(TAB_KEY)
  return TABS.some(t => t.id === v) ? v : 'files'
}
// view 三态：rail（只图标条）/ panel（对话+面板）/ focus（会话收窄、面板占满）。
const VIEW_KEY = 'friday-workspace-tools-view'
const view = ref(readView())
function readView() {
  return getPref(VIEW_KEY) === 'focus' ? 'focus' : 'rail'
}
function setView(v) {
  view.value = v
  setPref(VIEW_KEY, v)
}
// rail 时对话占满，panel/focus 时工具面板占一份。
const railOpen = computed(() => view.value !== 'rail')
const focusOn = computed(() => view.value === 'focus')

// 面板宽度（占工作区比例）：panel 态拖分隔线调，focus 态改成调会话列宽。
const PANEL_MIN = 0.28
const PANEL_MAX = 0.78
const panelW = ref(0.46)
const panelStyle = computed(() => (panelW.value * 100).toFixed(2) + '%')
const CHAT_MIN = 0.18
const CHAT_MAX = 0.6
const chatW = ref(0.32)

// 分隔线拖拽：pointer 事件跟着松手走；拖动中禁选中（body.wt-resizing）。
function startSplit(e) {
  const split = e.target.closest('.ag-split')
  if (!split) return
  e.preventDefault()
  document.body.classList.add('wt-resizing')
  const move = ev => {
    const rect = split.getBoundingClientRect()
    if (rect.width <= 0) return
    if (focusOn.value) {
      // focus 态拖的是左边会话列的宽（从左往右量的占比）。
      const ratio = ev.clientX - rect.left
      chatW.value = Math.min(CHAT_MAX, Math.max(CHAT_MIN, ratio / rect.width))
    } else {
      const ratio = (rect.right - ev.clientX) / rect.width
      panelW.value = Math.min(PANEL_MAX, Math.max(PANEL_MIN, ratio))
    }
  }
  const up = () => {
    window.removeEventListener('pointermove', move)
    window.removeEventListener('pointerup', up)
    window.removeEventListener('pointercancel', up)
    document.body.classList.remove('wt-resizing')
  }
  window.addEventListener('pointermove', move)
  window.addEventListener('pointerup', up)
  window.addEventListener('pointercancel', up)
}

// rail 上点图标：同一个标签再点一次就收回成窄条（VS Code 的行为）。
function pickTab(id) {
  // 终端标签：点开就是给「当前这条会话」开终端（每条会话各自一份实例）。
  // 已经开着且当前会话还没终端时，点它不该把面板收起来，而是把终端开起来。
  if (id === 'terminal' && props.thread && props.thread.id) {
    const id2 = props.thread.id
    if (!termOpened.value.has(id2)) {
      termOpened.value = new Set(termOpened.value).add(id2)
      tab.value = id
      setPref(TAB_KEY, id)
      if (view.value === 'rail') setView('panel')
      return
    }
  }
  if (tab.value === id && view.value !== 'rail') {
    setView('rail')
    return
  }
  tab.value = id
  setPref(TAB_KEY, id)
  if (view.value === 'rail') setView('panel')
  if (DRAWER_TABS.indexOf(id) >= 0 && drawer.value) drawer.value.open(id)
}

// 面板展开时把当前标签交给对应组件（抽屉那一类要 open() 才会去拉数据）。
watch(view, v => {
  if (v === 'rail') return
  if (DRAWER_TABS.indexOf(tab.value) >= 0 && drawer.value) drawer.value.open(tab.value)
})

// 展开按钮：面板态 <-> focus 态（工作区一分为二，左会话右工具视图）。
function toggleFocus() {
  setView(focusOn.value ? 'panel' : 'focus')
}

// 工具面板对外的入口：时间线里的「本轮改动」与「记到项目记忆」都走这里，
// 把面板切到对应标签并打开（而不是另开一个抽屉）。
const drawer = ref(null)
function open(which, arg) {
  const id = DRAWER_TABS.indexOf(which) >= 0 ? which : (TABS.some(t => t.id === which) ? which : 'files')
  tab.value = id
  setPref(TAB_KEY, id)
  if (view.value === 'rail') setView('panel')
  if (DRAWER_TABS.indexOf(id) >= 0 && drawer.value) drawer.value.open(id, id === 'changes' ? arg : '')
}
function openMemory(draft) {
  tab.value = 'memory'
  setPref(TAB_KEY, 'memory')
  if (view.value === 'rail') setView('panel')
  if (drawer.value) drawer.value.openMemory(draft)
}

// 改动数量角标：最近一轮 agent 改了几个文件，就在「改动」图标上标几个。
const changeCount = computed(() => {
  let n = 0
  for (const m of props.messages || []) {
    if (m.role === 'bot' && m.changes && m.changes.files && m.changes.files.length) n = m.changes.files.length
  }
  return n
})

// 终端实例跟着会话走：每条会话自己的终端（cwd 也是各自的主目录），
// 第一次打开该会话的终端才挂载；之后一律 v-show，连接与内容都留着。
// 终端实例按会话挂载：每条会话自己的终端（cwd 也是各自的主目录），
// 点过终端标签才挂这条会话的实例；之后一律 v-show——连接、缓冲与内容都留着。
const termOpened = ref(new Set())

// 布局量报给父组件：focus 态要改左列宽度（左列在父组件里），
// open 让父组件的入口按钮知道面板现在是开着还是收成图标条。
watch([focusOn, chatW, railOpen], () => {
  emit('layout', { focus: focusOn.value, chatW: chatW.value, open: railOpen.value })
}, { immediate: true })

onMounted(() => {
  if (view.value !== 'rail' && DRAWER_TABS.indexOf(tab.value) >= 0 && drawer.value) {
    drawer.value.open(tab.value)
  }
})
onBeforeUnmount(() => {
  document.body.classList.remove('wt-resizing')
})

// toggle：入口按钮用——开着就收回成图标条，收着就展开。
function toggle() {
  setView(view.value === 'rail' ? 'panel' : 'rail')
}

defineExpose({ open, openMemory, pickTab, setView, toggle })
</script>

<template>
  <div class="ag-tools-col">
    <!-- 分隔线：拖动调两栏比例（focus 态调的是左边会话列的宽） -->
    <div v-show="railOpen" class="wt-divider" role="separator" aria-orientation="vertical"
         :aria-valuenow="Math.round(focusOn ? chatW * 100 : panelW * 100)"
         :aria-valuemin="focusOn ? 18 : 28" :aria-valuemax="focusOn ? 60 : 78"
         :aria-label="focusOn ? '调整会话与工具视图的比例' : '调整对话与工具面板的比例'"
         title="拖动调整比例" @pointerdown="startSplit"></div>

    <section v-show="railOpen" class="ag-tools" :class="{ focus: focusOn }"
             :style="focusOn ? null : { width: panelStyle }"
             aria-label="工具面板">
      <header class="ag-tools-head">
        <div class="ag-tools-title">
          <b>{{ TAB_LABEL[tab] }}</b>
          <span class="meta" :title="thread && thread.cwd ? thread.cwd : ''">{{ thread && thread.agent ? thread.agent : '' }}</span>
        </div>
        <!-- 标签行已去掉：切工具只走右侧图标条，面板内不再重复一排文字入口 -->
        <!-- 右上角展开按钮：工作区内一分为二，左会话、右工具视图 -->
        <button class="ib-btn" :class="{ active: focusOn }"
                :aria-label="focusOn ? '退出展开视图' : '展开工具视图'"
                :aria-pressed="focusOn"
                :title="focusOn ? '退出展开（回到并排两栏）' : '展开：左边会话、右边工具视图'"
                @click="toggleFocus">
          <svg v-if="!focusOn" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor"
               stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 3h6v6M9 21H3v-6M21 3l-7 7M3 21l7-7"/></svg>
          <svg v-else width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor"
               stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 3H5a2 2 0 0 0-2 2v4M15 3h4a2 2 0 0 1 2 2v4M9 21H5a2 2 0 0 1-2-2v-4M15 21h4a2 2 0 0 0 2-2v-4"/></svg>
        </button>
        <button class="ib-btn" aria-label="收起工具面板" title="收起（只剩图标条）" @click="setView('rail')">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
               stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
        </button>
      </header>

      <div class="ag-tools-body">
        <!-- 终端：每条会话一个实例，切会话不打断连接 -->
        <div v-show="tab === 'terminal'" class="ag-tools-pane">
          <template v-for="t in threads" :key="t.id">
            <WorkspaceTerminal v-if="termOpened.has(t.id)"
                               v-show="t.id === (thread && thread.id) && tab === 'terminal'"
                               :open="t.id === (thread && thread.id) && tab === 'terminal'"
                               :cwd="t.cwd || ''"
                               @close="setView('rail')" />
          </template>
          <div v-if="!(thread && thread.id) || !termOpened.has(thread.id)" class="ag-tools-empty">
            <p class="meta">这条会话还没开过终端。</p>
            <button type="button" class="btn sm" @click="pickTab('terminal')">
              在本会话主目录打开终端
            </button>
          </div>
        </div>

        <!-- 文件 / 改动 / 项目记忆：抽屉那套内容，嵌在面板里 -->
        <WorkspaceFilesDrawer ref="drawer" v-show="DRAWER_TABS.indexOf(tab) >= 0" embedded
                              :thread="thread" :messages="messages" :workspace-id="workspaceId"
                              :active-tab="tab"
                              @reverted="emit('reverted')" />

        <!-- Git 管理 -->
        <div v-show="tab === 'git'" class="ag-tools-pane">
          <WorkspaceGit :thread="thread" :messages="messages" />
        </div>
      </div>
    </section>
  </div>

  <!-- 图标条：工作区右侧常驻的一条窄栏（默认形态）。点图标展开对应工具。 -->
  <div class="ag-tools-rail" role="tablist" aria-label="工具视图">
    <button v-for="t in TABS" :key="t.id" type="button"
            class="ag-tools-rail-btn" :class="{ on: tab === t.id && railOpen }"
            :aria-selected="tab === t.id" role="tab"
            :aria-label="t.label" :title="t.label"
            @click="pickTab(t.id)">
      <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
           stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use :href="'#' + t.icon"/></svg>
      <span v-if="t.id === 'changes' && changeCount" class="ag-tools-badge" aria-hidden="true">{{ changeCount }}</span>
    </button>
  </div>
</template>
