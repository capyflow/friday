<script setup>
// 工作区（ACP）：左侧「工作区 → 会话」两级栏，右侧进入某个会话后是原来的对话时间线。
// 工作区 = 一组目录（第一个是主目录）；会话在工作区里创建，创建时选 agent 工具与主目录，
// 之后不可切换（换 = 再建一个会话），工作区其余目录作为 additionalDirectories 扩展根下发
// （agent 声明支持才生效，生效情况在会话头部明说）。SSE 帧协议与聊天页一致，时间线块复用
// TimelineBlocks/blocks.js；status 帧显示在"输入中"气泡里（启动 agent /
// 重接会话等生命周期占位）。note 行是服务端投影的系统标注（上下文已重置、
// 中断原因），灰字居中展示。
// 一个会话正在回答时照样可以切到别的会话继续派活：会话状态按 thread 分开放
// （见下方 states），切走不打断进行中的一轮，各会话自己写自己的缓冲，列表上
// 标出哪几个正在生成。
// 在 App 里用 v-show 常驻（同聊天页）：切换左侧导航不卸载组件，进行中的一轮
// SSE 与滚动位置都不会丢。首次打开先拉配置/列表，拿到结果前显示加载态，
// 不能把"还没加载完"当成"功能未开启"。
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { api, authFailed } from '../api.js'
import { authHeaders, mediaURL } from '../auth.js'
import { popIn } from '../anim.js'
import { pushThinking, pushToolStep, pushText, closeRunningTools, turnTotalText, answerText } from '../blocks.js'
import { fmtTokens } from '../format.js'
import { getPref, setPref } from '../prefs.js'
import { store, toast } from '../store.js'
import TimelineBlocks from './TimelineBlocks.vue'
import CopyMessageButton from './CopyMessageButton.vue'
import ChatComposer from './ChatComposer.vue'
import FilePreviewDrawer from './FilePreviewDrawer.vue'

const threads = ref([])
const workspaces = ref([])     // 工作区（一组目录 + 挂在它下面的会话）
const acp = ref(null)          // /api/acp/threads 回带的配置（enabled/agents…）
const loaded = ref(false)      // 配置/列表是否已拉到：加载中 ≠ 功能未开启
const loadError = ref('')      // 首次加载失败原因（可重试）
const currentId = ref('')      // 正在"显示"哪个会话（thread）
const currentWsId = ref('')    // 正在展开哪个工作区（'' = 全收起）
// 左栏两种视图（照主流 agent 工具的项目栏习惯）：按工作区分组 / 跨工作区按最近活动
const VIEW_KEY = 'friday-acp-view'
const SEEN_KEY = 'friday-acp-seen'
const view = ref(readView())
function readView() {
  const v = getPref(VIEW_KEY)
  return v === 'recent' ? 'recent' : 'grouped'
}
function setView(v) {
  view.value = v
  setPref(VIEW_KEY, v)
}
// 每条会话上次被打开的时刻（未读判定用）：本机展示偏好，跟着 prefs 一起冷备
const seen = reactive(readSeen())
function readSeen() {
  const out = {}
  try {
    const raw = JSON.parse(getPref(SEEN_KEY) || '{}')
    for (const [k, v] of Object.entries(raw)) if (typeof v === 'string') out[k] = v
  } catch { /* 坏数据当没记录 */ }
  return out
}
function markSeen(id) {
  const t = threads.value.find(x => x.id === id)
  seen[id] = (t?.updatedAt) || new Date().toISOString()
  // 只留当前还存在的会话，别的设备/重装后不留脏键（prefs 单值上限 8KB）
  const live = new Set(threads.value.map(x => x.id))
  for (const k of Object.keys(seen)) if (!live.has(k)) delete seen[k]
  setPref(SEEN_KEY, JSON.stringify(seen))
}
function isUnread(t) {
  if (t.id === currentId.value) return false
  const at = seen[t.id]
  return !!at && !!t.updatedAt && t.updatedAt > at
}
const showAllWs = reactive(new Set()) // 点过「显示更多」的工作区
const SESSION_PREVIEW = 3             // 每组默认先露出最近 3 条会话
const scroller = ref(null)
const inputEl = ref(null)
const creatingWs = ref(false)  // 新建工作区表单展开
const wsForm = reactive({ name: '', dirs: [] })
const sessionFor = ref('')     // 正在新建会话的工作区 id（'' = 没在建）
const sessionForm = reactive({ agent: '' })
const confirmWsId = ref('')    // 二次确认删除的工作区 id
const confirmId = ref('')      // 二次确认删除的 thread id
const fileInput = ref(null)
const fileDrawer = ref(null)       // 文件预览抽屉（时间线里点文件路径 chip 打开）

/* 每个 thread 一份前端态：消息缓冲、进行中的一轮、能力清单、上下文水位、输入草稿、
   待发的图片。currentId 只是"现在显示哪一份"——切换 thread 不打断别的 thread 正在
   跑的一轮：它的 SSE 继续往自己的缓冲里写，切回去就是实时进度（服务端同一 thread
   的 turn 串行、不同 thread 可并行，上限 acp.maxConcurrent）。
   AbortController 放在响应式之外：原生对象经 proxy 调用 abort() 会丢内部状态。 */
const states = reactive({})    // thread id -> 该会话的前端态
const inflight = new Map()     // thread id -> AbortController

function stateOf(id) {
  if (!id) return null
  if (!states[id]) {
    states[id] = {
      loaded: false,    // 历史投影是否已取回（取回过就用本机缓冲，不再盖）
      fetching: false,  // 取历史在路上（连点列表不重复发 GET）
      messages: [],
      phase: 'idle',    // idle | streaming —— 这一会话自己的一轮
      caps: null, capsLoading: false, capBusy: false,
      ctx: null,        // { used, size }；null = 该 agent 未上报
      session: null,    // { cwd, dirs, extra, known, additionalDirectories } —— 目录生效情况
      draft: '', pending: [], // 输入草稿与待发的图片也按会话分开
      queue: [],        // 回答进行中排队的消息：{ text, shots:[{id,url}] }，本轮跑完依次发出
      queuePaused: false, // 暂停后本轮结束也不会继续放队列上路
    }
  }
  return states[id]
}
function dropState(id) {
  const st = states[id]
  if (!st) return
  // 缓冲里的对象 URL 用完即释放（含排着没发的图片）
  for (const p of st.pending) URL.revokeObjectURL(p.url)
  for (const q of st.queue) for (const s of q.shots) URL.revokeObjectURL(s.url)
  delete states[id]
}

const cur = computed(() => stateOf(currentId.value))
const messages = computed(() => (cur.value ? cur.value.messages : []))
const phase = computed(() => (cur.value ? cur.value.phase : 'idle'))
const busy = computed(() => phase.value === 'streaming')
const caps = computed(() => (cur.value ? cur.value.caps : null))
const capsLoading = computed(() => !!(cur.value && cur.value.capsLoading))
const capBusy = computed(() => !!(cur.value && cur.value.capBusy))
const ctx = computed(() => (cur.value ? cur.value.ctx : null))
const pendingImages = computed(() => (cur.value ? cur.value.pending : []))
const input = computed({
  get: () => (cur.value ? cur.value.draft : ''),
  set: v => { if (cur.value) cur.value.draft = v },
})
// 后台正在生成的会话（列表据此标"生成中"，不依赖只在轮末刷新的服务端状态）
function generating(id) { return !!states[id] && states[id].phase === 'streaming' }

/* 会话能力（统一清单）：后端把 configOptions 与协议原生 models/modes 归一成
   Capability 列表（kind=model/thought_level/approval_mode/other），切换按
   source 路由到 set_config_option / set_model / set_mode。协议可选能力——
   agent 不支持的能力直接缺席，对应 chip 不渲染。选中线程即拉清单（Zed 同款：
   不发消息也能切模型/思考强度）——清单只能在建会话后获得，GET 会顺带把 agent
   拉起建会话（新线程因此「选中即启动」，冷启动期间显示加载 chip），之后每轮
   结束刷新。 */
