<script setup>
// 聊天视图：SSE 流式对话、欢迎态（逐字标题/轮播副标题/建议 chips）、
// Qoder 风 composer（两行：输入独占一行 + 底部工具条，左附件/模式 chip、右提示与圆形主键）、图片附件、更多菜单。
// bot 回答按 zcode 风时间线渲染：思考块（可折叠，流式一行预览）、工具调用块（一行，可展开结果）
// 与正文块按轮次分开排列，刷新后从历史接口恢复同样的块结构与每轮 token 汇总。
// 本组件在登录期间常驻（App 里 v-show），切到别的视图不会打断进行中的回复。
//
// 事件驱动（2026-11-11 起）：本组件不再自己那条 SSE 连接直接消费帧，而是
// 订阅 /api/events/stream 统一收事件——同一份事件既驱动本端渲染，也驱动
// 另一端（手机端）渲染，于是「移动端问完、桌面端看不见」的问题从根上消失。
// 提交与订阅分离：POST /api/chat 立刻返回 turnId，回复内容从事件流里来。
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { gsap, animate, bubbleIn, popIn } from '../anim.js'
import { api, authFailed } from '../api.js'
import { authHeaders, mediaURL } from '../auth.js'
import { fmtTokens } from '../format.js'
import { turnTotalText, answerText, pushText } from '../blocks.js'
import { feedRate } from '../speed.js'
import { subscribe, clientId, deviceLabel } from '../events.js'
import { applyEvent } from '../turnevents.js'
import { store, toast } from '../store.js'
import TimelineBlocks from './TimelineBlocks.vue'
import CopyMessageButton from './CopyMessageButton.vue'
import ChatComposer from './ChatComposer.vue'
import FilePreviewDrawer from './FilePreviewDrawer.vue'

// 消息：{ role: 'user'|'bot'|'err'|'typing', text, images?, streaming?, paused?, plan?, turn? }
// bot 的 blocks 是时间线内容块（按到达顺序）：
//   { kind:'think', text, meta?, open? }        思考（meta 为轮次耗时摘要）
//   { kind:'tool', name, text, out?, status, open? }  工具调用（text=参数摘要，out=结果摘要）
//   { kind:'text', text }                       回答正文（markdown）
// 每块带 round（属于第几次 LLM 调用）：同一轮内思考与正文各自只占一块，
// 分片交错到达也不会把回答切成碎块。
// bot 的 turn 是本轮问答的 token 汇总 { in, out, ms? }，done 帧带来、历史接口同样回带。
const messages = reactive([])
const input = ref('')
const fileDrawer = ref(null)       // 文件预览抽屉（时间线里点文件路径 chip 打开）
const phase = ref('idle')          // 对话阶段：idle（空闲）| streaming（回复中）| paused（已暂停可继续）
const pausedTurn = ref(null)       // 暂停现场镜像 { question, partial }（服务端为准）
const statusLine = ref('')         // 事件流带来的状态说明（如「正在等另一端的问题跑完」）
const busy = computed(() => phase.value === 'streaming')
const clearing = ref(false)
const menuOpen = ref(false)
const popOpen = ref(false)
const mode = ref(null)             // null | 'plan' | 'goal'
const scroller = ref(null)
const composerEl = ref(null)
const fileInput = ref(null)
const moreWrap = ref(null)
const popWrap = ref(null)
const titleEl = ref(null)
const rotatorEl = ref(null)
const pendingImages = reactive([]) // { file, url }
const imageURLs = new Set()
let request = null
let disposed = false
let messagesVersion = 0
let unsub = null            // 事件订阅句柄（切页/登出时停）
let myTurnId = ''           // 本端最近一次提交的 turnId：停止/继续时按它精确操作
const device = deviceLabel() // 设备标签：另一端看到消息时标「来自手机端」
const SIZE_LIMIT = 5 * 1024 * 1024

/* ---------- 会话归属 ---------- */
// 这一页就是生活模式的那条主会话：key 固定 web，和钉钉/飞书/心跳共用同一份上下文。
// 工作模式不再有这个入口——在工作模式里跟小五干活走「工作区」（新建会话时选内置 friday
// 或外部 agent），两处都能聊就会有两份上下文，用户得自己记住该在哪边说话。
const MAIN_SESSION = 'web'

const MODE_TEXT = {
  plan: ['计划模式', '描述你的任务，向小五请求制定计划…'],
  goal: ['目标模式', '描述你的目标，成果定义越清晰效果越好…'],
}
const SUGGESTS = ['10 分钟后提醒我喝水', '记个待办：买牛奶', '上海今天天气怎么样', '每天早上 8 点叫我起床']
const PHRASES = ['提醒、待办、天气，都可以交给我', '说一句话，就能帮你安排好',
                 '本地运行，数据只属于你', '到点了，我会主动提醒你']

const welcome = computed(() => messages.length === 0)
const canSendNew = computed(() => !!input.value.trim() || pendingImages.length > 0)
const sendDisabled = computed(() => clearing.value || (phase.value === 'idle' && !canSendNew.value))
const sendLabel = computed(() =>
  phase.value === 'streaming' ? '暂停回复'
    : phase.value === 'paused' && !canSendNew.value ? '继续上次任务'
    : '发送')
const placeholder = computed(() =>
  (mode.value ? MODE_TEXT[mode.value][1] : '和小五说点什么…'))

/* ---------- 滚动 ---------- */
function nearBottom() {
  const el = scroller.value
  return !el || el.scrollHeight - el.scrollTop - el.clientHeight < 80
}
function stick(force) {
  const el = scroller.value
  if (!el) return
  if (force || nearBottom()) el.scrollTop = el.scrollHeight
}
function popLastBubble() {
  nextTick(() => {
    const el = scroller.value
    const last = el && el.querySelector('#msgList .mrow:last-child')
    if (last) bubbleIn(last)
  })
}

