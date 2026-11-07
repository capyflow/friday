<script setup>
// 移动端外壳：登录/首装门卫 + 三个底部标签（对话=主会话 / 工作区=智能体
// 会话与文件 / 通知）。登录态建立后拉起通知长链接（push.js），未读角标实时走
// WS，不轮询。首装建号只收本机请求，账号未建时登录页直接给指引（见 MobileLogin）。
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api } from '../api.js'
import { store, toast, toastState, hideModal, modalState } from '../store.js'
import { setToken, hydrateToken } from '../auth.js'
import { push, startPush, stopPush } from './push.js'
import MobileLogin from './MobileLogin.vue'
import MobileChat from './MobileChat.vue'
import MobileThreads from './MobileThreads.vue'
import MobileFiles from './MobileFiles.vue'
import MobileNotifications from './MobileNotifications.vue'

// phase: boot（探测中）| main（登录门卫之后的一切——由 store.loggedIn 分流）
const phase = ref('boot')
const bootErr = ref('')
const tab = ref('chat') // chat | threads | notices
const openThread = ref(null) // 当前打开的智能体会话对象
const filesOpen = ref(false) // 会话内「对话 ↔ 文件」切换

async function boot() {
  bootErr.value = ''
  try {
    await hydrateToken() // 浏览器直访是 no-op（无桌面壳），与桌面端保持一致
    store.setupStatus = await api('GET', '/api/setup/status')
    const s = await api('GET', '/api/session')
    if (s.authenticated) {
      store.loggedIn = true
      store.username = s.username || ''
    } else {
      setToken('') // token 缺失/过期：清掉本地残留
      store.loggedIn = false
    }
    phase.value = 'main' // 探测结束（登录与否交给 store.loggedIn 分流），离开启动屏
  } catch (e) {
    bootErr.value = e.message || String(e)
  }
}
onMounted(boot)

// 登录态变化：建立/断开长链接；登出后清现场。
watch(() => store.loggedIn, ok => {
  if (ok) {
    startPush()
  } else {
    stopPush()
    openThread.value = null
    tab.value = 'chat'
  }
})
onBeforeUnmount(stopPush)

function logout() {
  api('POST', '/api/logout', {}).catch(() => {})
  setToken('')
  store.loggedIn = false
  toast('已退出登录')
}

function openThreadFrom(t) {
  openThread.value = t
  filesOpen.value = false
}
</script>

<template>
  <div class="m-app">
    <!-- 启动探测 -->
    <div v-if="phase === 'boot'" class="m-boot">
      <span class="brand-mark m-boot-logo"></span>
      <p v-if="!bootErr">连接中…</p>
      <template v-else>
        <p class="m-boot-err">无法连接服务器：{{ bootErr }}</p>
        <button class="m-btn primary" type="button" @click="boot">重试</button>
      </template>
    </div>

    <!-- 登录 / 首装指引 -->
    <MobileLogin v-else-if="!store.loggedIn" :setup-status="store.setupStatus" />

    <!-- 主界面 -->
    <template v-else>
      <header class="m-head">
        <div class="m-head-brand">
          <span class="brand-mark"></span>
          <b>小五</b>
          <span v-if="store.username" class="m-dim">{{ store.username }}</span>
        </div>
        <div class="m-head-acts">
          <button class="m-ico-btn" type="button" aria-label="退出登录" title="退出登录" @click="logout">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/></svg>
          </button>
        </div>
      </header>

      <main class="m-body">
        <!-- 对话：主会话（生活模式） -->
        <MobileChat v-show="tab === 'chat'" kind="main" />

        <!-- 工作区：会话列表 ↔ 会话（对话 + 文件） -->
        <template v-if="tab === 'threads'">
          <div v-if="!openThread" class="m-fill">
            <MobileThreads @open="openThreadFrom" />
          </div>
          <div v-else class="m-fill m-thread">
            <div class="m-thread-head">
              <button class="m-ico-btn" type="button" aria-label="返回列表" @click="openThread = null">
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m15 18-6-6 6-6"/></svg>
              </button>
              <span class="m-thread-title">{{ openThread.title || '未命名会话' }}</span>
              <button class="m-btn small" type="button" @click="filesOpen = !filesOpen">
                {{ filesOpen ? '关文件' : '文件' }}
              </button>
            </div>
            <MobileChat v-if="!filesOpen" kind="thread" :thread="openThread" />
            <MobileFiles v-else :thread="openThread" />
          </div>
        </template>

        <!-- 通知 -->
        <MobileNotifications v-if="tab === 'notices'" />
      </main>

      <nav class="m-nav">
        <button class="m-nav-item" :class="{ on: tab === 'chat' }" type="button" @click="tab = 'chat'">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z"/></svg>
          <span>对话</span>
        </button>
        <button class="m-nav-item" :class="{ on: tab === 'threads' }" type="button" @click="tab = 'threads'">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="8" width="16" height="12" rx="2"/><path d="M12 8V4M8 4h8"/><circle cx="9" cy="13" r="1"/><circle cx="15" cy="13" r="1"/><path d="M9 17h6"/></svg>
          <span>工作区</span>
        </button>
        <button class="m-nav-item" :class="{ on: tab === 'notices' }" type="button" @click="tab = 'notices'">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9"/><path d="M10 21h4"/></svg>
          <span>通知</span>
          <i v-if="push.unread" class="m-badge">{{ push.unread > 99 ? '99+' : push.unread }}</i>
        </button>
      </nav>
    </template>

    <!-- 轻提示与弹窗（与桌面同一套 store） -->
    <div id="m-toast" :class="{ show: toastState.show, err: toastState.err }" role="status" aria-live="polite">{{ toastState.text }}</div>
    <div v-if="modalState.show" id="m-modal-mask" @click.self="hideModal">
      <div class="m-modal" role="alertdialog" aria-modal="true">
        <h3>{{ modalState.title || '提示' }}</h3>
        <p>{{ modalState.text }}</p>
        <button class="m-btn primary block" type="button" @click="hideModal">知道了</button>
      </div>
    </div>
  </div>
</template>