const capOpen = ref('')         // 展开中的能力 id（'' = 全收起）
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
const ctxOpen = ref(false)
const ctxWrap = ref(null)
const ctxPct = computed(() => (ctx.value ? Math.round(ctx.value.used / ctx.value.size * 100) : 0))
const ctxBarPct = computed(() => Math.min(ctxPct.value, 100)) // 超窗时进度条封顶，数字照实显示
const ctxHot = computed(() => !!ctx.value && ctxPct.value >= 95)
// 水位归属该 thread：后台会话跑完一轮也能刷新自己的数字，不会串到正在显示的会话上
function syncCtx(id, used, size) {
  const st = stateOf(id)
  if (!st) return
  st.ctx = size > 0 ? { used: used || 0, size } : null
  if (!st.ctx && id === currentId.value) ctxOpen.value = false
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
  const st = stateOf(id)
  if (!st || st.capsLoading) return null
  st.capsLoading = true
  try {
    const r = await api('GET', `/api/acp/threads/${id}/options`)
    st.caps = r.capabilities || [] // 写回它自己的会话：期间切走了也不串台
    if (r.session) st.session = r.session // 清单这条路径必然已拉起进程：顺带刷新目录生效情况
    return null
  } catch (e) { return e } // agent 拉起失败等：保持未加载态，下次选中重试
  finally { if (states[id]) st.capsLoading = false }
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
  const id = currentId.value
  const st = stateOf(id)
  if (!st || st.capBusy || st.phase === 'streaming') return
  if (value === cap.currentValue) { capOpen.value = ''; return }
  st.capBusy = true
  try {
    const r = await api('POST', `/api/acp/threads/${id}/options`, { id: cap.id, value })
    st.caps = r.capabilities || []
    const fresh = (st.caps || []).find(c => c.id === cap.id)
    toast(capLabel(cap) + '已切换为 ' + (fresh ? capCurrentName(fresh) : value))
    capOpen.value = ''
  } catch (e) { toast('切换失败：' + e.message, true) } finally { if (states[id]) st.capBusy = false }
}

const current = computed(() => threads.value.find(t => t.id === currentId.value) || null)
const currentWs = computed(() => workspaces.value.find(w => w.id === currentWsId.value) || null)
const enabled = computed(() => !!(acp.value && acp.value.enabled))
const canSend = computed(() => !!input.value.trim() || pendingImages.value.length > 0)
const stateLabel = { idle: '空闲', running: '对话中', interrupted: '已中断', orphaned: 'agent 已删除' }

// 工作区 → 会话的两级栏。排序按主流 agent 工具的项目栏习惯：置顶优先，其余按
// 组内最近活动（会话的最新 updatedAt，没有会话就用工作区自身时间）倒序；
// 组内会话同样最近优先，默认只露出最近几条，多的收在「显示更多」里。
function ts(v) { return v || '' }
function wsThreadsWithOrder(wsId) {
  return threads.value
    .filter(t => t.workspaceId === wsId)
    .sort((a, b) => ts(b.updatedAt).localeCompare(ts(a.updatedAt)))
}
function wsThreads(wsId) { return wsThreadsWithOrder(wsId) }
function wsActivity(ws) {
  const latest = wsThreadsWithOrder(ws.id)[0]
  return ts(latest?.updatedAt) || ts(ws.updatedAt) || ts(ws.createdAt)
}
function orderedWorkspaces() {
  return workspaces.value
    .slice()
    .sort((a, b) => {
      if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1
      return ts(wsActivity(b)).localeCompare(ts(wsActivity(a)))
    })
}
function visibleSessions(ws) {
  const list = wsThreadsWithOrder(ws.id)
  return showAllWs.has(ws.id) ? list : list.slice(0, SESSION_PREVIEW)
}
function hiddenCount(ws) { return Math.max(0, wsThreadsWithOrder(ws.id).length - visibleSessions(ws).length) }
function wsGenerating(id) { return wsThreadsWithOrder(id).filter(t => generating(t.id)).length }
function wsUnread(id) { return wsThreadsWithOrder(id).filter(isUnread).length }
// 「最近更新」视图：跨工作区摊平，行上标出它属于哪个工作区
const recentThreads = computed(() => threads.value
  .slice()
  .sort((a, b) => ts(b.updatedAt).localeCompare(ts(a.updatedAt)))
  .slice(0, 30))
function wsName(id) { return workspaces.value.find(w => w.id === id)?.name || '（工作区已删除）' }
function missingDirs(ws) { return ws.missingDirs || [] }
// 会话行上给"多久之前动过"，取代原来的目录名（列表里不铺路径）
function ago(iso) {
  const t = Date.parse(iso || '')
  if (!t) return ''
  const min = Math.floor((Date.now() - t) / 60000)
  if (min < 1) return '刚刚'
  if (min < 60) return min + ' 分钟'
  const hr = Math.floor(min / 60)
  if (hr < 24) return hr + ' 小时'
  const day = Math.floor(hr / 24)
  if (day < 30) return day + ' 天'
  return Math.floor(day / 30) + ' 个月'
}
// 目录路径太长，列表行只展示最后一段（完整路径在 title 里）
function base(p) {
  const s = (p || '').replace(/\/+$/, '')
  if (!s) return p || ''
  const i = s.lastIndexOf('/')
  return i >= 0 ? s.slice(i + 1) : s
}

// 目录列表变了（加/移）就把头部的"+N 个扩展目录"跟上：那份数字来自工作区当前目录，
// 而是否真的下发给 agent（known/additionalDirectories）仍沿用上次握手的结论，不猜。
function refreshDirsView() {
  const t = current.value
  const st = t ? states[t.id] : null
  if (!st || !st.session) return
  const ws = workspaces.value.find(w => w.id === t.workspaceId)
  st.session = { ...st.session, dirs: ws ? ws.dirs : [] }
}

const session = computed(() => (cur.value ? cur.value.session : null))
// 目录语义一行说清：ACP 只有一个 cwd，其余目录靠 additionalDirectories 能力——
// 没握手过就明说"待确认"，不支持就明说"不生效"，不假装多根生效。
const dirsNote = computed(() => {
  const s = session.value
  if (!s || !s.cwd) return ''
  const extra = (s.dirs || []).filter(d => d !== s.cwd).length
  if (!extra) return ''
  if (s.known) return s.additionalDirectories ? `+${extra} 个扩展目录已下发` : `另 ${extra} 个目录不生效（本 agent 只认主目录）`
  return `+${extra} 个扩展目录（生效情况待 agent 握手确认）`
})

const STATE_HINT = {
  interrupted: '上次会话被中断（服务重启或进程退出）。继续发消息会重新拉起 agent 并尝试重接上下文。',
  orphaned: '该会话绑定的智能体已被删除；历史仍可查看，但不能继续对话。',
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
  if (wsMenu.value && !e.target.closest('.ws-menu-wrap')) closeWsMenu()
  if (threadMenu.value && !e.target.closest('.ag-menu-wrap')) closeThreadMenu()
  if (popOpen.value && popWrap.value && !popWrap.value.contains(e.target)) setPop(false)
  if (capOpen.value && capWrap.value && !capWrap.value.contains(e.target)) capOpen.value = ''
  if (ctxOpen.value && ctxWrap.value && !ctxWrap.value.contains(e.target)) ctxOpen.value = false
}
/* Esc 的优先级：有弹层/菜单/弹窗就先收它们；都没有、焦点又在对话区、这一轮还在
   跑，就中断本轮。输入框自己会先处理（ChatComposer 里有 @keydown），它 preventDefault
   过的这里不再重复动作。全屏退出是系统/浏览器保留给 Esc 的行为，JS 拦不住。 */
