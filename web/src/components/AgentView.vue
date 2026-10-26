<script setup>
// 智能体对话空间（ACP）：左侧 thread 列表（新建时绑定 agent 与真实工作目录，
// 之后不可切换），右侧对话时间线。SSE 帧协议与聊天页一致，时间线块复用
// TimelineBlocks/blocks.js；status 帧显示在"输入中"气泡里（启动 agent /
// 重接会话等生命周期占位）。note 行是服务端投影的系统标注（上下文已重置、
// 中断原因），灰字居中展示。
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { popIn } from '../anim.js'
import { pushThinking, pushToolStep, pushText, turnTotalText } from '../blocks.js'
import { store, toast } from '../store.js'
import TimelineBlocks from './TimelineBlocks.vue'
import ChatComposer from './ChatComposer.vue'
import FilePreviewDrawer from './FilePreviewDrawer.vue'

const threads = ref([])
const acp = ref(null)          // /api/acp/threads 回带的配置（enabled/agents…）
const currentId = ref('')
const messages = reactive([])
const input = ref('')
const phase = ref('idle')      // idle | streaming
const busy = computed(() => phase.value === 'streaming')
const scroller = ref(null)
const inputEl = ref(null)
const creating = ref(false)    // 新建表单展开
const form = reactive({ agent: '', workspace: '', title: '' })
const confirmId = ref('')      // 二次确认删除的 thread id
const pendingImages = reactive([]) // { id, url }
const fileInput = ref(null)
const fileDrawer = ref(null)       // 文件预览抽屉（时间线里点文件路径 chip 打开）
let controller = null

/* 会话能力（统一清单）：后端把 configOptions 与协议原生 models/modes 归一成
   Capability 列表（kind=model/thought_level/approval_mode/other），切换按
   source 路由到 set_config_option / set_model / set_mode。协议可选能力——
   agent 不支持的能力直接缺席，对应 chip 不渲染。会话没建过时不主动拉列表
   （GET 会顺带拉起 agent，破坏「首条消息才启动」的惰性）：选中有会话的
   thread 时取一次，之后每轮结束刷新。 */
const caps = ref(null)          // 当前 thread 的能力清单（null = 未加载）
const capOpen = ref('')         // 展开中的能力 id（'' = 全收起）
const capBusy = ref(false)
const capWrap = ref(null)

const CAP_LABEL = { model: '模型', thought_level: '思考强度', approval_mode: '审批模式' }
function capLabel(cap) { return CAP_LABEL[cap.kind] || cap.name }
function capCurrentName(cap) {
  return (cap.values || []).find(v => v.value === cap.currentValue)?.name || cap.currentValue
}
function capTitle(cap) { return capLabel(cap) + '：' + capCurrentName(cap) }
// kind → 图标路径（stroke 风格，currentColor；未知 kind 用滑块兜底）
const CAP_ICON = {
  model: '<rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3"/>',
  thought_level: '<path d="M9 18h6M10 21h4M12 3a6 6 0 0 0-4 10c.6.6 1 1.2 1 2h6c0-.8.4-1.4 1-2a6 6 0 0 0-4-10z"/>',
  approval_mode: '<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/>',
  other: '<path d="M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6"/>',
}
function capIcon(kind) { return CAP_ICON[kind] || CAP_ICON.other }

const openCap = computed(() => (caps.value || []).find(c => c.id === capOpen.value) || null)
async function loadOptions(id) {
  try {
    const r = await api('GET', `/api/acp/threads/${id}/options`)
    if (currentId.value === id) caps.value = r.capabilities || []
    return null
  } catch (e) { return e } // agent 拉起失败等：保持未加载态，下一轮结束后重试
}
function toggleCap(cap) {
  if (busy.value || capBusy.value) return
  popOpen.value = false
  if (capOpen.value === cap.id) { capOpen.value = ''; return }
  capOpen.value = cap.id
  nextTick(() => { const el = document.getElementById('agCapPopover'); if (el) popIn(el) })
}
async function setCap(cap, value) {
  if (capBusy.value || busy.value) return
  if (value === cap.currentValue) { capOpen.value = ''; return }
  capBusy.value = true
  try {
    const r = await api('POST', `/api/acp/threads/${currentId.value}/options`, { id: cap.id, value })
    caps.value = r.capabilities || []
    const fresh = (caps.value || []).find(c => c.id === cap.id)
    toast(capLabel(cap) + '已切换为 ' + (fresh ? capCurrentName(fresh) : value))
    capOpen.value = ''
  } catch (e) { toast('切换失败：' + e.message, true) } finally { capBusy.value = false }
}

