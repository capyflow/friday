<script setup>
// 设置弹窗：左导航 + 右内容。已访问的 section 保持挂载（切来切去不丢未保存的编辑），
// 未访问的不挂载（进入哪一节才拉哪一节的接口）。
import { computed, markRaw, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { store } from '../store.js'
import ModelSection from './settings/ModelSection.vue'
import PersonaSection from './settings/PersonaSection.vue'
import McpSection from './settings/McpSection.vue'
import AcpSection from './settings/AcpSection.vue'
import SkillsSection from './settings/SkillsSection.vue'
import ToolsSection from './settings/ToolsSection.vue'
import PermissionSection from './settings/PermissionSection.vue'
import RuntimeSection from './settings/RuntimeSection.vue'
import CostSection from './settings/CostSection.vue'
import HeartbeatSection from './settings/HeartbeatSection.vue'
import StatusSection from './settings/StatusSection.vue'
import TraceSection from './settings/TraceSection.vue'
import SessionSection from './settings/SessionSection.vue'
import WikiSection from './settings/WikiSection.vue'
import AppearanceSection from './settings/AppearanceSection.vue'
import AboutSection from './settings/AboutSection.vue'

const emit = defineEmits(['close', 'logout'])

// 目前只有"关于与配置"一节需要往外抛事件（退出登录），其余节不绑监听，
// 避免 Vue 把多余的 logout 监听落到各自的 fragment 根上。
const GROUPS = [
  { label: '模型与人格', items: [
    { id: 'model', icon: 'i-model', label: '模型服务', comp: markRaw(ModelSection) },
    { id: 'persona', icon: 'i-persona', label: '人设提示词', comp: markRaw(PersonaSection) },
  ]},
  { label: '扩展', items: [
    { id: 'tools', icon: 'i-plug', label: '工具', comp: markRaw(ToolsSection) },
    { id: 'mcp', icon: 'i-mcp', label: 'MCP 服务器', comp: markRaw(McpSection) },
    { id: 'skills', icon: 'i-skill', label: '技能', comp: markRaw(SkillsSection) },
  ]},
  { label: '能力与行为', items: [
    { id: 'agent', icon: 'i-acp', label: '智能体', comp: markRaw(AcpSection) },
    { id: 'permissions', icon: 'i-shield', label: '权限模式', comp: markRaw(PermissionSection) },
    { id: 'runtime', icon: 'i-run', label: '运行上限', comp: markRaw(RuntimeSection) },
    { id: 'cost', icon: 'i-coin', label: '成本与预算', comp: markRaw(CostSection) },
    { id: 'heartbeat', icon: 'i-heart', label: '心跳清单', comp: markRaw(HeartbeatSection) },
  ]},
  { label: '运行观测', items: [
    { id: 'status', icon: 'i-status', label: '运行状态', comp: markRaw(StatusSection), wide: true },
    { id: 'trace', icon: 'i-trace', label: '调用轨迹', comp: markRaw(TraceSection), wide: true },
  ]},
  { label: '数据与系统', items: [
    { id: 'appearance', icon: 'i-sun', label: '外观', comp: markRaw(AppearanceSection) },
    { id: 'wiki', icon: 'i-mem', label: '记忆', comp: markRaw(WikiSection), wide: true },
    { id: 'sessions', icon: 'i-chat', label: '会话管理', comp: markRaw(SessionSection) },
    { id: 'about', icon: 'i-info', label: '关于与配置',
      comp: markRaw(AboutSection), on: { logout: () => emit('logout') } },
  ]},
]
const ITEMS = GROUPS.flatMap(g => g.items)
const itemById = id => ITEMS.find(x => x.id === id)

// 别的页面深链到某一节（自动化页「查看执行轮次」→ 调用轨迹）：读一次性定位值后清零，
// 下次打开仍默认落在模型服务。
const requested = store.settingsSection
store.settingsSection = ''
const initial = itemById(requested) ? requested : 'model'
const active = ref(initial)
const openedIds = ref([initial])
const openedItems = computed(() => openedIds.value.map(itemById))
const cur = computed(() => itemById(active.value))

function select(id) {
  active.value = id
  if (!openedIds.value.includes(id)) openedIds.value.push(id)
}

const dialog = ref(null)
let restoreFocusTo = null
function close() { emit('close') }
function onKeydown(e) { if (e.key === 'Escape') close() }

onMounted(async () => {
  document.addEventListener('keydown', onKeydown)
  restoreFocusTo = document.activeElement
  await nextTick()
  if (dialog.value) dialog.value.focus()
})
onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown)
  if (restoreFocusTo && document.contains(restoreFocusTo)) restoreFocusTo.focus()
})
</script>

<template>
  <Teleport to="body">
    <div class="set-mask" @click.self="close">
      <div ref="dialog" class="set-dialog" tabindex="-1" role="dialog" aria-modal="true" aria-labelledby="setDialogTitle">
        <nav class="set-nav" aria-label="设置导航">
          <div class="set-nav-brand">设置</div>
          <template v-for="g in GROUPS" :key="g.label">
            <div class="set-nav-group">{{ g.label }}</div>
            <button v-for="s in g.items" :key="s.id" type="button" class="set-nav-item"
                    :class="{ active: active === s.id }" :aria-current="active === s.id ? 'page' : undefined"
                    @click="select(s.id)">
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
                   stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use :href="'#' + s.icon"/></svg>
              <span>{{ s.label }}</span>
            </button>
          </template>
        </nav>

        <div class="set-main">
          <header class="set-head">
            <div class="set-head-txt">
              <h2 id="setDialogTitle">{{ cur.label }}</h2>
            </div>
            <button type="button" class="set-close" aria-label="关闭设置" @click="close">
              <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
                   stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
            </button>
          </header>
          <div class="set-scroll">
            <div v-for="s in openedItems" :key="s.id" v-show="active === s.id"
                 class="set-pane" :class="{ wide: s.wide }">
              <component :is="s.comp" v-on="s.on || {}" />
            </div>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>