function onKey(e) {
  if (e.key !== 'Escape' || e.defaultPrevented) return
  const hasLayer = popOpen.value || capOpen.value || ctxOpen.value || !!wsMenu.value ||
    !!threadMenu.value || wsDlg.open
  if (hasLayer) {
    e.preventDefault()
    setPop(false); capOpen.value = ''; ctxOpen.value = false
    closeWsMenu(); closeThreadMenu()
    if (wsDlg.open) closeWsDialog() // 弹窗里按 Esc 直接放弃暂存的编辑
    return
  }
  if (busy.value && e.target && typeof e.target.closest === 'function' && e.target.closest('.ag-foot, .ag-scroller')) {
    e.preventDefault()
    stop()
    return
  }
  // 没有弹层、也没有在跑的轮次：Esc 收起应用内全屏（普通窗口布局下什么都不做）
  if (store.fullscreen) {
    e.preventDefault()
    store.fullscreen = false
  }
}

async function load() {
  try {
    const r = await api('GET', '/api/acp/threads')
    threads.value = r.threads || []
    workspaces.value = r.workspaces || []
    acp.value = r.acp || { enabled: false, agents: [] }
    loaded.value = true
    loadError.value = ''
    // 会话在别处被删了：本机这一轮没有落点了，中断它并收起缓冲
    for (const id of Object.keys(states)) {
      if (threads.value.some(t => t.id === id)) continue
      inflight.get(id)?.abort()
      inflight.delete(id)
      dropState(id)
    }
    if (currentId.value && !threads.value.some(t => t.id === currentId.value)) currentId.value = ''
    // 展开着的工作区没了（本机删的或别处删的）：换第一个，保证目录与「新建会话」有落点
    if (currentWsId.value && !workspaces.value.some(w => w.id === currentWsId.value)) {
      currentWsId.value = workspaces.value.length ? workspaces.value[0].id : ''
    }
    // 刷新列表时顺带更新各会话的水位；正在跑的一轮以它 SSE 里 done 帧的数字为准，
    // 别拿上一轮的旧值盖掉实时进度。
    for (const t of threads.value) {
      if (states[t.id] && states[t.id].phase !== 'streaming') syncCtx(t.id, t.contextUsed, t.contextSize)
    }
    if (!currentId.value && threads.value.length) select(threads.value[0].id)
    // 一个会话都没有时也要摊开一个工作区：否则左栏全是折叠行，"新建会话"没有落点
    else if (!currentWsId.value && workspaces.value.length) currentWsId.value = workspaces.value[0].id
    refreshDirsView()
  } catch (e) {
    // 已经加载过（轮末刷新失败）就保留现有界面；首次加载失败给可重试的说明。
    if (!loaded.value) loadError.value = e.message || String(e)
    toast('加载工作区失败：' + e.message, true)
  }
}

/* 切换会话：换"显示哪一份缓冲"，不打断任何会话正在跑的一轮。 */
async function select(id) {
  if (!id || id === currentId.value) return
  const prev = currentId.value
  currentId.value = id
  markSeen(id) // 打开即算读过：未读点的语义就是"这条会话在你没看着的时候又动过"
  const owner = threads.value.find(t => t.id === id)
  if (owner?.workspaceId) currentWsId.value = owner.workspaceId
  capOpen.value = ''
  ctxOpen.value = false
  popOpen.value = false
  sessionFor.value = '' // 进了会话就收起"新建会话"表单
  const st = stateOf(id)
  if (!st.loaded) {
    if (st.fetching) return // 同一次拉取已在飞（连点列表），别发第二个 GET
    st.fetching = true
    try {
      const r = await api('GET', '/api/acp/threads/' + id)
      // 等回包期间用户可能已经发了一条出去：那一轮的内容只在本机缓冲里（服务端
      // 要到轮末才落投影），此时不能用历史投影盖掉它。
      if (st.phase !== 'streaming') {
        st.messages.length = 0
        for (const e of (r.thread.messages || [])) {
          if (e.role === 'user') {
            st.messages.push({ role: 'user', text: e.text,
              images: (e.files || []).map(f => '/api/upload/' + f), imgCount: e.images || 0 })
          } else if (e.role === 'note') {
            st.messages.push({ role: 'note', text: e.text })
          } else {
            st.messages.push({ role: 'bot', text: e.text, streaming: false, paused: false,
              blocks: [{ kind: 'text', text: e.text, round: 0 }],
              turn: (e.in || e.out || e.ms) ? { in: e.in, out: e.out, ms: e.ms, stopped: e.stopped } : null })
          }
        }
      }
      syncCtx(id, r.thread.contextUsed, r.thread.contextSize)
      if (r.session) st.session = r.session
      st.loaded = true
    } catch (e) {
      dropState(id) // 半成品缓冲会让下次重试更难判断，删掉重来
      if (currentId.value === id) {
        currentId.value = prev // 拉不到就别把空会话摆在眼前，留在原来那份界面上
        toast('加载对话失败：' + e.message, true)
        nextTick(() => stick(true))
      }
      return
    } finally {
      if (states[id]) st.fetching = false
    }
  }
  // 选中即拉清单（Zed 同款）：没会话的顺带建会话，冷启动可能数秒。
  // 该会话自己那一轮还在跑时不发——服务端按会话串行，options 请求会一直卡到轮末。
  if (st.phase !== 'streaming' && !st.capsLoading) {
    loadOptions(id).then(err => {
      if (err && currentId.value === id) toast('能力清单加载失败：' + (err.message || err), true)
    })
  }
  nextTick(() => {
    stick(true) // 打开会话默认落到最新一条（含切回实时进行中的那一轮）
    if (inputEl.value) inputEl.value.style.height = 'auto' // 不沿用上一个会话撑起来的高度
  })
}

/* ---------- 工作区：新建 / 目录增删 / 改名 / 删除 ---------- */
function enabledAgents() { return (acp.value?.agents || []).filter(a => a.enabled !== false) }

function openCreateWs() {
  creatingWs.value = !creatingWs.value
  if (!creatingWs.value) return
  wsForm.name = ''
  // 没想好目录也能建（后端回退临时工作区目录）；agent 配过默认目录就拿来做预填
  const first = enabledAgents()[0]
  wsForm.dirs = first && first.workspace ? [first.workspace] : []
}
function addWsFormDir(path) {
  if (path && !wsForm.dirs.includes(path)) wsForm.dirs.push(path)
}
function removeWsFormDir(i) { wsForm.dirs.splice(i, 1) }
async function submitWs() {
  try {
    const r = await api('POST', '/api/acp/workspaces', { name: wsForm.name.trim(), dirs: wsForm.dirs })
    creatingWs.value = false
    await load()
    currentWsId.value = r.workspace.id
    openSessionForm(r.workspace.id) // 建完工作区，下一步天然是建会话
    toast('已创建工作区，选个 agent 就能开聊')
  } catch (e) { toast('创建工作区失败：' + e.message, true) }
}

// 点工作区头＝展开/收起（再点一次收起，全收起也是合法状态）。展开时里面已有
// 会话就进到最近活动那条，空的直接把「新建会话」表单摊开——下一步总在最前面。
function toggleWs(id) {
  if (currentWsId.value === id) { currentWsId.value = ''; sessionFor.value = ''; return }
  currentWsId.value = id
  sessionFor.value = ''
  const list = wsThreadsWithOrder(id)
  if (list.some(t => t.id === currentId.value)) return
  if (list.length) select(list[0].id)
  else { currentId.value = ''; openSessionForm(id) }
}
function collapseAll() {
  currentWsId.value = ''
  sessionFor.value = ''
  showAllWs.clear()
}
function showMoreWs(id) { showAllWs.add(id) }
// 「最近更新」里点一条：选中它，同时把它所属的工作区标记成展开——
// 切回「按工作区」视图时直接落在那一组上，不用自己再找。
function openFromRecent(t) {
  if (t.workspaceId) currentWsId.value = t.workspaceId
  select(t.id)
}

