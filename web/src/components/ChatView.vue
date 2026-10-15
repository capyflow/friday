<script setup>
// 聊天视图：SSE 流式对话、欢迎态（逐字标题/轮播副标题/建议 chips）、
// Qoder 风 composer（两行：输入独占一行 + 底部工具条，左附件/模式 chip、右提示与圆形主键）、图片附件、更多菜单。
// bot 回答按 zcode 风时间线渲染：思考块（可折叠，流式一行预览）、工具调用块（一行，可展开结果）
// 与正文块按到达顺序分开排列，刷新后从历史接口恢复同样的块结构。
// 本组件在登录期间常驻（App 里 v-show），切到别的视图不会打断进行中的回复。
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { gsap, animate, bubbleIn, popIn } from '../anim.js'
import { api } from '../api.js'
import { fmtTokens } from '../format.js'
import { renderMD } from '../md.js'
import { store, toast } from '../store.js'

// 消息：{ role: 'user'|'bot'|'err'|'typing', text, images?, streaming?, paused?, plan?, turn? }
// bot 的 blocks 是时间线内容块（按到达顺序）：
//   { kind:'think', text, meta?, open? }        思考（meta 为轮次耗时摘要）
//   { kind:'tool', name, text, out?, status, open? }  工具调用（text=参数摘要，out=结果摘要）
//   { kind:'text', text }                       回答正文（markdown）
// bot 的 turn 是本轮问答的 token 汇总 { in, out, ms? }，随 done 帧到达、只活一次刷新。
const messages = reactive([])
const input = ref('')
const phase = ref('idle')          // 对话阶段：idle（空闲）| streaming（回复中）| paused（已暂停可继续）
const pausedTurn = ref(null)       // 暂停现场镜像 { question, partial }（服务端为准）
const busy = computed(() => phase.value === 'streaming')
const clearing = ref(false)
const menuOpen = ref(false)
const popOpen = ref(false)
const mode = ref(null)             // null | 'plan' | 'goal'
const scroller = ref(null)
const inputEl = ref(null)
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
const SIZE_LIMIT = 5 * 1024 * 1024

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
const placeholder = computed(() => (mode.value ? MODE_TEXT[mode.value][1] : '和小五说点什么…'))

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
// 旧格式（只有 text）兜底成单个正文块。
function histBlocks(m) {
  const src = (m.blocks && m.blocks.length) ? m.blocks : [{ kind: 'text', text: m.text }]
  return src.map(b => b.kind === 'think'
    ? { kind: 'think', text: b.text || '', meta: '', open: false }
    : b.kind === 'tool'
      ? { kind: 'tool', name: b.name || '', text: b.text || '', out: b.out || '',
          status: b.status || 'ok', open: false }
      : { kind: 'text', text: b.text || '' })
}

