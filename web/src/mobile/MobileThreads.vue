<script setup>
// 移动端工作区：两级视图——①项目（工作区）列表；②点进项目看该项目全部
// 会话并可新建会话（工作区已定，只选 agent）。新建项目在手机上直接浏览
// 电脑目录（GET /api/acp/dirs 逐层懒列举）后 POST /api/acp/workspaces 创建，
// 与桌面端同一组接口。终端入口刻意不做——终端通道只收本机回环，见
// internal/admin/terminal.go 的硬闸说明。
import { computed, onMounted, ref } from 'vue'
import { api } from '../api.js'
import { toast } from '../store.js'
import { ago } from '../format.js'
import SelectBox from '../components/SelectBox.vue'

const emit = defineEmits(['open'])

const workspaces = ref([])
const threads = ref([])
const agents = ref([])
const loading = ref(false)
const creating = ref(false)

// 两级导航：list = 项目列表；ws = 某项目的会话列表
const view = ref('list')
const currentWs = ref(null)

/* ---------- 数据 ---------- */

async function load() {
  loading.value = true
  try {
    const r = await api('GET', '/api/acp/threads')
    workspaces.value = r.workspaces || []
    threads.value = r.threads || []
    agents.value = (r.acp && r.acp.agents) || []
  } catch (e) {
    toast('加载会话失败：' + e.message, true)
  } finally {
    loading.value = false
  }
}
onMounted(load)

const wsName = id => (workspaces.value.find(w => w.id === id) || {}).name || '未知工作区'
const isRunning = t => t.state === 'running' || !!t.turnStartedAt

// 项目行信息：会话数 + 最近活跃
const wsInfo = computed(() => {
  const map = {}
  for (const t of threads.value) {
    const e = map[t.workspaceId] || (map[t.workspaceId] = { count: 0, latest: '' })
    e.count++
    if (t.updatedAt > e.latest) e.latest = t.updatedAt
  }
  return map
})

function openWs(w) {
  currentWs.value = w
  view.value = 'ws'
}
function backToList() {
  view.value = 'list'
  currentWs.value = null
}
function open(t) {
  emit('open', t)
}

/* ---------- 新建会话（工作区已定） ---------- */

const wsSheet = ref(false)
const newAgent = ref('')
const newTitle = ref('')

function openWsSheet() {
  newTitle.value = ''
  if (!newAgent.value && agents.value.length) newAgent.value = agents.value[0].name
  wsSheet.value = true
}

async function createThread() {
  if (creating.value || !currentWs.value || !newAgent.value) return
  creating.value = true
  try {
    const r = await api('POST', '/api/acp/threads', {
      workspaceId: currentWs.value.id,
      agent: newAgent.value,
      title: newTitle.value.trim(),
    })
    wsSheet.value = false
    await load()
    emit('open', r.thread)
  } catch (e) {
    toast('创建会话失败：' + e.message, true)
  } finally {
    creating.value = false
  }
}

/* ---------- 新建项目（浏览电脑目录后创建） ---------- */

const projSheet = ref(false)
const projName = ref('')
const browsePath = ref('')
const browseParent = ref('')
const browseDirs = ref([])
const browsing = ref(false)

const agentOptions = computed(() => agents.value.map(a => ({ value: a.name, label: a.name })))

async function openProjSheet() {
  projName.value = ''
  projSheet.value = true
  await browseTo('~/')
}

async function browseTo(path) {
  browsing.value = true
  try {
    const r = await api('GET', '/api/acp/dirs?path=' + encodeURIComponent(path))
    browsePath.value = r.path || ''
    browseParent.value = r.parent || ''
    browseDirs.value = r.dirs || []
    if (!projName.value) projName.value = (r.path || '').split('/').filter(Boolean).pop() || ''
  } catch (e) {
    toast('读取目录失败：' + e.message, true)
  } finally {
    browsing.value = false
  }
}

async function createProject() {
  if (creating.value || !browsePath.value) return
  creating.value = true
  try {
    const r = await api('POST', '/api/acp/workspaces', {
      name: projName.value.trim(),
      dirs: [browsePath.value],
    })
    projSheet.value = false
    await load()
    openWs(r.workspace)
    toast('项目已创建')
  } catch (e) {
    toast('创建项目失败：' + e.message, true)
  } finally {
    creating.value = false
  }
}

const shortPath = p => (p || '').replace(/^\/Users\/[^/]+/, '~')
</script>