async function patchWorkspace(id, body, okMsg) {
  try {
    await api('POST', '/api/acp/workspaces/' + id, body)
    await load()
    if (okMsg) toast(okMsg)
  } catch (e) { toast('更新工作区失败：' + e.message, true) }
}
function togglePin(ws) {
  patchWorkspace(ws.id, { pinned: !ws.pinned }, ws.pinned ? '已取消置顶' : '已置顶')
}
/* 「…」菜单：一组只留一个入口（编辑工作区 / 置顶 / 删除），新建会话另放一颗按钮。
   同一时刻只展开一组的菜单，点外面或 Esc 收起。 */
const wsMenu = ref('')          // 展开菜单的工作区 id
const threadMenu = ref('')      // 展开菜单的会话 id
const threadEdit = reactive({ id: '', title: '' }) // 行内改标题
function toggleWsMenu(id) { wsMenu.value = wsMenu.value === id ? '' : id }
function closeWsMenu() { wsMenu.value = '' }
function toggleThreadMenu(id) { threadMenu.value = threadMenu.value === id ? '' : id }
function closeThreadMenu() { threadMenu.value = '' }

// 会话标题：在行内改。留空 = 回到自动（下一轮按那时的消息重新派生）
function startRenameThread(t) {
  closeThreadMenu()
  threadEdit.id = t.id
  threadEdit.title = t.title || ''
}
async function commitRenameThread() {
  if (!threadEdit.id) return
  const id = threadEdit.id
  const title = threadEdit.title.trim()
  threadEdit.id = ''
  try {
    await api('POST', `/api/acp/threads/${id}`, { title })
    await load()
  } catch (e) { toast('改名失败：' + e.message, true) }
}

/* 编辑工作区弹窗：改名 + 源文件夹的增删与主目录，都在这里改。
   列表里默认不铺路径（一组一行只有名字和计数），要看/要改路径是这件明确的事。
   弹窗内是暂存编辑：点保存才按差异落到后端（改主目录要等目录先加进来）。 */
const wsDlg = reactive({ open: false, id: '', name: '', dirs: [], missing: [], saving: false })
function openWsDialog(ws) {
  closeWsMenu()
  wsDlg.open = true
  wsDlg.id = ws.id
  wsDlg.name = ws.name
  wsDlg.dirs = [...(ws.dirs || [])]
  wsDlg.missing = [...(ws.missingDirs || [])]
}
function closeWsDialog() { wsDlg.open = false }
function dlgAddDir() {
  openDirPicker(path => {
    if (wsDlg.dirs.includes(path)) { toast('这个文件夹已经在工作区里'); return }
    wsDlg.missing = wsDlg.missing.filter(d => d !== path)
    wsDlg.dirs.push(path)
  }, wsDlg.dirs[0] || '')
}
function dlgRemoveDir(i) { wsDlg.dirs.splice(i, 1) }
function dlgSetPrimary(i) {
  const [d] = wsDlg.dirs.splice(i, 1)
  wsDlg.dirs.unshift(d)
}
async function saveWsDialog() {
  if (!wsDlg.id) return
  const ws = workspaces.value.find(w => w.id === wsDlg.id)
  if (!ws) { closeWsDialog(); return }
  const want = wsDlg.dirs.filter(Boolean)
  if (!want.length) { toast('至少留一个源文件夹（会话的主目录从这些文件夹里选）', true); return }
  wsDlg.saving = true
  try {
    const removed = (ws.dirs || []).filter(d => !want.includes(d))
    const added = want.filter(d => !(ws.dirs || []).includes(d))
    for (const d of removed) {
      await api('DELETE', `/api/acp/workspaces/${ws.id}/dirs?path=` + encodeURIComponent(d))
    }
    for (const d of added) {
      await api('POST', `/api/acp/workspaces/${ws.id}/dirs`, { path: d })
    }
    // 首位变了才发这一枪：换主目录只影响以后新建的会话，既有会话仍锚自己的主目录
    const body = {}
    if ((ws.name || '') !== wsDlg.name.trim()) body.name = wsDlg.name.trim()
    if (want[0] !== (ws.dirs || [])[0]) body.primaryPath = want[0]
    if (Object.keys(body).length) await api('POST', '/api/acp/workspaces/' + ws.id, body)
    await load()
    wsDlg.open = false
    toast('工作区已更新')
  } catch (e) {
    toast('保存失败：' + e.message, true)
  } finally {
    wsDlg.saving = false
  }
}

// 第一下只武装确认（返回 false，菜单留着让人点第二下），第二下才真删
function removeWorkspace(id) {
  if (confirmWsId.value !== id) {
    confirmWsId.value = id
    setTimeout(() => { if (confirmWsId.value === id) confirmWsId.value = '' }, 3000)
    return false
  }
  confirmWsId.value = ''
  api('DELETE', '/api/acp/workspaces/' + id)
    .then(r => {
      // 连带删掉的会话：本机在途的请求与缓冲都要收起
      for (const tid of r.threadIds || []) {
        inflight.get(tid)?.abort()
        inflight.delete(tid)
        dropState(tid)
        if (currentId.value === tid) currentId.value = ''
      }
      if (currentWsId.value === id) currentWsId.value = ''
      return load()
    })
    .then(() => toast('已删除该工作区及其会话（磁盘目录不动）'))
    .catch(e => toast('删除失败：' + e.message, true))
}

/* ---------- 会话：在工作区里选 agent + 主目录 ---------- */
function toggleSessionForm(wsId) {
  if (sessionFor.value === wsId) { sessionFor.value = ''; return }
  openSessionForm(wsId)
}
function openSessionForm(wsId) {
  sessionFor.value = wsId
  const list = enabledAgents()
  sessionForm.agent = list.length ? list[0].name : '' // 只有一个候选时也直接选中
}
async function submitSession() {
  if (!sessionForm.agent) { toast('请先在「设置 → 智能体」添加并开启 agent', true); return }
  try {
    const r = await api('POST', '/api/acp/threads', {
      workspaceId: sessionFor.value, agent: sessionForm.agent,
    })
    sessionFor.value = ''
    await load()
    select(r.thread.id)
    toast('已创建会话，正在启动 agent（就绪后即可切模型/思考强度）')
  } catch (e) { toast('创建会话失败：' + e.message, true) }
}

// 目录选择器：后端 /api/acp/dirs 只读逐层列举（浏览器与桌面壳共用，浏览器
// 原生 API 拿不到绝对路径所以不走 showDirectoryPicker）。弹窗里可逐层点选，
// 也可在路径栏直接粘贴回车跳转；「使用此目录」把绝对路径交给打开时登记的落点。
const dirPicker = reactive({ open: false, path: '', parent: '', input: '', dirs: [], loading: false,
  showHidden: false })
let dirPickOnPick = null
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
function openDirPicker(onPick, startPath) {
  dirPickOnPick = onPick
  dirPicker.open = true
  dirPicker.showHidden = false
  loadDirs(startPath || '')
}
function submitDirPath() {
  if (dirPicker.input.trim()) loadDirs(dirPicker.input.trim())
}
function chooseDir() {
  const onPick = dirPickOnPick
  dirPickOnPick = null
  dirPicker.open = false
  if (onPick && dirPicker.path) onPick(dirPicker.path)
}
// 第一下只武装确认（菜单留着让人点第二下），第二下才真删。菜单项的点击是同步的，
// 所以"武装/删除"的分流必须同步做——异步函数返回 Promise 会让 && 永远成立、菜单提前关。
function removeThread(id) {
  if (confirmId.value !== id) {
    confirmId.value = id
    setTimeout(() => { if (confirmId.value === id) confirmId.value = '' }, 3000)
    return
  }
  confirmId.value = ''
  closeThreadMenu()
  doRemoveThread(id)
}

