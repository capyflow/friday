<script setup>
// 智能体对话空间（ACP）：左侧 thread 列表（新建时绑定 agent 与真实工作目录，
// 之后不可切换），右侧对话时间线。SSE 帧协议与聊天页一致，时间线块复用
// TimelineBlocks/blocks.js；status 帧显示在"输入中"气泡里（启动 agent /
// 重接会话等生命周期占位）。note 行是服务端投影的系统标注（上下文已重置、
// 中断原因），灰字居中展示。
// 在 App 里用 v-show 常驻（同聊天页）：切换左侧导航不卸载组件，进行中的一轮
// SSE 与滚动位置都不会丢。首次打开先拉配置/列表，拿到结果前显示加载态，
// 不能把"还没加载完"当成"功能未开启"。
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { api, authFailed } from '../api.js'
import { authHeaders, mediaURL } from '../auth.js'
import { popIn } from '../anim.js'
import { pushThinking, pushToolStep, pushText, closeRunningTools, turnTotalText, answerText } from '../blocks.js'
import { fmtTokens } from '../format.js'
import { store, toast } from '../store.js'
import TimelineBlocks from './TimelineBlocks.vue'
import CopyMessageButton from './CopyMessageButton.vue'
import ChatComposer from './ChatComposer.vue'
import FilePreviewDrawer from './FilePreviewDrawer.vue'

const threads = ref([])
const acp = ref(null)          // /api/acp/threads 回带的配置（enabled/agents…）
const loaded = ref(false)      // 配置/列表是否已拉到：加载中 ≠ 功能未开启
const loadError = ref('')      // 首次加载失败原因（可重试）
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
   agent 不支持的能力直接缺席，对应 chip 不渲染。选中线程即拉清单（Zed 同款：
   不发消息也能切模型/思考强度）——清单只能在建会话后获得，GET 会顺带把 agent
   拉起建会话（新线程因此「选中即启动」，冷启动期间显示加载 chip），之后每轮
   结束刷新。 */
const caps = ref(null)          // 当前 thread 的能力清单（null = 未加载）
const capsLoading = ref(false)  // 清单拉取中（新线程含冷启动建会话，可能数秒）
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

/* 上下文水位（只读）：agent 经 usage_update 自报的窗口占用。上下文归 agent
   的会话自己管理，ACP 协议没有压缩/清除接口——这里只展示数字，agent 不上报
   就整块缺席（与主对话那个带压缩/清除的 chip 区分开）。 */
const ctx = ref(null)       // { used, size }；null = 该 agent 未上报
const ctxOpen = ref(false)
const ctxWrap = ref(null)
const ctxPct = computed(() => (ctx.value ? Math.round(ctx.value.used / ctx.value.size * 100) : 0))
const ctxBarPct = computed(() => Math.min(ctxPct.value, 100)) // 超窗时进度条封顶，数字照实显示
const ctxHot = computed(() => !!ctx.value && ctxPct.value >= 95)
function syncCtx(used, size) {
  ctx.value = size > 0 ? { used: used || 0, size } : null
  if (!ctx.value) ctxOpen.value = false
}
function toggleCtx() {
  if (!ctx.value) return
  popOpen.value = false
  capOpen.value = ''
  ctxOpen.value = !ctxOpen.value
  if (ctxOpen.value) nextTick(() => { const el = document.getElementById('agCtxPopover'); if (el) popIn(el) })
}

const openCap = computed(() => (caps.value || []).find(c => c.id === capOpen.value) || null)
async function loadOptions(id) {
  capsLoading.value = true
  try {
    const r = await api('GET', `/api/acp/threads/${id}/options`)
    if (currentId.value === id) caps.value = r.capabilities || []
    return null
  } catch (e) { return e } // agent 拉起失败等：保持未加载态，下次选中重试
  finally { if (currentId.value === id) capsLoading.value = false }
}
function toggleCap(cap) {
  if (busy.value || capBusy.value) return
  popOpen.value = false
  ctxOpen.value = false
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
    ctxOpen.value = false
    nextTick(() => { const el = document.getElementById('agAddPopover'); if (el) popIn(el) })
  }
}
function onDocClick(e) {
  if (popOpen.value && popWrap.value && !popWrap.value.contains(e.target)) setPop(false)
  if (capOpen.value && capWrap.value && !capWrap.value.contains(e.target)) capOpen.value = ''
  if (ctxOpen.value && ctxWrap.value && !ctxWrap.value.contains(e.target)) ctxOpen.value = false
}
function onKey(e) {
  if (e.key === 'Escape') { setPop(false); capOpen.value = ''; ctxOpen.value = false }
}

