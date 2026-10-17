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
import PermissionSection from './settings/PermissionSection.vue'
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
    { id: 'model', icon: 'i-model', label: '模型服务', desc: '模型库：多模型的地址、密钥、上下文窗口与输入格式，保存后热生效', comp: markRaw(ModelSection) },
    { id: 'persona', icon: 'i-persona', label: '人设提示词', desc: '小五的性格与语气，应用后热生效', comp: markRaw(PersonaSection) },
  ]},
  { label: '能力与行为', items: [
    { id: 'mcp', icon: 'i-mcp', label: 'MCP 服务器', desc: '本地命令或远程 URL 接入外部工具，保存后热生效', comp: markRaw(McpSection) },
    { id: 'agent', icon: 'i-acp', label: '智能体', desc: 'ACP agent 清单与运行参数：独立对话空间，绑定工作目录干活', comp: markRaw(AcpSection) },
    { id: 'skills', icon: 'i-skill', label: '技能', desc: 'SKILL.md 定义的做事套路，编辑保存即生效', comp: markRaw(SkillsSection) },
    { id: 'permissions', icon: 'i-shield', label: '权限模式', desc: '危险操作的把关力度，切换后立即生效', comp: markRaw(PermissionSection) },
    { id: 'cost', icon: 'i-coin', label: '成本与预算', desc: 'Token 用量热力图与预算额度，超线只停主动任务', comp: markRaw(CostSection) },
    { id: 'heartbeat', icon: 'i-heart', label: '心跳清单', desc: '定时交给小五自主检查的事项', comp: markRaw(HeartbeatSection) },
  ]},
  { label: '运行观测', items: [
    { id: 'status', icon: 'i-status', label: '运行状态', desc: '服务状态与 Token 用量账本，可按模型/来源/会话/运行拆分', comp: markRaw(StatusSection), wide: true },
    { id: 'trace', icon: 'i-trace', label: '调用轨迹', desc: '全局一条调用流，跟随对话历史（清除上下文时一并清除）', comp: markRaw(TraceSection), wide: true },
  ]},
  { label: '数据与系统', items: [
    { id: 'appearance', icon: 'i-sun', label: '外观', desc: '浅色 / 深色 / 跟随系统，只存本机浏览器', comp: markRaw(AppearanceSection) },
    { id: 'wiki', icon: 'i-mem', label: '记忆', desc: '记忆 Wiki：小五的长期知识，可搜索、新建与编辑', comp: markRaw(WikiSection), wide: true },
    { id: 'sessions', icon: 'i-chat', label: '会话管理', desc: '各入口的会话上下文，可单条清除', comp: markRaw(SessionSection) },
    { id: 'about', icon: 'i-info', label: '关于与配置', desc: '运行时生效的配置快照',
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
              <p class="meta">{{ cur.desc }}</p>
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
