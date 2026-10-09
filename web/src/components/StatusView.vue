<script setup>
// 运行状态：服务状态卡片 + Token 用量账本（按天）。
import { computed, nextTick, onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { fmtNum, upStr } from '../format.js'
import { toast } from '../store.js'
import { runCountUp } from '../anim.js'

const status = ref(null)
const usage = reactive({ days: [], total: {}, err: '' })

const cards = computed(() => {
  const st = status.value
  if (!st) return []
  return [
    ['LLM 模型', st.provider.model + (st.apiKeyConfigured ? '' : '（未配密钥）'), 'is-orange'],
    ['待触发任务', st.tasks.total, ''],
    ['待办未完成', st.todos ? st.todos.pending : 0, 'is-mint'],
    ['记忆页面', st.memories, 'is-sky'],
    ['已运行', upStr(st.uptimeSeconds), ''],
    ['服务地址', location.host, ''],
  ]
})

const todayCards = computed(() => {
  const localToday = new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 10)
  const todayRow = usage.days.find(d => d.date === localToday) || { calls: 0, in: 0, out: 0 }
  const total = usage.total
  return [
    ['今日调用', todayRow.calls, ''],
    ['今日 IN', fmtNum(todayRow.in), ''],
    ['今日 OUT', fmtNum(todayRow.out), ''],
    ['累计 IN', fmtNum(total.in || 0), ''],
    ['累计 OUT', fmtNum(total.out || 0), ''],
    ['累计合计', fmtNum((total.in || 0) + (total.out || 0)), 'is-yellow'],
  ]
})

onMounted(async () => {
  try {
    status.value = await api('GET', '/api/status')
  } catch (e) { toast('状态加载失败：' + e.message, true); return }
  try {
    const u = await api('GET', '/api/usage')
    usage.days = u.days || []
    usage.total = u.total || {}
  } catch (e) { usage.err = e.message }
  nextTick(() => runCountUp(document.getElementById('view-status')))
})
</script>

<template>
  <section class="view" id="view-status">
    <div class="view-inner">
      <h2>运行状态 <span class="count"><span class="dot ok"></span>在线</span></h2>
      <div class="cards" v-if="status">
        <div v-for="([k, v, cls]) in cards" :key="k" class="card" :class="cls">
          <div class="k">{{ k }}</div>
          <div class="v" v-if="typeof v === 'number'" :data-count="v">0</div>
          <div class="v" v-else style="font-size:15px">{{ v }}</div>
        </div>
      </div>

      <h2 class="mt22">Token 用量 <span class="count">按天累计（本地时区）</span></h2>
      <div class="cards">
        <div v-for="([k, v, cls]) in todayCards" :key="k" class="card" :class="cls">
          <div class="k">{{ k }}</div>
          <div class="v" v-if="typeof v === 'number'" :data-count="v">0</div>
          <div class="v" v-else style="font-size:15px">{{ v }}</div>
        </div>
      </div>
      <div class="panel" style="margin-top:14px" v-if="usage.days.length">
        <table>
          <thead><tr><th>日期</th><th>调用次数</th><th>IN</th><th>OUT</th><th>合计</th></tr></thead>
          <tbody>
            <tr v-for="d in usage.days.slice(0, 30)" :key="d.date">
              <td>{{ d.date }}</td>
              <td>{{ fmtNum(d.calls) }}</td>
              <td class="mono">{{ fmtNum(d.in) }}</td>
              <td class="mono">{{ fmtNum(d.out) }}</td>
              <td class="mono">{{ fmtNum(d.in + d.out) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-else-if="usage.err" class="empty">用量加载失败：{{ usage.err }}</div>
      <div v-else class="empty">还没有 token 记录，和小五聊一句就有了</div>
    </div>
  </section>
</template>