async function load() {
  try {
    const r = await api('GET', '/api/acp/threads')
    threads.value = r.threads || []
    acp.value = r.acp || { enabled: false, agents: [] }
    loaded.value = true
    loadError.value = ''
    // 每轮结束后刷新列表时顺带更新水位；线程已被别处删掉则收起 chip
    if (currentId.value) {
      const t = threads.value.find(x => x.id === currentId.value)
      syncCtx(t && t.contextUsed, t && t.contextSize)
    }
    if (!currentId.value && threads.value.length) select(threads.value[0].id)
  } catch (e) {
    // 已经加载过（轮末刷新失败）就保留现有界面；首次加载失败给可重试的说明。
    if (!loaded.value) loadError.value = e.message || String(e)
    toast('加载智能体对话失败：' + e.message, true)
  }
}

async function select(id) {
  if (busy.value) { toast('当前有进行中的对话，等它结束再切换', true); return }
  try {
    const r = await api('GET', '/api/acp/threads/' + id)
    currentId.value = id
    caps.value = null
    capOpen.value = ''
    ctxOpen.value = false
    syncCtx(r.thread.contextUsed, r.thread.contextSize)
    loadOptions(id).then(err => { // 选中即拉清单（Zed 同款）：没会话的顺带建会话，冷启动可能数秒
      if (err && currentId.value === id) toast('能力清单加载失败：' + (err.message || err), true)
    })
    messages.length = 0
    for (const e of (r.thread.messages || [])) {
      if (e.role === 'user') {
        messages.push({ role: 'user', text: e.text,
          images: (e.files || []).map(id => '/api/upload/' + id), imgCount: e.images || 0 })
      } else if (e.role === 'note') {
        messages.push({ role: 'note', text: e.text })
      } else {
        messages.push({ role: 'bot', text: e.text, streaming: false, paused: false,
          blocks: [{ kind: 'text', text: e.text, round: 0 }],
          turn: (e.in || e.out || e.ms) ? { in: e.in, out: e.out, ms: e.ms, stopped: e.stopped } : null })
      }
    }
    nextTick(() => stick(true)) // 打开会话默认落到最新一条，不沿用上一个 thread 的滚动位置
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
const dirPicker = reactive({ open: false, path: '', parent: '', input: '', dirs: [], loading: false,
  showHidden: false, back: [], fwd: [] })
// 点开头的隐藏目录默认不展示，弹窗内按钮切换；过滤在前端做，切换不用重新请求
const visibleDirs = computed(() =>
  dirPicker.showHidden ? dirPicker.dirs : dirPicker.dirs.filter(d => !d.name.startsWith('.')))
async function loadDirs(path) {
  dirPicker.loading = true
  try {
    const r = await api('GET', '/api/acp/dirs' + (path ? '?path=' + encodeURIComponent(path) : ''))
    dirPicker.path = r.path
    dirPicker.parent = r.parent || ''
    dirPicker.input = r.path
    dirPicker.dirs = r.dirs || []
  } catch (e) { toast('读取目录失败：' + e.message, true) } finally { dirPicker.loading = false }
}
// 前往新目录并记入历史（后退/前进栈）；后退前进走 loadDirs，不重复入栈
async function browseDirs(path) {
  const cur = dirPicker.path
  await loadDirs(path)
  if (dirPicker.path && dirPicker.path !== cur) {
    if (cur) dirPicker.back.push(cur)
    dirPicker.fwd.length = 0
  }
}
async function dirBack() {
  if (!dirPicker.back.length || dirPicker.loading) return
  dirPicker.fwd.push(dirPicker.path)
  await loadDirs(dirPicker.back.pop())
}
async function dirForward() {
  if (!dirPicker.fwd.length || dirPicker.loading) return
  dirPicker.back.push(dirPicker.path)
  await loadDirs(dirPicker.fwd.pop())
}
function openDirPicker() {
  dirPicker.open = true
  dirPicker.showHidden = false
  dirPicker.back.length = 0
  dirPicker.fwd.length = 0
  loadDirs(form.workspace || '')
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
    toast('已创建对话，正在启动 agent（就绪后即可切模型/思考强度）')
  } catch (e) { toast('创建失败：' + e.message, true) }
}

async function removeThread(id) {
  if (confirmId.value !== id) { confirmId.value = id; setTimeout(() => { if (confirmId.value === id) confirmId.value = '' }, 3000); return }
  confirmId.value = ''
  try {
    await api('DELETE', '/api/acp/threads/' + id)
    if (currentId.value === id) { currentId.value = ''; messages.length = 0; syncCtx(0, 0) }
    load()
    toast('已删除该对话')
  } catch (e) { toast('删除失败：' + e.message, true) }
}

/* ---------- 发送（SSE 流式，帧协议同聊天页，另有 status 帧） ---------- */
function nearBottom() {
  const el = scroller.value
  return !el || el.scrollHeight - el.scrollTop - el.clientHeight < 80
}
function stick(force) {
  const el = scroller.value
  // 组件隐藏（display:none）时布局高度为 0，不能在这里改 scrollTop，否则会把
  // 位置重置到顶部；回到本页时再按离开前的意图落底。
  if (!el || store.view !== 'agent') return
  if (force || nearBottom()) el.scrollTop = el.scrollHeight
}

// 切走时记住是否停在底部（或正在流式），切回来按需落到最新一条。
let backToBottom = false
watch(() => store.view, (v, prev) => {
  if (prev === 'agent' && v !== 'agent') backToBottom = nearBottom() || busy.value
  else if (v === 'agent') nextTick(() => { if (backToBottom) stick(true) })
})

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
  // 气泡直接渲染原图：用服务端 URL（/api/upload/:id），刷新后历史重载也能显示
  const sentImgs = attachments.map(id => '/api/upload/' + id)
  const userMsg = reactive({ role: 'user', text,
    images: sentImgs, imgCount: attachments.length })
  const typing = reactive({ role: 'typing', label: '' })
  messages.push(userMsg)
  messages.push(typing)
  // 预览即时清空（气泡已接手展示，对象 URL 用完即释放）
  for (const p of pendingImages.splice(0)) URL.revokeObjectURL(p.url)
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
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ message: text, attachments }),
      signal: controller.signal,
    })
    if (res.status === 403) { authFailed(); return }
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
        // M-5：agent 自报的上下文水位，并入轮末汇总一起展示；chip 立即跟上，
        // 不必等 finally 里的列表刷新。
        if (data.context && turnTotal) {
          turnTotal.contextUsed = data.context.used || 0
          turnTotal.contextSize = data.context.size || 0
        }
        if (data.context) syncCtx(data.context.used, data.context.size)
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
      // 本轮已了结：漏收尾的工具块不会再有结果事件了，就地收掉
      bot.streaming = false
      closeRunningTools(bot)
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
    load() // 刷新列表状态（标题/状态/更新时间）
    if (currentId.value) loadOptions(currentId.value) // 一轮跑完会话必已建立：配置项（含 agent 侧的切换回推）跟着刷新
  }
}