async function doRemoveThread(id) {
  try {
    await api('DELETE', '/api/acp/threads/' + id)
    // 服务端删除已回收进程：本机还在读的那条流没有落点，就地断开
    inflight.get(id)?.abort()
    inflight.delete(id)
    dropState(id)
    if (currentId.value === id) currentId.value = ''
    load()
    toast('已删除该会话')
  } catch (e) { toast('删除失败：' + e.message, true) }
}

/* ---------- 滚动跟随（发送时与切会话时用） ---------- */
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

/* ---------- 发送与排队 ----------
   回答进行中照样能继续输入：这条消息进该会话自己的队列，本轮跑完按顺序依次发出。
   ACP 要求同一 session 的 prompt 串行（C3），所以队列一次只放一条上路——前一轮
   finally 之后再取下一条，不是并发打两轮，而是"排队等轮到它"。 */
async function send() {
  if (!enabled.value) { toast('智能体功能未开启（设置 → 智能体）', true); return }
  const text = input.value.trim()
  if (!text && !pendingImages.value.length) return
  if (!currentId.value) { toast('先新建一个工作区并在里面建会话', true); return }
  const owner = currentId.value
  const st = stateOf(owner)
  if (st.phase === 'streaming') { enqueue(st, text); return }
  await startTurn(owner, text, takePending(st))
}

function queueState(id) { return states[id] || null }
function queueToFront(id, i) {
  const st = queueState(id)
  if (!st || i <= 0 || i >= st.queue.length) return
  st.queue.unshift(...st.queue.splice(i, 1))
}
// 取回编辑：文本并回草稿（不覆盖已经敲了的内容），图片退回待发预览
function editQueueItem(id, i) {
  const st = queueState(id)
  if (!st || !st.queue[i]) return
  const [item] = st.queue.splice(i, 1)
  input.value = input.value.trim() ? input.value + '\n' + item.text : item.text
  st.pending.push(...item.shots)
}
function dropQueueItem(id, i) {
  const st = queueState(id)
  if (!st || !st.queue[i]) return
  const [item] = st.queue.splice(i, 1)
  for (const s of item.shots) URL.revokeObjectURL(s.url)
}
function clearQueue(id) {
  const st = queueState(id)
  if (!st) return
  for (const q of st.queue.splice(0)) for (const s of q.shots) URL.revokeObjectURL(s.url)
}
function toggleQueuePause(id) {
  const st = queueState(id)
  if (!st) return
  st.queuePaused = !st.queuePaused
  toast(st.queuePaused ? '队列已暂停：本轮跑完先停在这里' : '队列已继续发送')
  if (!st.queuePaused) pumpQueue(id)
}

// 把待发图片从输入区交给这一轮（或这条队列项）；对象 URL 由接收方负责释放
function takePending(st) {
  return st.pending.splice(0).map(p => ({ id: p.id, url: p.url }))
}

function enqueue(st, text) {
  st.queue.push({ text, shots: takePending(st) })
  input.value = ''
  if (inputEl.value) inputEl.value.style.height = 'auto'
  toast(st.queuePaused
    ? `已排入队列（共 ${st.queue.length} 条，队列当前是暂停的）`
    : `已排入队列（共 ${st.queue.length} 条，本轮跑完依次发出）`)
}

// 本轮了结后接着放队列的下一条上路（暂停或已删会话就不放）
function pumpQueue(owner) {
  const st = states[owner]
  if (!st || st.phase !== 'idle' || st.queuePaused || !st.queue.length) return
  const item = st.queue.shift()
  startTurn(owner, item.text, item.shots)
}

