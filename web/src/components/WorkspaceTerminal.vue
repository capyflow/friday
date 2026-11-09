<script setup>
// WorkspaceTerminal 是工作区对话右侧的内嵌终端面板：与「终端」页（TerminalView）同一套
// 内核协议（一条 WebSocket 一个真 PTY），只是长在对话旁边。三处刻意不一样：
//   1. 面板收起用 v-show，组件不卸载：WebSocket 与 xterm 实例都还活着，输出照常进
//      缓冲，重新展开内容原样都在；只有卸载（登出、会话被删）或重启软件才会收掉。
//   2. 新终端的起始目录由父组件传（cwd = 当前会话的主目录，即工作区主文件夹），
//      开 tab 那一刻取值；之后换会话不影响已开的终端。
//   3. 回滚行数写死在代码里（SCROLLBACK），不做成设置：超过这个行数最上面的输出
//      就被顶掉，跟日常终端一个做法。
// xterm / WebSocket / ResizeObserver 实例一律放在响应式之外（engines 这张普通 Map）：
// 它们是有内部状态的类实例，被 reactive 包一层代理后行为会变（focus、事件回调
// 里的 this 都可能坏），列表里只留渲染要用的几个字段。
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { api } from '../api.js'
import { getToken } from '../auth.js'
import { toast } from '../store.js'
import EmptyState from './art/EmptyState.vue'

const props = defineProps({
  cwd: { type: String, default: '' },   // 新终端的起始目录（会话主目录 = 工作区主文件夹）
  open: { type: Boolean, default: false }, // 面板当前是否可见（父组件 v-show 同一值，进来好重新 fit）
})
const emit = defineEmits(['close'])

// 回滚上限写死：一次 npm install 的输出上万行，8000 行的缓冲够回看，也不至于把内存
// 吃穿。不暴露成设置项，注释即文档。
const SCROLLBACK = 8000

// xterm 要实底颜色，而深色主题的 --card 是半透明，所以这里按主题给一组确定值。
const THEME = {
  light: { background: '#FFFFFF', foreground: '#18181B', cursor: '#18181B', selectionBackground: '#D6D6DC' },
  dark: { background: '#141414', foreground: '#E8E8E8', cursor: '#E8E8E8', selectionBackground: '#3A3A3F' },
}

const info = ref(null)      // GET /api/terminal；null = 还没拉到（不能当"未开启"画）
const tabs = ref([])        // { id, label, status, note }
const activeId = ref('')
const paneEls = {}          // id → 容器 DOM（模板 ref 回调填）
const engines = new Map()   // id → { term, fit, ws, ro, cwd }
let seq = 0
let disposed = false

function themeNow() {
  return document.documentElement.dataset.theme === 'dark' ? THEME.dark : THEME.light
}

async function loadInfo() {
  try {
    info.value = await api('GET', '/api/terminal')
  } catch (e) {
    info.value = { enabled: false, reason: '终端状态读不到：' + e.message }
  }
  // 挂载时面板就开着（比如收起状态下换了会话）：状态到位后补开第一个终端
  if (!disposed && props.open) ensureTab()
}