/* ---------- 欢迎态动效 ---------- */
let rotatorStop = null
function playWelcome() {
  if (!welcome.value) return
  nextTick(() => {
    // 标题逐字
    const el = titleEl.value
    let chars = []
    if (el) {
      const text = el.textContent
      el.textContent = ''
      for (const ch of text) {
        const s = document.createElement('span')
        s.className = 'ch'
        s.textContent = ch
        el.appendChild(s)
        chars.push(s)
      }
    }
    if (animate) {
      gsap.set(chars, { yPercent: 110, opacity: 0 })
      gsap.fromTo('.welcome-logo', { scale: .4, opacity: 0, y: 12 },
        { scale: 1, opacity: 1, y: 0, duration: .8, ease: 'back.out(1.7)', clearProps: 'transform' })
      gsap.to(chars, { yPercent: 0, opacity: 1, duration: .6,
        stagger: .055, ease: 'back.out(1.6)', delay: .28 })
      gsap.fromTo('.welcome-sub', { y: 12, opacity: 0 },
        { y: 0, opacity: 1, duration: .5, delay: .5, ease: 'power2.out', clearProps: 'opacity,transform' })
      gsap.fromTo('.suggest', { y: 18, opacity: 0, scale: .94 },
        { y: 0, opacity: 1, scale: 1, duration: .45, stagger: .07, delay: .58,
          ease: 'back.out(1.5)', clearProps: 'opacity,transform' })
    }
    // 副标题打字机轮播
    let idx = 0
    const play = () => {
      if (!welcome.value) return
      const phrase = PHRASES[idx++ % PHRASES.length]
      if (rotatorEl.value) rotatorEl.value.textContent = phrase
      if (animate) {
        const st = { len: 0 }
        gsap.timeline()
          .to(st, { len: phrase.length, duration: phrase.length * .06, ease: 'none',
            onUpdate: () => { if (rotatorEl.value) rotatorEl.value.textContent = phrase.slice(0, Math.round(st.len)) } })
          .to({}, { duration: 1.8 })
          .to(st, { len: 0, duration: phrase.length * .03, ease: 'none',
            onUpdate: () => { if (rotatorEl.value) rotatorEl.value.textContent = phrase.slice(0, Math.round(st.len)) } })
          .to({}, { duration: .3, onComplete: play })
      } else {
        rotatorStop = setTimeout(play, 2800)
      }
    }
    play()
  })
}
watch(welcome, v => {
  if (v) {
    playWelcome()
  } else if (rotatorStop) {
    clearTimeout(rotatorStop); rotatorStop = null
  }
})

/* ---------- 历史 ---------- */
// histBlocks 把历史接口的块结构转成时间线块（思考/工具默认收起）；
// 旧格式（只有 text）兜底成单个正文块。块顺序由后端排好（先思考后正文），
// 与实时时间线同构；一条历史消息就是一轮，轮次号统一记 0。
function histBlocks(m) {
  const src = (m.blocks && m.blocks.length) ? m.blocks : [{ kind: 'text', text: m.text }]
  return src.map(b => b.kind === 'think'
    ? { kind: 'think', text: b.text || '', meta: '', open: false, round: 0 }
    : b.kind === 'tool'
      ? { kind: 'tool', name: b.name || '', text: b.text || '', out: b.out || '',
          status: b.status || 'ok', open: false, round: 0 }
      : { kind: 'text', text: b.text || '', round: 0 })
}

async function loadHistory() {
  const version = messagesVersion
  let list = []
  let paused = null
  try {
    const r = await api('GET', '/api/chat/history?session=' + MAIN_SESSION)
    list = r.messages || []
    paused = r.paused || null
  } catch (e) { /* 拉取失败按空会话处理 */ }
  if (disposed || version !== messagesVersion) return
  messages.splice(0, messages.length, ...list.map(m => ({
    role: m.role === 'user' ? 'user' : 'bot', text: m.text, blocks: histBlocks(m),
    turn: m.turn || null,
  })))
  // 恢复暂停现场：渲染被暂停的问答，发送键变为"继续"。
  if (paused && paused.question) {
    const partial = paused.partial || ''
    messages.push({ role: 'user', text: paused.question })
    messages.push({ role: 'bot', text: partial, paused: true, blocks: histBlocks({ text: partial }) })
    phase.value = 'paused'
    pausedTurn.value = paused
  }
  if (messages.length) nextTick(() => stick(true))
}

/* ---------- 附件：归一化压缩 + 预览 + 上传 ---------- */
function removePreview(p) {
  const i = pendingImages.indexOf(p)
  // 回复中也允许撤：发出去的那批已经挪进 draft/messages（URL 由那边保着），
  // pendingImages 里剩下的是这轮新选的，撤掉不影响正在跑的那次请求。
  if (i >= 0) { pendingImages.splice(i, 1); releaseUnusedImages() }
}
function releaseUnusedImages() {
  const used = new Set([...pendingImages.map(p => p.url), ...messages.flatMap(m => m.images || [])])
  for (const url of imageURLs) {
    if (!used.has(url)) { URL.revokeObjectURL(url); imageURLs.delete(url) }
  }
}

// normalizeImage 把任意图片统一转为 JPEG：≤5MB 保持原尺寸仅转格式；
// 超过 5MB 的叠加降采样阶梯（降尺寸 → 降质量）压到上限以内。
async function normalizeImage(file) {
  let bitmap = null
  try { bitmap = await createImageBitmap(file, { imageOrientation: 'from-image' }) } catch (_) {}
  if (!bitmap) {
    try { bitmap = await createImageBitmap(file) } catch (_) { return null }
  }
  const draw = async (maxDim, quality) => {
    const scale = Math.min(1, maxDim / Math.max(bitmap.width, bitmap.height))
    const w = Math.max(1, Math.round(bitmap.width * scale))
    const h = Math.max(1, Math.round(bitmap.height * scale))
    const canvas = document.createElement('canvas')
    canvas.width = w; canvas.height = h
    const ctx = canvas.getContext('2d')
    ctx.fillStyle = '#fff'
    ctx.fillRect(0, 0, w, h)
    ctx.drawImage(bitmap, 0, 0, w, h)
    return new Promise(r => canvas.toBlob(r, 'image/jpeg', quality))
  }
  const toJPG = blob => new File([blob], file.name.replace(/\.\w+$/, '') + '.jpg', { type: 'image/jpeg' })
  const attempts = file.size > SIZE_LIMIT
    ? [[2048, .85], [2048, .7], [2048, .55], [1536, .7], [1280, .6], [1024, .5]]
    : [[Infinity, .9], [2048, .85], [2048, .7], [1280, .6]]
  try {
    for (const [maxDim, q] of attempts) {
      const blob = await draw(maxDim, q)
      if (blob && blob.size <= SIZE_LIMIT) {
        return { file: toJPG(blob), down: file.size > SIZE_LIMIT,
                 note: (file.size / 1048576).toFixed(1) + 'MB → ' + (blob.size / 1048576).toFixed(1) + 'MB' }
      }
    }
    return null
  } finally { bitmap.close() }
}

async function addImageFiles(files) {
  for (const f of files) {
    // 回复中也允许加：发出去的那批在 sendChat 里已经快照走人，这里新加的
    // 只挂在 pendingImages，等下一条消息用（清历史/离开页面仍会回收 URL）。
    if (clearing.value || disposed) break
    if (!imageSupported.value) { toast('当前模型不支持图片输入', true); break }
    if (pendingImages.length >= 4) { toast('一条消息最多 4 张图片', true); break }
    if (!f.type.startsWith('image/')) { toast('仅支持图片文件', true); continue }
    let r
    try { r = await normalizeImage(f) } catch { r = null }
    if (clearing.value || disposed) break
    if (!r) { toast('图片无法解码，已跳过', true); continue }
    if (pendingImages.length >= 4) { toast('一条消息最多 4 张图片', true); break }
    if (r.down) toast('图片过大，已降采样：' + r.note)
    const url = URL.createObjectURL(r.file)
    imageURLs.add(url)
    pendingImages.push({ file: r.file, url })
  }
}

