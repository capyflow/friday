<script setup>
// 聊天视图：SSE 流式对话、欢迎态（逐字标题/轮播副标题/建议 chips）、
// Qoder 风 composer（两行：输入独占一行 + 底部工具条，左附件/模式 chip、右提示与圆形主键）、图片附件、更多菜单。
// bot 回答按 zcode 风时间线渲染：思考块（可折叠，流式一行预览）、工具调用块（一行，可展开结果）
// 与正文块按轮次分开排列，刷新后从历史接口恢复同样的块结构与每轮 token 汇总。
// 本组件在登录期间常驻（App 里 v-show），切到别的视图不会打断进行中的回复。
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { gsap, animate, bubbleIn, popIn } from '../anim.js'
import { api, authFailed } from '../api.js'
import { authHeaders, mediaURL } from '../auth.js'
import { ago, fmtTokens } from '../format.js'
import { getPref, removePref, setPref } from '../prefs.js'
import { pushThinking, finishRound, pushToolStep, pushText, closeRunningTools, turnTotalText, answerText } from '../blocks.js'
import { store, toast } from '../store.js'
import TimelineBlocks from './TimelineBlocks.vue'
import CopyMessageButton from './CopyMessageButton.vue'
import ChatComposer from './ChatComposer.vue'
import FilePreviewDrawer from './FilePreviewDrawer.vue'
import DirPickerDialog from './DirPickerDialog.vue'

// 消息：{ role: 'user'|'bot'|'err'|'typing', text, images?, streaming?, paused?, plan?, turn? }
// bot 的 blocks 是时间线内容块（按到达顺序）：
//   { kind:'think', text, meta?, open? }        思考（meta 为轮次耗时摘要）
//   { kind:'tool', name, text, out?, status, open? }  工具调用（text=参数摘要，out=结果摘要）
//   { kind:'text', text }                       回答正文（markdown）
// 每块带 round（属于第几次 LLM 调用）：同一轮内思考与正文各自只占一块，
// 分片交错到达也不会把回答切成碎块。
// bot 的 turn 是本轮问答的 token 汇总 { in, out, ms? }，done 帧带来、历史接口同样回带。
const fileDrawer = ref(null)       // 文件预览抽屉（时间线里点文件路径 chip 打开）
const clearing = ref(false)        // 正在清除上下文（这一条会话的瞬时闸门）
const menuOpen = ref(false)
const popOpen = ref(false)
const scroller = ref(null)
const composerEl = ref(null)
const fileInput = ref(null)
const moreWrap = ref(null)
const popWrap = ref(null)
const titleEl = ref(null)
const rotatorEl = ref(null)
const imageURLs = new Set()        // 本次登录期间创建过的 object URL（回收用）
let disposed = false
const SIZE_LIMIT = 5 * 1024 * 1024

/* ---------- 使用模式与会话归属 ---------- */
// 会话本体在后端（internal/web/sessions.go）：生活模式只有主会话（key 固定 web，
// 和钉钉/飞书/心跳共用同一份上下文），工作模式可以在左侧栏里新建多条独立会话。
// 这里只管三件事：当前对话归属哪个 key、那份列表、以及每条会话各自的现场。
const SESSION_PREF = 'friday-chat-session'
const workMode = computed(() => store.mode === 'work')
const sessions = ref([])                    // 工作会话列表（不含主会话，也不含后台会话）
const sessionKey = ref(workMode.value ? '' : 'web') // 当前对话归属的会话 key
const edit = reactive({ key: '', title: '' })       // 会话行内改名
const creating = ref(false)                          // 新建会话表单（名字 + 项目目录都可选）
const sessionForm = reactive({ title: '', dir: '' })
const dirDialog = ref(null)                          // 共用的目录选择弹窗
const noSession = computed(() => workMode.value && !sessionKey.value)
const activeSession = computed(() => sessions.value.find(s => s.key === sessionKey.value) || null)
const headTitle = computed(() => workMode.value ? (activeSession.value?.title || '工作会话') : '小五')