function wsURL() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${location.host}/api/terminal/ws?token=${encodeURIComponent(getToken() || '')}`
}

function setPane(id, el) {
  if (el) {
    paneEls[id] = el
    // 容器刚挂上就把终端装进去：ref 回调在挂载提交后跑，此时元素已有尺寸
    const eng = engines.get(id)
    if (eng && !eng.term) mountTerm(id)
  } else {
    delete paneEls[id]
  }
}

// 面板开着就得有终端可用：一个都没有就开一个（落在传入的主目录），
// 已有的只重量尺寸、归还焦点——收起再展开不能把人正在跑的东西弄丢。
function ensureTab() {
  if (!info.value?.enabled) return // 不可用时面板里明说原因，不空连
  if (!tabs.value.length) openTab()
  nextTick(refitActive)
}

function openTab() {
  seq += 1
  const tab = { id: 'wterm' + seq, label: '终端 ' + seq, status: 'connecting', note: '' }
  engines.set(tab.id, { cwd: props.cwd }) // 起始目录在开 tab 这一刻定格
  tabs.value = [...tabs.value, tab]
  activeId.value = tab.id
  nextTick(() => mountTerm(tab.id))
}

function mountTerm(id) {
  const el = paneEls[id]
  const eng = engines.get(id)
  if (!el || !eng || eng.term) return
  const term = new Terminal({
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    fontSize: 12,
    lineHeight: 1.4,
    scrollback: SCROLLBACK,
    theme: themeNow(),
    cursorBlink: true,
  })
  const fit = new FitAddon()
  term.loadAddon(fit)
  term.open(el)
  fit.fit()
  eng.term = term
  eng.fit = fit
  term.onData(d => send(id, { t: 'input', d }))
  term.onResize(({ cols, rows }) => send(id, { t: 'resize', cols, rows }))
  if (typeof window.ResizeObserver !== 'undefined') {
    eng.ro = new window.ResizeObserver(() => {
      // 面板收起时容器没有尺寸，fit 算出来是 0 行 0 列，别发给内核；
      // 展开时 watch(open) 会主动补一次。
      if (id === activeId.value && props.open && el.clientWidth > 0 && el.clientHeight > 0) fit.fit()
    })
    eng.ro.observe(el)
  }
  term.focus()
  connect(id, eng.cwd)
}

// connect 起一条 WebSocket：一个终端一条连接，断开即由内核收掉那条 shell。
function connect(id, cwd) {
  const eng = engines.get(id)
  if (!eng) return
  const ws = new window.WebSocket(wsURL())
  eng.ws = ws
  ws.binaryType = 'arraybuffer'
  ws.onopen = () => {
    send(id, { t: 'start', cols: eng.term?.cols || 80, rows: eng.term?.rows || 24, cwd: cwd || '' })
  }
  ws.onmessage = ev => {
    if (typeof ev.data !== 'string') {
      eng.term?.write(new Uint8Array(ev.data))
      return
    }
    let msg
    try { msg = JSON.parse(ev.data) } catch { return }
    if (msg.t === 'ready') {
      patch(id, { status: 'ready', note: `${msg.shell} · ${msg.cwd}` })
    } else if (msg.t === 'error') {
      patch(id, { status: 'error', note: msg.message || '终端出错' })
      toast(msg.message || '终端出错', true)
    } else if (msg.t === 'exit') {
      const note = msg.code === 0 ? '进程已结束' : `进程已退出（code ${msg.code}）`
      patch(id, { status: 'closed', note })
      eng.term?.writeln('\x1b[90m— 进程结束，这个标签可以关掉了 —\x1b[0m')
    }
  }
  ws.onclose = () => {
    const tab = tabs.value.find(t => t.id === id)
    if (tab && tab.status !== 'closed' && tab.status !== 'error') {
      patch(id, { status: 'closed', note: tab.note || '连接已断开' })
    }
  }
  ws.onerror = () => patch(id, { status: 'error', note: '连不上终端通道（本机以外不服务这个功能）' })
}

function patch(id, fields) {
  const i = tabs.value.findIndex(t => t.id === id)
  if (i < 0) return
  tabs.value = [...tabs.value.slice(0, i), { ...tabs.value[i], ...fields }, ...tabs.value.slice(i + 1)]
}

function send(id, msg) {
  const ws = engines.get(id)?.ws
  if (ws && ws.readyState === window.WebSocket.OPEN) ws.send(JSON.stringify(msg))
}

function activate(tab) {
  if (tab.id === activeId.value) return
  activeId.value = tab.id
  nextTick(() => {
    const eng = engines.get(tab.id)
    refitEng(eng)
    eng?.term?.focus()
  })
}

// 量尺寸前先确认容器真的有大小（面板收起、切换动画途中都可能是 0）
function refitEng(eng) {
  if (!eng?.fit) return
  const el = eng.term?.element
  if (el && el.clientWidth > 0 && el.clientHeight > 0) eng.fit.fit()
}

function refitActive() {
  refitEng(engines.get(activeId.value))
}

function closeTab(id) {
  if (!tabs.value.some(t => t.id === id)) return
  const eng = engines.get(id) || {}
  send(id, { t: 'close' }) // 让内核明确知道是主动关，而不是网络抖了一下
  eng.ro?.disconnect()
  try { eng.ws?.close() } catch { /* 已经断了 */ }
  eng.term?.dispose()
  engines.delete(id)
  delete paneEls[id]
  const rest = tabs.value.filter(t => t.id !== id)
  tabs.value = rest
  if (activeId.value === id) {
    const next = rest[0]
    activeId.value = next ? next.id : ''
    if (next) nextTick(() => {
      const eng = engines.get(next.id)
      refitEng(eng)
      eng?.term?.focus()
    })
  }
}

async function newTerminal() {
  if (info.value && !info.value.enabled) {
    toast(info.value.reason || '内置终端不可用', true)
    return
  }
  // maxSessions <= 0 表示内核不设上限（默认），这里就不用拦
  const max = Number(info.value?.maxSessions) || 0
  if (max > 0 && tabs.value.length >= max) {
    toast(`已经开了 ${tabs.value.length} 个终端，上限 ${max}：先关掉一个`, true)
    return
  }
  openTab()
}

function retry() { loadInfo() }

function onThemeChange() {
  for (const eng of engines.values()) {
    if (eng.term) eng.term.options.theme = themeNow()
  }
}

// 面板开合：展开时确保有终端（第一次进来开一个）、重新量尺寸并归还焦点。
// 收起什么都不做——连接与缓冲都在，输出继续往里写。
watch(() => props.open, v => { if (v) ensureTab() })

onMounted(() => {
  window.addEventListener('friday:theme', onThemeChange)
  loadInfo()
})

onBeforeUnmount(() => {
  disposed = true
  window.removeEventListener('friday:theme', onThemeChange)
  for (const tab of [...tabs.value]) closeTab(tab.id)
})
</script>

<template>
  <aside class="wt-panel" role="complementary" aria-label="工作区终端">
    <div class="wt-head">
      <div class="title">终端</div>
      <div class="term-tabs">
        <button v-for="t in tabs" :key="t.id" class="term-tab" :class="{ active: t.id === activeId }"
                :aria-pressed="t.id === activeId" :title="t.note || t.status" @click="activate(t)">
          <span class="term-tab-txt">{{ t.label }}</span>
          <span class="ag-del" role="button" :aria-label="'关闭 ' + t.label" title="关闭终端"
                @click.stop="closeTab(t.id)">×</span>
        </button>
      </div>
      <button class="ib-btn" aria-label="新建终端" title="新建终端" @click="newTerminal">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
             stroke-linecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>
      </button>
      <button class="ib-btn" aria-label="收起终端面板" title="收起面板（终端与内容保留）" @click="emit('close')">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
             stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
      </button>
    </div>

    <p v-if="tabs.length" class="meta wt-hint">
      {{ tabs.find(t => t.id === activeId)?.note }} · 收起面板不打断终端，关掉标签才终止这条 shell 里正在跑的命令
    </p>

    <div v-if="tabs.length" class="wt-panes">
      <div v-for="t in tabs" :key="t.id" class="term-pane" :class="{ active: t.id === activeId }"
           :ref="el => setPane(t.id, el)"></div>
    </div>

    <!-- 不可用：明说原因（配置没开 / 不是本机访问），不摆一个点不动的空壳界面 -->
    <div v-else-if="info && !info.enabled" class="wt-off">
      <EmptyState kind="error" :size="72">{{ info.reason || '内置终端不可用' }}</EmptyState>
      <p class="meta wt-off-hint">开启方式见 config.yaml 的 <code>terminal</code> 段；本机以外的访问一律不服务这个功能。</p>
      <button class="btn sm" @click="retry">重新检查</button>
    </div>
    <p v-else-if="!info" class="meta wt-off">正在确认终端是否可用…</p>
    <!-- 可用但一个终端都没有（全被手动关了）：留一句怎么开 -->
    <div v-else class="wt-off"><p class="meta">按上方 + 新建一个终端。</p></div>
  </aside>
</template>