const current = computed(() => threads.value.find(t => t.id === currentId.value) || null)
const enabled = computed(() => !!(acp.value && acp.value.enabled))
const canSend = computed(() => !!input.value.trim() || pendingImages.length > 0)
const stateLabel = { idle: '空闲', running: '对话中', interrupted: '已中断', orphaned: 'agent 已删除' }

const STATE_HINT = {
  interrupted: '上次会话被中断（服务重启或进程退出）。继续发消息会重新拉起 agent 并尝试重接上下文。',
  orphaned: '该对话绑定的智能体已被删除；历史仍可查看，但不能继续对话。',
}

onMounted(() => {
  load()
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKey)
})
onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKey)
})

/* 「添加」弹窗：与主会话同款交互（外点/Esc 关闭），agent 只保留添加图片一项 */
const popOpen = ref(false)
const popWrap = ref(null)
function setPop(open) {
  if (open === popOpen.value) return
  popOpen.value = open
  if (open) {
    capOpen.value = '' // 弹层互斥：都从 composer 左组向上长
    nextTick(() => { const el = document.getElementById('agAddPopover'); if (el) popIn(el) })
  }
}
function onDocClick(e) {
  if (popOpen.value && popWrap.value && !popWrap.value.contains(e.target)) setPop(false)
  if (capOpen.value && capWrap.value && !capWrap.value.contains(e.target)) capOpen.value = ''
}
function onKey(e) {
  if (e.key === 'Escape') { setPop(false); capOpen.value = '' }
}

async function load() {
  try {
    const r = await api('GET', '/api/acp/threads')
    threads.value = r.threads || []
    acp.value = r.acp || { enabled: false, agents: [] }
    if (!currentId.value && threads.value.length) select(threads.value[0].id)
  } catch (e) { toast('加载智能体对话失败：' + e.message, true) }
}

async function select(id) {
  if (busy.value) { toast('当前有进行中的对话，等它结束再切换', true); return }
  try {
    const r = await api('GET', '/api/acp/threads/' + id)
    currentId.value = id
    caps.value = null
    capOpen.value = ''
    if (r.thread.sessionId) loadOptions(id) // 会话已存在：清单顺带取到；没建过则等首条消息
    messages.length = 0
    for (const e of (r.thread.messages || [])) {
      if (e.role === 'user') {
        messages.push({ role: 'user', text: e.text, images: e.images || 0 })
      } else if (e.role === 'note') {
        messages.push({ role: 'note', text: e.text })
      } else {
        messages.push({ role: 'bot', text: e.text, streaming: false, paused: false,
          blocks: [{ kind: 'text', text: e.text, round: 0 }],
          turn: (e.in || e.out || e.ms) ? { in: e.in, out: e.out, ms: e.ms, stopped: e.stopped } : null })
      }
    }
    nextTick(stick)
  } catch (e) { toast('加载对话失败：' + e.message, true) }
}

