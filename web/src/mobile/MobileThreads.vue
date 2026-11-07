<script setup>
// 移动端工作区会话列表：一个请求拉齐工作区 + 会话 + agent 清单（同桌面
// GET /api/acp/threads 的骨架数据），点行进入会话，「新建」走底部弹层选
// 工作区 + agent 后创建。终端入口刻意不做——终端通道只收本机回环，见
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
const sheet = ref(false)

// 新建弹层的选择状态
const newWorkspace = ref('')
const newAgent = ref('')
const newTitle = ref('')

async function load() {
  loading.value = true
  try {
    const r = await api('GET', '/api/acp/threads')
    workspaces.value = r.workspaces || []
    threads.value = r.threads || []
    agents.value = (r.acp && r.acp.agents) || []
    if (!newWorkspace.value && workspaces.value.length) newWorkspace.value = workspaces.value[0].id
    if (!newAgent.value && agents.value.length) newAgent.value = agents.value[0].name
  } catch (e) {
    toast('加载会话失败：' + e.message, true)
  } finally {
    loading.value = false
  }
}

onMounted(load)

const wsName = id => (workspaces.value.find(w => w.id === id) || {}).name || '未知工作区'

// SelectBox 选项集（仓库约定：下拉一律用 SelectBox 自绘弹出层）
const wsOptions = computed(() => workspaces.value.map(w => ({ value: w.id, label: w.name })))
const agentOptions = computed(() => agents.value.map(a => ({ value: a.name, label: a.name })))

// state 尚无稳定的枚举文档，桌面端也是按真值显示；这里只把 running 亮出来。
const isRunning = t => t.state === 'running' || !!t.turnStartedAt

function open(t) {
  emit('open', t)
}

function openSheet() {
  newTitle.value = ''
  sheet.value = true
}

async function create() {
  if (creating.value || !newWorkspace.value || !newAgent.value) return
  creating.value = true
  try {
    const r = await api('POST', '/api/acp/threads', {
      workspaceId: newWorkspace.value,
      agent: newAgent.value,
      title: newTitle.value.trim(),
    })
    sheet.value = false
    await load()
    emit('open', r.thread)
  } catch (e) {
    toast('创建会话失败：' + e.message, true)
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <div class="m-pane">
    <div class="m-pane-head">
      <span class="m-pane-title">工作区会话</span>
      <button class="m-btn primary small" type="button" @click="openSheet">＋ 新会话</button>
    </div>

    <div class="m-list">
      <div v-if="!threads.length && !loading" class="m-empty">
        还没有智能体会话。点右上角「＋ 新会话」选一个工作区和 agent 开始。
      </div>
      <button v-for="t in threads" :key="t.id" type="button" class="m-row" @click="open(t)">
        <span class="m-row-main">
          <span class="m-row-title">{{ t.title || '未命名会话' }}</span>
          <span class="m-row-sub">{{ wsName(t.workspaceId) }} · {{ t.agent }}<template v-if="t.updatedAt"> · {{ ago(t.updatedAt) }}前</template></span>
        </span>
        <span v-if="isRunning(t)" class="m-dot run" title="运行中"></span>
      </button>
      <div v-if="loading" class="m-empty">加载中…</div>
    </div>

    <div v-if="sheet" class="m-sheet-mask" @click.self="sheet = false">
      <div class="m-sheet" role="dialog" aria-label="新建会话">
        <div class="m-sheet-title">新建会话</div>
        <label class="m-field">
          <span class="m-field-label">工作区</span>
          <SelectBox v-model="newWorkspace" :options="wsOptions" aria-label="工作区" />
        </label>
        <label class="m-field">
          <span class="m-field-label">智能体</span>
          <SelectBox v-model="newAgent" :options="agentOptions" aria-label="智能体" />
        </label>
        <label class="m-field">
          <span class="m-field-label">标题（可选）</span>
          <input v-model="newTitle" class="m-input" placeholder="留空自动取首条消息" autocapitalize="none">
        </label>
        <div class="m-sheet-acts">
          <button class="m-btn" type="button" @click="sheet = false">取消</button>
          <button class="m-btn primary" type="button" :disabled="creating || !newWorkspace || !newAgent" @click="create">
            {{ creating ? '创建中…' : '创建' }}
          </button>
        </div>
        <div v-if="!workspaces.length" class="m-sheet-note">还没有工作区，先在桌面端「工作区」里建一个。</div>
      </div>
    </div>
  </div>
</template>