function onFilesPicked(ev) {
  const files = Array.from(ev.target.files || [])
  ev.target.value = ''
  addImageFiles(files)
}

// 剪贴板里有文件才接管：只认图片（上传链路仅 png/jpeg/webp），非图片文件明确忽略；
// 纯文本粘贴保持浏览器默认行为。
function onPaste(ev) {
  const files = Array.from(ev.clipboardData?.files || [])
  if (!files.length) return
  ev.preventDefault()
  const imgs = files.filter(f => f.type.startsWith('image/'))
  const skipped = files.length - imgs.length
  if (skipped) toast(`剪贴板里有 ${skipped} 个非图片文件，已忽略（目前只支持图片）`, true)
  if (imgs.length) addImageFiles(imgs)
}

async function uploadImages(images, signal) {
  const ids = []
  for (const p of images) {
    const fd = new FormData()
    fd.append('file', p.file)
    const res = await fetch('/api/upload', { method: 'POST', headers: authHeaders(), body: fd, signal })
    if (res.status === 403) {
      authFailed()
      throw new Error('登录已过期，请重新登录')
    }
    const data = await res.json()
    if (!res.ok) throw new Error((data.body && data.body.error) || ('上传失败 HTTP ' + res.status))
    if (!data.body?.id) throw new Error('上传响应缺少图片 ID，请重试')
    ids.push(data.body.id)
  }
  return ids
}

/* ---------- 加号弹窗 / 模式 ---------- */
function setPop(open) {
  if (open === popOpen.value) return
  popOpen.value = open
  if (open) {
    permOpen.value = false // 弹层互斥：都从 composer 向上长，叠在一起没法读
    ctxOpen.value = false
    modelOpen.value = false
    nextTick(() => { const el = document.getElementById('addPopover'); if (el) popIn(el) })
  }
}
// 回复中输入框是禁的（对话页没有排队），凡是往输入框里写东西的入口这时都要说清原因，
// 别让人对着一个点不动的按钮猜。权限模式与「添加图片」不受这条限制。
const BUSY_INPUT_HINT = '小五正在回复：等这轮跑完（或先点停止）再改输入'
function setMode(m) {
  if (busy.value || clearing.value) return
  mode.value = mode.value === m ? null : m
}
function fill(text) {
  if (busy.value || clearing.value) return
  input.value = text
  nextTick(() => composerEl.value && composerEl.value.focus())
}

/* ---------- 权限模式（参照 zcode：chip 常驻输入框，弹层切换，全局热生效） ---------- */
const PERM_META = {
  confirm: { short: '确认', label: '确认（默认）', desc: '危险操作逐次网页批准，可“批准并记住”' },
  acceptEdits: { short: '自动编辑', label: '自动接受编辑', desc: '改文件/改配置免审批，跑命令仍把关' },
  fullAccess: { short: '完全信任', label: '完全信任', desc: '全部免审批直行，仅毁灭性命令黑名单仍拦截' },
}
const permMode = ref('confirm')
const permOpen = ref(false)
const permBusy = ref(false)
const permWrap = ref(null)
async function loadPerm() {
  try { permMode.value = (await api('GET', '/api/permissions')).mode || 'confirm' } catch (_) {}
}
function togglePerm() {
  if (clearing.value) return
  permOpen.value = !permOpen.value
  if (permOpen.value) {
    setPop(false)
    ctxOpen.value = false // 弹层互斥：都从 composer 左组向上长，叠在一起没法读
    modelOpen.value = false
    loadPerm() // 每次展开都取最新值：设置页等其他入口可能刚改过
    nextTick(() => { const el = document.getElementById('permPopover'); if (el) popIn(el) })
  }
}
async function setPerm(m) {
  if (m === permMode.value) { permOpen.value = false; return }
  if (m === 'fullAccess' &&
    !confirm('完全信任模式下，执行命令、写文件、改配置都不再询问（毁灭性命令仍拦截）。确定切换？')) return
  permBusy.value = true
  try {
    await api('POST', '/api/permissions', { mode: m })
    permMode.value = m
    permOpen.value = false
    const base = '权限模式：' + PERM_META[m].short
    // 回复中切：编辑免审当场生效；命令审批档位要等当前那次判定（可能正卡在审批卡片上）结束
    toast(busy.value ? base + '（编辑免审已即时生效，命令审批档位在当前审批答完后跟上）' : base)
  } catch (e) { toast('切换失败：' + e.message) } finally { permBusy.value = false }
}
// 设置页「权限模式」保存后广播同步，chip 即时跟上
function onPermSync(e) { if (e.detail) permMode.value = e.detail }

/* ---------- 上下文占用（chip 常驻 composer，点开看数字、达阈值可手动压缩） ---------- */
const ctxSnap = ref(null)   // /api/context 快照；null = 上下文治理未启用或没取到（此时不显示 chip）
const ctxOpen = ref(false)
const ctxBusy = ref(false)
const ctxWrap = ref(null)
const ctxPct = computed(() => (ctxSnap.value ? Math.round(ctxSnap.value.ratio * 100) : 0))
// 三档配色：未达阈值中性、达软阈值提醒、逼近硬阈值告警（软阈值来自 config.context）
const ctxLevel = computed(() => {
  const s = ctxSnap.value
  if (!s) return ''
  return s.ratio >= 0.95 ? 'hot' : (s.ratio >= s.softRatio ? 'warm' : '')
})
const ctxSoftPct = computed(() => (ctxSnap.value ? Math.round(ctxSnap.value.softRatio * 100) : 70))
const ctxBarPct = computed(() => Math.min(ctxPct.value, 100)) // 超窗时进度条封顶，数字照实显示
const ctxCompactHint = computed(() => {
  const s = ctxSnap.value
  if (!s) return '上下文信息不可用'
  if (busy.value) return '本轮回复进行中，等结束再压缩'
  if (!s.canCompact) return `未达归档阈值（${ctxSoftPct.value}%），现在没有可归档的历史`
  return '归档旧块并重写活跃历史，旧内容仍可用检索找回'
})
async function loadCtx() {
  try {
    const s = await api('GET', '/api/context?session=' + MAIN_SESSION)
    ctxSnap.value = s && s.enabled ? s : null
  } catch (_) { ctxSnap.value = null }
}
function toggleCtx() {
  if (clearing.value) return
  ctxOpen.value = !ctxOpen.value
  if (ctxOpen.value) {
    setPop(false)
    permOpen.value = false
    modelOpen.value = false
    loadCtx() // 占比随每轮问答变化，缓存没有意义：展开即取最新值
    nextTick(() => { const el = document.getElementById('ctxPopover'); if (el) popIn(el) })
  }
}