async function startTurn(owner, text, shots) {
  const st = stateOf(owner)
  const shown = () => currentId.value === owner
  const scrollAfterRender = () => { if (shown()) nextTick(() => stick()) }

  if (shown()) {
    input.value = ''
    if (inputEl.value) inputEl.value.style.height = 'auto'
  }
  const attachments = shots.map(s => s.id)
  // 气泡直接渲染原图：用服务端 URL（/api/upload/:id），刷新后历史重载也能显示
  const sentImgs = attachments.map(id => '/api/upload/' + id)
  const userMsg = reactive({ role: 'user', text,
    images: sentImgs, imgCount: attachments.length })
  const typing = reactive({ role: 'typing', label: '' })
  st.messages.push(userMsg)
  st.messages.push(typing)
  // 预览即时清空（气泡已接手展示，对象 URL 用完即释放）
  for (const s of shots) URL.revokeObjectURL(s.url)
  st.phase = 'streaming'
  scrollAfterRender()

  const controller = new AbortController()
  inflight.set(owner, controller)
  const removeTyping = () => { const i = st.messages.indexOf(typing); if (i >= 0) st.messages.splice(i, 1) }
  let bot = null
  const ensureBot = () => {
    removeTyping()
    if (!bot) {
      bot = reactive({ role: 'bot', text: '', streaming: true, paused: false, blocks: [], turn: null, round: 0 })
      st.messages.push(bot)
    }
    return bot
  }
  let done = false
  let turnTotal = null
  let errMsg = ''
  try {
    const res = await fetch(`/api/acp/threads/${owner}/chat`, {
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
      if (data.thinking) { pushThinking(ensureBot(), data.thinking); scrollAfterRender() }
      if (data.step) { pushToolStep(ensureBot(), data.step); scrollAfterRender() }
      if (data.delta) {
        if (typing.label) typing.label = ''
        pushText(ensureBot(), data.delta)
        scrollAfterRender()
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
        if (data.context) syncCtx(owner, data.context.used, data.context.size)
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
    if (inflight.get(owner) === controller) inflight.delete(owner)
    st.phase = 'idle'
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
      st.messages.push({ role: 'note', text: errMsg })
    }
    // 刷新列表状态（标题/状态/更新时间）；正在看着的这条会话顺手记成"已读"，
    // 免得自己刚跑完的一轮反过来给自己挂个未读点。
    load().then(() => { if (currentId.value === owner) markSeen(owner) })
    // 一轮跑完会话必已建立：配置项（含 agent 侧的切换回推）跟着刷新。
    // 会话被删掉时缓冲已回收，别再为它拉起 agent。
    if (states[owner]) loadOptions(owner)
    // 队列里还排着消息就放下一条上路（没暂停的话）
    pumpQueue(owner)
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
  const st = stateOf(currentId.value) // 图片归当时正在输入的那个会话，中途切走也不串台
  if (!st) { toast('先新建一个工作区并在里面建会话', true); return }
  for (const file of files) {
    if (st.pending.length >= 4) { toast('一条消息最多 4 张图片', true); break }
    if (file.size > 5 * 1024 * 1024) { toast(`图片 ${file.name} 超过 5MB 上限`, true); continue }
    const fd = new FormData()
    fd.append('file', file)
    try {
      const r = await api('POST', '/api/upload', fd)
      st.pending.push({ id: r.id, url: URL.createObjectURL(file) })
    } catch (e) { toast('上传失败：' + e.message, true) }
  }
}
function removePreview(p) {
  const st = stateOf(currentId.value)
  if (!st) return
  const i = st.pending.indexOf(p)
  if (i >= 0) st.pending.splice(i, 1)
  URL.revokeObjectURL(p.url)
}
</script>

<template>
  <!-- 组件多根（抽屉/Teleport 是兄弟节点），v-show 只能落在主根上；
       App 只在首次进入时挂载，之后靠这里随导航隐藏/恢复。 -->
  <div id="workspaceView" class="view" v-show="store.view === 'agent'">
    <!-- 左：工作区（一组目录 + 挂在它里面的会话） -->
    <aside class="ag-rail">
      <div class="ag-rail-head">
        <b>工作区</b>
        <div class="ag-rail-acts">
          <button class="ib-btn" aria-label="收起全部工作区" title="收起全部工作区"
                  :disabled="!currentWsId" @click="collapseAll">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
                 stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m7 10 5-5 5 5M7 14l5 5 5-5"/></svg>
          </button>
          <button class="ib-btn" :class="{ active: creatingWs }" aria-label="新建工作区" title="新建工作区" @click="openCreateWs">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>
          </button>
        </div>
      </div>
      <!-- 列表视图切换：分组管理 vs 跨工作区找最近活动 -->
      <div class="ws-seg" role="group" aria-label="列表视图">
        <button :class="{ on: view === 'grouped' }" :aria-pressed="view === 'grouped'" @click="setView('grouped')">按工作区</button>
        <button :class="{ on: view === 'recent' }" :aria-pressed="view === 'recent'" @click="setView('recent')">最近更新</button>
      </div>

      <!-- 新建工作区：名字留空回落主目录名；目录一个都不给则用临时工作区目录 -->
      <div v-if="creatingWs" class="ag-form">
        <label class="meta">名字（留空用主目录名）</label>
        <input v-model="wsForm.name" aria-label="工作区名字" placeholder="比如 proj-a">
        <label class="meta">源文件夹（agent 在这里干活，会写入文件，可加多个）</label>
        <ul v-if="wsForm.dirs.length" class="ws-dirs">
          <li v-for="(d, i) in wsForm.dirs" :key="d" class="ws-dir" :class="{ primary: i === 0 }">
            <span class="ws-dir-txt" :title="d">{{ base(d) }}</span>
            <span v-if="i === 0" class="ws-tag">主</span>
            <span class="ag-del" role="button" :aria-label="'移除目录 ' + d" @click="removeWsFormDir(i)">×</span>
          </li>
        </ul>
        <button type="button" class="ag-ws-pick" title="选择目录加入工作区"
                @click="openDirPicker(addWsFormDir, wsForm.dirs[0] || '')">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>
          <span>添加源文件夹…</span>
        </button>
        <p class="meta ag-ws-hint">第一个源文件夹是会话的主目录，其余作为扩展根下发（agent 声明支持才生效）。<br>都不填则用临时工作区目录（数据目录 acp-sessions/）。</p>
        <div class="ag-form-acts">
          <button class="btn sm primary" @click="submitWs">创建</button>
          <button class="btn sm" @click="creatingWs = false">取消</button>
        </div>
      </div>

      <div class="ag-list">
        <!-- 「最近更新」：跨工作区摊平，多项目切换时先找活、不先找项目 -->
        <template v-if="view === 'recent'">
          <div v-for="t in recentThreads" :key="t.id" class="ag-item" role="button" tabindex="0"
               :class="{ active: t.id === currentId }"
               @click="openFromRecent(t)" @keydown.enter.prevent="openFromRecent(t)">
            <span class="ag-item-main">
              <span class="ag-title">
                {{ t.title || '新会话' }}
                <span v-if="t.state === 'interrupted'" class="ag-state warn" title="上次会话被中断，继续发消息会重接上下文">已中断</span>
                <span v-else-if="t.state === 'orphaned'" class="ag-state bad" title="绑定的 agent 已被删除，历史仍可查看">不可用</span>
              </span>
              <span class="meta">{{ wsName(t.workspaceId) }} · {{ t.agent }}{{ ago(t.updatedAt) ? ' · ' + ago(t.updatedAt) : '' }}</span>
            </span>
            <span v-if="isUnread(t)" class="ws-unread" title="上次打开之后这条会话又有新动静">未读</span>
            <span v-else-if="generating(t.id) && t.id !== currentId" class="ag-gen"
                  title="这一轮还在后台跑，切到别的会话不会打断它">生成中</span>
            <span class="ag-del" role="button" :aria-label="'删除 ' + (t.title || '会话')"
                  @click.stop="removeThread(t.id)">{{ confirmId === t.id ? '确认删除?' : '×' }}</span>
          </div>
          <div v-if="loaded && !recentThreads.length" class="ag-empty meta">还没有会话。切到「按工作区」，在工作区里新建一条。</div>
        </template>

        <!-- 「按工作区」：分组列表，展开的那组里管理源文件夹与会话 -->
        <template v-else>
          <div v-for="ws in orderedWorkspaces()" :key="ws.id" class="ws-item" :class="{ active: ws.id === currentWsId }">
            <div class="ws-head" role="button" tabindex="0" :aria-expanded="ws.id === currentWsId"
                 @click="toggleWs(ws.id)" @keydown.enter.prevent="toggleWs(ws.id)">
              <svg class="ws-caret" :class="{ open: ws.id === currentWsId }" width="12" height="12" viewBox="0 0 24 24"
                   fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg>
              <span class="ag-item-main">
                <span class="ag-title" :title="ws.name">
                  <span v-if="ws.pinned" class="ws-pin-mark" title="已置顶" aria-hidden="true">★</span>{{ ws.name }}</span>
                <span class="meta">{{ ws.dirs.length }} 个文件夹 · {{ wsThreads(ws.id).length }} 条会话</span>
              </span>
              <!-- 折叠着也要能看出这组里有事：缺文件夹、未读、还在生成 -->
              <span v-if="missingDirs(ws).length" class="ws-warn"
                    :title="'有 ' + missingDirs(ws).length + ' 个源文件夹已被删除或移动，去「编辑工作区」看'">缺{{ missingDirs(ws).length }}</span>
              <span v-if="ws.id !== currentWsId && wsUnread(ws.id)" class="ws-unread"
                    :title="'该工作区有 ' + wsUnread(ws.id) + ' 条会话有未读新内容'">{{ wsUnread(ws.id) }} 未读</span>
              <span v-if="ws.id !== currentWsId && wsGenerating(ws.id)" class="ag-gen"
                    :title="'该工作区有 ' + wsGenerating(ws.id) + ' 条会话正在生成'">{{ wsGenerating(ws.id) }} 生成中</span>
              <!-- 新建会话与「…」并列：一行只留这两个动作，其余收进菜单 -->
              <button class="ib-btn ws-icon" :class="{ active: sessionFor === ws.id }"
                      :aria-label="'在 ' + ws.name + ' 新建会话'" title="新建会话（选 agent 与主目录）"
                      @click.stop="toggleSessionForm(ws.id)">
                <!-- 对话气泡里带个加号：与「…」区分开，"在这组里再开一条" -->
                <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"
                     stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                  <path d="M21 11.5a8.4 8.4 0 0 1-8.5 8.3 9 9 0 0 1-3.4-.7L4 20.5l1.4-4.3a8 8 0 0 1-1.4-4.7A8.4 8.4 0 0 1 12.5 3.2 8.4 8.4 0 0 1 21 11.5Z"/>
                  <path d="M12.3 8.6v5.6M9.5 11.4H15"/>
                </svg>
              </button>
              <span class="ws-menu-wrap" @click.stop>
                <button class="ib-btn ws-icon" :class="{ active: wsMenu === ws.id }" :aria-label="ws.name + ' 的更多操作'"
                        aria-haspopup="menu" :aria-expanded="wsMenu === ws.id" title="更多"
                        @click="toggleWsMenu(ws.id)">
                  <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.7"/><circle cx="12" cy="12" r="1.7"/><circle cx="19" cy="12" r="1.7"/></svg>
                </button>
                <div v-if="wsMenu === ws.id" class="ws-menu pop-anim" role="menu">
                  <button role="menuitem" :aria-label="'编辑工作区 ' + ws.name" @click="openWsDialog(ws)">
                    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
                         stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/></svg>
                    编辑工作区
                  </button>
                  <button role="menuitem" :aria-label="(ws.pinned ? '取消置顶 ' : '置顶 ') + ws.name" @click="togglePin(ws); closeWsMenu()">
                    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
                         stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 17v5M9 3h6l1 7 2 2H6l2-2 1-7Z"/></svg>
                    {{ ws.pinned ? '取消置顶' : '置顶' }}
                  </button>
                  <button role="menuitem" class="danger" :aria-label="'删除工作区 ' + ws.name"
                          @click="removeWorkspace(ws.id) && closeWsMenu()">
                    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"
                         stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14"/></svg>
                    {{ confirmWsId === ws.id ? '再点一次确认删除' : '删除工作区' }}
                  </button>
                  <p class="ws-menu-note">删除会连同里面的会话记录一起删；磁盘上的文件夹不动。</p>
                </div>
              </span>
            </div>

            <div v-if="ws.id === currentWsId" class="ws-body">
              <!-- 展开体只列会话：源文件夹的查看与增删收进「…」→「编辑工作区」弹窗 -->
              <div class="ws-sec">
                <div v-for="t in visibleSessions(ws)" :key="t.id" class="ag-item" role="button" tabindex="0"
                     :class="{ active: t.id === currentId }"
                     @click="select(t.id)" @keydown.enter.prevent="select(t.id)">
                  <span class="ag-item-main">
                    <span class="ag-title">
                      <input v-if="threadEdit.id === t.id" v-model="threadEdit.title" class="ag-title-edit"
                             aria-label="会话标题" placeholder="新会话"
                             @click.stop @keydown.enter.prevent="commitRenameThread" @blur="commitRenameThread">
                      <template v-else>
                        <span @dblclick.stop="startRenameThread(t)" :title="'双击改标题：' + (t.title || '新会话')">{{ t.title || '新会话' }}</span>
                        <span v-if="t.state === 'interrupted'" class="ag-state warn" title="上次会话被中断，继续发消息会重接上下文">已中断</span>
                        <span v-else-if="t.state === 'orphaned'" class="ag-state bad" title="绑定的 agent 已被删除，历史仍可查看">不可用</span>
                      </template>
                    </span>
                    <!-- 行上不铺路径：agent + 多久之前动过（完整目录在会话头部与「编辑工作区」里看） -->
                    <span class="meta">{{ t.agent }}{{ ago(t.updatedAt) ? ' · ' + ago(t.updatedAt) : '' }}</span>
                  </span>
                  <span v-if="isUnread(t)" class="ws-unread" title="上次打开之后这条会话又有新动静">未读</span>
                  <span v-else-if="generating(t.id) && t.id !== currentId" class="ag-gen"
                        title="这一轮还在后台跑，切到别的会话不会打断它">生成中</span>
                  <span class="ag-menu-wrap" @click.stop>
                    <button class="ib-btn ws-icon" :class="{ active: threadMenu === t.id }"
                            :aria-label="(t.title || '会话') + ' 的更多操作'" aria-haspopup="menu"
                            :aria-expanded="threadMenu === t.id" title="更多" @click="toggleThreadMenu(t.id)">
                      <svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.7"/><circle cx="12" cy="12" r="1.7"/><circle cx="19" cy="12" r="1.7"/></svg>
                    </button>
                    <div v-if="threadMenu === t.id" class="ws-menu pop-anim" role="menu">
                      <button role="menuitem" :aria-label="'重命名 ' + (t.title || '会话')" @click="startRenameThread(t)">重命名</button>
                      <button role="menuitem" class="danger" :aria-label="'删除 ' + (t.title || '会话')"
                              @click="removeThread(t.id)">
                        {{ confirmId === t.id ? '再点一次确认删除' : '删除会话' }}
                      </button>
                    </div>
                  </span>
                </div>
                <button v-if="hiddenCount(ws)" class="ws-act" @click="showMoreWs(ws.id)">
                  显示更多（还有 {{ hiddenCount(ws) }} 条）
                </button>
                <div v-if="!wsThreads(ws.id).length && sessionFor !== ws.id" class="meta ws-none">
                  这个工作区还没有会话。
                </div>

                <!-- 新建会话：只选 agent 工具。主目录就是工作区的第一个源文件夹，
                     标题建好后在会话行的「…」里改（首条消息也会自动起一个） -->
                <div v-if="sessionFor === ws.id" class="ag-form ws-session-form">
                  <label class="meta">智能体工具</label>
                  <select v-model="sessionForm.agent" aria-label="选择智能体">
                    <option v-for="a in (acp ? acp.agents : [])" :key="a.name" :value="a.name"
                            :disabled="a.enabled === false">{{ a.name }}{{ a.enabled === false ? '（已停用）' : '' }}</option>
                  </select>
                  <p class="meta ws-session-hint" :title="ws.dirs[0]">
                    主目录：{{ base(ws.dirs[0]) }}（工作区的第一个源文件夹；要换去「编辑工作区」）
                  </p>
                  <div class="ag-form-acts">
                    <button class="btn sm primary" :disabled="!sessionForm.agent" @click="submitSession">创建会话</button>
                    <button class="btn sm" @click="sessionFor = ''">取消</button>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </template>
        <div v-if="!loaded" class="ag-empty meta">正在加载工作区…</div>
        <div v-else-if="!workspaces.length" class="ag-empty meta">还没有工作区。右上角 + 建一个：起个名字、把要干活的目录加进去（可以先不填，用临时目录）。</div>
      </div>
    </aside>

    <!-- 右：对话（进入某个会话后就是原来那套时间线） -->
    <section class="ag-main">
      <template v-if="!loaded">
        <div class="ag-blank">
          <div class="ag-blank-logo">工</div>
          <h3>{{ loadError ? '加载失败' : '正在加载工作区…' }}</h3>
          <p class="meta">{{ loadError || '读取已配置的 agent、工作区与会话列表。' }}</p>
          <button v-if="loadError" class="btn primary" @click="load()">重试</button>
        </div>
      </template>
      <template v-else-if="!enabled">
        <div class="ag-blank">
          <div class="ag-blank-logo">工</div>
          <h3>智能体功能未开启</h3>
          <p class="meta">在「设置 → 智能体」里添加 agent（如 codex、opencode）并打开总开关，<br>然后回到这里建工作区、把活交给它。</p>
          <button class="btn primary" @click="store.settingsSection = 'agent'; store.settingsOpen = true">去设置</button>
        </div>
      </template>
      <template v-else-if="!workspaces.length">
        <div class="ag-blank">
          <div class="ag-blank-logo">工</div>
          <h3>还没有工作区</h3>
          <p class="meta">工作区 = 一组目录（第一个是会话的主目录）。<br>左侧点 + 建一个，再在里面建会话选 agent 工具。</p>
          <button class="btn primary" @click="openCreateWs">新建工作区</button>
        </div>
      </template>
      <template v-else-if="!current">
        <div class="ag-blank">
          <div class="ag-blank-logo">会</div>
          <h3>{{ currentWs ? '在这个工作区里建一个会话' : '选择一个工作区' }}</h3>
          <p class="meta" v-if="currentWs">
            {{ currentWs.name }} 里有 {{ currentWs.dirs.length }} 个目录。建会话时选 agent 工具与主目录，<br>
            第一条消息发出时 agent 进程才会启动。
          </p>
          <p class="meta" v-else>左侧点一个工作区展开它。</p>
          <button v-if="currentWs" class="btn primary" @click="openSessionForm(currentWs.id)">新建会话</button>
        </div>
      </template>
      <template v-else>
        <header class="ag-head">
          <div class="ag-head-txt">
            <b>{{ current.title || '新会话' }}</b>
            <span class="meta" :title="(currentWs ? currentWs.name + ' · ' : '') + current.cwd">
              <template v-if="currentWs">{{ currentWs.name }} · </template>{{ current.agent }} · {{ current.cwd }}
              · {{ stateLabel[current.state] || current.state }}
              <template v-if="dirsNote"> · {{ dirsNote }}</template>
              <template v-if="current.degraded"> · 会话恢复不可靠</template>
            </span>
          </div>
          <!-- 应用内全屏：收起左侧全局导航、对话吃满窗口；Esc 归页面处理（先停本轮，空闲时退出） -->
          <button class="ib-btn ag-full" :class="{ active: store.fullscreen }"
                  :aria-label="store.fullscreen ? '退出全屏对话' : '进入全屏对话'"
                  :title="store.fullscreen ? '退出全屏（Esc）' : '全屏对话（Esc 退出）'"
                  @click="store.fullscreen = !store.fullscreen">
            <svg v-if="store.fullscreen" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor"
                 stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 3H5a2 2 0 0 0-2 2v4M15 3h4a2 2 0 0 1 2 2v4M9 21H5a2 2 0 0 1-2-2v-4M15 21h4a2 2 0 0 0 2-2v-4"/></svg>
            <svg v-else width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor"
                 stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 3h6v6M9 21H3v-6M21 3l-7 7M3 21l7-7"/></svg>
          </button>
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
          <ChatComposer v-model="input" :placeholder="busy ? '正在回答，这条会排队…' : '给 ' + current.agent + ' 派活…'"
                        aria-label="智能体输入" :busy="busy" :queue-on="busy"
                        :send-label="busy ? '加入队列' : '发送'" :send-disabled="!canSend"
                        @primary="send" @stop="stop" @paste="onPaste">
            <template #previews>
              <!-- 排队条：本轮跑完按顺序依次发出；每条可以提前、取回编辑或撤掉 -->
              <div v-if="cur && cur.queue.length" class="qbar">
                <div class="qbar-head">
                  <span class="meta">
                    {{ cur.queue.length }} 条排队中 ·
                    {{ cur.queuePaused ? '已暂停（本轮跑完先停在这里）' : '本轮跑完依次发出' }}
                  </span>
                  <button class="ib-btn sm" :aria-label="cur.queuePaused ? '继续发送队列' : '暂停发送队列'"
                          :aria-pressed="cur.queuePaused" @click="toggleQueuePause(currentId)">
                    {{ cur.queuePaused ? '继续' : '暂停' }}
                  </button>
                  <button class="ib-btn sm" aria-label="清空队列" @click="clearQueue(currentId)">清空</button>
                </div>
                <ul class="qlist">
                  <li v-for="(q, i) in cur.queue" :key="i" class="qrow">
                    <span class="qidx" aria-hidden="true">{{ i + 1 }}</span>
                    <span class="qtext" :title="q.text">{{ q.text || '（只发图片）' }}</span>
                    <span v-if="q.shots.length" class="qimg">图 {{ q.shots.length }}</span>
                    <button class="ib-btn sm" :aria-label="'把这条提到队首'" :disabled="i === 0"
                            title="提到队首" @click="queueToFront(currentId, i)">↑</button>
                    <button class="ib-btn sm" :aria-label="'取回这条编辑'" @click="editQueueItem(currentId, i)">编辑</button>
                    <button class="ib-btn sm" :aria-label="'撤掉这条'" @click="dropQueueItem(currentId, i)">×</button>
                  </li>
                </ul>
              </div>
              <div v-if="pendingImages.length" id="attachPreviews">
                <div v-for="p in pendingImages" :key="p.url" class="thumb">
                  <img :src="p.url" alt="">
                  <button class="rm" aria-label="移除图片" @click="removePreview(p)">×</button>
                </div>
              </div>
            </template>
            <template #left>
              <!-- 回答进行中：主键归"加入队列"，停止另给一颗（原来挤在同一个键上，
                   现在两件事都能做，不必为停止而放弃输入下一句） -->
              <button v-if="busy" class="ib-btn halt-btn" aria-label="停止本轮" title="停止本轮（队列里已排的不受影响）"
                      @click.stop="stop()">
                <svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><rect x="6" y="5" width="4" height="14" rx="1.2"/><rect x="14" y="5" width="4" height="14" rx="1.2"/></svg>
              </button>
              <button class="ib-btn plus-btn" :class="{ active: popOpen }" aria-label="添加" title="添加"
                      aria-haspopup="true" @click.stop="setPop(!popOpen)">
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
  <!-- 编辑工作区弹窗：改名 + 源文件夹的增删与主目录（列表里不铺路径，要改来这里改）。
       弹窗内是暂存编辑，点「保存」才按差异落后端；Esc/点遮罩 = 放弃这次编辑。 -->
  <Teleport to="body">
    <div v-if="wsDlg.open" class="dlg-mask" @click.self="closeWsDialog">
      <div class="dlg" role="dialog" aria-modal="true" :aria-label="'编辑工作区 ' + wsDlg.name" @keydown.esc="closeWsDialog">
        <div class="dlg-form">
          <header class="dlg-head">
            <h3 class="dlg-title">编辑工作区</h3>
            <button type="button" class="set-close" aria-label="关闭编辑工作区" @click="closeWsDialog">
              <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
                   stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
            </button>
          </header>
          <div class="dlg-body">
            <label class="meta" for="wsDlgName">名字（留空回落到主文件夹名）</label>
            <input id="wsDlgName" v-model="wsDlg.name" aria-label="工作区名字" placeholder="比如 proj-a">
            <div class="ws-dlg-sec">
              <span class="meta">源文件夹（第一个是会话的主目录，其余作为扩展根下发）</span>
              <ul class="ws-dirs">
                <li v-for="(d, i) in wsDlg.dirs" :key="d" class="ws-dir"
                    :class="{ primary: i === 0, missing: wsDlg.missing.includes(d) }">
                  <span class="ws-dir-txt" :title="d">{{ d }}</span>
                  <span v-if="i === 0" class="ws-tag">主</span>
                  <span v-else class="ws-link" role="button" :aria-label="'设为主目录 ' + d" title="设为会话默认的主目录"
                        @click="dlgSetPrimary(i)">设为主</span>
                  <span v-if="wsDlg.missing.includes(d)" class="ws-missing">已不存在</span>
                  <span class="ag-del" role="button" :aria-label="'从工作区移出 ' + d" title="移出工作区（不删磁盘文件夹）"
                        @click="dlgRemoveDir(i)">×</span>
                </li>
                <li v-if="!wsDlg.dirs.length" class="meta ws-dlg-empty">至少留一个源文件夹。</li>
              </ul>
              <button type="button" class="ag-ws-pick" title="选择文件夹加入工作区" @click="dlgAddDir">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>
                <span>添加源文件夹…</span>
              </button>
              <p class="meta ws-dlg-hint">移出只是解除分组，不删磁盘上的文件夹；已经建好的会话仍锚它自己当时的主目录。</p>
            </div>
          </div>
          <footer class="dlg-foot">
            <span class="meta ws-dlg-state">{{ wsDlg.dirs.length }} 个源文件夹</span>
            <button class="btn sm primary" aria-label="保存" :disabled="wsDlg.saving || !wsDlg.dirs.length" @click="saveWsDialog">
              {{ wsDlg.saving ? '保存中…' : '保存' }}
            </button>
            <button class="btn sm" aria-label="放弃这次编辑" @click="closeWsDialog">取消</button>
          </footer>
        </div>
      </div>
    </div>
  </Teleport>
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
              <button type="button" class="ib-btn" title="上一级" aria-label="上一级"
                      :disabled="!dirPicker.parent || dirPicker.loading" @click="loadDirs(dirPicker.parent)">
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
                <button type="button" @click="loadDirs(d.path)">{{ d.name }}</button>
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