/* ---------- 每条会话一份现场 ---------- */
// 进行中的那一轮写的是它自己那份记录，所以切会话、切生活/工作模式都不必打断回复：
// 切走的那条继续在后台收字，切回来就是当时的进度（与「工作区」页同一个口径）。
// 记录里装的是这条会话的全部易失状态：时间线、阶段、暂停现场、上下文占比、
// 草稿、模式 chip、待发的附件，以及那一轮的 AbortController。
const records = reactive({})
function recordOf(key) {
  if (!key) return null
  let rec = records[key]
  if (!rec) {
    rec = records[key] = reactive({
      messages: [], phase: 'idle', pausedTurn: null, ctx: null,
      draft: '', mode: null, images: [], controller: null, version: 0,
    })
  }
  return rec
}
// 当前显示的那份现场。记录只在选中时建（applySession / bootstrap），所以这里不建，
// 免得渲染期往 reactive 里写东西。
const cur = computed(() => (sessionKey.value ? records[sessionKey.value] || null : null))
const messages = computed(() => (cur.value ? cur.value.messages : []))
const phase = computed(() => (cur.value ? cur.value.phase : 'idle'))
const busy = computed(() => phase.value === 'streaming')
const ctxSnap = computed(() => (cur.value ? cur.value.ctx : null))
const pendingImages = computed(() => (cur.value ? cur.value.images : []))
const mode = computed({
  get: () => (cur.value ? cur.value.mode : null),
  set: v => { if (cur.value) cur.value.mode = v },
})
const input = computed({
  get: () => (cur.value ? cur.value.draft : ''),
  set: v => { if (cur.value) cur.value.draft = v },
})
// 会话行上的「回复中」：读的是每条会话自己那份记录，不依赖服务端状态（那只在轮末刷新）。
function generating(key) {
  const rec = records[key]
  return !!(rec && rec.phase === 'streaming')
}

const MODE_TEXT = {
  plan: ['计划模式', '描述你的任务，向小五请求制定计划…'],
  goal: ['目标模式', '描述你的目标，成果定义越清晰效果越好…'],
}
const SUGGESTS = ['10 分钟后提醒我喝水', '记个待办：买牛奶', '上海今天天气怎么样', '每天早上 8 点叫我起床']
const PHRASES = ['提醒、待办、天气，都可以交给我', '说一句话，就能帮你安排好',
                 '本地运行，数据只属于你', '到点了，我会主动提醒你']

// welcome 是"这条会话还没内容"的空态；工作模式下一条会话都没有时走另一套
// 空态（见 noSession），别把"还没建会话"画成"建好了但没说话"。
const welcome = computed(() => !noSession.value && messages.value.length === 0)
const canSendNew = computed(() => !!input.value.trim() || pendingImages.value.length > 0)
const sendDisabled = computed(() => clearing.value || (phase.value === 'idle' && !canSendNew.value))
const sendLabel = computed(() =>
  phase.value === 'streaming' ? '暂停回复'
    : phase.value === 'paused' && !canSendNew.value ? '继续上次任务'
    : '发送')
const placeholder = computed(() => {
  if (noSession.value) return '先新建一条会话再开工'
  return mode.value ? MODE_TEXT[mode.value][1] : '和小五说点什么…'
})

// rowMeta 是会话行的小字：说过话就报条数与"多久之前动过"，一条都还没有就明说
// "还没有内容"（这时后端回的是创建时刻，拿它算"多少个月"没意义）；选了项目目录
// 就把目录名带上，一眼能看出这条会话在哪个项目里干活。
function rowMeta(s) {
  const parts = [s.messages ? `${s.messages} 条${ago(s.updatedAt) ? ' · ' + ago(s.updatedAt) : ''}` : '还没有内容']
  if (s.dir) parts.push(baseName(s.dir))
  return parts.join(' · ')
}

// baseName 取目录最后一段（行上铺全路径太长，完整路径在 title 里）
function baseName(p) {
  const segs = (p || '').replace(/\/+$/, '').split('/')
  return segs[segs.length - 1] || p
}

