<script setup>
// 移动端对话视图：主会话（/api/chat）与智能体线程（/api/acp/threads/:id/chat）
// 共用这一个组件，靠 kind 区分——事件协议、块状态机（blocks.js）与时间线渲染
// （TimelineBlocks）都是桌面端原样复用，只换一层数字端版式。
//
// 事件驱动（2026-11-11 起）：不再自己那条 SSE 连接消费帧，而是订阅
// /api/events/stream 统一收事件——与桌面端走同一条渲染路径，所以移动端问完
// 桌面端那边也能实时看见，桌面端问完这里也一样。
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import { api, authFailed } from '../api.js'
import { authHeaders, mediaURL } from '../auth.js'
import { toast } from '../store.js'
import { turnTotalText } from '../blocks.js'
import { feedRate } from '../speed.js'
import { subscribe, clientId, deviceLabel } from '../events.js'
import { applyEvent } from '../turnevents.js'
import TimelineBlocks from '../components/TimelineBlocks.vue'

const props = defineProps({
  kind: { type: String, required: true }, // 'main'（生活模式）| 'thread'（智能体）
  thread: { type: Object, default: null }, // kind=thread 时的会话对象（至少有 id）
})

const emit = defineEmits(['preview'])

const messages = ref([])
const input = ref('')
const streaming = ref(false)
const typing = ref('')
const attachments = ref([]) // 已上传图片的 id（/api/upload 返回的文件名）
const paused = ref(null) // 主会话暂停现场 { question, partial }
const scroller = ref(null)
const fileInput = ref(null)

let controller = null
let loadGen = 0
let unsub = null            // 事件订阅句柄
let myTurnId = ''           // 本端最近一次提交的 turnId：停止时按它精确操作
const device = deviceLabel() // 设备标签：另一端看到消息时标「来自手机端」

const canSend = () => !streaming.value && (input.value.trim() || attachments.value.length)