async function stop() {
  if (!currentId.value) return
  try { await api('POST', `/api/acp/threads/${currentId.value}/stop`, {}) } catch (e) { toast('停止失败：' + e.message, true) }
}

/* ---------- 图片附件（复用聊天页上传链路，单张 ≤5MB；按钮选图与粘贴共用） ---------- */
function pick() { fileInput.value && fileInput.value.click() }
function onFilesPicked(ev) {
  const files = Array.from(ev.target.files || [])
  ev.target.value = ''
  addImageFiles(files)
}
// 剪贴板里有文件才接管：只认图片（上传链路仅 png/jpeg/webp），非图片文件明确忽略；
// 纯文本粘贴保持浏览器默认行为（同主会话聊天页）。
function onPaste(ev) {
  const files = Array.from(ev.clipboardData?.files || [])
  if (!files.length) return
  ev.preventDefault()
  const imgs = files.filter(f => f.type.startsWith('image/'))
  const skipped = files.length - imgs.length
  if (skipped) toast(`剪贴板里有 ${skipped} 个非图片文件，已忽略（目前只支持图片）`, true)
  if (imgs.length) addImageFiles(imgs)
}
async function addImageFiles(files) {
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
  <!-- 组件多根（抽屉/Teleport 是兄弟节点），v-show 只能落在主根上；
       App 只在首次进入时挂载，之后靠这里随导航隐藏/恢复。 -->
  <div id="agentView" class="view" v-show="store.view === 'agent'">
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
        <p class="meta ag-ws-hint">留空则用临时会话目录（数据目录 acp-sessions/）</p>
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
        <div v-if="!loaded" class="ag-empty meta">正在加载对话…</div>
        <div v-else-if="!threads.length" class="ag-empty meta">还没有对话。右上角 + 新建一个，把 agent 绑到一个工作目录上。</div>
      </div>
    </aside>

    <!-- 右：对话 -->
    <section class="ag-main">
      <template v-if="!loaded">
        <div class="ag-blank">
          <div class="ag-blank-logo">智</div>
          <h3>{{ loadError ? '加载失败' : '正在加载智能体…' }}</h3>
          <p class="meta">{{ loadError || '读取已配置的 agent 与对话列表。' }}</p>
          <button v-if="loadError" class="btn primary" @click="load()">重试</button>
        </div>
      </template>
      <template v-else-if="!enabled">
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
                  <div v-if="m.turn || (!m.streaming && answerText(m.blocks))" class="turn-foot">
                    <div v-if="m.turn" class="turn-total">
                      {{ turnTotalText(m.turn) }}<template v-if="m.turn.stopped && m.turn.stopped !== 'end_turn'"> · {{ m.turn.stopped }}</template>
                    </div>
                    <div v-if="!m.streaming" class="msg-acts">
                      <CopyMessageButton :blocks="m.blocks" kind="answer" />
                    </div>
                  </div>
                </div>
                <div v-else class="msg-col">
                  <div class="bubble">
                    {{ m.text }}
                    <span v-if="m.imgCount && !(m.images && m.images.length)" class="meta"> 📷 ×{{ m.imgCount }}</span>
                    <img v-for="(src, j) in m.images || []" :key="j" class="chat-img" :src="mediaURL(src)" alt="">
                  </div>
                  <div class="msg-acts">
                    <CopyMessageButton :text="m.text" :kind="m.role === 'err' ? 'error' : 'question'" />
                  </div>
                </div>
              </div>
            </template>
          </div>
        </div>
        <div class="ag-foot">
          <ChatComposer v-model="input" :placeholder="'给 ' + current.agent + ' 派活…'"
                        aria-label="智能体输入" :busy="busy"
                        :send-label="busy ? '停止' : '发送'" :send-disabled="!busy && !canSend"
                        @primary="send" @paste="onPaste">
            <template #previews>
              <div v-if="pendingImages.length" id="attachPreviews">
                <div v-for="p in pendingImages" :key="p.url" class="thumb">
                  <img :src="p.url" alt="">
                  <button class="rm" aria-label="移除图片" @click="removePreview(p)">×</button>
                </div>
              </div>
            </template>
            <template #left>
              <button class="ib-btn plus-btn" :class="{ active: popOpen }" aria-label="添加" title="添加"
                      aria-haspopup="true" :disabled="busy" @click.stop="setPop(!popOpen)">
                <svg width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>
              </button>
              <input type="file" ref="fileInput" accept="image/png,image/jpeg,image/webp" multiple hidden @change="onFilesPicked">
              <!-- 上下文水位：agent 自报的窗口占用，常驻只读；未上报就不出现 -->
              <button v-if="ctx" class="ib-btn ctx-btn" :class="[ctxHot ? 'hot' : '', { active: ctxOpen }]"
                      aria-label="上下文使用率" aria-haspopup="true"
                      :title="'上下文占用 ' + ctxPct + '%（agent 自报），点击查看'"
                      @click.stop="toggleCtx">
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3 3 8l9 5 9-5-9-5z"/><path d="m3 16 9 5 9-5"/><path d="m3 12 9 5 9-5"/></svg>
                <span class="ctx-short">{{ ctxPct }}%</span>
              </button>
              <!-- 会话能力 chips（统一清单，agent 报什么显示什么；不支持的自动缺席） -->
              <button v-if="capsLoading && !caps" class="ib-btn ctx-btn model-btn" disabled
                      title="正在拉起 agent、建立会话…" aria-label="能力加载中">
                <span class="ctx-short model-short cap-loading">能力加载中…</span>
              </button>
              <button v-for="cap in (caps || [])" :key="cap.id"
                      class="ib-btn ctx-btn model-btn" :class="{ active: capOpen === cap.id }"
                      aria-haspopup="true" :title="capTitle(cap)" :disabled="busy || capBusy"
                      @click.stop="toggleCap(cap)">
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" v-html="capIcon(cap.kind)"></svg>
                <span class="ctx-short model-short">{{ capCurrentName(cap) }}</span>
              </button>
            </template>
          </ChatComposer>
          <!-- 加号弹窗：与主会话同款；权限/模式/上下文/模型 chips 是 Friday 大脑功能，ACP agent 不适用 -->
          <div v-show="popOpen" ref="popWrap" class="popover" id="agAddPopover">
            <div class="popover-title pop-anim">添加</div>
            <button class="pop-item pop-anim" @click="pick(); setPop(false)">
              <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/><path d="M12 10v6"/><path d="M9 13h6"/></svg></span>
              <span class="pl">添加图片</span>
              <span class="pd">≤5MB，最多 4 张</span>
            </button>
          </div>
          <!-- 能力弹层：按 source 路由热切换，会话上下文保持连续 -->
          <div v-if="openCap" v-show="capOpen" ref="capWrap" class="popover model-popover" id="agCapPopover">
            <div class="popover-title pop-anim">{{ capLabel(openCap) }}</div>
            <button v-for="v in openCap.values" :key="v.value" class="pop-item pop-anim"
                    :class="{ selected: v.value === openCap.currentValue }" :disabled="capBusy"
                    @click="setCap(openCap, v.value)">
              <span class="pi">
                <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" v-html="capIcon(openCap.kind)"></svg>
              </span>
              <span class="pl">{{ v.name }}</span>
              <span class="pd">{{ v.value }}</span>
            </button>
          </div>
          <!-- 上下文弹层：agent 自报的窗口水位，只读；ACP 无压缩/清除接口 -->
          <div v-if="ctx" v-show="ctxOpen" ref="ctxWrap" class="popover ctx-popover" id="agCtxPopover">
            <div class="popover-title pop-anim">上下文</div>
            <div class="ctx-meter pop-anim" :class="ctxHot ? 'hot' : ''">
              <div class="ctx-fill" :style="{ width: ctxBarPct + '%' }"></div>
            </div>
            <p class="ctx-line pop-anim">
              <b>{{ ctxPct }}%</b>
              <span class="meta">占用约 {{ fmtTokens(ctx.used) }} / 窗口 {{ fmtTokens(ctx.size) }} tokens（agent 自报）</span>
            </p>
            <div class="perm-foot pop-anim">
              数字来自 agent 的 usage_update，上下文由 agent 的会话自己管理；ACP 协议不提供压缩或清除，重开会话会丢历史。
            </div>
          </div>
        </div>      </template>
    </section>
  </div>
  <FilePreviewDrawer ref="fileDrawer" />
  <!-- 目录选择器弹窗：逐层点选或路径栏直达，绝对路径由后端返回 -->
  <Teleport to="body">
    <div v-if="dirPicker.open" class="dlg-mask" @click.self="dirPicker.open = false">
      <div class="dlg" role="dialog" aria-modal="true" aria-label="选择工作目录" @keydown.esc="dirPicker.open = false">
        <!-- .dlg 是横向 flex 外壳，纵向堆叠靠 .dlg-form（同 AutoView 的弹窗结构） -->
        <div class="dlg-form">
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
              <button type="button" class="ib-btn" title="后退" aria-label="后退"
                      :disabled="!dirPicker.back.length || dirPicker.loading" @click="dirBack">
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
              </button>
              <button type="button" class="ib-btn" title="前进" aria-label="前进"
                      :disabled="!dirPicker.fwd.length || dirPicker.loading" @click="dirForward">
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 12h14M12 5l7 7-7 7"/></svg>
              </button>
              <button type="button" class="ib-btn" title="上一级" aria-label="上一级"
                      :disabled="!dirPicker.parent || dirPicker.loading" @click="browseDirs(dirPicker.parent)">
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 19V5M5 12l7-7 7 7"/></svg>
              </button>
              <button type="button" class="ib-btn ag-dir-show" :class="{ active: dirPicker.showHidden }"
                      :title="dirPicker.showHidden ? '不显示隐藏目录' : '显示隐藏目录'"
                      :aria-label="dirPicker.showHidden ? '不显示隐藏目录' : '显示隐藏目录'"
                      :aria-pressed="dirPicker.showHidden" @click="dirPicker.showHidden = !dirPicker.showHidden">
                <svg v-if="dirPicker.showHidden" width="18" height="18" viewBox="0 0 24 24" fill="none"
                     stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                  <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/>
                  <path d="M1 1l22 22"/>
                </svg>
                <svg v-else width="18" height="18" viewBox="0 0 24 24" fill="none"
                     stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                  <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>
                </svg>
              </button>
            </div>
            <ul class="ag-dirlist" aria-label="子目录列表">
              <li v-for="d in visibleDirs" :key="d.path">
                <button type="button" @click="browseDirs(d.path)">{{ d.name }}</button>
              </li>
              <li v-if="!visibleDirs.length && !dirPicker.loading" class="meta ag-dir-empty">
                {{ dirPicker.dirs.length ? '（只有隐藏子目录，可点上方按钮显示）' : '（没有子目录）' }}
              </li>
            </ul>
          </div>
          <footer class="dlg-foot">
            <span class="meta ag-dir-cur" :title="dirPicker.path">{{ dirPicker.path }}</span>
            <button class="btn sm primary" :disabled="dirPicker.loading || !dirPicker.path" @click="chooseDir">使用此目录</button>
            <button class="btn sm" @click="dirPicker.open = false">取消</button>
          </footer>
        </div>
      </div>
    </div>
  </Teleport>
</template>