async function loadSessions() {
  try {
    const r = await api('GET', '/api/sessions')
    sessions.value = (r.sessions || []).filter(s => s.kind === 'work')
  } catch (e) {
    sessions.value = []
    toast('会话列表加载失败：' + e.message, true)
  }
  return sessions.value
}

// showRecord 切到某条会话的现场。正在跑的那条不重拉历史：历史接口只有已落盘的
// 部分，覆盖回去会把实时进度打回半截。空闲的那条按 key 重拉——主会话会被钉钉/
// 飞书/心跳这些别的渠道写消息，拿着旧缓存看就是"少了几轮"。
async function showRecord(key) {
  menuOpen.value = false
  const rec = recordOf(key)
  if (!rec) return
  if (rec.phase !== 'streaming') await loadHistory(rec, key)
  loadCtx(rec, key)
}

// applySession 落到某条会话并记住它（下次回到工作模式接着这条）。
// 只有工作模式才记：生活模式恒为主会话，把 web 写进去会把上次的选择顶掉。
async function applySession(key) {
  recordOf(key) // 现场先建好，模板这一帧就能拿到（不是"还没选中会话"的空壳）
  sessionKey.value = key
  if (workMode.value) {
    if (key) setPref(SESSION_PREF, key)
    else removePref(SESSION_PREF)
  }
  await showRecord(key)
}

// pickSession 决定工作模式下当前哪条会话：优先上次选的那条（还在列表里才算，
// 被删了就换最近活动的），一条都没有就留空让页面明说"还没有会话"。
async function pickSession() {
  const list = await loadSessions()
  const remembered = list.find(s => s.key === getPref(SESSION_PREF))
  await applySession(remembered ? remembered.key : (list[0]?.key || ''))
}

// 切会话/新建会话/改名都不看"有没有别的会话在跑"：每条会话自己一份现场，
// 后台那一轮不受影响（会话行上会标出它还在生成）。
async function selectSession(key) {
  if (key === sessionKey.value) return
  await applySession(key)
}

async function createSession() {
  try {
    const r = await api('POST', '/api/sessions', {
      title: sessionForm.title.trim(), dir: sessionForm.dir,
    })
    sessionForm.title = ''
    sessionForm.dir = ''
    creating.value = false
    await loadSessions()
    await applySession(r.session?.key || '')
    toast(r.session?.dir ? '已新建会话，读写都在 ' + baseName(r.session.dir) + ' 里' : '已新建会话，直接开说')
    composerEl.value?.focus?.()
  } catch (e) {
    // 目录不合法（被删了、指到了文件）后端会说要什么，表单留着照着改
    toast(e.message, true)
  }
}

// openSessionForm 打开新建会话表单：名字与项目目录都是可选的。
function openSessionForm() {
  sessionForm.title = ''
  sessionForm.dir = ''
  creating.value = true
}

function pickSessionDir() {
  dirDialog.value?.open(path => { sessionForm.dir = path }, sessionForm.dir || '')
}

async function removeSession(s) {
  const name = s.title || '新会话'
  if (!confirm('删除会话「' + name + '」？这段上下文和它的调用轨迹一起清掉，找不回来。')) return
  try {
    await api('DELETE', '/api/sessions/' + encodeURIComponent(s.key))
  } catch (e) { toast(e.message, true); return }
  toast('已删除会话「' + name + '」')
  // 现场跟着删：正在跑的那一轮先取消（会话已经没了，留着只会往不存在的归属上写字），
  // 附件预览的 object URL 也一并回收。
  const rec = records[s.key]
  if (rec) {
    rec.controller?.abort()
    delete records[s.key]
    releaseUnusedImages()
  }
  const rest = sessions.value.filter(x => x.key !== s.key)
  sessions.value = rest
  // 删的是当前这条：接着落到剩下的第一条（都删完就回到"还没有会话"）。
  if (s.key === sessionKey.value) await applySession(rest[0]?.key || '')
}

function startRename(s) {
  edit.key = s.key
  edit.title = s.hasTitle ? s.title : '' // 自动取的名不预填：清空就是"继续自动取名"
}