function scrollBottom() {
  nextTick(() => {
    const el = scroller.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

function histBlocks(m) {
  // 历史里 bot 消息自带 blocks（think/tool/text）；空 blocks 退回整段文本一块。
  if (Array.isArray(m.blocks) && m.blocks.length) return m.blocks
  return (m.text || '') ? [{ kind: 'text', text: m.text, round: 0 }] : []
}

async function load() {
  const gen = ++loadGen
  try {
    if (props.kind === 'main') {
      const r = await api('GET', '/api/chat/history?session=web')
      if (gen !== loadGen) return
      messages.value = (r.messages || []).map(m => ({
        role: m.role === 'user' ? 'user' : 'bot',
        text: m.text || '', blocks: histBlocks(m), turn: m.turn || null,
        streaming: false, paused: false, round: 0,
      }))
    } else {
      const r = await api('GET', '/api/acp/threads/' + props.thread.id)
      if (gen !== loadGen) return
      const th = r.thread || {}
      messages.value = (th.messages || []).map(e => {
        if (e.role === 'note') return { role: 'note', text: e.text || '' }
        if (e.role === 'user') {
          return {
            role: 'user', text: e.text || '',
            images: (e.files || []).map(f => '/api/upload/' + f),
            imgCount: (e.files || []).length,
          }
        }
        return { role: 'bot', text: e.text || '', blocks: histBlocks({ text: e.text }), turn: e.in != null ? { in: e.in, out: e.out || 0, ms: e.ms } : null, streaming: false, paused: false, round: 0 }
      })
    }
  } catch (e) { /* 拉取失败按空会话处理，输入框还在 */ }
  scrollBottom()
}

onMounted(() => {
  load()
  startEvents()
})
watch(() => props.kind === 'thread' && props.thread && props.thread.id, () => {
  load()
  startEvents()
})

/* ---------- 事件订阅（双端同步的核心） ---------- */
// 会话 key：主会话固定 web；智能体线程用 acp:<threadID>——与后端一致。
function sessionKey() {
  return props.kind === 'main' ? 'web' : 'acp:' + (props.thread && props.thread.id)
}

// onEvent 把事件交给共享翻译层：移动端与桌面端同一条渲染路径。
function onEvent(ev) {
  applyEvent(ev, {
    messages: messages.value,
    mine: clientId,
    makeBot: () => reactive({ role: 'bot', text: '', streaming: true, paused: false, blocks: [], turn: null, round: 0 }),
    makeUser: (text, images) => reactive({ role: 'user', text, images: images || [] }),
    onDelta: (bot, delta) => {
      if (bot) bot.rate = feedRate(bot, delta)
      if (typing.value) typing.value = ''
      scrollBottom()
    },
    onThinking: () => scrollBottom(),
    onStep: () => scrollBottom(),
    onPaused: info => {
      streaming.value = false
      typing.value = ''
      let q = ''
      for (let i = messages.value.length - 1; i >= 0; i--) {
        if (messages.value[i].role === 'user') { q = messages.value[i].text; break }
      }
      paused.value = { question: q, partial: info.partial || '' }
      scrollBottom()
    },
    onEnd: () => {
      streaming.value = false
      typing.value = ''
      scrollBottom()
    },
    onError: (text, bot) => {
      streaming.value = false
      typing.value = ''
      if (bot) {
        bot.streaming = false
        if (!bot.text) bot.text = text
        else bot.text += String.fromCharCode(10, 10) + text
      } else {
        messages.value.push({ role: 'err', text })
      }
      scrollBottom()
    },
    onStatus: text => { typing.value = text || '' },
    onRemoteUser: () => scrollBottom(),
  })
}

// onResync：缺口过大时全量重拉历史（宁可重拉，也不假装消息连续）。
function onResync() {
  load()
}

// startEvents 订阅当前会话的事件流。切会话时先停旧的再订新的。
function startEvents() {
  if (unsub) { unsub.stop(); unsub = null }
  const key = sessionKey()
  if (!key || key === 'acp:') return
  unsub = subscribe(key, 0, { onEvent, onResync, onState: () => {} })
}

/* ---------- 图片附件：与桌面端同一条上传链路（png/jpeg/webp，单张 ≤5MB） ---------- */
function pickImage() { fileInput.value && fileInput.value.click() }
function onPicked(ev) {
  const files = Array.from(ev.target.files || [])
  ev.target.value = ''
  files.forEach(f => uploadImage(f))
}
async function uploadImage(file) {
  const fd = new FormData()
  fd.append('file', file)
  try {
    const r = await api('POST', '/api/upload', fd)
    attachments.value.push(r.id)
  } catch (e) {
    toast('图片上传失败：' + e.message, true)
  }
}
const attachURL = id => '/api/upload/' + id

/* ---------- 发送 / 停止 / 继续（SSE 帧协议与桌面端完全一致） ---------- */
function chatURL() {
  return props.kind === 'main' ? '/api/chat' : `/api/acp/threads/${props.thread.id}/chat`
}

async function send() {
  const text = input.value.trim()
  if (!canSend()) return
  if (paused.value) return resume()
  capSheet.value = ''
  const atts = [...attachments.value]
  messages.value.push({
    role: 'user', text,
    images: atts.map(attachURL),
    imgCount: atts.length,
  })
  input.value = ''
  attachments.value = []
  scrollBottom()
  await stream({ message: text, attachments: atts }, null)
}

async function resume() {
  if (!paused.value) return
  paused.value = null
  // 续跑接在被暂停的那条回答上，不另起新块——partial 已经在屏上了。
  const bot = messages.value.find(m => m.paused)
  if (bot) { bot.paused = false; bot.streaming = true }
  await stream({ session: 'web' }, bot, '/api/chat/resume')
}

async function stop() {
  try {
    if (props.kind === 'main') await api('POST', '/api/chat/stop', { session: 'web', turnId: myTurnId })
    else await api('POST', `/api/acp/threads/${props.thread.id}/stop`, {})
  } catch (e) { toast('停止失败：' + e.message, true) }
}

// stream 提交一轮对话。
//
// 改造前这里自己建 SSE 连接、直接消费帧，发起端独占——桌面端因此看不到
// 手机端问的东西。改造后 POST 只负责提交（立刻返回 turnId），回复内容统一
// 从 /api/events/stream 的事件流里来（与桌面端同一条路径）。
async function stream(body, reuseBot, urlOverride) {
  controller = new AbortController()
  streaming.value = true
  typing.value = ''
  // 继续对话时复用上一轮那条被暂停的 bot 气泡；新的一轮由事件流创建。
  let bot = reuseBot || null
  if (bot) { bot.paused = false; bot.streaming = true }
  try {
    const res = await fetch(urlOverride || chatURL(), {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ ...body, clientId, device }),
      signal: controller.signal,
    })
    if (res.status === 403) { authFailed(); return }
    let data = null
    try { data = await res.json() } catch { /* 非 JSON 响应 */ }
    if (!res.ok) {
      let msg = 'HTTP ' + res.status
      if (data && data.body && data.body.error) msg = data.body.error
      throw new Error(msg)
    }
    const r = (data && data.body) || {}
    myTurnId = r.turnId || ''
    if (r.status === 'queued') {
      // 会话忙（另一端正在生成）：这一轮进了 FIFO 队列。
      streaming.value = false
      typing.value = ''
      const last = messages.value[messages.value.length - 1]
      if (last && last.role === 'user') {
        last.queued = true
        last.position = r.position || 1
      }
      toast('小五正在回答（来自另一端），你的问题排在第 ' + (r.position || 1) + ' 位')
    }
  } catch (e) {
    if (controller.signal.aborted) return
    streaming.value = false
    typing.value = ''
    messages.value.push({ role: 'err', text: e.message })
    scrollBottom()
  } finally {
    if (!controller.signal.aborted) {
      // 事件流负责把这一轮收尾；这里只保证输入框不被锁死。
      if (streaming.value && !myTurnId) streaming.value = false
    }
  }
}


/* ---------- 会话配置：模型 / 思考强度 / 审批档位 ---------- */
// 与桌面端同一份能力清单（GET /api/acp/threads/:id/options）：agent 报什么显示什么，
// 不支持的能力整条缺席。手机端没有 hover/右键，chips 排在输入框上方，点开底部弹层选。
// 回答途中不给切：服务端按会话串行，options 请求会一直卡到轮末。
const caps = ref(null)
const capsLoading = ref(false)
const capBusy = ref(false)
const capSheet = ref('') // 展开的能力 id（'' = 收起）
const CAP_LABEL = { model: '模型', thought_level: '思考强度', approval_mode: '审批模式' }
const capLabel = cap => CAP_LABEL[cap.kind] || cap.name
const capCurrentName = cap =>
  ((cap.values || []).find(v => v.value === cap.currentValue) || {}).name || cap.currentValue
const openCap = computed(() => (caps.value || []).find(c => c.id === capSheet.value) || null)

async function loadCaps() {
  if (props.kind !== 'thread' || !props.thread?.id || capsLoading.value) return
  capsLoading.value = true
  try {
    const r = await api('GET', `/api/acp/threads/${props.thread.id}/options`)
    caps.value = r.capabilities || []
  } catch (e) {
    // 拉不到就整行不显示，不拿这条错误打断对话
  } finally { capsLoading.value = false }
}
function openCapSheet(cap) {
  if (streaming.value || capBusy.value) return
  capSheet.value = capSheet.value === cap.id ? '' : cap.id
}
async function setCap(cap, value) {
  if (capBusy.value || streaming.value) return
  if (value === cap.currentValue) { capSheet.value = ''; return }
  capBusy.value = true
  try {
    const r = await api('POST', `/api/acp/threads/${props.thread.id}/options`, { id: cap.id, value })
    caps.value = r.capabilities || []
    const fresh = (caps.value || []).find(c => c.id === cap.id)
    toast(capLabel(cap) + '已切换为 ' + (fresh ? capCurrentName(fresh) : value))
    capSheet.value = ''
  } catch (e) {
    toast('切换失败：' + e.message, true)
  } finally { capBusy.value = false }
}

// 选中/切换会话即拉清单；轮末再拉一次，agent 自己改过的默认值也能跟上。
watch(() => (props.kind === 'thread' && props.thread ? props.thread.id : ''), id => {
  caps.value = null
  capSheet.value = ''
  if (id) loadCaps()
}, { immediate: true })

// 线程会话的 agent 名，给输入框占位符一点上下文。
const agentName = () => (props.kind === 'thread' && props.thread ? props.thread.agent : '')

defineExpose({ load })
</script>

<template>
  <div class="m-chat">
    <div ref="scroller" class="m-chat-list" @click="e => { const c = e.target.closest?.('.file-chip'); if (c?.dataset.path) emit('preview', c.dataset.path) }">
      <template v-for="(m, i) in messages" :key="i">
        <div v-if="m.role === 'note'" class="ag-note">{{ m.text }}</div>
        <div v-else class="mrow" :class="m.role">
          <div v-if="m.role === 'bot'" class="turn">
            <TimelineBlocks :blocks="m.blocks" :streaming="m.streaming" :paused="m.paused" :round="m.round"
                            :rate="m.rate || 0" @preview="path => emit('preview', path)" />
            <div v-if="m.turn || (!m.streaming && m.blocks.some(b => b.kind === 'text' && b.text))" class="turn-foot">
              <div v-if="m.turn" class="turn-total">
                {{ turnTotalText(m.turn) }}<template v-if="m.turn.stopped && m.turn.stopped !== 'end_turn'"> · {{ m.turn.stopped }}</template>
              </div>
            </div>
            <button v-if="m.paused" class="m-btn small" type="button" @click="resume">继续回答</button>
          </div>
          <div v-else class="msg-col">
            <div v-if="m.images && m.images.length" class="msg-imgs">
              <a v-for="(src, j) in m.images" :key="j" class="msg-img" :href="mediaURL(src)" target="_blank" rel="noopener">
                <img :src="mediaURL(src)" alt="" loading="lazy">
              </a>
            </div>
            <div v-if="m.text || (m.imgCount && !(m.images && m.images.length))" class="bubble">
              {{ m.text }}
              <span v-if="m.imgCount && !(m.images && m.images.length)" class="meta"> 📷 ×{{ m.imgCount }}</span>
            </div>
          </div>
        </div>
      </template>
      <div v-if="streaming" class="mrow bot">
        <div class="bubble">
          <span v-if="typing" class="ag-typing-label">{{ typing }}</span>
          <span class="typing"><i></i><i></i><i></i></span>
        </div>
      </div>
      <div class="m-chat-pad"></div>
    </div>

    <div class="m-composer">
      <div v-if="paused" class="m-composer-hint">上一条回答已暂停，部分内容已保留</div>
      <!-- 会话配置：模型 / 思考强度 / 审批档位。主会话没有会话级配置，整行不出现。 -->
      <div v-if="kind === 'thread' && (capsLoading || (caps && caps.length))" class="m-cap-row">
        <span v-if="capsLoading && !caps" class="m-cap-chip is-loading">配置加载中…</span>
        <button v-for="cap in (caps || [])" :key="cap.id" type="button" class="m-cap-chip"
                :class="{ open: capSheet === cap.id }" :disabled="streaming || capBusy"
                :aria-label="capLabel(cap) + '：' + capCurrentName(cap)"
                @click="openCapSheet(cap)">
          <span class="m-cap-k">{{ capLabel(cap) }}</span>
          <span class="m-cap-v">{{ capCurrentName(cap) }}</span>
        </button>
      </div>
      <div v-if="attachments.length" class="m-attach-row">
        <span v-for="(id, j) in attachments" :key="id" class="m-attach">
          <img :src="mediaURL(attachURL(id))" alt="">
          <button type="button" class="m-attach-x" :aria-label="'移除第 ' + (j + 1) + ' 张附件'" @click="attachments.splice(j, 1)">×</button>
        </span>
      </div>
      <div class="m-composer-row">
        <button v-if="!streaming" class="m-ico-btn" type="button" aria-label="添加图片" @click="pickImage">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="18" height="18" rx="3"/><circle cx="8.5" cy="8.5" r="1.5"/><path d="m21 15-5-5L5 21"/></svg>
        </button>
        <textarea v-model="input" class="m-input grow" rows="1" enterkeyhint="send"
                  :placeholder="streaming ? '正在回答…' : (agentName() ? '给 ' + agentName() + ' 派活…' : '说点什么…')"
                  :disabled="streaming"
                  @keydown.enter.exact.prevent="send"
                  @input="e => { e.target.style.height = 'auto'; e.target.style.height = Math.min(e.target.scrollHeight, 120) + 'px' }"></textarea>
        <button v-if="streaming" class="m-btn stop" type="button" aria-label="停止" @click="stop">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor"><rect x="6" y="6" width="12" height="12" rx="2"/></svg>
        </button>
        <button v-else class="m-btn primary" type="button" aria-label="发送" :disabled="!canSend()" @click="send">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19V5M5 12l7-7 7 7"/></svg>
        </button>
      </div>
      <input ref="fileInput" type="file" accept="image/png,image/jpeg,image/webp" multiple hidden @change="onPicked">
    </div>

    <!-- 会话配置底部弹层：点 chip 展开，选完即关（ZCode / MiniMax 手机端同款） -->
    <div v-if="openCap" class="m-sheet-mask" @click.self="capSheet = ''">
      <div class="m-sheet m-cap-sheet" role="dialog" :aria-label="capLabel(openCap)">
        <div class="m-sheet-title">{{ capLabel(openCap) }}<span class="m-sheet-note">{{ agentName() }}</span></div>
        <div class="m-opt-list">
          <button v-for="v in openCap.values" :key="v.value" type="button" class="m-opt"
                  :class="{ on: v.value === openCap.currentValue }" :disabled="streaming || capBusy"
                  @click="setCap(openCap, v.value)">
            <span class="m-opt-name">{{ v.name }}</span>
            <span v-if="v.value !== v.name" class="m-opt-val">{{ v.value }}</span>
            <svg v-if="v.value === openCap.currentValue" class="m-opt-check" width="16" height="16"
                 viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"
                 stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m20 6-11 11-5-5"/></svg>
          </button>
        </div>
        <div class="m-sheet-acts">
          <button class="m-btn block" type="button" @click="capSheet = ''">关闭</button>
        </div>
      </div>
    </div>
  </div>
</template>