/* ---------- 模型快捷切换（chip 常驻 composer，点开切模型；能力声明管图片入口） ---------- */
const modelLib = ref(null)   // /api/provider/models；null = 未加载（chip 显示占位、不弹层）
const modelOpen = ref(false)
const modelBusy = ref(false)
const modelWrap = ref(null)
const activeModel = computed(() => modelLib.value?.models?.find(m => m.name === modelLib.value.active) || null)
const modelShort = computed(() => activeModel.value?.name || '')
// 模型库明确为空（首次安装）才算「未配置」；加载失败/旧响应缺字段时仍按未知处理。
const noModels = computed(() => Array.isArray(modelLib.value?.models) && modelLib.value.models.length === 0)
const modelChipLabel = computed(() => modelShort.value || (noModels.value ? '未配置模型' : '模型'))
// 未建库（合成条目无 inputs 声明）时视为全支持，向后兼容老配置
const imageSupported = computed(() => !activeModel.value || (activeModel.value.inputs || []).includes('image'))

async function loadModels() {
  try { modelLib.value = await api('GET', '/api/provider/models') } catch (_) { /* 保持旧值，chip 不闪没 */ }
}
function toggleModel() {
  if (clearing.value || !modelLib.value) return
  modelOpen.value = !modelOpen.value
  if (modelOpen.value) {
    setPop(false)
    permOpen.value = false
    ctxOpen.value = false
    loadModels() // 设置页可能刚改过库：展开即取最新
    nextTick(() => { const el = document.getElementById('modelPopover'); if (el) popIn(el) })
  }
}
async function setModel(name) {
  if (modelBusy.value) return
  if (name === modelLib.value?.active) { modelOpen.value = false; return }
  modelBusy.value = true
  try {
    const r = await api('POST', '/api/provider/activate', { name })
    toast(r.message || '已切换到 ' + name)
    modelOpen.value = false
    loadModels()
    loadCtx() // 不同模型窗口不同，占比口径跟着变
  } catch (e) { toast('切换失败：' + e.message) }
  finally { modelBusy.value = false }
}
function goModelSettings() {
  modelOpen.value = false
  store.settingsSection = 'model'
  store.settingsOpen = true
}
async function compactCtx() {
  if (ctxBusy.value || busy.value) return
  ctxBusy.value = true
  try {
    const r = await api('POST', '/api/context/compact', { session: MAIN_SESSION })
    if (r.after && r.after.enabled) ctxSnap.value = r.after
    toast(r.message || '上下文已压缩')
  } catch (e) { toast('压缩失败：' + e.message, true) } finally { ctxBusy.value = false }
}

/* ---------- 事件订阅（双端同步的核心） ---------- */
// onEvent 把一条事件交给共享翻译层 turnevents.js 处理：本端与另一端走同一条
// 渲染路径，两端显示的时间线因此一致。
function onEvent(ev) {
  applyEvent(ev, {
    messages,
    mine: clientId,
    // turnevents 的 botFor 会自己 push，这里只负责造一条空 bot 气泡。
    makeBot: () => newBot(),
    makeUser: (text, images) => reactive({ role: 'user', text, images: images || [], turnId: '' }),
    onPlan: plan => { if (plan && plan.id) watchPlan(plan.id) },
    onPaused: info => {
      // 暂停现场：服务端为准，本端据此把发送键变成「继续」。
      const q = messages.filter(m => m.role === 'user').pop()
      pausedTurn.value = { question: (q && q.text) || '', partial: info.partial || '' }
      phase.value = 'paused'
      const bot = info.bot
      if (bot) { bot.paused = true; bot.streaming = false }
      for (const m of messages) { if (m.paused) m.paused = false }
    },
    onEnd: () => {
      phase.value = 'idle'
      pausedTurn.value = null
      loadCtx()
      window.dispatchEvent(new CustomEvent('friday:trace-refresh'))
    },
    onError: (text, bot) => {
      phase.value = 'idle'
      if (bot) {
        bot.streaming = false
        // 错误接在那一轮的正文后面，不另起一条气泡——用户看得见答到哪儿断的。
        if (!bot.text) bot.text = text
        // 注意要同时写进正文块：bot 正文是按 blocks 渲染的，只改 text 看不见。
        else bot.text += '\n\n' + text
        pushText(bot, '\n\n' + text)
      } else {
        messages.push({ role: 'err', text })
      }
      popLastBubble()
    },
    onRemoteUser: () => { popLastBubble() },
    onStatus: text => { if (text) statusLine.value = text },
    onDelta: (bot, delta) => {
      if (bot) bot.rate = feedRate(bot, delta)
    },
    onStep: bot => { if (bot) nextTick(() => stick()) },
  })
  nextTick(() => stick())
}

// onResync：缺口过大或缓冲过期，服务端让前端全量重拉历史。宁可重拉，
// 也不假装消息是连续的（那样用户会以为 agent 漏答了）。
function onResync() {
  loadHistory()
}

// startEvents 订阅主会话事件流。登出/卸载时停掉，避免连接泄漏。
function startEvents() {
  if (unsub) return
  unsub = subscribe(MAIN_SESSION, 0, {
    onEvent,
    onResync,
    onState: () => {},
  })
}

function stopEvents() {
  if (!unsub) return
  unsub.stop()
  unsub = null
}

/* ---------- 发送（SSE 流式）：发送 / 暂停 / 继续 ---------- */
// 主按钮一位三态：回复中=暂停；暂停且无输入=继续；其余=发送（暂停时输入新问题
// 即丢弃上次任务、开启新对话）。
function onSendClick() {
  if (phase.value === 'streaming') { pauseChat(); return }
  if (phase.value === 'paused' && !canSendNew.value) { resumeChat(); return }
  sendChat()
}

async function pauseChat() {
  if (phase.value !== 'streaming') return
  // turnId 必填：服务端按轮次精确定位停哪一轮。不带 turnId 会停错对象
  // （同一会话上另一端也在跑的时候尤其明显）。
  try { await api('POST', '/api/chat/stop', { session: MAIN_SESSION, turnId: myTurnId }) }
  catch (e) { if (!disposed) toast(e.message, true) }
  // 服务端取消后以 paused 事件收尾，状态在事件回调里落定
}

function newBot() {
  return reactive({ role: 'bot', text: '', streaming: true, paused: false, blocks: [], turn: null, round: 0 })
}

