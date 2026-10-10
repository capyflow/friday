<script setup>
// 危险操作审批卡片（confirm 模式）：登录期间每 5s 轮询一次。
import { onMounted, onUnmounted, reactive, watch } from 'vue'
import { api } from '../api.js'
import { store, toast } from '../store.js'

const items = reactive([])
const decided = new Set()   // 本会话已处理过的审批 id：服务端列表尚未移除前不再显示
const pending = reactive(new Set())
let timer = null
let generation = 0
let polling = false
let disposed = false

// 登录切换后，旧请求即使晚到也不能更新新会话的卡片或提示。
watch(() => [store.loggedIn, store.username], () => {
  generation++
  items.splice(0, items.length)
  decided.clear()
  pending.clear()
  polling = false
  if (store.loggedIn) poll()
}, { flush: 'sync' })

async function poll() {
  if (!store.loggedIn || disposed || polling) return
  const current = generation
  polling = true
  try {
    const r = await api('GET', '/api/approvals')
    if (disposed || current !== generation || !store.loggedIn) return
    const list = r.approvals || []
    items.splice(0, items.length, ...list.filter(it => !decided.has(it.id)))
  } catch { /* 静默：审批接口未启用等情况 */ }
  finally { if (current === generation) polling = false }
}

async function decide(it, approve, remember) {
  if (pending.has(it.id) || !store.loggedIn || disposed) return
  const current = generation
  pending.add(it.id)
  try {
    const r = await api('POST', '/api/approvals/' + encodeURIComponent(it.id), { approve, remember })
    if (disposed || current !== generation || !store.loggedIn) return
    decided.add(it.id)
    const idx = items.findIndex(item => item.id === it.id)
    if (idx >= 0) items.splice(idx, 1)
    toast(r.message || (approve ? '已批准' : '已拒绝'))
  } catch (e) {
    if (!disposed && current === generation && store.loggedIn) toast(e.message, true)
  } finally {
    if (current === generation) pending.delete(it.id)
  }
}

onMounted(() => { poll(); timer = setInterval(poll, 5000) })
onUnmounted(() => { disposed = true; generation++; clearInterval(timer) })
</script>

<template>
  <div v-if="store.loggedIn" id="approvalBar" aria-live="polite">
    <div v-for="it in items" :key="it.id" class="approval-card" :aria-busy="pending.has(it.id)">
      <h4>⚠️ {{ it.summary }}</h4>
      <pre>{{ it.detail || '' }}</pre>
      <div class="acts">
        <button class="btn sm" :disabled="pending.has(it.id)" @click="decide(it, false, false)">拒绝</button>
        <button class="btn sm" :disabled="pending.has(it.id)" @click="decide(it, true, true)">批准并记住</button>
        <button class="btn sm primary" :disabled="pending.has(it.id)" @click="decide(it, true, false)">{{ pending.has(it.id) ? '处理中…' : '批准' }}</button>
      </div>
    </div>
  </div>
</template>
