<script setup>
import { nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { store, hideModal, toast, toastState, modalState } from './store.js'
import { api } from './api.js'
import { initTheme, setTheme } from './theme.js'
import { auroraDrift, shellIn } from './anim.js'
import LoginView from './components/LoginView.vue'
import ChatView from './components/ChatView.vue'
import StatusView from './components/StatusView.vue'
import TraceView from './components/TraceView.vue'
import AutoView from './components/AutoView.vue'
import TodoView from './components/TodoView.vue'
import WikiView from './components/WikiView.vue'
import SettingsView from './components/SettingsView.vue'
import ApprovalCards from './components/ApprovalCards.vue'

const NAV = [
  { id: 'chat', icon: 'i-chat', label: '聊天' },
  { id: 'status', icon: 'i-status', label: '运行状态' },
  { id: 'trace', icon: 'i-trace', label: '调用轨迹' },
  { id: 'auto', icon: 'i-auto', label: '自动化' },
  { id: 'todo', icon: 'i-todo', label: '待办' },
  { id: 'wiki', icon: 'i-mem', label: '记忆' },
]

/* ---------- 主题 ---------- */
initTheme()
const theme = ref(document.documentElement.dataset.theme || 'dark')
function toggleTheme() {
  theme.value = theme.value === 'dark' ? 'light' : 'dark'
  setTheme(theme.value)
}

/* ---------- 用户悬浮窗 ---------- */
const userPopOpen = ref(false)
const userInitial = ref('U')
watch(() => store.username, v => { userInitial.value = (v || 'U').charAt(0).toUpperCase() }, { immediate: true })
const host = ref(location.host)

function toggleUserPop() { userPopOpen.value = !userPopOpen.value }
function onDocClick(e) {
  if (userPopOpen.value && !e.target.closest('.side-foot') && !e.target.closest('.user-pop')) {
    userPopOpen.value = false
  }
}
function onKeydown(e) {
  if (e.key === 'Escape') { hideModal(); userPopOpen.value = false }
}
onMounted(() => {
  document.addEventListener('keydown', onKeydown)
  document.addEventListener('click', onDocClick)
  auroraDrift(document)
})
onUnmounted(() => {
  document.removeEventListener('keydown', onKeydown)
  document.removeEventListener('click', onDocClick)
})

onMounted(async () => {
  try {
    const s = await api('GET', '/api/session')
    if (s.authenticated) { store.loggedIn = true; store.username = s.username }
  } catch (e) { /* 服务不可达时停在登录页 */ }
})

/* 登录成功：外壳入场 */
watch(() => store.loggedIn, async ok => {
  if (ok) { await nextTick(); shellIn() }
})

async function logout() {
  try { await api('POST', '/api/logout', {}) } catch (e) {}
  userPopOpen.value = false
  store.loggedIn = false
  toast('已退出登录')
}
</script>

<template>
  <!-- SVG 图标（Lucide 风格描边） -->
  <svg style="display:none" aria-hidden="true">
    <symbol id="i-chat" viewBox="0 0 24 24"><path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z"/></symbol>
    <symbol id="i-status" viewBox="0 0 24 24"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></symbol>
    <symbol id="i-trace" viewBox="0 0 24 24"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/></symbol>
    <symbol id="i-auto" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></symbol>
    <symbol id="i-mem" viewBox="0 0 24 24"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></symbol>
    <symbol id="i-settings" viewBox="0 0 24 24"><line x1="4" y1="21" x2="4" y2="14"/><line x1="4" y1="10" x2="4" y2="3"/><line x1="12" y1="21" x2="12" y2="12"/><line x1="12" y1="8" x2="12" y2="3"/><line x1="20" y1="21" x2="20" y2="16"/><line x1="20" y1="12" x2="20" y2="3"/><line x1="1" y1="14" x2="7" y2="14"/><line x1="9" y1="8" x2="15" y2="8"/><line x1="17" y1="16" x2="23" y2="16"/></symbol>
    <symbol id="i-route" viewBox="0 0 24 24"><circle cx="6" cy="19" r="3"/><path d="M9 19h8.5a3.5 3.5 0 0 0 0-7h-11a3.5 3.5 0 0 1 0-7H15"/><circle cx="18" cy="5" r="3"/></symbol>
    <symbol id="i-send" viewBox="0 0 24 24"><path d="M12 19V5M5 12l7-7 7 7"/></symbol>
    <symbol id="i-out" viewBox="0 0 24 24"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/></symbol>
    <symbol id="i-reset" viewBox="0 0 24 24"><polyline points="1 4 1 10 7 10"/><path d="M3.51 15a9 9 0 1 0 2.13-9.36L1 10"/></symbol>
    <symbol id="i-clip" viewBox="0 0 24 24"><path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48"/></symbol>
    <symbol id="i-more" viewBox="0 0 24 24"><circle cx="12" cy="5" r="1.7"/><circle cx="12" cy="12" r="1.7"/><circle cx="12" cy="19" r="1.7"/></symbol>
    <symbol id="i-todo" viewBox="0 0 24 24"><path d="M9 11l3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/></symbol>
    <symbol id="i-sun" viewBox="0 0 24 24"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41"/></symbol>
    <symbol id="i-moon" viewBox="0 0 24 24"><path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/></symbol>
  </svg>

  <!-- 极光背景（深色可见光斑，浅色极淡点阵） -->
  <div class="aurora" aria-hidden="true"><i></i><i></i><i></i></div>

  <LoginView v-if="!store.loggedIn" />

  <div v-else id="app">
    <aside id="sidebar">
      <div class="brand"><div class="avatar">五</div><b>小五</b></div>
      <nav id="nav" aria-label="主导航">
        <button v-for="n in NAV" :key="n.id" class="nav-item" :class="{ active: store.view === n.id }"
                :data-view="n.id" @click="store.view = n.id">
          <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
               stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use :href="'#' + n.icon"/></svg>
          <span class="txt">{{ n.label }}</span>
        </button>
      </nav>
      <div class="grow"></div>

      <!-- 用户悬浮窗 -->
      <div v-if="userPopOpen" class="user-pop" role="dialog" aria-label="账号">
        <div class="up-head">
          <span class="u-avatar">{{ userInitial }}</span>
          <div class="up-id">
            <b>{{ store.username }}</b>
            <span class="meta">管理员 · 已登录</span>
          </div>
        </div>
        <div class="up-meta">
          <div class="row-line"><span class="meta">服务地址</span><span class="mono">{{ host }}</span></div>
          <div class="row-line"><span class="meta">会话有效期</span><span class="mono">7 天</span></div>
        </div>
        <button class="btn" style="width:100%; margin-bottom:8px" @click="toggleTheme">
          <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use :href="theme === 'dark' ? '#i-sun' : '#i-moon'"/></svg>
          切换到{{ theme === 'dark' ? '浅色' : '深色' }}模式
        </button>
        <button class="btn danger" style="width:100%" @click="logout">
          <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-out"/></svg>退出登录
        </button>
      </div>

      <!-- 底部：用户 + 设置 -->
      <div class="side-foot">
        <button class="user-btn" :aria-expanded="userPopOpen" aria-label="账号" @click="toggleUserPop">
          <span class="u-avatar">{{ userInitial }}</span>
          <span class="u-name">{{ store.username }}</span>
          <svg class="u-chev" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg>
        </button>
        <button class="foot-btn" :class="{ active: store.view === 'settings' }" aria-label="设置"
                @click="store.view = 'settings'">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-settings"/></svg>
        </button>
      </div>
    </aside>

    <main id="main">
      <!-- 聊天常驻（切页不打断流式回复），其余视图按需挂载；
           Transition 在挂载同帧应用入场初始态，消除闪帧 -->
      <Transition name="view"><ChatView v-show="store.view === 'chat'" /></Transition>
      <Transition name="view"><StatusView v-if="store.view === 'status'" /></Transition>
      <Transition name="view"><TraceView v-if="store.view === 'trace'" /></Transition>
      <Transition name="view"><AutoView v-if="store.view === 'auto'" /></Transition>
      <Transition name="view"><TodoView v-if="store.view === 'todo'" /></Transition>
      <Transition name="view"><WikiView v-if="store.view === 'wiki'" /></Transition>
      <Transition name="view"><SettingsView v-if="store.view === 'settings'" @logout="logout" /></Transition>
    </main>
  </div>

  <ApprovalCards />
  <div id="toast" :class="{ show: toastState.show, err: toastState.err }" role="status" aria-live="polite">{{ toastState.text }}</div>
  <div v-if="modalState.show" id="modalMask" @click.self="hideModal">
    <div class="modal" role="alertdialog" aria-modal="true" aria-labelledby="modalTitle">
      <h3 id="modalTitle">登录失败</h3>
      <p>{{ modalState.text }}</p>
      <button class="btn primary" type="button" @click="hideModal">知道了</button>
    </div>
  </div>
</template>
