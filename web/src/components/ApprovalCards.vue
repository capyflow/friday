<script setup>
// 待处理卡片（confirm 模式审批 + agent 提问）：登录期间每 5s 轮询一次。
// 两种卡分开画：授权卡是「批准/拒绝/批准并记住」；问答卡（kind=question，agent
// 借权限通道问用户）的按钮就是它自己给的选项，点哪条把那条的 id 回给 agent——
// 压成批准/拒绝会把答案丢掉。
// 归属：只有"当前正在看的会话"的卡才铺在页面上；其他会话的卡收进右上角提示
// （几条、哪条会话），点一下跳过去再处理——不再在任何页面凭空弹出来。
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { api } from '../api.js'
import { store, toast, setMode } from '../store.js'

const items = reactive([])
const texts = reactive({})      // 问答卡的自由文本答案（按卡片 id 存）
const selections = reactive({}) // 多选问答卡勾选中的 optionId（按卡片 id 存）
const decided = new Set()   // 本会话已处理过的审批 id：服务端列表尚未移除前不再显示
const pending = reactive(new Set())
let timer = null
let generation = 0
let polling = false
let disposed = false

const isQuestion = it => it.kind === 'question'
// 卡面声明了可多选（interaction 通道的卡，一次能提交多条勾选）。
const isMulti = it => isQuestion(it) && !!it.multiSelect
// 问题正文里标了多选但卡是单选（权限通道协议一次只认一个 optionId）：
// 明说一句，别让人以为点完还能补选。
const multiHint = it => !isMulti(it) && (it.questions || []).some(g => g.multiSelect)
const isSelected = (it, oid) => (selections[it.id] || []).includes(oid)

/* ---------- 卡片归属：当前对话就地展开，其他会话收进右上角提示 ---------- */
// source 由后端带上：acp = 工作区里某条会话（threadId 指路），空/chat = 主对话。
// 只有"正在看的那条会话"的卡才铺在页面上；否则用户分不清是哪个窗口在申请。
const isAcp = it => it.source === 'acp'
function inCurrentChat(it) {
  if (!isAcp(it)) return store.view === 'chat'
  return store.view === 'agent' && !!it.threadId && store.acpCurrentThread === it.threadId
}
const inlineItems = computed(() => items.filter(inCurrentChat))
const otherItems = computed(() => items.filter(it => !inCurrentChat(it)))
const hintOpen = ref(false)

function targetLabel(it) {
  if (!isAcp(it)) return '聊天 · 小五'
  return (it.threadTitle || '').trim() || it.agent || '智能体会话'
}

// jumpTo 跳到这条卡所属的会话：到了那边卡片会自己铺开，再决定批/答。
function jumpTo(it) {
  hintOpen.value = false
  if (!isAcp(it)) {
    setMode('life')
    store.view = 'chat'
    return
  }
  setMode('work')
  if (it.threadId) store.acpOpenThread = it.threadId
  store.view = 'agent'
}

function onDocClick(e) {
  if (hintOpen.value && !e.target?.closest?.('#approvalHint')) hintOpen.value = false
}

// 卡全处理完就收起提示层，别留一个空面板。
watch(() => otherItems.value.length, n => { if (!n) hintOpen.value = false })

function toggleOption(it, oid) {
  if (pending.has(it.id) || !store.loggedIn || disposed) return
  const picked = selections[it.id] || []
  const idx = picked.indexOf(oid)
  if (idx >= 0) picked.splice(idx, 1)
  else picked.push(oid)
  selections[it.id] = picked
}

// 登录切换后，旧请求即使晚到也不能更新新会话的卡片或提示。
watch(() => [store.loggedIn, store.username], () => {
  generation++
  items.splice(0, items.length)
  decided.clear()
  pending.clear()
  for (const k of Object.keys(texts)) delete texts[k]
  for (const k of Object.keys(selections)) delete selections[k]
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
    delete texts[it.id]
    const idx = items.findIndex(item => item.id === it.id)
    if (idx >= 0) items.splice(idx, 1)
    toast(r.message || '已回答')
  } catch (e) {
    if (!disposed && current === generation && store.loggedIn) toast(e.message, true)
  } finally {
    if (current === generation) pending.delete(it.id)
  }
}

// answerMulti 把多选卡的勾选一次提交给后端（choices 是 optionId 数组）。
async function answerMulti(it) {
  const choices = (selections[it.id] || []).slice()
  if (!choices.length || pending.has(it.id) || !store.loggedIn || disposed) return
  const current = generation
  pending.add(it.id)
  try {
    const r = await api('POST', '/api/approvals/' + encodeURIComponent(it.id), { choices })
    if (disposed || current !== generation || !store.loggedIn) return
    decided.add(it.id)
    delete texts[it.id]
    delete selections[it.id]
    const idx = items.findIndex(item => item.id === it.id)
    if (idx >= 0) items.splice(idx, 1)
    toast(r.message || '已回答')
  } catch (e) {
    if (!disposed && current === generation && store.loggedIn) toast(e.message, true)
  } finally {
    if (current === generation) pending.delete(it.id)
  }
}

