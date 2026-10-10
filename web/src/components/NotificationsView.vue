<script setup>
// 通知中心保存提醒正文与投递结果；远程投递失败后可在此重试。
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { fmtDT } from '../format.js'
import { toast } from '../store.js'
import { store } from '../store.js'
import { notificationSettings, enableSystemNotifications } from '../notifications.js'

const notifications = ref([])
const retrying = reactive(new Set())
const loading = ref(false)
const STATUS = {
  pending: ['待处理', 'warn'], processing: ['处理中', 'blue'], delivered: ['已送达', 'teal'],
  failed: ['失败', 'warn'], silent: ['静默', ''],
}
const CHANNELS = { web: '通知中心', console: '本地输出', dingtalk: '钉钉', feishu: '飞书' }
let timer = null
let disposed = false

async function load() {
  if (loading.value || disposed) return
  loading.value = true
  try {
    const result = await api('GET', '/api/notifications')
    if (disposed) return
    notifications.value = (result.notifications || []).slice().sort((a, b) =>
      new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime())
    store.unread = result.unread || 0
  } catch (e) { if (!disposed) toast('通知加载失败：' + e.message, true) }
  finally { loading.value = false }
}

async function markRead(item) {
  if (retrying.has(item.id)) return
  retrying.add(item.id)
  try { await api('POST', '/api/notifications/' + encodeURIComponent(item.id) + '/read', {}); if (!disposed) await load() }
  catch (e) { if (!disposed) toast(e.message, true) }
  finally { retrying.delete(item.id) }
}

async function snooze(item) {
  if (retrying.has(item.id)) return
  retrying.add(item.id)
  try {
    const r = await api('POST', '/api/notifications/' + encodeURIComponent(item.id) + '/snooze', { minutes:10 })
    if (disposed) return
    toast(r.message || '已安排10分钟后提醒'); await load()
  } catch (e) { if (!disposed) toast(e.message,true) }
  finally { retrying.delete(item.id) }
}

async function retry(item) {
  if (retrying.has(item.id) || disposed) return
  retrying.add(item.id)
  try {
    const result = await api('POST', '/api/notifications/' + encodeURIComponent(item.id) + '/retry', {})
    if (disposed) return
    toast(result.message || '已提交重试')
    await load()
  } catch (e) { if (!disposed) toast('重试失败：' + e.message, true) }
  finally { retrying.delete(item.id) }
}

onMounted(() => { load(); timer = setInterval(load, 5000) })
onUnmounted(() => { disposed = true; clearInterval(timer) })
</script>

<template>
  <section class="view" id="view-notifications">
    <div class="view-inner">
      <h2>通知 <span class="count">{{ notifications.length ? notifications.length + ' 条' : '' }}</span></h2>
      <div class="panel" :aria-busy="loading">
        <div class="row" style="margin-bottom:16px">
          <button class="btn" :disabled="notificationSettings.busy" @click="enableSystemNotifications">开启系统通知</button>
          <span class="meta" role="status">{{ notificationSettings.status }} · 提醒同时保留在此页面</span>
        </div>
        <div class="panel-title">提醒与投递记录</div>
        <div v-if="notifications.length" style="overflow-x:auto">
        <table>
          <thead><tr><th>内容</th><th>状态</th><th>送达位置</th><th>记录于</th><th>重试</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="item in notifications" :key="item.id">
              <td style="white-space:pre-wrap;overflow-wrap:anywhere">
                {{ item.text }}
                <span v-if="item.prepared && item.push && !item.readAt" class="badge warn">未读</span>
                <div v-if="item.parentId" class="meta">稍后提醒 · 来自此前通知</div>
                <div v-if="item.lastError" class="meta">{{ item.lastError }}</div>
                <div v-if="item.attempts" class="meta">已尝试 {{ item.attempts }} 次</div>
              </td>
              <td><span class="badge" :class="(STATUS[item.status] || [])[1]">{{ (STATUS[item.status] || ['未知'])[0] }}</span></td>
              <td class="meta">{{ (item.deliveredTo || []).map(channel => CHANNELS[channel] || channel).join('、') || '—' }}</td>
              <td class="meta">{{ fmtDT(item.createdAt) }}</td>
              <td><button v-if="item.status === 'failed'" class="btn sm" :disabled="retrying.has(item.id)"
                          @click="retry(item)">{{ retrying.has(item.id) ? '重试中…' : '重试' }}</button></td>
              <td v-if="item.prepared && item.push">
                <button v-if="!item.readAt" class="btn sm" :disabled="retrying.has(item.id)" @click="markRead(item)">标为已读</button>
                <span v-if="item.task?.todoId" class="meta">提醒时间请在待办页调整</span>
                <button v-else-if="!item.snoozeTaskId" class="btn sm" :disabled="retrying.has(item.id)" @click="snooze(item)">10分钟后提醒</button>
                <span v-else class="meta">已安排稍后提醒</span>
              </td>
            </tr>
          </tbody>
        </table>
        </div>
        <div v-else class="empty">{{ loading ? '正在加载通知…' : '当前没有通知，提醒触发后会在这里保留记录' }}</div>
      </div>
    </div>
  </section>
</template>