/* ---------- 时间线块（zcode 风） ---------- */
// 块的构造/推进与渲染已抽到 ../blocks.js 与 TimelineBlocks.vue（与智能体页共用）；
// 这里只保留 SSE 帧到块的搬运。
// streamTurn 跑一次 SSE 对话（发送与继续共用）：userMsg 为乐观插入的用户气泡
//（继续时为 null）；prepare 在建立连接前上传图片返回附件 ID；出错时按 draft
// 恢复输入现场（继续失败则回到"已暂停"状态）。
// streamTurn 提交一轮对话（发送与继续共用）。
//
// 改造前：这里自己建 SSE 连接、直接消费帧，发起端独占这条流——另一端什么
// 都收不到。改造后：POST 只负责提交（立刻返回 turnId），回复内容统一从
// /api/events/stream 的事件流里来（见文件头注释）。本端与另一端因此走
// 完全相同的渲染路径，不会出现「两端显示成两个样子」。
//
// userMsg 为乐观插入的用户气泡（继续时为 null）；prepare 在提交前上传图片
// 返回附件 ID；出错时按 draft 恢复输入现场（继续失败则回到"已暂停"状态）。
async function streamTurn({ url, body, userMsg, draft, prepare }) {
  messagesVersion++
  const typing = reactive({ role: 'typing' })
  let submitted = false
  const removeTyping = () => {
    const i = messages.indexOf(typing)
    if (i >= 0) messages.splice(i, 1)
  }
  try {
    if (prepare) {
      const ids = await prepare()
      if (disposed) return
      body = { ...body, attachments: ids }
    }
    if (userMsg) messages.push(userMsg)
    messages.push(typing)
    submitted = true
    popLastBubble()
    nextTick(() => stick(true))

    // 提交：带上 clientId/device 让另一端知道这条是谁发的。
    const res = await fetch(url, {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ ...body, clientId, device }),
    })
    if (res.status === 403) {
      authFailed()
      return
    }
    let data = null
    try { data = await res.json() } catch (e) { /* 非 JSON 响应 */ }
    if (!res.ok) {
      const msg = (data && data.body && data.body.error) || ('HTTP ' + res.status)
      throw new Error(msg)
    }
    const r = data && data.body ? data.body : {}
    myTurnId = r.turnId || ''
    // 排队态：这一轮没能立刻开跑（另一端正在生成），先把气泡标成排队中。
    if (r.status === 'queued') {
      removeTyping()
      if (userMsg) {
        userMsg.queued = true
        userMsg.position = r.position || 1
      }
      phase.value = 'idle'
      toast('小五正在回答（来自另一端），你的问题排在第 ' + (r.position || 1) + ' 位')
    }
  } catch (e) {
    if (disposed || !store.loggedIn) return
    if (draft) {
      input.value = draft.text
      mode.value = draft.mode
      pendingImages.push(...draft.images)
      composerEl.value && composerEl.value.focus()
    } else {
      phase.value = 'paused' // 继续失败：回到可继续状态
    }
    if (submitted) {
      // 未收到回复时撤销乐观消息；已有部分输出则保留，方便检查失败前的结果。
      if (userMsg) {
        const i = messages.indexOf(userMsg)
        if (i >= 0) messages.splice(i, 1)
      }
      messages.push({ role: 'err', text: e.message + (draft ? '，输入和图片已保留，可以重试。' : '') })
      popLastBubble()
    } else { toast(e.message + (draft ? '，输入和图片已保留，可以重试。' : ''), true) }
  } finally {
    removeTyping()
    if (request === null) phase.value = phase.value === 'streaming' ? 'idle' : phase.value
    if (!disposed) nextTick(() => composerEl.value && composerEl.value.focus())
  }
}

async function sendChat(presetText) {
  if (clearing.value || disposed || phase.value === 'streaming') return
  const draft = {
    text: typeof presetText === 'string' ? presetText : input.value,
    mode: mode.value,
    images: [...pendingImages],
  }
  const text = draft.text.trim()
  if (!text && !draft.images.length) return
  // 首次安装是「未配置模型」：先明确拦住并指路，别拿占位地址发请求。
  if (noModels.value) {
    toast('还没有配置模型，请先在「设置 → 模型服务」添加', true)
    return
  }
  // 计划与目标都走后端 mode 字段：计划=可审阅清单 + 确认后逐步执行；
  // 目标=登记成后台多轮推进的即时目标。不再往消息里拼 【计划】/【目标】 前缀。
  const reqMode = draft.mode === 'plan' || draft.mode === 'goal' ? draft.mode : undefined
  const raw = text
  setPop(false)
  // 暂停期间发新问题 = 中止上次任务、开启下一段对话（服务端同步丢弃暂停现场）。
  phase.value = 'streaming'
  pausedTurn.value = null
  for (const m of messages) { if (m.paused) m.paused = false }
  mode.value = null
  input.value = ''
  // 把预览交给此次请求，保留 URL 直到对应消息/草稿被移除。
  pendingImages.splice(0, pendingImages.length)
  const user = reactive({ role: 'user', text: raw, images: draft.images.map(p => p.url) })
  await streamTurn({
    url: '/api/chat',
    body: { message: raw, session: MAIN_SESSION, mode: reqMode },
    userMsg: user,
    draft,
    prepare: async () => uploadImages(draft.images),
  })
}

/* ---------- 计划卡：确认后靠轮询同步步骤进度（不新建 SSE 通道，关标签页也不影响执行） ---------- */
const PLAN_STATE = {
  draft: '制定中', awaiting_confirm: '待确认', running: '执行中',
  paused: '已暂停', completed: '已完成', failed: '未完成', canceled: '已取消',
}
const STEP_STATE = { pending: '·', running: '…', done: '✓', failed: '✕', skipped: '−' }
const PLAN_DONE_STATES = ['completed', 'failed', 'canceled']
const planTimers = new Map()

async function refreshPlan(id) {
  try {
    const r = await api('GET', '/api/plans/' + encodeURIComponent(id))
    const target = messages.find(m => m.plan && m.plan.id === id)
    if (target && r.plan) target.plan = { ...target.plan, ...r.plan }
    if (!target) stopWatchingPlan(id)
    else if (PLAN_DONE_STATES.includes(r.plan?.state)) stopWatchingPlan(id)
  } catch (e) { /* 计划状态刷新失败不打断聊天，下一轮轮询再试 */ }
}

function stopWatchingPlan(id) {
  const t = planTimers.get(id)
  if (t) clearInterval(t)
  planTimers.delete(id)
}

function watchPlan(id) {
  if (!id || planTimers.has(id)) return
  planTimers.set(id, setInterval(() => refreshPlan(id), 2500))
  refreshPlan(id)
}

async function confirmPlan(id, approve) {
  try {
    const r = await api('POST', '/api/plans/' + encodeURIComponent(id) + '/confirm', { approve })
    toast(r.message || '已处理')
    if (approve) watchPlan(id)
    await refreshPlan(id)
  } catch (e) { toast(e.message, true) }
}

async function planAction(id, action) {
  try {
    const r = await api('POST', '/api/plans/' + encodeURIComponent(id) + '/' + action, {})
    toast(r.message || '已处理')
    if (action === 'resume') watchPlan(id)
    await refreshPlan(id)
  } catch (e) { toast(e.message, true) }
}

async function stepAction(id, n, action) {
  try {
    const r = await api('POST',
      '/api/plans/' + encodeURIComponent(id) + '/steps/' + n + '/' + action, {})
    toast(r.message || '已处理')
    watchPlan(id)
    await refreshPlan(id)
  } catch (e) { toast(e.message, true) }
}

onUnmounted(() => { for (const id of planTimers.keys()) stopWatchingPlan(id) })