// answerText 用自由文本回答（CodeBuddy 的自由输入、MiniMax 的 Other 字段）。
async function answerText(it) {
  const text = (texts[it.id] || '').trim()
  if (!text || pending.has(it.id) || !store.loggedIn || disposed) return
  const current = generation
  pending.add(it.id)
  try {
    const r = await api('POST', '/api/approvals/' + encodeURIComponent(it.id), { text })
    if (disposed || current !== generation || !store.loggedIn) return
    decided.add(it.id)
    delete texts[it.id]
    const idx = items.findIndex(item => item.id === it.id)
    if (idx >= 0) items.splice(idx, 1)
    toast(r.message || '已回答')
  } catch (e) {
    if (!disposed && current === generation && store.loggedIn) toast(e.message, true)
  } finally {
    if (current === generation) pending.delete(it.id)
  }
}

onMounted(() => {
  document.addEventListener('click', onDocClick)
  poll()
  timer = setInterval(poll, 5000)
})
onUnmounted(() => {
  disposed = true
  generation++
  clearInterval(timer)
  document.removeEventListener('click', onDocClick)
})
</script>

<template>
  <template v-if="store.loggedIn">
    <div v-if="inlineItems.length" id="approvalBar" aria-live="polite">
      <div v-for="it in inlineItems" :key="it.id" class="approval-card" :class="{ 'is-question': isQuestion(it) }"
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
        <div v-if="(it.options || []).length" class="acts aq-acts">
          <!-- 选项一律中性底色：这里没有"默认动作"，染蓝等于替用户推荐一条答案；
               多选卡的选中态例外——那是用户自己勾出来的，不是我们推荐的 -->
          <template v-if="isMulti(it)">
            <button v-for="o in it.options || []" :key="o.id" type="button" class="btn sm"
                    :class="{ 'is-on': isSelected(it, o.id) }"
                    :disabled="pending.has(it.id)" @click="toggleOption(it, o.id)">
              {{ o.name }}
            </button>
            <button class="btn sm primary aq-multi-submit" type="button"
                    :disabled="pending.has(it.id) || !(selections[it.id] || []).length"
                    @click="answerMulti(it)">
              {{ pending.has(it.id) ? '处理中…' : `提交所选（${(selections[it.id] || []).length}）` }}
            </button>
          </template>
          <template v-else>
            <button v-for="o in it.options || []" :key="o.id" type="button" class="btn sm"
                    :disabled="pending.has(it.id)" @click="answer(it, o.id)">
              {{ pending.has(it.id) ? '处理中…' : o.name }}
            </button>
          </template>
        </div>
        <div v-if="it.allowText" class="aq-text">
          <textarea v-model="texts[it.id]" rows="2" maxlength="4000" aria-label="自由文本答案"
                    :placeholder="it.textPlaceholder || '也可以直接输入你的答案…'"
                    :disabled="pending.has(it.id)"></textarea>
          <button class="btn sm" type="button"
                  :disabled="pending.has(it.id) || !(texts[it.id] || '').trim()"
                  @click="answerText(it)">
            {{ pending.has(it.id) ? '处理中…' : '提交答案' }}
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

    <!-- 右上角提示：不属于当前会话的待处理卡。点一条跳到对应会话再处理——
         卡片不会凭空换页面弹出来，用户始终知道是"哪条会话在等自己"。 -->
    <div v-if="otherItems.length" id="approvalHint" :class="{ open: hintOpen }">
      <button type="button" class="ah-btn" :aria-expanded="hintOpen" aria-haspopup="dialog"
              :aria-label="otherItems.length + ' 个待处理审批或提问，点击查看'"
              @click.stop="hintOpen = !hintOpen">
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor"
             stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <use href="#i-bell"/>
        </svg>
        <span>{{ otherItems.length }} 个待处理</span>
      </button>
      <div v-if="hintOpen" class="ah-pop" role="dialog" aria-label="其他会话的待处理请求">
        <p class="ah-tip">这些请求来自其他会话，点一条跳过去处理：</p>
        <button v-for="it in otherItems" :key="it.id" type="button" class="ah-item" @click="jumpTo(it)">
          <span class="ah-where">{{ targetLabel(it) }}</span>
          <span class="ah-kind" :class="{ q: isQuestion(it) }">{{ isQuestion(it) ? '提问' : '审批' }}</span>
          <span class="ah-sum">{{ it.summary }}</span>
        </button>
      </div>
    </div>
  </template>
</template>