async function commitRename() {
  if (!edit.key) return
  const key = edit.key
  const title = edit.title.trim()
  edit.key = ''
  try {
    const r = await api('POST', '/api/sessions/' + encodeURIComponent(key), { title })
    const i = sessions.value.findIndex(s => s.key === key)
    if (i >= 0 && r.session) sessions.value[i] = { ...sessions.value[i], ...r.session }
  } catch (e) { toast(e.message, true) }
}

// 切模式 = 换归属：生活模式回主会话，工作模式按上面的规则挑一条工作会话。
watch(() => store.mode, async m => {
  if (m === 'work') await pickSession()
  else await applySession('web')
})

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

// loadHistory 按 key 拉历史，写进那条会话自己的记录（rec）。rec.version 是这条记录的
// 代数：期间又切走/又发了一轮就不该再覆盖（旧请求的回包晚到也会落错地方）。
async function loadHistory(rec, key) {
  if (!rec || !key) return // 工作模式还没选中会话：没有历史可拉，也不该去问后端要 web 的
  const version = rec.version
  let list = []
  let paused = null
  try {
    const r = await api('GET', '/api/chat/history?session=' + encodeURIComponent(key))
    list = r.messages || []
    paused = r.paused || null
  } catch (e) { /* 拉取失败按空会话处理 */ }
  if (disposed || version !== rec.version || rec.phase === 'streaming') return
  rec.messages.splice(0, rec.messages.length, ...list.map(m => ({
    role: m.role === 'user' ? 'user' : 'bot', text: m.text, blocks: histBlocks(m),
    turn: m.turn || null,
  })))
  // 恢复暂停现场：渲染被暂停的问答，发送键变为"继续"。
  if (paused && paused.question) {
    const partial = paused.partial || ''
    rec.messages.push({ role: 'user', text: paused.question })
    rec.messages.push({ role: 'bot', text: partial, paused: true, blocks: histBlocks({ text: partial }) })
    rec.phase = 'paused'
    rec.pausedTurn = paused
  } else {
    rec.phase = 'idle'
    rec.pausedTurn = null
  }
  if (rec.messages.length && sessionKey.value === key) nextTick(() => stick(true))
}