function openCreate() {
  creating.value = !creating.value
  if (creating.value && !form.agent) {
    const first = (acp.value.agents || []).find(a => a.enabled !== false)
    form.agent = first ? first.name : ''
    syncWorkspace()
  }
}
function syncWorkspace() {
  const a = (acp.value.agents || []).find(x => x.name === form.agent)
  form.workspace = a && a.workspace ? a.workspace : (form.workspace || '')
}
// 目录选择器：后端 /api/acp/dirs 只读逐层列举（浏览器与桌面壳共用，浏览器
// 原生 API 拿不到绝对路径所以不走 showDirectoryPicker）。弹窗里可逐层点选，
// 也可在路径栏直接粘贴回车跳转；「使用此目录」把绝对路径回填进表单。
const dirPicker = reactive({ open: false, path: '', parent: '', input: '', dirs: [], loading: false })
async function browseDirs(path) {
  dirPicker.loading = true
  try {
    const r = await api('GET', '/api/acp/dirs' + (path ? '?path=' + encodeURIComponent(path) : ''))
    dirPicker.path = r.path
    dirPicker.parent = r.parent || ''
    dirPicker.input = r.path
    dirPicker.dirs = r.dirs || []
  } catch (e) { toast('读取目录失败：' + e.message, true) } finally { dirPicker.loading = false }
}
function openDirPicker() {
  dirPicker.open = true
  browseDirs(form.workspace || '')
}
function submitDirPath() {
  if (dirPicker.input.trim()) browseDirs(dirPicker.input.trim())
}
function chooseDir() {
  form.workspace = dirPicker.path
  dirPicker.open = false
}
async function createThread() {
  if (!form.agent) { toast('请选择智能体', true); return }
  try {
    const r = await api('POST', '/api/acp/threads', { ...form })
    creating.value = false
    form.title = ''
    form.workspace = ''
    await load()
    select(r.thread.id)
    toast('已创建对话，发第一条消息即会启动 agent')
  } catch (e) { toast('创建失败：' + e.message, true) }
}

async function removeThread(id) {
  if (confirmId.value !== id) { confirmId.value = id; setTimeout(() => { if (confirmId.value === id) confirmId.value = '' }, 3000); return }
  confirmId.value = ''
  try {
    await api('DELETE', '/api/acp/threads/' + id)
    if (currentId.value === id) { currentId.value = ''; messages.length = 0 }
    load()
    toast('已删除该对话')
  } catch (e) { toast('删除失败：' + e.message, true) }
}

/* ---------- 发送（SSE 流式，帧协议同聊天页，另有 status 帧） ---------- */
function nearBottom() {
  const el = scroller.value
  return !el || el.scrollHeight - el.scrollTop - el.clientHeight < 80
}
function stick() {
  const el = scroller.value
  if (el && nearBottom()) el.scrollTop = el.scrollHeight
}

async function uploadImages() {
  const ids = []
  for (const p of pendingImages) ids.push(p.id)
  return ids
}