<template>
  <div class="m-pane">
    <!-- ① 项目列表 -->
    <template v-if="view === 'list'">
      <div class="m-pane-head">
        <span class="m-pane-title">项目</span>
        <button class="m-btn primary small" type="button" @click="openProjSheet">＋ 新项目</button>
      </div>
      <div class="m-list">
        <div v-if="!workspaces.length && !loading" class="m-empty">
          还没有项目。点右上角「＋ 新项目」，选一个电脑上的目录创建。
        </div>
        <button v-for="w in workspaces" :key="w.id" type="button" class="m-row" @click="openWs(w)">
          <span class="m-row-main">
            <span class="m-row-title">{{ w.name }}</span>
            <span class="m-row-sub">
              {{ (wsInfo[w.id] || {}).count || 0 }} 个会话<template v-if="(wsInfo[w.id] || {}).latest"> · 最近 {{ ago((wsInfo[w.id]).latest) }}前</template>
            </span>
          </span>
        </button>
        <div v-if="loading" class="m-empty">加载中…</div>
      </div>
    </template>

    <!-- ② 项目会话列表 -->
    <template v-else>
      <div class="m-pane-head">
        <button class="m-ico-btn" type="button" aria-label="返回项目列表" @click="backToList">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m15 18-6-6 6-6"/></svg>
        </button>
        <span class="m-pane-title">{{ currentWs && currentWs.name }}</span>
        <button class="m-btn primary small" type="button" @click="openWsSheet">＋ 新会话</button>
      </div>
      <div class="m-list">
        <div v-if="!threads.some(t => currentWs && t.workspaceId === currentWs.id) && !loading" class="m-empty">
          这个项目还没有会话。点右上角「＋ 新会话」开始。
        </div>
        <template v-for="t in threads" :key="t.id">
          <button v-if="currentWs && t.workspaceId === currentWs.id" type="button" class="m-row" @click="open(t)">
            <span class="m-row-main">
              <span class="m-row-title">{{ t.title || '未命名会话' }}</span>
              <span class="m-row-sub">{{ t.agent }}<template v-if="t.updatedAt"> · {{ ago(t.updatedAt) }}前</template></span>
            </span>
            <span v-if="isRunning(t)" class="m-dot run" title="运行中"></span>
          </button>
        </template>
        <div v-if="loading" class="m-empty">加载中…</div>
      </div>
    </template>

    <!-- 新建会话（工作区已定，选 agent） -->
    <div v-if="wsSheet" class="m-sheet-mask" @click.self="wsSheet = false">
      <div class="m-sheet" role="dialog" aria-label="新建会话">
        <div class="m-sheet-title">新建会话 · {{ currentWs && currentWs.name }}</div>
        <label class="m-field">
          <span class="m-field-label">智能体</span>
          <SelectBox v-model="newAgent" :options="agentOptions" aria-label="智能体" />
        </label>
        <label class="m-field">
          <span class="m-field-label">标题（可选）</span>
          <input v-model="newTitle" class="m-input" placeholder="留空自动取首条消息" autocapitalize="none">
        </label>
        <div class="m-sheet-acts">
          <button class="m-btn" type="button" @click="wsSheet = false">取消</button>
          <button class="m-btn primary" type="button" :disabled="creating || !newAgent" @click="createThread">
            {{ creating ? '创建中…' : '创建' }}
          </button>
        </div>
      </div>
    </div>

    <!-- 新建项目：名称 + 电脑目录浏览 -->
    <div v-if="projSheet" class="m-sheet-mask" @click.self="projSheet = false">
      <div class="m-sheet" role="dialog" aria-label="新建项目">
        <div class="m-sheet-title">新建项目</div>
        <label class="m-field">
          <span class="m-field-label">项目名</span>
          <input v-model="projName" class="m-input" placeholder="默认取目录名" autocapitalize="none">
        </label>
        <div class="m-field">
          <span class="m-field-label">在电脑上选择目录</span>
          <div class="m-dir-path">{{ shortPath(browsePath) || '…' }}</div>
          <div class="m-dir-list">
            <button v-if="browseParent" class="m-dir-row" type="button" @click="browseTo(browseParent)">
              ← 上一级
            </button>
            <button v-for="d in browseDirs" :key="d.path" class="m-dir-row" type="button" @click="browseTo(d.path)">
              {{ d.name }}
            </button>
            <div v-if="browsing" class="m-empty">读取中…</div>
            <div v-else-if="!browseDirs.length" class="m-empty">没有子目录</div>
          </div>
        </div>
        <div class="m-sheet-acts">
          <button class="m-btn" type="button" @click="projSheet = false">取消</button>
          <button class="m-btn primary" type="button" :disabled="creating || !browsePath || !projName.trim()" @click="createProject">
            {{ creating ? '创建中…' : '在此创建项目' }}
          </button>
        </div>
        <div class="m-sheet-note">目录只读浏览，创建项目不会改动电脑上的文件。</div>
      </div>
    </div>
  </div>
</template>