/* ---------- 附件：归一化压缩 + 预览 + 上传 ---------- */
function removePreview(p) {
  const rec = cur.value
  if (!rec) return
  // 回复中也允许撤：发出去的那批已经挪进 messages（URL 由那边保着），
  // 这里剩下的是这轮新选的，撤掉不影响正在跑的那次请求。
  const i = rec.images.indexOf(p)
  if (i >= 0) { rec.images.splice(i, 1); releaseUnusedImages() }
}
// 回收没人引用的 object URL。判断范围是**所有**会话的现场——别的会话还带着预览，
// 当前这条撤掉一张不该把人家那张也 revoke 掉。
function releaseUnusedImages() {
  const used = new Set()
  for (const rec of Object.values(records)) {
    if (!rec) continue
    for (const p of rec.images) used.add(p.url)
    for (const m of rec.messages) for (const url of m.images || []) used.add(url)
  }
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
  const rec = cur.value
  if (!rec) return
  for (const f of files) {
    // 回复中也允许加：发出去的那批在 sendChat 里已经快照走人，这里新加的
    // 只挂在这条会话的待发送附件上，等下一条消息用（清历史/离开页面仍会回收 URL）。
    if (clearing.value || disposed || noSession.value) break
    if (!imageSupported.value) { toast('当前模型不支持图片输入', true); break }
    if (rec.images.length >= 4) { toast('一条消息最多 4 张图片', true); break }
    if (!f.type.startsWith('image/')) { toast('仅支持图片文件', true); continue }
    let r
    try { r = await normalizeImage(f) } catch { r = null }
    if (clearing.value || disposed || cur.value !== rec) break
    if (!r) { toast('图片无法解码，已跳过', true); continue }
    if (rec.images.length >= 4) { toast('一条消息最多 4 张图片', true); break }
    if (r.down) toast('图片过大，已降采样：' + r.note)
    const url = URL.createObjectURL(r.file)
    imageURLs.add(url)
    rec.images.push({ file: r.file, url })
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
// 回复中这条会话的输入框是禁的（对话页没有排队），凡是往输入框里写东西的入口这时都要说清原因，
// 别让人对着一个点不动的按钮猜。限制只针对**这一条会话**：换一条会话、换个模式都不受影响。
// 权限模式与「添加图片」不受这条限制。
const BUSY_INPUT_HINT = '这条会话正在回复：等这轮跑完（或先点停止）再改这里的输入'
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
// ctxSnap 是每条会话自己那份（在记录里，见上面的现场声明）：这里只留弹层开关。
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
// loadCtx 取某条会话的上下文占比，写进它自己的记录（默认就是当前这条）。
// 后台那一轮结束时也要刷自己那份，不能把数字写到用户已经切走的那条会话上。
async function loadCtx(rec = cur.value, key = sessionKey.value) {
  if (!rec) return
  if (!key) { rec.ctx = null; return }
  try {
    const s = await api('GET', '/api/context?session=' + encodeURIComponent(key))
    rec.ctx = s && s.enabled ? s : null
  } catch (_) { rec.ctx = null }
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
  const rec = cur.value
  if (ctxBusy.value || busy.value || !rec) return
  ctxBusy.value = true
  try {
    const r = await api('POST', '/api/context/compact', { session: sessionKey.value })
    if (r.after && r.after.enabled) rec.ctx = r.after
    toast(r.message || '上下文已压缩')
  } catch (e) { toast('压缩失败：' + e.message, true) } finally { ctxBusy.value = false }
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
  try { await api('POST', '/api/chat/stop', { session: sessionKey.value }) }
  catch (e) { if (!disposed) toast(e.message, true) }
  // 服务端取消后以 {paused:true} 收尾流，状态在 streamTurn 的帧处理里落定
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
// streamTurn 跑一次 SSE 对话（发送与继续共用）：rec/key 是这一轮归属的那条会话现场
//（切走也不换：写的一直是它自己那份）；userMsg 为乐观插入的用户气泡（继续时为 null）；
// prepare 在建立连接前上传图片返回附件 ID；出错时按 draft 恢复输入现场
//（继续失败则回到"已暂停"状态）。
async function streamTurn({ rec, key, url, body, userMsg, draft, prepare }) {
  rec.version++
  const controller = new AbortController()
  rec.controller = controller
  const typing = reactive({ role: 'typing' })
  let submitted = false
  let bot = null
  // 这一轮的现场是不是正摊在屏幕上：滚动、气泡入场动画、聚焦这类"看得见才有意义"
  // 的动作只在是的时候做；写数据则一直写 rec，切走了也照收。
  const visible = () => !disposed && sessionKey.value === key
  const scroll = force => { if (visible()) nextTick(() => stick(force)) }
  const removeTyping = () => {
    const i = rec.messages.indexOf(typing)
    if (i >= 0) rec.messages.splice(i, 1)
  }
  let reader
  try {
    if (prepare) {
      const ids = await prepare(controller.signal)
      if (disposed || controller.signal.aborted) return
      body = { ...body, attachments: ids }
    }
    if (userMsg) rec.messages.push(userMsg)
    rec.messages.push(typing)
    submitted = true
    if (visible()) popLastBubble()
    scroll(true)
    const res = await fetch(url, {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      body: JSON.stringify(body),
      signal: controller.signal,
    })
    if (res.status === 403) {
      authFailed()
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
      if (!bot) { bot = newBot(); rec.messages.push(bot) }
      return bot
    }
    const handle = ev => {
      let data
      try { data = JSON.parse(ev) } catch { throw new Error('聊天响应格式异常') }
      if (data.error) throw new Error(data.error)
      if (data.thinking) {
        pushThinking(ensureBot(), data.thinking)
        scroll()
      }
      if (data.step) {
        const b = ensureBot()
        if (data.step.kind === 'round') finishRound(b, data.step.detail)
        else pushToolStep(b, data.step)
        scroll()
      }
      if (data.plan) {
        ensureBot().plan = data.plan
        watchPlan(data.plan.id)
        scroll()
      }
      if (data.delta) {
        pushText(ensureBot(), data.delta)
        scroll()
      }
      if (data.paused) {
        // 对话被暂停：保留已生成部分，发送键变为"继续"。
        paused = true
        removeTyping()
        if (bot) { bot.paused = true; bot.streaming = false }
        rec.phase = 'paused'
        rec.pausedTurn = {
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
      rec.messages.push(bot)
    }
    if (bot && turnTotal) bot.turn = turnTotal
    // 本轮结束（含被暂停）：刷新这条会话自己的上下文占比（不是当前屏幕那份）
    loadCtx(rec, key)
    if (!paused) {
      // 正常完成：清掉历史暂停标记（任务已续完或已放弃），回到空闲。
      rec.phase = 'idle'
      rec.pausedTurn = null
      if (bot) bot.streaming = false
      for (const m of rec.messages) { if (m.paused) m.paused = false }
    }
    scroll()
    // 调用轨迹收进了设置弹窗：节挂载着才会收到，未挂载时无监听者、空抛无害
    window.dispatchEvent(new CustomEvent('friday:trace-refresh'))
  } catch (e) {
    if (disposed || controller.signal.aborted || !store.loggedIn) return
    if (draft) {
      // 输入现场回到**这条**会话的记录里：用户可能已经切走了，回来还能重试
      rec.draft = draft.text
      rec.mode = draft.mode
      rec.images.push(...draft.images)
      if (visible()) composerEl.value && composerEl.value.focus()
    } else {
      rec.phase = 'paused' // 继续失败：回到可继续状态
    }
    if (submitted) {
      // 未收到回复时撤销乐观消息；已有部分输出则保留，方便检查失败前的结果。
      if (!bot && userMsg) {
        const i = rec.messages.indexOf(userMsg)
        if (i >= 0) rec.messages.splice(i, 1)
      }
      rec.messages.push({ role: 'err', text: e.message + (draft ? '，输入和图片已保留，可以重试。' : '') })
      if (visible()) popLastBubble()
    } else { toast(e.message + (draft ? '，输入和图片已保留，可以重试。' : ''), true) }
  } finally {
    if (reader) {
      try { await reader.cancel() } catch { /* 连接已关闭 */ }
      reader.releaseLock()
    }
    removeTyping()
    if (bot && rec.phase !== 'paused') {
      // 本轮已了结（完成或出错断流）：漏收尾的工具块不会再有结果事件了，就地收掉
      bot.streaming = false
      closeRunningTools(bot)
    }
    if (rec.controller === controller) rec.controller = null
    if (rec.phase === 'streaming') rec.phase = 'idle' // 兜底归位
    // 工作模式：会话名按首条提问自动取，消息数与"多久之前"也在动，跑完一轮补一次列表。
    if (workMode.value && !disposed) loadSessions()
    if (visible()) nextTick(() => composerEl.value && composerEl.value.focus())
  }
}

async function sendChat(presetText) {
  const rec = cur.value
  const key = sessionKey.value
  if (!rec || clearing.value || disposed || rec.phase === 'streaming') return
  if (noSession.value) { toast('先新建一条会话再开工', true); return }
  const draft = {
    text: typeof presetText === 'string' ? presetText : rec.draft,
    mode: rec.mode,
    images: [...rec.images],
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
  rec.phase = 'streaming'
  rec.pausedTurn = null
  for (const m of rec.messages) { if (m.paused) m.paused = false }
  rec.mode = null
  rec.draft = ''
  // 把预览交给此次请求，保留 URL 直到对应消息/草稿被移除。
  rec.images.splice(0, rec.images.length)
  const user = reactive({ role: 'user', text: raw, images: draft.images.map(p => p.url) })
  await streamTurn({
    rec, key,
    url: '/api/chat',
    body: { message: raw, session: key, mode: reqMode },
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
    // 计划卡挂在**哪条会话**上都得跟着刷新：切走的那条仍在执行，轮询不该只盯屏幕上这份
    let found = false
    for (const rec of Object.values(records)) {
      const target = rec.messages.find(m => m.plan && m.plan.id === id)
      if (!target) continue
      found = true
      if (r.plan) target.plan = { ...target.plan, ...r.plan }
    }
    if (!found) stopWatchingPlan(id)
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
  const rec = cur.value
  const key = sessionKey.value
  if (!rec || rec.phase !== 'paused' || clearing.value || disposed) return
  rec.phase = 'streaming'
  await streamTurn({ rec, key, url: '/api/chat/resume', body: { session: key } })
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
// bootstrap 起手：生活模式落到主会话（web）那份现场；工作模式先挑一条工作会话。
// 两条路都走 applySession，历史与占用按那条 key 拉——不能拿主会话的历史开台。
function bootstrap() {
  if (workMode.value) return pickSession()
  return applySession('web')
}
onMounted(() => {
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKeydown)
  window.addEventListener('friday:perm', onPermSync)
  window.addEventListener('friday:setup-refresh', loadModels) // 设置页保存模型库后 chip 跟上
  bootstrap()
  loadPerm()
  loadModels()
})
onUnmounted(() => {
  disposed = true
  // 每条会话自己的那一轮都要取消：组件卸载意味着登录态没了或页面要走，
  // 留着只会往已经没人看的现场上写字。
  for (const rec of Object.values(records)) rec.controller?.abort()
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
  const rec = cur.value
  if (!rec || busy.value || clearing.value || noSession.value) return
  menuOpen.value = false
  ctxOpen.value = false
  clearing.value = true
  rec.version++ // 让这条会话在途的历史请求作废（回来时不该再覆盖已清空的现场）
  try {
    const r = await api('POST', '/api/context/clear', { session: sessionKey.value })
    if (disposed) return
    rec.messages.splice(0, rec.messages.length)
    rec.phase = 'idle'
    rec.pausedTurn = null
    releaseUnusedImages()
    rec.ctx = null
    loadCtx(rec, sessionKey.value) // 清空后占比归零，chip 要立刻反映，不能等下一次问答
    toast(r.message || '上下文已清除')
  } catch (e) { if (!disposed) toast(e.message, true) }
  finally { clearing.value = false }
}
</script>

<template>
  <section id="view-chat" class="view" :class="{ 'chat-split': workMode }">
    <!-- 工作模式的会话栏：一条会话一段上下文。交互跟工作区页那套列表一致——
         点行切换、双击改名（留空回到按首条提问自动取名）、行尾 × 删除。 -->
    <aside v-if="workMode" class="ag-rail">
      <div class="ag-rail-head" data-tauri-drag-region>
        <b>会话</b>
        <div class="ag-rail-acts">
          <button class="ib-btn" :class="{ active: creating }" aria-label="新建会话"
                  title="新建会话" @click="openSessionForm">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
                 stroke-linecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>
          </button>
        </div>
      </div>

      <!-- 新建会话：名字与项目目录都是可选的。目录建好后不可换（换 = 再建一条），
           跟工作区会话"主目录创建后不可切"是同一条纪律 -->
      <div v-if="creating" class="ag-form">
        <label class="meta">名字（留空按第一句提问自动取）</label>
        <input v-model="sessionForm.title" aria-label="会话名字" placeholder="比如 季度 OKR">
        <label class="meta">项目目录（可选）</label>
        <p v-if="sessionForm.dir" class="ws-dir" :title="sessionForm.dir">
          <span class="ws-dir-txt">{{ sessionForm.dir }}</span>
          <span class="ag-del" role="button" aria-label="不指定目录" title="不指定目录"
                @click="sessionForm.dir = ''">×</span>
        </p>
        <button v-else type="button" class="ag-ws-pick" title="选择项目目录" @click="pickSessionDir">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
               stroke-linecap="round" aria-hidden="true"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>
          <span>选择目录…</span>
        </button>
        <p class="meta ag-ws-hint">选了目录，这条会话读写文件、交代项目约定（AGENTS.md / README.md）都在这个目录里；
          不选就沿用日常目录。建好后不能换。</p>
        <div class="ag-form-acts">
          <button class="btn sm primary" @click="createSession">创建</button>
          <button class="btn sm" @click="creating = false">取消</button>
        </div>
      </div>
      <div class="ag-list">
        <div v-for="s in sessions" :key="s.key" class="ag-item" role="button" tabindex="0"
             :class="{ active: s.key === sessionKey }"
             @click="selectSession(s.key)" @keydown.enter.prevent="selectSession(s.key)">
          <span class="ag-item-main">
            <span class="ag-title">
              <input v-if="edit.key === s.key" v-model="edit.title" class="ag-title-edit"
                     aria-label="会话名" placeholder="留空按首条提问自动取名"
                     @click.stop @keydown.enter.prevent="commitRename" @blur="commitRename">
              <span v-else @dblclick.stop="startRename(s)" :title="'双击改名：' + (s.title || '新会话')">{{ s.title || '新会话' }}</span>
            </span>
            <!-- 行上不铺会话 key：说了几条、多久之前动过（key 只在设置页的会话列表里看） -->
            <span class="meta">{{ rowMeta(s) }}</span>
          </span>
          <!-- 后台那一轮也标出来：切走之后这条还在收字，行上不说就没人知道它没结束 -->
          <span v-if="generating(s.key)" class="ag-gen"
                :title="s.key === sessionKey ? '这一轮正在回复' : '这一轮还在后台跑，切到别的会话不会打断它'"
          >回复中</span>
          <span class="ag-del" role="button" :aria-label="'删除会话 ' + (s.title || '新会话')"
                title="删除会话" @click.stop="removeSession(s)">×</span>
        </div>
        <div v-if="!sessions.length" class="ag-empty meta">还没有会话，点上面的 + 建一条。</div>
      </div>
    </aside>

    <div id="chatHead" data-tauri-drag-region>
      <div class="title">{{ headTitle }}</div>
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
      <!-- 工作模式一条会话都还没有：明说没有会话，给一个建会话的入口，
           不拿主会话顶上，也不凭空造一条空会话摆在那儿 -->
      <div v-if="noSession" class="welcome">
        <div class="brand-mark welcome-logo"></div>
        <h3 class="welcome-title">工作模式还没有会话</h3>
        <p class="welcome-sub">一条会话一段上下文，互不串。建一条就能开工。</p>
        <div class="welcome-suggests">
          <button class="suggest" :disabled="clearing" @click="openSessionForm">新建第一条会话</button>
        </div>
      </div>
      <!-- 欢迎态（这条会话还没内容） -->
      <div v-else-if="welcome" class="welcome">
        <div class="brand-mark welcome-logo"></div>
        <h3 class="welcome-title" ref="titleEl">{{ workMode ? '这条会话还是空的' : '你好，我是小五' }}</h3>
        <p v-if="workMode" class="welcome-sub">说点什么开始——这段上下文只属于这条会话<template v-if="activeSession?.dir">，文件读写都在 {{ baseName(activeSession.dir) }} 里</template>。</p>
        <p v-else class="welcome-sub">你的私人 AI 生活管家 · <span ref="rotatorEl">提醒、待办、天气，都可以交给我</span><span class="caret"></span></p>
        <div v-if="!workMode" class="welcome-suggests">
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
            <TimelineBlocks :blocks="m.blocks" :streaming="m.streaming" :paused="m.paused" :round="m.round"
                            own-tools @preview="path => fileDrawer.open(path)" />
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
            <div class="msg-acts">
              <CopyMessageButton :text="m.text" :kind="m.role === 'err' ? 'error' : 'question'" />
            </div>
          </div>
        </div>
      </div>
    </div>

    <div id="chatFoot">
      <ChatComposer ref="composerEl" v-model="input" :placeholder="placeholder" aria-label="聊天输入"
                    :busy="phase === 'streaming'" :disabled="clearing || noSession"
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
    <DirPickerDialog ref="dirDialog" />
  </section>
</template>