async function resumeChat() {
  if (phase.value !== 'paused' || clearing.value || disposed) return
  phase.value = 'streaming'
  await streamTurn({ url: '/api/chat/resume', body: { session: MAIN_SESSION } })
}

/* ---------- 更多菜单 ---------- */
function onDocClick(e) {
  if (menuOpen.value && moreWrap.value && !moreWrap.value.contains(e.target)) menuOpen.value = false
  if (popOpen.value && popWrap.value && !popWrap.value.contains(e.target)) setPop(false)
  if (permOpen.value && permWrap.value && !permWrap.value.contains(e.target)) permOpen.value = false
  if (ctxOpen.value && ctxWrap.value && !ctxWrap.value.contains(e.target)) ctxOpen.value = false
  if (modelOpen.value && modelWrap.value && !modelWrap.value.contains(e.target)) modelOpen.value = false
}
/* Esc：先收弹层；都没有且焦点在对话区、正在回答时中断本轮（输入框自己的 keydown
   会先处理并 preventDefault，这里不重复）。 */
function onKeydown(e) {
  if (e.key !== 'Escape' || e.defaultPrevented) return
  const hasLayer = menuOpen.value || popOpen.value || permOpen.value || ctxOpen.value || modelOpen.value
  if (hasLayer) {
    e.preventDefault()
    menuOpen.value = false; setPop(false); permOpen.value = false; ctxOpen.value = false; modelOpen.value = false
    return
  }
  if (phase.value === 'streaming' && e.target && typeof e.target.closest === 'function' &&
      e.target.closest('#chatFoot, #msgList, .inputbar')) {
    e.preventDefault()
    onSendClick() // 流式期间这个入口就是"停止"
    return
  }
  // 没在跑：Esc 收起应用内全屏（普通窗口布局下什么都不做）
  if (store.fullscreen) {
    e.preventDefault()
    store.fullscreen = false
  }
}
// bootstrap 起手：生活模式直接拉主会话历史；工作模式先挑一条工作会话，历史与
// 占用由 applySession 按那条 key 拉——不能拿主会话的历史开台。
// bootstrap 起手：这一页恒为主会话，拉它的历史与上下文占用。
// 切到工作模式不会走到这里——聊天页常驻不卸载，切模式只是把它藏起来，
// 进行中的那一轮继续在后台收字。
function bootstrap() {
  loadHistory()
  loadCtx()
  return Promise.resolve()
}
onMounted(() => {
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKeydown)
  window.addEventListener('friday:perm', onPermSync)
  window.addEventListener('friday:setup-refresh', loadModels) // 设置页保存模型库后 chip 跟上
  startEvents() // 订阅主会话事件流：双端同步靠它
  bootstrap()
  loadPerm()
  loadModels()
})
onUnmounted(() => {
  disposed = true
  stopEvents()
  request?.abort()
  imageURLs.forEach(url => URL.revokeObjectURL(url))
  imageURLs.clear()
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKeydown)
  window.removeEventListener('friday:perm', onPermSync)
  window.removeEventListener('friday:setup-refresh', loadModels)
  if (rotatorStop) clearTimeout(rotatorStop)
})

function goTraces() { menuOpen.value = false; store.settingsSection = 'trace'; store.settingsOpen = true }
async function clearContext() {
  if (busy.value || clearing.value) return
  menuOpen.value = false
  ctxOpen.value = false
  clearing.value = true
  messagesVersion++
  try {
    const r = await api('POST', '/api/context/clear', { session: MAIN_SESSION })
    if (disposed) return
    messages.splice(0, messages.length)
    releaseUnusedImages()
    ctxSnap.value = null
    loadCtx() // 清空后占比归零，chip 要立刻反映，不能等下一次问答
    toast(r.message || '上下文已清除')
  } catch (e) { if (!disposed) toast(e.message, true) }
  finally { clearing.value = false }
}
</script>

