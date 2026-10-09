<script setup>
// 危险操作审批卡片（confirm 模式）：登录期间每 5s 轮询一次。
import { onMounted, onUnmounted, reactive } from 'vue'
import { api } from '../api.js'
import { store, toast } from '../store.js'

const items = reactive([])
const decided = new Set()   // 本会话已处理过的审批 id：服务端列表尚未移除前不再显示
let timer = null

async function poll() {
  if (!store.loggedIn) return
  try {
    const r = await api('GET', '/api/approvals')
    const list = r.approvals || []
    items.splice(0, items.length, ...list.filter(it => !decided.has(it.id)))
  } catch (e) { /* 静默：审批接口未启用等情况 */ }
}

async function decide(it, approve, remember) {
  decided.add(it.id)
  const idx = items.indexOf(it)
  if (idx >= 0) items.splice(idx, 1)
  try {
    const r = await api('POST', '/api/approvals/' + it.id, { approve, remember })
    toast(r.message || (approve ? '已批准' : '已拒绝'))
  } catch (e) { toast(e.message, true) }
}

onMounted(() => { poll(); timer = setInterval(poll, 5000) })
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <div id="approvalBar" aria-live="polite">
    <div v-for="it in items" :key="it.id" class="approval-card">
      <h4>⚠️ {{ it.summary }}</h4>
      <pre>{{ it.detail || '' }}</pre>
      <div class="acts">
        <button class="btn sm" @click="decide(it, false, false)">拒绝</button>
        <button class="btn sm" @click="decide(it, true, true)">批准并记住</button>
        <button class="btn sm primary" @click="decide(it, true, false)">批准</button>
      </div>
    </div>
  </div>
</template>
