<script setup>
// 待处理卡片（confirm 模式审批 + agent 提问）：登录期间每 5s 轮询一次。
// 两种卡分开画：授权卡是「批准/拒绝/批准并记住」；问答卡（kind=question，agent
// 借权限通道问用户）的按钮就是它自己给的选项，点哪条把那条的 id 回给 agent——
// 压成批准/拒绝会把答案丢掉。
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

const isQuestion = it => it.kind === 'question'
// 问题正文里标了可多选，但 agent 给的仍是一条条独立选项（协议没有多选通道）：
// 明说一句，别让人以为点完还能补选。
const multiHint = it => (it.questions || []).some(g => g.multiSelect)

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

// answer 回答一张问答卡：把选中的 optionId 原样交给后端。
async function answer(it, optionID) {
  if (pending.has(it.id) || !store.loggedIn || disposed) return
  const current = generation
  pending.add(it.id)
  try {
    const r = await api('POST', '/api/approvals/' + encodeURIComponent(it.id), { choose: optionID })
    if (disposed || current !== generation || !store.loggedIn) return
    decided.add(it.id)
    const idx = items.findIndex(item => item.id === it.id)
    if (idx >= 0) items.splice(idx, 1)
    toast(r.message || '已回答')
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
    <div v-for="it in items" :key="it.id" class="approval-card" :class="{ 'is-question': isQuestion(it) }"
         :aria-busy="pending.has(it.id)">
      <h4>
        <svg v-if="isQuestion(it)" width="15" height="15" viewBox="0 0 24 24" fill="none"
             stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"
             aria-hidden="true">
          <circle cx="12" cy="12" r="9"/><path d="M9.6 9a2.5 2.5 0 1 1 3.4 2.3c-.7.3-1 .9-1 1.6v.4"/><path d="M12 17.2h.01"/>
        </svg>
        <template v-else>⚠️</template>
        {{ it.summary }}
      </h4>

      <template v-if="isQuestion(it)">
        <ol v-if="(it.questions || []).length" class="aq-list">
          <li v-for="(g, gi) in it.questions" :key="gi" class="aq-item">
            <p v-if="g.header" class="aq-head">{{ g.header }}</p>
            <p v-if="g.question" class="aq-q">{{ g.question }}</p>
            <ul v-if="(g.options || []).length" class="aq-opts">
              <li v-for="(o, oi) in g.options" :key="oi">
                <b>{{ o.label }}</b>
                <span v-if="o.description" class="meta"> —— {{ o.description }}</span>
              </li>
            </ul>
          </li>
        </ol>
        <pre v-else>{{ it.detail || '' }}</pre>
        <p v-if="multiHint(it)" class="aq-note meta">
          这题标了可多选，但 agent 给的是一条条独立选项——点一项就是答案。
        </p>
        <div class="acts aq-acts">
          <!-- 选项一律中性底色：这里没有"默认动作"，染蓝等于替用户推荐一条答案 -->
          <button v-for="o in it.options || []" :key="o.id" type="button" class="btn sm"
                  :disabled="pending.has(it.id)" @click="answer(it, o.id)">
            {{ pending.has(it.id) ? '处理中…' : o.name }}
          </button>
        </div>
      </template>
      <template v-else>
        <pre>{{ it.detail || '' }}</pre>
        <div class="acts">
          <button class="btn sm" :disabled="pending.has(it.id)" @click="decide(it, false, false)">拒绝</button>
          <button class="btn sm" :disabled="pending.has(it.id)" @click="decide(it, true, true)">批准并记住</button>
          <button class="btn sm primary" :disabled="pending.has(it.id)" @click="decide(it, true, false)">{{ pending.has(it.id) ? '处理中…' : '批准' }}</button>
        </div>
      </template>
    </div>
  </div>
</template>