<template>
  <section id="view-chat" class="view">
    <div id="chatHead" data-tauri-drag-region>
      <div class="title">小五</div>
      <button class="btn icon" :class="{ active: store.fullscreen }"
              :aria-label="store.fullscreen ? '退出全屏对话' : '进入全屏对话'"
              :title="store.fullscreen ? '退出全屏（Esc）' : '全屏对话（Esc 退出）'"
              @click="store.fullscreen = !store.fullscreen">
        <svg v-if="store.fullscreen" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor"
             stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 3H5a2 2 0 0 0-2 2v4M15 3h4a2 2 0 0 1 2 2v4M9 21H5a2 2 0 0 1-2-2v-4M15 21h4a2 2 0 0 0 2-2v-4"/></svg>
        <svg v-else width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor"
             stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 3h6v6M9 21H3v-6M21 3l-7 7M3 21l7-7"/></svg>
      </button>
      <div ref="moreWrap" class="more-wrap">
        <button class="btn icon" aria-label="更多操作" aria-haspopup="true" @click.stop="menuOpen = !menuOpen">
          <svg width="17" height="17" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><use href="#i-more"/></svg>
        </button>
        <div v-show="menuOpen" id="moreMenu" role="menu">
          <button class="menu-item" role="menuitem" @click="goTraces">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-route"/></svg>调用轨迹
          </button>
          <button class="menu-item" role="menuitem" :disabled="busy || clearing" @click="clearContext">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-reset"/></svg>清除上下文
          </button>
        </div>
      </div>
    </div>

    <div ref="scroller" id="chatScroller">
      <!-- 欢迎态（主会话还没内容） -->
      <div v-if="welcome" class="welcome">
        <div class="brand-mark welcome-logo"></div>
        <h3 class="welcome-title" ref="titleEl">你好，我是小五</h3>
        <p class="welcome-sub">你的私人 AI 生活管家 · <span ref="rotatorEl">提醒、待办、天气，都可以交给我</span><span class="caret"></span></p>
        <div class="welcome-suggests">
          <button v-for="s in SUGGESTS" :key="s" class="suggest" :disabled="busy || clearing" @click="sendChat(s)">{{ s }}</button>
        </div>
      </div>
      <!-- 对话态 -->
      <div v-else id="msgList">
        <div v-for="(m, i) in messages" :key="i" class="mrow" :class="m.role === 'typing' ? 'bot' : m.role">
          <!-- 另一端发来的消息：标出来源设备，用户才知道对面有人在用 -->
          <div v-if="m.remote && m.role === 'user'" class="remote-tag">{{ m.device || '另一端' }}</div>
          <div v-if="m.role === 'typing'" class="bubble">
            <span class="typing"><i></i><i></i><i></i></span>
          </div>
          <!-- bot：zcode 风时间线——思考 / 工具调用 / 正文按到达顺序分开成块 -->
          <div v-else-if="m.role === 'bot'" class="turn">
            <TimelineBlocks :blocks="m.blocks" :streaming="m.streaming" :paused="m.paused" :round="m.round"
                            :rate="m.rate || 0" own-tools @preview="path => fileDrawer.open(path)" />
            <span v-if="m.paused" class="paused-chip">已暂停 · 点发送键继续，或直接输入新问题</span>
            <div v-if="m.plan" class="plan-card" :data-plan="m.plan.id">
                <div class="pc-head">
                  <b>{{ m.plan.title || '计划' }}</b>
                  <span class="chip">{{ PLAN_STATE[m.plan.state] || m.plan.state }}</span>
                  <span class="meta">共 {{ (m.plan.steps || []).length }} 步</span>
                </div>
                <div v-if="m.plan.summary" class="pc-summary">{{ m.plan.summary }}</div>
                <ol class="pc-steps">
                  <li v-for="st in m.plan.steps" :key="st.n" :class="'step-' + st.state">
                    <span class="pc-state">{{ STEP_STATE[st.state] || '·' }}</span>
                    <span class="pc-title">{{ st.title }}</span>
                    <span v-if="st.detail" class="meta">{{ st.detail }}</span>
                    <span v-if="st.in || st.out" class="meta">{{ st.in + st.out }} tokens</span>
                    <span v-if="st.error" class="pc-error">{{ st.error }}</span>
                    <span v-if="st.state === 'failed'" class="pc-acts">
                      <button class="btn sm" type="button" @click="stepAction(m.plan.id, st.n, 'retry')">重试</button>
                      <button class="btn sm" type="button" @click="stepAction(m.plan.id, st.n, 'skip')">跳过</button>
                    </span>
                  </li>
                </ol>
                <div v-if="m.plan.error" class="pc-error">{{ m.plan.error }}</div>
                <div class="pc-acts">
                  <template v-if="m.plan.state === 'awaiting_confirm'">
                    <button class="btn sm primary" type="button" @click="confirmPlan(m.plan.id, true)">确认执行</button>
                    <button class="btn sm" type="button" @click="confirmPlan(m.plan.id, false)">先不做</button>
                  </template>
                  <template v-else-if="m.plan.state === 'running'">
                    <button class="btn sm" type="button" @click="planAction(m.plan.id, 'pause')">暂停</button>
                  </template>
                  <template v-else-if="m.plan.state === 'paused'">
                    <button class="btn sm primary" type="button" @click="planAction(m.plan.id, 'resume')">继续</button>
                    <button class="btn sm" type="button" @click="planAction(m.plan.id, 'cancel')">取消</button>
                  </template>
                  <template v-else-if="m.plan.state === 'completed' || m.plan.state === 'failed' || m.plan.state === 'canceled'">
                    <span class="meta">已结束</span>
                  </template>
                </div>
              </div>
            <!-- 轮末：token/耗时汇总在左，复制正文在右（回复进行中不给复制，避免复制半截答案） -->
            <div v-if="m.turn || (!m.streaming && answerText(m.blocks))" class="turn-foot">
              <div v-if="m.turn" class="turn-total">{{ turnTotalText(m.turn) }}</div>
              <div v-if="!m.streaming" class="msg-acts">
                <CopyMessageButton :blocks="m.blocks" kind="answer" />
              </div>
            </div>
          </div>
          <!-- 用户 / 错误消息仍是气泡（发出即可复制，不等本轮回复结束） -->
          <div v-else class="msg-col">
            <div class="bubble">
              {{ m.text }}
              <img v-for="(src, j) in m.images || []" :key="j" class="chat-img" :src="mediaURL(src)" alt="">
            </div>
            <!-- 排队态：这一轮还没开跑（另一端正在生成），标出排队位次 -->
            <div v-if="m.queued" class="queued-chip">排队中 · 第 {{ m.position || 1 }} 位</div>
            <div v-else-if="m.canceled" class="queued-chip">已取消</div>
            <div class="msg-acts">
              <CopyMessageButton :text="m.text" :kind="m.role === 'err' ? 'error' : 'question'" />
            </div>
          </div>
        </div>
      </div>
    </div>

    <div id="chatFoot">
      <ChatComposer ref="composerEl" v-model="input" :placeholder="placeholder" aria-label="聊天输入"
                    :busy="phase === 'streaming'" :disabled="clearing"
                    :send-label="sendLabel" :send-disabled="sendDisabled"
                    @primary="onSendClick" @stop="onSendClick" @paste="onPaste">
        <template #previews>
          <!-- 图片附件在输入容器内部：有图时撑起整个输入框 -->
          <div v-if="pendingImages.length" id="attachPreviews">
            <div v-for="p in pendingImages" :key="p.url" class="thumb">
              <img :src="p.url" alt="">
              <button class="rm" aria-label="移除图片" @click="removePreview(p)">×</button>
            </div>
          </div>
          <!-- 回复中选的图片先挂着：这轮跑完才发得出去，说清楚免得以为已经发出 -->
          <p v-if="busy && pendingImages.length" class="meta attach-hint">
            小五正在回复：图片挂在下一条消息上，这轮跑完再发
          </p>
        </template>
        <template #left>
          <!-- 这两个入口在回复中也点得动：权限模式改的是全局档位（后端当场生效），
               加号选图片是给下一条消息预选附件，都不受「输入框禁用」影响 -->
          <button class="ib-btn plus-btn" :class="{ active: popOpen }" aria-label="添加" title="添加"
                  :disabled="clearing" @click.stop="setPop(!popOpen)">
            <svg width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>
          </button>
          <input type="file" ref="fileInput" accept="image/*" multiple hidden @change="onFilesPicked">
          <button class="ib-btn perm-btn" :class="['pm-' + permMode, { active: permOpen }]"
                  aria-label="权限模式" :title="'权限模式：' + PERM_META[permMode].label"
                  :disabled="clearing" @click.stop="togglePerm">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
                 stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg>
            <span class="perm-short">{{ PERM_META[permMode].short }}</span>
          </button>
          <div class="mode-chip" :class="{ show: !!mode }">
            <span class="mode-name">{{ mode ? MODE_TEXT[mode][0] : '' }}</span>
            <button aria-label="退出模式" @click="setMode(mode)">
              <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path d="M18 6 6 18M6 6l12 12"/></svg>
            </button>
          </div>
          <!-- 上下文占用：点击查看数字与压缩入口。未启用上下文治理时后端不返回快照，chip 不出现 -->
          <button v-if="ctxSnap" class="ib-btn ctx-btn" :class="[ctxLevel, { active: ctxOpen }]"
                  aria-label="上下文使用率" aria-haspopup="true"
                  :title="'上下文占用 ' + ctxPct + '%，点击查看'" :disabled="clearing"
                  @click.stop="toggleCtx">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
                 stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3 3 8l9 5 9-5-9-5z"/><path d="m3 16 9 5 9-5"/><path d="m3 12 9 5 9-5"/></svg>
            <span class="ctx-short">{{ ctxPct }}%</span>
          </button>
          <!-- 当前模型：点开切模型（库只有一条时是快捷入口去设置），无库不出现 -->
          <button v-if="modelLib" class="ib-btn ctx-btn model-btn" :class="{ active: modelOpen }"
                  aria-label="当前模型" aria-haspopup="true"
                  :title="'当前模型：' + (modelShort || (noModels ? '未配置模型' : '未配置'))" :disabled="clearing"
                  @click.stop="toggleModel">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
                 stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3"/></svg>
            <span class="ctx-short model-short">{{ modelChipLabel }}</span>
          </button>
        </template>
      </ChatComposer>

        <!-- 加号弹窗 -->
        <div v-show="popOpen" ref="popWrap" class="popover" id="addPopover">
          <div class="popover-title pop-anim">添加</div>
          <button class="pop-item pop-anim" :disabled="!imageSupported"
                  :title="imageSupported ? '' : '当前模型不支持图片输入'"
                  @click="fileInput.click(); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/><path d="M12 10v6"/><path d="M9 13h6"/></svg></span>
            <span class="pl">添加图片</span>
            <span v-if="!imageSupported" class="pd">当前模型不支持图片输入</span>
          </button>
          <div class="pop-sep pop-anim"></div>
          <button class="pop-item pop-anim" :class="{ selected: mode === 'plan' }" :disabled="busy"
                  :title="busy ? BUSY_INPUT_HINT : ''" @click="setMode('plan'); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m3 17 2 2 4-4"/><path d="m3 7 2 2 4-4"/><path d="M13 6h8"/><path d="M13 12h8"/><path d="M13 18h8"/></svg></span>
            <span class="pl">计划</span>
            <span class="pd">向小五请求制定任务计划</span>
          </button>
          <button class="pop-item pop-anim" :class="{ selected: mode === 'goal' }" :disabled="busy"
                  :title="busy ? BUSY_INPUT_HINT : ''" @click="setMode('goal'); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><circle cx="12" cy="12" r="6"/><circle cx="12" cy="12" r="2"/></svg></span>
            <span class="pl">目标</span>
            <span class="pd">描述你的目标，成果定义越清晰效果越好</span>
          </button>
          <div class="pop-group pop-anim">快捷创建</div>
          <button class="pop-item pop-anim" :disabled="busy" :title="busy ? BUSY_INPUT_HINT : ''"
                  @click="fill('提醒我 '); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg></span>
            <span class="pl">定时提醒</span>
          </button>
          <button class="pop-item pop-anim" :disabled="busy" :title="busy ? BUSY_INPUT_HINT : ''"
                  @click="fill('待办：'); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/></svg></span>
            <span class="pl">新建待办</span>
          </button>
        </div>

        <!-- 权限模式弹层：三档热切换，全局生效；黑名单在任何档位都拦截 -->
        <div v-show="permOpen" ref="permWrap" class="popover perm-popover" id="permPopover">
          <div class="popover-title pop-anim">权限模式</div>
          <button v-for="(meta, m) in PERM_META" :key="m" class="pop-item pop-anim"
                  :class="{ selected: permMode === m }" :disabled="permBusy" @click="setPerm(m)">
            <span class="pi">
              <svg v-if="m === 'confirm'" width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg>
              <svg v-else-if="m === 'acceptEdits'" width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z"/></svg>
              <svg v-else width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2 3 14h9l-1 8 10-12h-9l1-8z"/></svg>
            </span>
            <span class="pl">{{ meta.label }}</span>
            <span class="pd">{{ meta.desc }}</span>
          </button>
          <div class="perm-foot pop-anim">毁灭性命令黑名单（rm -rf、shutdown 等）在任何档位都强制拦截</div>
        </div>

        <!-- 上下文弹层：占用数字 + 手动压缩 / 清除。占比口径与框架自动归档的闸门一致 -->
        <div v-if="ctxSnap" v-show="ctxOpen" ref="ctxWrap" class="popover ctx-popover" id="ctxPopover">
          <div class="popover-title pop-anim">上下文</div>
          <div class="ctx-meter pop-anim" :class="ctxLevel">
            <div class="ctx-fill" :style="{ width: ctxBarPct + '%' }"></div>
            <span class="ctx-mark" :style="{ left: ctxSoftPct + '%' }" :title="'归档阈值 ' + ctxSoftPct + '%'"></span>
          </div>
          <p class="ctx-line pop-anim">
            <b>{{ ctxPct }}%</b>
            <span class="meta">活跃区约 {{ fmtTokens(ctxSnap.used) }} / 窗口 {{ fmtTokens(ctxSnap.window) }} tokens</span>
          </p>
          <p class="ctx-line pop-anim meta">
            {{ ctxSnap.blocks }} 个活跃块 · {{ ctxSnap.messages }} 条消息 · 压缩后保留最近 {{ ctxSnap.keepRecent }} 块
          </p>
          <button class="pop-item ctx-item pop-anim" :disabled="!ctxSnap.canCompact || busy || ctxBusy" @click.stop="compactCtx">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m7 20 5-5 5 5"/><path d="m7 4 5 5 5-5"/></svg></span>
            <span class="ctx-item-text">
              <span class="pl">{{ ctxBusy ? '压缩中…' : '压缩上下文' }}</span>
              <span class="ctx-tip">{{ ctxCompactHint }}</span>
            </span>
          </button>
          <div class="pop-sep pop-anim"></div>
          <button class="pop-item pop-anim" :disabled="busy || clearing" @click.stop="clearContext()">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-reset"/></svg></span>
            <span class="pl">清除上下文</span>
            <span class="pd">整段历史清空，不可找回</span>
          </button>
          <div class="perm-foot pop-anim">
            占比按活跃历史估算，不含系统提示词与摘要区；达 {{ ctxSoftPct }}% 时每轮结束会自动归档一次
          </div>
        </div>

        <!-- 模型弹层：点选即热切换（对话上下文保持连续），入口去设置管理模型库 -->
        <div v-if="modelLib" v-show="modelOpen" ref="modelWrap" class="popover model-popover" id="modelPopover">
          <div class="popover-title pop-anim">模型</div>
          <p v-if="noModels" class="pop-empty pop-anim">还没有配置模型，先添加一个再对话</p>
          <button v-for="m in (modelLib.models || [])" :key="m.name" class="pop-item pop-anim"
                  :class="{ selected: m.name === modelLib.active }" :disabled="modelBusy" @click="setModel(m.name)">
            <span class="pi">
              <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"
                   stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3"/></svg>
            </span>
            <span class="pl">{{ m.name }}</span>
            <span class="pd">{{ (m.inputs || []).includes('image') ? '支持图片' : '仅文本' }}<template v-if="m.contextWindow"> · {{ fmtTokens(m.contextWindow) }} tokens</template></span>
          </button>
          <div class="pop-sep pop-anim"></div>
          <button class="pop-item pop-anim" @click="goModelSettings">
            <span class="pi"><svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-settings"/></svg></span>
            <span class="pl">管理模型库…</span>
            <span class="pd">设置 → 模型服务</span>
          </button>
        </div>
    </div>
    <FilePreviewDrawer ref="fileDrawer" />
  </section>
</template>