async function send() {
  if (busy.value) { await stop(); return }
  if (!enabled.value) { toast('智能体功能未开启（设置 → 智能体）', true); return }
  const text = input.value.trim()
  if (!text && !pendingImages.length) return
  if (!currentId.value) { toast('先选择或新建一个对话', true); return }

  input.value = ''
  if (inputEl.value) inputEl.value.style.height = 'auto'
  const attachments = await uploadImages()
  const userMsg = reactive({ role: 'user', text: text || `（发送了 ${attachments.length} 张图片）`, images: attachments.length })
  const typing = reactive({ role: 'typing', label: '' })
  messages.push(userMsg)
  messages.push(typing)
  phase.value = 'streaming'
  nextTick(stick)

  controller = new AbortController()
  const removeTyping = () => { const i = messages.indexOf(typing); if (i >= 0) messages.splice(i, 1) }
  let bot = null
  const ensureBot = () => {
    removeTyping()
    if (!bot) {
      bot = reactive({ role: 'bot', text: '', streaming: true, paused: false, blocks: [], turn: null, round: 0 })
      messages.push(bot)
    }
    return bot
  }
  let done = false
  let turnTotal = null
  let errMsg = ''
  try {
    const res = await fetch(`/api/acp/threads/${currentId.value}/chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: text, attachments }),
      signal: controller.signal,
    })
    if (res.status === 401) { store.loggedIn = false; toast('登录已过期，请重新登录', true); return }
    if (!res.ok || !res.body) {
      let msg = 'HTTP ' + res.status
      try { const d = await res.json(); if (d.body && d.body.error) msg = d.body.error } catch {}
      throw new Error(msg)
    }
    const reader = res.body.getReader()
    const decoder = new TextDecoder('utf-8')
    let buf = ''
    const handle = ev => {
      let data
      try { data = JSON.parse(ev) } catch { throw new Error('响应格式异常') }
      if (data.error) throw new Error(data.error)
      if (data.status) typing.label = data.status
      if (data.thinking) { pushThinking(ensureBot(), data.thinking); nextTick(stick) }
      if (data.step) { pushToolStep(ensureBot(), data.step); nextTick(stick) }
      if (data.delta) {
        if (typing.label) typing.label = ''
        pushText(ensureBot(), data.delta)
        nextTick(stick)
      }
      if (data.done) {
        done = true
        if (data.turn) turnTotal = data.turn
        // M-5：agent 自报的上下文水位，并入轮末汇总一起展示。
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
  } catch (e) {
    if (e.name !== 'AbortError') errMsg = e.message
  } finally {
    controller = null
    phase.value = 'idle'
    removeTyping()
    if (bot) {
      bot.streaming = false
      bot.turn = turnTotal
      if (errMsg) {
        bot.blocks.push({ kind: 'text', text: '⚠️ ' + errMsg })
        bot.text += errMsg
      } else if (!bot.blocks.length) {
        bot.blocks.push({ kind: 'text', text: '（本轮没有产出文本）' })
      }
    } else if (errMsg) {
      messages.push({ role: 'note', text: errMsg })
    }
    for (const p of pendingImages.splice(0)) URL.revokeObjectURL(p.url)
    load() // 刷新列表状态（标题/状态/更新时间）
    if (currentId.value) loadOptions(currentId.value) // 一轮跑完会话必已建立：配置项（含 agent 侧的切换回推）跟着刷新
  }
}

async function stop() {
  if (!currentId.value) return
  try { await api('POST', `/api/acp/threads/${currentId.value}/stop`, {}) } catch (e) { toast('停止失败：' + e.message, true) }
}

/* ---------- 图片附件（复用聊天页上传链路，单张 ≤5MB） ---------- */
function pick() { fileInput.value && fileInput.value.click() }
async function onFilesPicked(ev) {
  const files = Array.from(ev.target.files || [])
  ev.target.value = ''
  for (const file of files) {
    if (pendingImages.length >= 4) { toast('一条消息最多 4 张图片', true); break }
    if (file.size > 5 * 1024 * 1024) { toast(`图片 ${file.name} 超过 5MB 上限`, true); continue }
    const fd = new FormData()
    fd.append('file', file)
    try {
      const r = await api('POST', '/api/upload', fd)
      pendingImages.push({ id: r.id, url: URL.createObjectURL(file) })
    } catch (e) { toast('上传失败：' + e.message, true) }
  }
}
function removePreview(p) {
  const i = pendingImages.indexOf(p)
  if (i >= 0) pendingImages.splice(i, 1)
  URL.revokeObjectURL(p.url)
}
</script>

<template>
  <div id="agentView" class="view">
    <!-- 左：thread 列表 -->
    <aside class="ag-rail">
      <div class="ag-rail-head">
        <b>智能体</b>
        <button class="ib-btn" :class="{ active: creating }" aria-label="新建对话" title="新建对话" @click="openCreate">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>
        </button>
      </div>
      <!-- 新建表单：agent 与工作目录在此刻绑定，之后不可切换 -->
      <div v-if="creating" class="ag-form">
        <label class="meta">智能体</label>
        <select v-model="form.agent" @change="syncWorkspace">
          <option v-for="a in (acp ? acp.agents : [])" :key="a.name" :value="a.name"
                  :disabled="a.enabled === false">{{ a.name }}{{ a.enabled === false ? '（已停用）' : '' }}</option>
        </select>
        <label class="meta">工作目录（agent 在这里干活，会写入文件）</label>
        <button type="button" class="ag-ws-pick" :title="form.workspace || '点击选择，留空则用临时会话目录'"
                @click="openDirPicker">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>
          <span>{{ form.workspace || '选择目录…' }}</span>
        </button>
        <p class="meta ag-ws-hint">留空则用临时会话目录（~/.vortex/acp-sessions/）</p>
        <div class="ag-form-acts">
          <button class="btn sm primary" :disabled="!form.agent" @click="createThread">创建</button>
          <button class="btn sm" @click="creating = false">取消</button>
        </div>
      </div>
      <div class="ag-list">
        <button v-for="t in threads" :key="t.id" class="ag-item" :class="{ active: t.id === currentId }"
                @click="select(t.id)">
          <span class="ag-dot" :class="'st-' + t.state" :title="stateLabel[t.state] || t.state"></span>
          <span class="ag-item-main">
            <span class="ag-title">{{ t.title || '（未命名）' }}</span>
            <span class="meta">{{ t.agent }} · {{ t.workspace }}</span>
          </span>
          <span class="ag-del" role="button" :aria-label="'删除 ' + (t.title || '对话')"
                @click.stop="removeThread(t.id)">{{ confirmId === t.id ? '确认删除?' : '×' }}</span>
        </button>
        <div v-if="!threads.length" class="ag-empty meta">还没有对话。右上角 + 新建一个，把 agent 绑到一个工作目录上。</div>
      </div>
    </aside>

    <!-- 右：对话 -->
    <section class="ag-main">
      <template v-if="!enabled">
        <div class="ag-blank">
          <div class="ag-blank-logo">智</div>
          <h3>智能体功能未开启</h3>
          <p class="meta">在「设置 → 智能体」里添加 agent（如 codex、opencode）并打开总开关，<br>然后回到这里新建对话，把活交给它。</p>
          <button class="btn primary" @click="store.settingsSection = 'agent'; store.settingsOpen = true">去设置</button>
        </div>
      </template>
      <template v-else-if="!current">
        <div class="ag-blank">
          <div class="ag-blank-logo">智</div>
          <h3>选择一个对话</h3>
          <p class="meta">左侧选择已有对话，或新建一个：选 agent、定工作目录，第一条消息发出时 agent 进程才会启动。</p>
        </div>
      </template>
      <template v-else>
        <header class="ag-head">
          <div class="ag-head-txt">
            <b>{{ current.title || '（未命名）' }}</b>
            <span class="meta" :title="current.workspace">
              {{ current.agent }} · {{ current.workspace }} · {{ stateLabel[current.state] || current.state }}
              <template v-if="current.degraded"> · 会话恢复不可靠</template>
            </span>
          </div>
        </header>
        <p v-if="STATE_HINT[current.state]" class="ag-hint">{{ STATE_HINT[current.state] }}</p>
        <div ref="scroller" class="ag-scroller">
          <div id="msgList">
            <template v-for="(m, i) in messages" :key="i">
              <div v-if="m.role === 'note'" class="ag-note">{{ m.text }}</div>
              <div v-else class="mrow" :class="m.role === 'typing' ? 'bot' : m.role">
                <div v-if="m.role === 'typing'" class="bubble">
                  <span v-if="m.label" class="ag-typing-label">{{ m.label }}</span>
                  <span class="typing"><i></i><i></i><i></i></span>
                </div>
                <div v-else-if="m.role === 'bot'" class="turn">
                  <TimelineBlocks :blocks="m.blocks" :streaming="m.streaming" :paused="m.paused" :round="m.round"
                                  @preview="path => fileDrawer.open(path)" />
                  <div v-if="m.turn" class="turn-total">
                    {{ turnTotalText(m.turn) }}<template v-if="m.turn.stopped && m.turn.stopped !== 'end_turn'"> · {{ m.turn.stopped }}</template>
                  </div>
                </div>
                <div v-else class="bubble">
                  {{ m.text }}
                  <span v-if="m.images" class="meta"> 📷 ×{{ m.images }}</span>
                </div>
              </div>
            </template>
          </div>
        </div>
        <div class="ag-foot">
          <ChatComposer v-model="input" :placeholder="'给 ' + current.agent + ' 派活…'"
                        aria-label="智能体输入" :busy="busy"
                        :send-label="busy ? '停止' : '发送'" :send-disabled="!busy && !canSend"
                        @primary="send">
            <template #previews>
              <div v-if="pendingImages.length" id="attachPreviews">
                <div v-for="p in pendingImages" :key="p.url" class="thumb">
                  <img :src="p.url" alt="">
                  <button class="rm" aria-label="移除图片" @click="removePreview(p)">×</button>
                </div>
              </div>
            </template>
            <template #left>
              <button class="ib-btn plus-btn" aria-label="添加图片" title="添加图片（≤5MB，最多 4 张）"
                      :disabled="busy" @click="pick">
                <svg width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>
              </button>
              <input type="file" ref="fileInput" accept="image/png,image/jpeg,image/webp" multiple hidden @change="onFilesPicked">
              <!-- 会话能力 chips（统一清单，agent 报什么显示什么；不支持的自动缺席） -->
              <button v-for="cap in (caps || [])" :key="cap.id"
                      class="ib-btn ctx-btn model-btn" :class="{ active: capOpen === cap.id }"
                      aria-haspopup="true" :title="capTitle(cap)" :disabled="busy || capBusy"
                      @click.stop="toggleCap(cap)">
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" v-html="capIcon(cap.kind)"></svg>
                <span class="ctx-short model-short">{{ capCurrentName(cap) }}</span>
            </template>
          </ChatComposer>
              <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/><path d="M12 10v6"/><path d="M9 13h6"/></svg></span>
          <div v-if="openCap" v-show="capOpen" ref="capWrap" class="popover model-popover" id="agCapPopover">
            <div class="popover-title pop-anim">{{ capLabel(openCap) }}</div>
                    :class="{ selected: v.value === openCap.currentValue }" :disabled="capBusy"
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" v-html="capIcon(openCap.kind)"></svg>
        </div>      </template>
    </section>
  </div>
  <FilePreviewDrawer ref="fileDrawer" />
  <!-- 目录选择器弹窗：逐层点选或路径栏直达，绝对路径由后端返回 -->
  <Teleport to="body">
    <div v-if="dirPicker.open" class="dlg-mask" @click.self="dirPicker.open = false">
      <div class="dlg" role="dialog" aria-modal="true" aria-label="选择工作目录" @keydown.esc="dirPicker.open = false">
        <header class="dlg-head">
          <h3 class="dlg-title">选择工作目录</h3>
          <button type="button" class="set-close" aria-label="关闭目录选择" @click="dirPicker.open = false">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
                 stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </header>
        <div class="dlg-body">
          <div class="ag-dir-bar">
            <input v-model="dirPicker.input" aria-label="目录路径" placeholder="输入路径后回车跳转"
                   :disabled="dirPicker.loading" @keydown.enter="submitDirPath">
            <button type="button" class="btn sm" :disabled="!dirPicker.parent || dirPicker.loading"
                    @click="browseDirs(dirPicker.parent)">上一级</button>
          </div>
          <ul class="ag-dirlist" aria-label="子目录列表">
            <li v-for="d in dirPicker.dirs" :key="d.path">
              <button type="button" @click="browseDirs(d.path)">{{ d.name }}</button>
            </li>
            <li v-if="!dirPicker.dirs.length && !dirPicker.loading" class="meta ag-dir-empty">（没有子目录）</li>
          </ul>
        </div>
        <footer class="dlg-foot">
          <span class="meta ag-dir-cur" :title="dirPicker.path">{{ dirPicker.path }}</span>
          <button class="btn sm primary" :disabled="dirPicker.loading || !dirPicker.path" @click="chooseDir">使用此目录</button>
          <button class="btn sm" @click="dirPicker.open = false">取消</button>
        </footer>
      </div>
    </div>
  </Teleport>
</template>
