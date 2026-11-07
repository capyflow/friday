<script setup>
// 移动端通知页：列表来自 push.js 的共享 reactive（长链接 hello/notice 帧实时
// 并入），动作（已读/重试/延后）与桌面端同一组 REST。进页面拉一次全量兜底。
import { onMounted } from 'vue'
import { notices, push, loadNotices, markRead, retryNotice, snoozeNotice } from './push.js'
import { ago } from '../format.js'

onMounted(() => { loadNotices().catch(() => {}) })

// status → 展示名（与桌面 NotificationsView 同口径）。
const STATUS = { pending: '处理中', delivered: '已送达', failed: '失败', silent: '静默', processing: '处理中' }
const CHANNEL = { web: '网页', console: '控制台', dingtalk: '钉钉', feishu: '飞书' }

const fmtWhen = n => {
  if (!n.createdAt) return ''
  const a = ago(n.createdAt)
  return a === '刚刚' ? a : a + '前'
}
</script>

<template>
  <div class="m-pane">
    <div class="m-pane-head">
      <span class="m-pane-title">通知</span>
      <span class="m-push-state" :class="push.status">
        <i></i>{{ push.status === 'on' ? '实时已连接' : push.status === 'connecting' ? '连接中…' : '未连接' }}
      </span>
    </div>

    <div class="m-list">
      <div v-if="!notices.length" class="m-empty">还没有通知</div>
      <div v-for="n in notices" :key="n.id" class="m-card m-notice" :class="{ unread: !n.readAt }">
        <div class="m-notice-head">
          <span v-if="!n.readAt" class="m-dot" aria-hidden="true"></span>
          <span class="m-notice-status" :class="'st-' + n.status">{{ STATUS[n.status] || n.status }}</span>
          <span class="m-dim">{{ fmtWhen(n) }}</span>
        </div>
        <div class="m-notice-text">{{ n.text }}</div>
        <div v-if="n.deliveredTo && n.deliveredTo.length" class="m-notice-chs">
          <span v-for="ch in n.deliveredTo" :key="ch" class="m-chip">{{ CHANNEL[ch] || ch }}</span>
        </div>
        <div v-if="n.lastError" class="m-notice-err">{{ n.lastError }}</div>
        <div class="m-notice-acts">
          <button v-if="!n.readAt" class="m-btn small" type="button" @click="markRead(n.id)">已读</button>
          <button v-if="n.status === 'failed'" class="m-btn small" type="button" @click="retryNotice(n.id)">重试</button>
          <button v-if="!n.readAt && n.status !== 'failed'" class="m-btn small" type="button" @click="snoozeNotice(n.id, 10)">延后 10 分钟</button>
        </div>
      </div>
    </div>
  </div>
</template>
