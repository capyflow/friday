<script setup>
// TerminalView 是内置终端：一条 WebSocket 对应一个真 PTY（内核侧 creack/pty 起的
// shell），界面用 xterm.js 渲染。标签之间互不影响。
//
// 三条要说白的语义：
//   1. 关掉标签、退出登录、或刷新页面，都会把那条 shell 的进程组收掉——正在跑的
//      命令会被终止。这条写在头部提示里，不让人靠猜。
//   2. 人在这里敲的命令不经过 exec_command 的白名单/审批：这是终端本身，不是 agent
//      的动作。远程部署里这个功能整体不服务（内核只接本机回环），页面会说明原因。
//   3. 尺寸是量出来的：fit 之后才发 start，窗口变化再发 resize，否则 shell 按
//      80x24 排版，回看历史会错位。
//
// xterm / WebSocket / ResizeObserver 实例一律放在响应式之外（engines 这张普通 Map）：
// 它们都是有内部状态的类实例，被 reactive 包一层代理后行为会变（focus、事件回调
// 里的 this 都可能坏），列表里只留渲染要用的几个字段。
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { api } from '../api.js'
import { getToken } from '../auth.js'
import { store, toast } from '../store.js'
import EmptyState from './art/EmptyState.vue'

// xterm 要实底颜色，而深色主题的 --card 是半透明，所以这里按主题给一组确定值。
const THEME = {
  light: { background: '#FFFFFF', foreground: '#18181B', cursor: '#18181B', selectionBackground: '#D6D6DC' },
  dark: { background: '#141414', foreground: '#E8E8E8', cursor: '#E8E8E8', selectionBackground: '#3A3A3F' },
}

const info = ref(null)      // GET /api/terminal；null = 还没拉到（不能当"未开启"画）
const tabs = ref([])        // { id, label, status, note }
const activeId = ref('')
const paneEls = {}          // id → 容器 DOM（模板 ref 回调填）
const engines = new Map()   // id → { term, fit, ws, ro }
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

function openTab(initial) {
  seq += 1
  const tab = { id: 'term' + seq, label: '终端 ' + seq, status: 'connecting', note: '' }
  engines.set(tab.id, {})
  // initial 是要在这条终端里自动敲的内容：等 ready 之后再送（先送会被 shell
  // 还没起来时的噪声吃掉）。多行时按行依次敲，间隔留给前一个程序就绪——
  // 「codebuddy」起来后隔几秒再敲 "/login" 就是这个用法。
  if (initial) engines.get(tab.id).pending = String(initial).split('\n').filter(Boolean)
  tabs.value = [...tabs.value, tab]
  activeId.value = tab.id
  nextTick(() => mountTerm(tab.id))
}

// flushPending 敲下一行待敲内容；多行时按行间隔几秒依次送（后面的行要等
// 前面的程序起来才有效，比如 codebuddy 起完交互界面才能收 /login）。
function flushPending(id) {
  const eng = engines.get(id)
  const lines = eng?.pending
  if (!lines || !lines.length) return
  const next = lines.shift()
  send(id, { t: 'input', d: next.endsWith('\n') ? next : next + '\n' })
  if (lines.length) setTimeout(() => flushPending(id), 3500)
}

function mountTerm(id) {
  const el = paneEls[id]
  const eng = engines.get(id)
  if (!el || !eng || eng.term) return
  const term = new Terminal({
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    fontSize: 13,
    lineHeight: 1.5,
    // 一次 npm install 的输出就上万行，回滚看不等于没存
    scrollback: 8000,
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
      if (id === activeId.value) fit.fit()
    })
    eng.ro.observe(el)
  }
  term.focus()
  connect(id)
}

// connect 起一条 WebSocket：一个终端一条连接，断开即由内核收掉那条 shell。
function connect(id) {
  const eng = engines.get(id)
  if (!eng) return
  const ws = new window.WebSocket(wsURL())
  eng.ws = ws
  ws.binaryType = 'arraybuffer'
  ws.onopen = () => {
    send(id, { t: 'start', cols: eng.term?.cols || 80, rows: eng.term?.rows || 24, cwd: '' })
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
      // 这条终端刚起来：把带来的命令敲进去（设置页「在终端里登录」）。
      flushPending(id)
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
    if (eng?.fit) eng.fit.fit()
    eng?.term?.focus()
  })
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
      if (eng?.fit) eng.fit.fit()
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

// 侧栏切到别的页面时本组件不卸载（App 里 v-show），连接照常活着；回到这一页
// 只需要重新量一次尺寸——期间窗口可能变过，不重量就会排版错位。
watch(() => store.view, v => {
  if (v !== 'terminal') return
  const eng = engines.get(activeId.value)
  if (eng?.fit) eng.fit.fit()
  eng?.term?.focus()
})

// 设置页「在终端里登录」带来的命令：开一条新终端自动敲进去（人不用复制粘贴）。
watch(() => store.terminalCommand, cmd => {
  if (!cmd) return
  store.terminalCommand = ''
  if (!info.value?.enabled) {
    toast(info.value?.reason || '内置终端不可用（本机以外不服务这个功能）', true)
    return
  }
  openTab(cmd.endsWith('\n') ? cmd : cmd + '\n')
})

onMounted(async () => {
  window.addEventListener('friday:theme', onThemeChange)
  await loadInfo()
  if (disposed) return
  // 一进来就有一个终端可用，比"先要点一下才有"顺手；不可用时页面直接说原因
  if (info.value?.enabled) openTab()
  // 命令可能在本组件挂载前就到了（设置页先切视图，组件后挂）：这里补跑一次。
  const cmd = store.terminalCommand
  if (cmd) {
    store.terminalCommand = ''
    if (info.value?.enabled) openTab(cmd.endsWith('\n') ? cmd : cmd + '\n')
    else toast(info.value?.reason || '内置终端不可用', true)
  }
})

onBeforeUnmount(() => {
  disposed = true
  window.removeEventListener('friday:theme', onThemeChange)
  for (const tab of [...tabs.value]) closeTab(tab.id)
})
</script>

<template>
  <section id="view-terminal" class="view" v-show="store.view === 'terminal'">
    <div id="termHead" data-tauri-drag-region>
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
    </div>

    <p v-if="tabs.length" class="meta term-hint">
      {{ tabs.find(t => t.id === activeId)?.note }} · 关掉标签或刷新页面会终止这条 shell 里正在跑的命令
    </p>

    <div v-if="tabs.length" id="termPanes">
      <div v-for="t in tabs" :key="t.id" class="term-pane" :class="{ active: t.id === activeId }"
           :ref="el => setPane(t.id, el)"></div>
    </div>

    <!-- 不可用：明说原因（配置没开 / 不是本机访问），不摆一个点不动的空壳界面 -->
    <div v-else-if="info && !info.enabled" class="term-off">
      <EmptyState kind="error" :size="96">{{ info.reason || '内置终端不可用' }}</EmptyState>
      <p class="meta term-off-hint">
        开启方式见 config.yaml 的 <code>terminal</code> 段；本机以外的访问一律不服务这个功能。
      </p>
      <button class="btn sm" @click="retry">重新检查</button>
    </div>
    <p v-else-if="!info" class="meta term-off-hint">正在确认终端是否可用…</p>
  </section>
</template>