async function loadHistory() {
  const version = messagesVersion
  let list = []
  let paused = null
  try {
    const r = await api('GET', '/api/chat/history?session=web')
    list = r.messages || []
    paused = r.paused || null
  } catch (e) { /* 拉取失败按空会话处理 */ }
  if (disposed || version !== messagesVersion) return
  messages.splice(0, messages.length, ...list.map(m => ({
    role: m.role === 'user' ? 'user' : 'bot', text: m.text, blocks: histBlocks(m),
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
  if (i >= 0 && !busy.value) { pendingImages.splice(i, 1); releaseUnusedImages() }
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
    if (busy.value || clearing.value || disposed) break
    if (!imageSupported.value) { toast('当前模型不支持图片输入', true); break }
    if (pendingImages.length >= 4) { toast('一条消息最多 4 张图片', true); break }
    if (!f.type.startsWith('image/')) { toast('仅支持图片文件', true); continue }
    let r
    try { r = await normalizeImage(f) } catch { r = null }
    if (busy.value || clearing.value || disposed) break
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

// 剪贴板里有文件才接管：纯文本粘贴保持浏览器默认行为。
function onPaste(ev) {
  const files = Array.from(ev.clipboardData?.files || [])
  if (!files.length) return
  ev.preventDefault()
  addImageFiles(files)
}

async function uploadImages(images, signal) {
  const ids = []
  for (const p of images) {
    const fd = new FormData()
    fd.append('file', p.file)
    const res = await fetch('/api/upload', { method: 'POST', body: fd, signal })
    if (res.status === 401) {
      toast('登录已过期，请重新登录', true)
      store.loggedIn = false
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
function setMode(m) {
  if (busy.value || clearing.value) return
  mode.value = mode.value === m ? null : m
}
function fill(text) {
  if (busy.value || clearing.value) return
  input.value = text
  nextTick(() => { inputEl.value && inputEl.value.focus(); autoGrow() })
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
  if (busy.value || clearing.value) return
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
    toast('权限模式：' + PERM_META[m].short)
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
    const s = await api('GET', '/api/context?session=web')
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
    const r = await api('POST', '/api/context/compact', { session: 'web' })
    if (r.after && r.after.enabled) ctxSnap.value = r.after
    toast(r.message || '上下文已压缩')
  } catch (e) { toast('压缩失败：' + e.message, true) } finally { ctxBusy.value = false }
}

/* ---------- 发送（SSE 流式）：发送 / 暂停 / 继续 ---------- */
function autoGrow() {
  const el = inputEl.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = Math.min(el.scrollHeight, 160) + 'px'
}
function onInputKeydown(e) {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault()
    if (phase.value !== 'streaming') sendChat()
  }
}
// 主按钮一位三态：回复中=暂停；暂停且无输入=继续；其余=发送（暂停时输入新问题
// 即丢弃上次任务、开启新对话）。
function onSendClick() {
  if (phase.value === 'streaming') { pauseChat(); return }
  if (phase.value === 'paused' && !canSendNew.value) { resumeChat(); return }
  sendChat()
}

async function pauseChat() {
  if (phase.value !== 'streaming') return
  try { await api('POST', '/api/chat/stop', { session: 'web' }) }
  catch (e) { if (!disposed) toast(e.message, true) }
  // 服务端取消后以 {paused:true} 收尾流，状态在 streamTurn 的帧处理里落定
}

function newBot() {
  return reactive({ role: 'bot', text: '', streaming: true, paused: false, blocks: [], turn: null })
}

/* ---------- 时间线块（zcode 风）：思考 / 工具调用 / 正文按到达顺序分开 ---------- */
function lastBlock(m) { return m.blocks[m.blocks.length - 1] }

// 思考预览行：收起时只显示一行，取思考文本最新非空行的尾部，随增量不断更新。
const THINK_LINE_MAX = 120
function thinkPreview(text) {
  const lines = (text || '').split('\n')
  for (let i = lines.length - 1; i >= 0; i--) {
    const s = lines[i].trim()
    if (!s) continue
    return s.length > THINK_LINE_MAX ? '…' + s.slice(-THINK_LINE_MAX) : s
  }
  return ''
}

// 正在写的思考块（最后一块且轮次未定稿）：标题实时一行预览、灯泡呼吸。
function liveThink(m, bi, b) {
  return m.streaming && !m.paused && bi === m.blocks.length - 1 && !b.meta
}
function thinkTitle(m, b, bi) {
  if (liveThink(m, bi, b)) return thinkPreview(b.text) || '思考中…'
  return b.meta ? '思考 · ' + b.meta : '思考'
}
// 工具调用折叠行的可访问标题：出错时把状态说在前面，运行中没有可展开的内容故留空。
function toolTitle(b) {
  if (b.status === 'fail') {
    if (!b.out) return '工具执行出错'
    return b.open ? '收起出错详情' : '工具执行出错，展开详情'
  }
  if (!b.out) return ''
  return b.open ? '收起详情' : '展开详情'
}
// 轮末汇总文案：token 走 k/百万缩写；耗时缺失（被暂停的现场没有结束时刻）就不写。
function turnTotalText(t) {
  if (!t) return ''
  const s = `本轮 · ${fmtTokens(t.in || 0)}+${fmtTokens(t.out || 0)} tokens`
  return t.ms ? `${s} · 耗时 ${(t.ms / 1000).toFixed(1)}s` : s
}
function cursorOn(m, bi) {
  return m.streaming && bi === m.blocks.length - 1
}

// 思考增量并入正在写的思考块，否则新起一块（每个 LLM 轮次的思考各自成块）。
function pushThinking(m, delta) {
  const last = lastBlock(m)
  if (last && last.kind === 'think' && !last.meta) last.text += delta
  else m.blocks.push({ kind: 'think', text: delta, meta: '', open: false })
}

// 轮次结束事件（耗时/token）：挂到本轮刚写完的思考块上，收起时即"思考 · 耗时…"。
function finishRound(m, detail) {
  const last = lastBlock(m)
  if (last && last.kind === 'think' && !last.meta) last.meta = detail || ''
}

// 工具调用事件：run 新起一行；ok/fail 回填最近的同名运行中块。
function pushToolStep(m, s) {
  const name = s.name || ''
  if (s.status === 'run') {
    m.blocks.push({ kind: 'tool', name, text: s.detail || '', out: '', status: 'run', open: false })
    return
  }
  for (let i = m.blocks.length - 1; i >= 0; i--) {
    const b = m.blocks[i]
    if (b.kind === 'tool' && b.status === 'run' && b.name === name) {
      b.out = s.detail || ''
      b.status = s.status === 'fail' ? 'fail' : 'ok'
      return
    }
  }
}

// 回答增量并入最后一块正文，中间插入了工具调用则新起一块正文。
function pushText(m, delta) {
  m.text += delta
  const last = lastBlock(m)
  if (last && last.kind === 'text') last.text += delta
  else m.blocks.push({ kind: 'text', text: delta })
}

// streamTurn 跑一次 SSE 对话（发送与继续共用）：userMsg 为乐观插入的用户气泡
//（继续时为 null）；prepare 在建立连接前上传图片返回附件 ID；出错时按 draft
// 恢复输入现场（继续失败则回到"已暂停"状态）。
async function streamTurn({ url, body, userMsg, draft, prepare }) {
  messagesVersion++
  const controller = new AbortController()
  request = controller
  const typing = reactive({ role: 'typing' })
  let submitted = false
  let bot = null
  const removeTyping = () => {
    const i = messages.indexOf(typing)
    if (i >= 0) messages.splice(i, 1)
  }
  let reader
  try {
    if (prepare) {
      const ids = await prepare(controller.signal)
      if (disposed || controller.signal.aborted) return
      body = { ...body, attachments: ids }
    }
    if (userMsg) messages.push(userMsg)
    messages.push(typing)
    submitted = true
    popLastBubble()
    nextTick(() => stick(true))
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal: controller.signal,
    })
    if (res.status === 401) {
      store.loggedIn = false
      toast('登录已过期，请重新登录', true)
      return
    }
    if (!res.ok || !res.body) {
      let msg = 'HTTP ' + res.status
      try {
        const data = await res.json()
        if (data.body && data.body.error) msg = data.body.error
      } catch { /* 非 JSON 响应保持状态码提示 */ }
      throw new Error(msg)
    }
    reader = res.body.getReader()
    const decoder = new TextDecoder('utf-8')
    let buf = ''
    let done = false
    let paused = false
    let turnTotal = null // done 帧带回的本轮 token 汇总，循环结束后才挂到 bot 上
    const ensureBot = () => {
      removeTyping()
      if (!bot) { bot = newBot(); messages.push(bot) }
      return bot
    }
    const handle = ev => {
      let data
      try { data = JSON.parse(ev) } catch { throw new Error('聊天响应格式异常') }
      if (data.error) throw new Error(data.error)
      if (data.thinking) {
        pushThinking(ensureBot(), data.thinking)
        nextTick(() => stick())
      }
      if (data.step) {
        const b = ensureBot()
        if (data.step.kind === 'round') finishRound(b, data.step.detail)
        else pushToolStep(b, data.step)
        nextTick(() => stick())
      }
      if (data.plan) {
        ensureBot().plan = data.plan
        watchPlan(data.plan.id)
        nextTick(() => stick())
      }
      if (data.delta) {
        pushText(ensureBot(), data.delta)
        nextTick(() => stick())
      }
      if (data.paused) {
        // 对话被暂停：保留已生成部分，发送键变为"继续"。
        paused = true
        removeTyping()
        if (bot) { bot.paused = true; bot.streaming = false }
        phase.value = 'paused'
        pausedTurn.value = {
          question: (draft && draft.text) || (userMsg && userMsg.text) || '',
          partial: data.partial || (bot && bot.text) || '',
        }
      }
      if (data.done) {
        done = true
        if (data.turn) turnTotal = data.turn
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
      if (disposed || controller.signal.aborted) return
      buf += decoder.decode(chunk.value || new Uint8Array(), { stream: !chunk.done })
      consume()
      if (chunk.done) break
    }
    if (!done && !paused) throw new Error('回复连接已中断')
    if (!bot && !paused) {
      bot = { role: 'bot', text: '……', blocks: [{ kind: 'text', text: '……' }] }
      messages.push(bot)
    }
    if (bot && turnTotal) bot.turn = turnTotal
    // 本轮结束（含被暂停）：上下文占比刷新，chip 与弹窗里的数字跟着这一轮走
    loadCtx()
    if (!paused) {
      // 正常完成：清掉历史暂停标记（任务已续完或已放弃），回到空闲。
      phase.value = 'idle'
      pausedTurn.value = null
      if (bot) bot.streaming = false
      for (const m of messages) { if (m.paused) m.paused = false }
    }
    nextTick(() => stick())
    // 调用轨迹收进了设置弹窗：节挂载着才会收到，未挂载时无监听者、空抛无害
    window.dispatchEvent(new CustomEvent('friday:trace-refresh'))
  } catch (e) {
    if (disposed || controller.signal.aborted || !store.loggedIn) return
    if (draft) {
      input.value = draft.text
      mode.value = draft.mode
      pendingImages.push(...draft.images)
      nextTick(autoGrow)
    } else {
      phase.value = 'paused' // 继续失败：回到可继续状态
    }
    if (submitted) {
      // 未收到回复时撤销乐观消息；已有部分输出则保留，方便检查失败前的结果。
      if (!bot && userMsg) {
        const i = messages.indexOf(userMsg)
        if (i >= 0) messages.splice(i, 1)
      }
      messages.push({ role: 'err', text: e.message + (draft ? '，输入和图片已保留，可以重试。' : '') })
      popLastBubble()
    } else { toast(e.message + (draft ? '，输入和图片已保留，可以重试。' : ''), true) }
  } finally {
    if (reader) {
      try { await reader.cancel() } catch { /* 连接已关闭 */ }
      reader.releaseLock()
    }
    removeTyping()
    if (bot && phase.value !== 'paused') bot.streaming = false
    if (request === controller) request = null
    if (phase.value === 'streaming') phase.value = 'idle' // 兜底归位
    if (!disposed) nextTick(() => inputEl.value && inputEl.value.focus())
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
  autoGrow()
  const user = reactive({ role: 'user', text: raw, images: draft.images.map(p => p.url) })
  await streamTurn({
    url: '/api/chat',
    body: { message: raw, session: 'web', mode: reqMode },
    userMsg: user,
    draft,
    prepare: async signal => uploadImages(draft.images, signal),
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
  await streamTurn({ url: '/api/chat/resume', body: { session: 'web' } })
}

/* ---------- 更多菜单 ---------- */
function onDocClick(e) {
  if (menuOpen.value && moreWrap.value && !moreWrap.value.contains(e.target)) menuOpen.value = false
  if (popOpen.value && popWrap.value && !popWrap.value.contains(e.target)) setPop(false)
  if (permOpen.value && permWrap.value && !permWrap.value.contains(e.target)) permOpen.value = false
  if (ctxOpen.value && ctxWrap.value && !ctxWrap.value.contains(e.target)) ctxOpen.value = false
  if (modelOpen.value && modelWrap.value && !modelWrap.value.contains(e.target)) modelOpen.value = false
}
function onKeydown(e) {
  if (e.key === 'Escape') {
    menuOpen.value = false; setPop(false); permOpen.value = false; ctxOpen.value = false; modelOpen.value = false
  }
}
onMounted(() => {
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKeydown)
  window.addEventListener('friday:perm', onPermSync)
  window.addEventListener('friday:setup-refresh', loadModels) // 设置页保存模型库后 chip 跟上
  loadHistory()
  loadPerm()
  loadCtx()
  loadModels()
})
onUnmounted(() => {
  disposed = true
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
    const r = await api('POST', '/api/context/clear', { session: 'web' })
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
    <div id="chatHead">
      <div class="title">小五</div>
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
      <!-- 欢迎态（空会话） -->
      <div v-if="welcome" class="welcome">
        <div class="welcome-logo">五</div>
        <h3 class="welcome-title" ref="titleEl">你好，我是小五</h3>
        <p class="welcome-sub">你的私人 AI 生活管家 · <span ref="rotatorEl">提醒、待办、天气，都可以交给我</span><span class="caret"></span></p>
        <div class="welcome-suggests">
          <button v-for="s in SUGGESTS" :key="s" class="suggest" :disabled="busy || clearing" @click="sendChat(s)">{{ s }}</button>
        </div>
      </div>
      <!-- 对话态 -->
      <div v-else id="msgList">
        <div v-for="(m, i) in messages" :key="i" class="mrow" :class="m.role === 'typing' ? 'bot' : m.role">
          <div v-if="m.role === 'typing'" class="bubble">
            <span class="typing"><i></i><i></i><i></i></span>
          </div>
          <!-- bot：zcode 风时间线——思考 / 工具调用 / 正文按到达顺序分开成块 -->
          <div v-else-if="m.role === 'bot'" class="turn">
            <template v-for="(b, bi) in m.blocks" :key="bi">
              <!-- 思考块：收起时一行实时预览（流式）或轮次摘要，点击展开全文 -->
              <div v-if="b.kind === 'think'" class="think" :class="{ open: b.open }">
                <button class="think-head" type="button" :title="b.open ? '收起思考' : '展开思考'"
                        @click="b.open = !b.open">
                  <span class="step-ico think-ico" :class="{ live: liveThink(m, bi, b) }" aria-hidden="true">
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 14c.2-1 .7-1.7 1.5-2.4 1.1-1 2.5-2.4 2.5-4.6A6 6 0 0 0 6 7c0 2 1.4 3.4 2.5 4.6.8.7 1.3 1.4 1.5 2.4"/><path d="M9 18h6"/><path d="M10 22h4"/></svg>
                  </span>
                  <span class="think-title">{{ thinkTitle(m, b, bi) }}</span>
                  <svg class="think-chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
                </button>
                <div v-show="b.open" class="think-body">
                  <div class="think-text">{{ b.text }}</div>
                </div>
              </div>
              <!-- 工具调用块：收起只报"工具调用"，工具名与参数、结果都收进展开区 -->
              <div v-else-if="b.kind === 'tool'" class="tool"
                   :class="{ open: b.open, 'is-fail': b.status === 'fail' }">
                <button class="tool-head" type="button" :disabled="!b.out" :title="toolTitle(b)"
                        @click="b.open = b.out ? !b.open : false">
                  <span class="step-ico" :class="b.status" aria-hidden="true">
                    <i v-if="b.status === 'run'" class="spin step-ring"></i>
                    <svg v-else-if="b.status === 'fail'" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/></svg>
                    <svg v-else width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/></svg>
                  </span>
                  <span class="step-name">工具调用</span>
                  <svg v-if="b.out" class="think-chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>
                </button>
                <div v-show="b.open" class="tool-body">
                  <div class="tool-call">
                    <span class="step-name">{{ b.name }}</span>
                    <span class="step-detail">{{ b.text }}</span>
                  </div>
                  <div class="tool-out">{{ b.out }}</div>
                </div>
              </div>
              <!-- 正文块 -->
              <div v-else class="turn-text chat-md"><span v-html="renderMD(b.text)"></span><span v-if="cursorOn(m, bi)" class="cursor"></span></div>
            </template>
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
            <!-- 轮末一行汇总：本轮 token 与耗时（思考块只报每轮耗时，总量在这里收口） -->
            <div v-if="m.turn" class="turn-total">{{ turnTotalText(m.turn) }}</div>
          </div>
          <!-- 用户 / 错误消息仍是气泡 -->
          <div v-else class="bubble">
            {{ m.text }}
            <img v-for="(src, j) in m.images || []" :key="j" class="chat-img" :src="src" alt="">
          </div>
        </div>
      </div>
    </div>

    <div id="chatFoot">
      <div class="inputbar" :aria-busy="busy || clearing">
        <!-- 图片附件在输入容器内部：有图时撑起整个输入框 -->
        <div v-if="pendingImages.length" id="attachPreviews">
          <div v-for="p in pendingImages" :key="p.url" class="thumb">
            <img :src="p.url" alt="">
            <button class="rm" aria-label="移除图片" @click="removePreview(p)">×</button>
          </div>
        </div>
        <!-- 第一行：输入区独占整行并自动增高；操作键全部落到第二行 -->
        <textarea ref="inputEl" v-model="input" rows="1" :placeholder="placeholder" aria-label="聊天输入" :disabled="busy || clearing"
                  @keydown="onInputKeydown" @input="autoGrow" @paste="onPaste"></textarea>
        <!-- 第二行：左=附件与模式 chip，右=快捷键提示与主按钮 -->
        <div class="composer-bar">
          <div class="bar-left">
            <button class="ib-btn plus-btn" :class="{ active: popOpen }" aria-label="添加" title="添加"
                    :disabled="busy || clearing" @click.stop="setPop(!popOpen)">
              <svg width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>
            </button>
            <input type="file" ref="fileInput" accept="image/*" multiple hidden @change="onFilesPicked">
            <button class="ib-btn perm-btn" :class="['pm-' + permMode, { active: permOpen }]"
                    aria-label="权限模式" :title="'权限模式：' + PERM_META[permMode].label"
                    :disabled="busy || clearing" @click.stop="togglePerm">
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
                    :title="'当前模型：' + (modelShort || '未配置')" :disabled="clearing"
                    @click.stop="toggleModel">
              <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
                   stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3"/></svg>
              <span class="ctx-short model-short">{{ modelShort || '模型' }}</span>
            </button>
          </div>
          <div class="bar-right">
            <span class="kbd-hint" aria-hidden="true">⏎ 发送 · ⇧⏎ 换行</span>
            <button class="ib-btn send-btn" :class="{ 'halt-btn': phase === 'streaming', 'resume-btn': sendLabel === '继续上次任务' }"
                    :aria-label="sendLabel" :title="sendLabel" :disabled="sendDisabled" @click="onSendClick">
              <svg v-if="phase === 'streaming'" width="15" height="15" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><rect x="6" y="5" width="4" height="14" rx="1.2"/><rect x="14" y="5" width="4" height="14" rx="1.2"/></svg>
              <svg v-else-if="sendLabel === '继续上次任务'" width="17" height="17" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5.6v12.8c0 .9 1 1.5 1.8 1L20 13a1.2 1.2 0 0 0 0-2L9.8 4.6C9 4.1 8 4.7 8 5.6Z"/></svg>
              <svg v-else width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-send"/></svg>
            </button>
          </div>
        </div>

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
          <button class="pop-item pop-anim" :class="{ selected: mode === 'plan' }" @click="setMode('plan'); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m3 17 2 2 4-4"/><path d="m3 7 2 2 4-4"/><path d="M13 6h8"/><path d="M13 12h8"/><path d="M13 18h8"/></svg></span>
            <span class="pl">计划</span>
            <span class="pd">向小五请求制定任务计划</span>
          </button>
          <button class="pop-item pop-anim" :class="{ selected: mode === 'goal' }" @click="setMode('goal'); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><circle cx="12" cy="12" r="6"/><circle cx="12" cy="12" r="2"/></svg></span>
            <span class="pl">目标</span>
            <span class="pd">描述你的目标，成果定义越清晰效果越好</span>
          </button>
          <div class="pop-group pop-anim">快捷创建</div>
          <button class="pop-item pop-anim" @click="fill('提醒我 '); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg></span>
            <span class="pl">定时提醒</span>
          </button>
          <button class="pop-item pop-anim" @click="fill('待办：'); setPop(false)">
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
          <button v-for="m in modelLib.models" :key="m.name" class="pop-item pop-anim"
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
    </div>
  </section>
</template>
