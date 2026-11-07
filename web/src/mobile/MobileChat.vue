<script setup>
// 移动端对话视图：主会话（/api/chat）与智能体线程（/api/acp/threads/:id/chat）
// 共用这一个组件，靠 kind 区分——SSE 帧协议、块状态机（blocks.js）与时间线渲染
// （TimelineBlocks）都是桌面端原样复用，只换一层数字端版式。
import { nextTick, onMounted, ref, watch } from 'vue'
import { api, authFailed } from '../api.js'
import { authHeaders, mediaURL } from '../auth.js'
import { toast } from '../store.js'
import {
  closeRunningTools, finishRound, pushText, pushThinking, pushToolStep, turnTotalText,
} from '../blocks.js'
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

onMounted(load)
watch(() => props.kind === 'thread' && props.thread && props.thread.id, () => load())

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
    if (props.kind === 'main') await api('POST', '/api/chat/stop', { session: 'web' })
    else await api('POST', `/api/acp/threads/${props.thread.id}/stop`, {})
  } catch (e) { toast('停止失败：' + e.message, true) }
}

async function stream(body, reuseBot, urlOverride) {
  controller = new AbortController()
  streaming.value = true
  typing.value = ''
  let bot = reuseBot || null
  let done = false
  let turnTotal = null
  let errMsg = ''
  const ensureBot = () => {
    if (!bot) {
      bot = { role: 'bot', text: '', streaming: true, paused: false, blocks: [], turn: null, round: 0 }
      messages.value.push(bot)
      scrollBottom()
    }
    return bot
  }
  try {
    const res = await fetch(urlOverride || chatURL(), {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      body: JSON.stringify(body),
      signal: controller.signal,
    })
    if (res.status === 403) { authFailed(); return }
    if (!res.ok || !res.body) {
      let msg = 'HTTP ' + res.status
      try { const d = await res.json(); if (d.body && d.body.error) msg = d.body.error } catch { /* 无错误体 */ }
      throw new Error(msg)
    }
    const reader = res.body.getReader()
    const decoder = new TextDecoder('utf-8')
    let buf = ''
    const handle = ev => {
      let data
      try { data = JSON.parse(ev) } catch { throw new Error('响应格式异常') }
      if (data.error) throw new Error(data.error)
      if (data.status) typing.value = data.status
      if (data.thinking) { pushThinking(ensureBot(), data.thinking); scrollBottom() }
      if (data.step) {
        if (data.step.kind === 'round') finishRound(ensureBot(), data.step.detail)
        else pushToolStep(ensureBot(), data.step)
        scrollBottom()
      }
      if (data.delta) {
        if (typing.value) typing.value = ''
        pushText(ensureBot(), data.delta)
        scrollBottom()
      }
      if (data.plan && data.plan.title) typing.value = '计划：' + data.plan.title
      if (data.paused) {
        // 主会话被暂停（帧不带 done）：partial 已在 bot 上，整条置为暂停现场，
        // 出现「继续回答」按钮；续跑时把同一条接回流里。
        done = true
        if (bot) { bot.streaming = false; bot.paused = true }
        let q = ''
        for (let i = messages.value.length - 1; i >= 0; i--) {
          if (messages.value[i].role === 'user') { q = messages.value[i].text; break }
        }
        paused.value = { question: q, partial: data.partial || '' }
        return
      }
      if (data.done) {
        done = true
        if (data.turn) turnTotal = data.turn
        if (data.context && turnTotal) {
          turnTotal.contextUsed = data.context.used || 0
          turnTotal.contextSize = data.context.size || 0
        }
        if (data.error) errMsg = data.error
      }
    }
    const consume = () => {
      buf = buf.replace(/\r\n/g, '\n')
      let idx
      while (!done && (idx = buf.indexOf('\n\n')) >= 0) {
        const chunk = buf.slice(0, idx)
        buf = buf.slice(idx + 2)
        const data = chunk.split('\n').filter(line => line.startsWith('data:'))
          .map(line => line.slice(5).trimStart()).join('\n')
        if (data) handle(data)
      }
    }
    while (!done) {
      const chunk = await reader.read()
      if (controller.signal.aborted) return
      buf += decoder.decode(chunk.value || new Uint8Array(), { stream: !chunk.done })
      consume()
      if (chunk.done) break
    }
    if (!done && !paused.value) errMsg = errMsg || '回复连接已中断'
  } catch (e) {
    if (e.name !== 'AbortError') errMsg = e.message
  } finally {
    controller = null
    streaming.value = false
    typing.value = ''
    if (bot && !paused.value) {
      bot.streaming = false
      closeRunningTools(bot)
      bot.turn = turnTotal
      if (errMsg) {
        bot.blocks.push({ kind: 'text', text: '⚠️ ' + errMsg })
        bot.text += errMsg
      } else if (!bot.blocks.length) {
        bot.blocks.push({ kind: 'text', text: '（本轮没有产出文本）' })
      }
    }
    scrollBottom()
  }
}

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
                            @preview="path => emit('preview', path)" />
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
  </div>
</template>
