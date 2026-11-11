<script setup>
// 全局快捷键弹窗（桌面外壳 Alt+Space 创建的透明无边框窗口加载本页）。
// 定位是「随手问一句，也能连着聊」：与主窗口同源共享登录 token，且走同一个
// /api/chat 会话（后端固定 web 会话），所以弹窗里的追问不会丢上下文，主窗口
// 那边同样看得见这段对话。窗口失焦/Esc 只隐藏不销毁，消息列表跨多次唤起保留。
// 放在 Go 侧是刻意设计（docs/DESKTOP.md）：浏览器直接访问 :port/palette 亦可用。
import { nextTick, reactive, ref } from 'vue'
import { authHeaders, hydrateToken } from './auth.js'
import { nativeInvoke } from './tauri.js'
import { renderMD } from './md.js'
import { toast } from './store.js'
import { subscribe, clientId, deviceLabel } from './events.js'
import { applyEvent } from './turnevents.js'

// 弹窗可能先于主窗口打开：先把原生冷备的 token 回填到本页的 localStorage，
// 否则第一次提问会误报"还未登录"（memoized，重复调用零成本）。
hydrateToken()

// 首开回显最近几条问答：空面板会让人不知道上一句问了什么。完整历史在 420px
// 的浮层里没意义，只取尾部若干条；后端同一会话，所以回显与续聊天然一致。
const HISTORY_TAIL = 8

const turns = reactive([]) // { role: 'user' | 'bot', text, html?, streaming?, paused?, kind? }
const q = ref('')
const status = ref('')    // 状态行：思考中 / 正在调用工具 / 连接提示
const busy = ref(false)
const pausedTurn = ref(null) // 后端暂停现场 { question, partial }：有则发送键变「继续」
const log = ref(null)
const input = ref(null)
let stick = true          // 用户没往上翻就跟着流走，翻了就先停在那儿
let unsub = null            // 事件订阅句柄
let myTurnId = ''           // 本端最近一次提交的 turnId：停止时按它精确操作
const device = deviceLabel() // 设备标签：另一端看到消息时标「来自手机端」

function hideWindow() {
  try {
    const win = window.__TAURI__ && window.__TAURI__.window && window.__TAURI__.window.getCurrentWindow()
    if (win) { win.hide(); return }
  } catch (_) { /* 外壳不可用（浏览器直接访问）时降级 */ }
  window.close()
}

function onScroll() {
  const el = log.value
  if (el) stick = el.scrollHeight - el.scrollTop - el.clientHeight < 24
}

function toBottom() {
  if (!stick) return
  nextTick(() => { const el = log.value; if (el) el.scrollTop = el.scrollHeight })
}

// 输入框随内容长高，最多四行：浮层高度固定，再多就该滚动正文而不是吞掉输入区。
function autosize() {
  const el = input.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = Math.min(el.scrollHeight, 96) + 'px'
}

// Markdown 解析跟着每个 delta 跑会重复解析全部已收正文，这里按帧节流：
// 一帧内多次增量只解析一次，收流后再补一次终态。
let renderScheduled = false
function scheduleRender(t) {
  if (renderScheduled) return
  renderScheduled = true
  requestAnimationFrame(() => {
    renderScheduled = false
    t.html = renderMD(t.text)
    toBottom()
  })
}

function lastQuestion() {
  for (let i = turns.length - 1; i >= 0; i--) if (turns[i].role === 'user') return turns[i].text
  return ''
}

function onKeydown(e) {
  if (e.key === 'Escape') { e.preventDefault(); hideWindow(); return }
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'l') { e.preventDefault(); clearScreen(); return }
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send() }
}

async function send() {
  const text = q.value.trim()
  if (!text || busy.value) return
  await hydrateToken() // 兜底：极快输入时也保证 token 已回填
  q.value = ''
  autosize()
  // 暂停期间发新问题 = 放弃被暂停的对话（服务端 HandleSSE 同步丢弃暂停现场）。
  pausedTurn.value = null
  turns.push({ role: 'user', text })
  stick = true
  toBottom()
  await stream('/api/chat', { message: text })
}

async function resumeTurn() {
  if (!pausedTurn.value || busy.value) return
  const last = turns[turns.length - 1]
  const target = last && last.role === 'bot' && last.paused ? last : null
  if (target) { target.paused = false; target.streaming = true }
  pausedTurn.value = null
  await stream('/api/chat/resume', {}, target)
}

// 暂停：只通知后端取消生成，不掐本地连接——后端会补一帧 paused 带回落地的
// 部分回答，「继续」按钮要靠它出现（与主窗口同一套时序）。
async function stop() {
  if (!busy.value) return
  status.value = '暂停中…'
  try {
    await fetch('/api/chat/stop', {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      // turnId 必填：服务端按轮次精确定位停哪一轮（另一端也在跑时尤其关键）。
      body: JSON.stringify({ session: 'web', turnId: myTurnId }),
    })
  } catch (_) { /* 暂停失败不打断流，回答照常继续 */ }
}

// 清屏只清本面板的显示，不动后端会话：上下文是主窗口与弹窗共用的，
// 在浮层里一键抹掉整条对话太容易误伤，真要说"忘了刚才"该去主窗口清上下文。
function clearScreen() {
  if (busy.value) return
  turns.splice(0, turns.length)
  pausedTurn.value = null
  status.value = ''
}

// stream 提交一轮对话。
//
// 改造前这里自己建 SSE 连接、直接消费帧——弹窗与主窗口虽然同一会话，但
// 弹窗独占这条流，主窗口看不见弹窗里问的东西。改造后统一从事件流收，
// 弹窗、主窗口、手机端三处看到的内容一致。
async function stream(url, body, existing) {
  busy.value = true
  status.value = '思考中…'
  // turnId 挂上：turnevents 按 turnId 归属气泡，缺了同一条回答会分裂成两块。
  const msg = existing || { role: 'bot', text: '', html: '', streaming: true, turnId: '' }
  if (!existing) turns.push(msg)
  stick = true
  toBottom()
  try {
    const res = await fetch(url, {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ ...body, clientId, device }),
    })
    if (res.status === 403) {
      msg.kind = 'err'
      msg.text = '还未登录：请先打开 Friday 主窗口登录，再回来使用。'
      msg.html = renderMD(msg.text)
      status.value = ''
      return
    }
    let data = null
    try { data = await res.json() } catch (e) { /* 非 JSON 响应 */ }
    if (!res.ok) {
      const detail = (data && data.body && data.body.error) || ('HTTP ' + res.status)
      throw Object.assign(new Error(detail), { surfaced: true })
    }
    const r = (data && data.body) || {}
    myTurnId = r.turnId || ''
    if (myTurnId) msg.turnId = myTurnId
    if (r.status === 'queued') {
      // 会话忙（另一端正在生成）：这一轮进了 FIFO 队列。
      busy.value = false
      status.value = ''
      toast('小五正在回答（来自另一端），你的问题排在第 ' + (r.position || 1) + ' 位')
    }
  } catch (e) {
    // 已经流出半截正文时不丢弃，只补一行错误说明：用户看得见答到哪儿断的。
    // surfaced = 后端自己给的话（权限/参数问题），别再套「连接不上」这层误导描述。
    msg.kind = 'err'
    const why = e.surfaced ? e.message : '连接不上小五（' + e.message + '）'
    msg.text = (msg.text ? msg.text + '\n\n' : '') + why + '。'
    status.value = ''
  } finally {
    // 回复内容由事件流补齐；这里只把状态行让位，busy 由事件收尾时归位。
    if (!myTurnId) busy.value = false
  }
}



/* ---------- 事件订阅（双端同步） ---------- */
// 弹窗与主窗口、手机端同属一条主会话（web）。统一从事件流收，三处内容一致。
function onEvent(ev) {
  applyEvent(ev, {
    messages: turns,
    mine: clientId,
    // 只造不 push：turnevents 的 botFor 负责挂上去（这里再 push 会渲染两份）。
    // blocks/round 必须有：turnevents 的 delta/step/paused 都走 blocks.js，
    // 缺了会在 m.blocks.length 上炸掉（面板永远显示不出回答）。
    makeBot: () => ({ role: 'bot', text: '', html: '', streaming: true, blocks: [], round: 0 }),
    makeUser: (text, images) => ({ role: 'user', text, images: images || [] }),
    onDelta: bot => {
      if (bot) {
        status.value = '' // 正文开始流出，状态行让位
        scheduleRender(bot)
      }
    },
    onStep: s => {
      // 弹窗不铺开过程块，只把「在忙什么」压进一行状态，浮层里堆工具卡片是噪音。
      if (s && s.kind === 'tool') {
        status.value = s.status === 'run'
          ? '正在 ' + (s.name || s.detail || '处理')
          : '思考中…'
      }
    },
    onThinking: () => { status.value = '思考中…' },
    onPaused: info => {
      busy.value = false
      status.value = '已暂停，可继续'
      pausedTurn.value = { question: lastQuestion(), partial: info.partial || '' }
    },
    onEnd: () => {
      busy.value = false
      status.value = ''
    },
    onError: (text, bot) => {
      busy.value = false
      status.value = ''
      if (bot) {
        bot.kind = 'err'
        bot.text = (bot.text ? bot.text + '\n\n' : '') + text
        bot.html = renderMD(bot.text)
      }
    },
    onStatus: text => { if (text) status.value = text },
    onRemoteUser: () => toBottom(),
  })
}

// onResync：缺口过大时全量重拉历史（宁可重拉，也不假装消息连续）。
function onResync() {
  loadHistory()
}

// startEvents 订阅主会话事件流。
function startEvents() {
  if (unsub) return
  unsub = subscribe('web', 0, { onEvent, onResync, onState: () => {} })
}

function stopEvents() {
  if (!unsub) return
  unsub.stop()
  unsub = null
}

function onLogClick(e) {
  const a = e.target.closest && e.target.closest('a')
  if (!a) return
  e.preventDefault()
  nativeInvoke('open_url', { url: a.href })
}

// 首次打开回显历史；未登录或内核没起时静默跳过，不打扰第一次输入。
async function loadHistory() {
  await hydrateToken()
  try {
    const res = await fetch('/api/chat/history?session=web', { headers: authHeaders() })
    if (!res.ok) return
    const data = await res.json()
    const list = (data.body && data.body.messages) || []
    const paused = data.body && data.body.paused
    for (const m of list.slice(-HISTORY_TAIL)) {
      const text = m.text || ''
      if (!text) continue
      if (m.role === 'user') turns.push({ role: 'user', text })
      else turns.push({ role: 'bot', text, html: renderMD(text) })
    }
    if (paused && paused.question && paused.partial) {
      turns.push({ role: 'user', text: paused.question })
      turns.push({ role: 'bot', text: paused.partial, html: renderMD(paused.partial), paused: true })
      pausedTurn.value = paused
      status.value = '上一轮被暂停，可继续'
    }
    toBottom()
  } catch (_) { /* 历史拉不到按空会话处理 */ }
}

window.addEventListener('focus', () => { if (input.value) input.value.focus() })
loadHistory()
startEvents()
// 页面关闭时停掉订阅：留着连接会在内核里留下一条没人读的连接。
window.addEventListener('pagehide', () => stopEvents())
</script>

<template>
  <div id="panel">
    <div id="inputRow">
      <div id="logo"></div>
      <textarea id="q" ref="input" v-model="q" rows="1" placeholder="问小五任何事，或让它安排事项…"
                autofocus autocomplete="off" spellcheck="false"
                @keydown="onKeydown" @input="autosize"></textarea>
      <span id="hint">Esc 收起</span>
    </div>
    <div id="log" ref="log" @scroll="onScroll" @click="onLogClick">
      <div v-if="!turns.length" id="empty">
        直接输入问题，<b>Enter</b> 发送、<b>Shift+Enter</b> 换行；收起后回答在后台继续跑完。<br>
        这里和主窗口是同一条对话，<b>⌘L</b> 只清本面板显示。
      </div>
      <div v-for="(t, i) in turns" :key="i" class="turn" :class="[t.role, t.kind, { paused: t.paused }]">
        <div v-if="t.role === 'user'" class="ask">{{ t.text }}</div>
        <div v-else class="chat-md" v-html="t.html"></div>
      </div>
      <div v-if="busy" id="running"><span class="dot"></span>小五在忙</div>
    </div>
    <div id="foot">
      <span id="status">{{ status }}</span>
      <span id="actions">
        <button v-if="busy" type="button" @click="stop">暂停</button>
        <button v-else-if="pausedTurn" type="button" @click="resumeTurn">继续</button>
        <button v-if="!busy && turns.length" type="button" @click="clearScreen">清屏</button>
      </span>
    </div>
  </div>
</template>

<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  html, body { background: transparent; height: 100%; overflow: hidden; }
  #palette-root { height: 100%; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", "Segoe UI",
                 "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
    padding: 10px;
    -webkit-user-select: none; user-select: none;
  }
  /* 深色炭灰面板（参考 Cursor：无色相表面 + 细线边框），浮于桌面 */
  #panel {
    display: flex; flex-direction: column;
    height: 100%;
    border-radius: 18px;
    background: rgba(20, 20, 20, .92);
    border: 1px solid rgba(255, 255, 255, .10);
    box-shadow: 0 24px 64px -14px rgba(0, 0, 0, .7);
    backdrop-filter: blur(28px) saturate(1.4);
    -webkit-backdrop-filter: blur(28px) saturate(1.4);
    overflow: hidden;
    color: #E8E8E8;
  }
  #inputRow {
    display: flex; align-items: flex-start; gap: 10px;
    padding: 14px 16px;
    border-bottom: 1px solid rgba(255, 255, 255, .08);
  }
  /* 品牌标：白底 + 中心黑点（弹窗自带样式，不引 style.css，几何与全站一致：
     圆角 28%、圆点 44%）。深色桌面上底板用亮灰面，仍是同一块标。 */
  #logo { position: relative; width: 30px; height: 30px; border-radius: 8px; flex: none;
    background: #E8E8E8; }
  #logo::after { content: ""; position: absolute; left: 50%; top: 50%;
    width: 13px; height: 13px; transform: translate(-50%, -50%);
    border-radius: 50%; background: rgba(20, 20, 20, .92); }
  #q {
    flex: 1; background: transparent; border: 0; outline: 0; resize: none;
    color: #E8E8E8; font-size: 16px; line-height: 1.5; caret-color: #6EA8F2;
    max-height: 96px; padding-top: 3px;
    font-family: inherit;
  }
  #q::placeholder { color: #616161; }
  #hint { font-size: 11px; color: #616161; border: 1px solid rgba(255,255,255,.14);
    border-radius: 999px; padding: 2px 10px; flex: none; margin-top: 4px; }
  #log {
    flex: 1; overflow-y: auto; padding: 12px 18px 4px;
    display: flex; flex-direction: column; gap: 12px;
    font-size: 14px; line-height: 1.65; word-break: break-word;
    -webkit-user-select: text; user-select: text;
  }
  #empty { color: #616161; font-size: 13px; line-height: 1.9; }
  #empty b { color: #9A9A9A; font-weight: 600; }
  .turn { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
  .turn.user { align-items: flex-end; }
  .turn.err .chat-md { color: #FCA5A5; }
  .ask {
    max-width: 88%; padding: 5px 11px; border-radius: 12px;
    background: rgba(255, 255, 255, .07); border: 1px solid rgba(255,255,255,.08);
    color: #CFCFCF; font-size: 13.5px; white-space: pre-wrap;
  }
  .turn.paused .chat-md { border-left: 2px solid rgba(110, 168, 242, .5); padding-left: 10px; }
  #running { font-size: 12px; color: #949494; display: flex; align-items: center; gap: 7px; }
  #running .dot { width: 6px; height: 6px; border-radius: 50%; background: #6EA8F2;
    animation: pulse 1.1s ease-in-out infinite; }
  @keyframes pulse { 0%, 100% { opacity: .25; } 50% { opacity: 1; } }
  #foot {
    display: flex; align-items: center; justify-content: space-between; gap: 10px;
    padding: 6px 18px 12px; font-size: 12px; color: #949494;
  }
  #status:empty { display: none; }
  #actions { display: flex; gap: 8px; margin-left: auto; }
  #actions button {
    appearance: none; font: inherit; color: #CFCFCF; cursor: pointer;
    background: rgba(255,255,255,.06); border: 1px solid rgba(255,255,255,.12);
    border-radius: 999px; padding: 2px 12px;
  }
  #actions button:hover { background: rgba(255,255,255,.12); color: #fff; }
  /* 弹窗不加载全局 style.css，chat-md 排版自带一份精简版（配色沿用面板色板）。 */
  .chat-md { white-space: normal; line-height: 1.7; }
  .chat-md p { margin: 0 0 6px; }
  .chat-md > :last-child { margin-bottom: 0; }
  .chat-md ul, .chat-md ol { margin: 2px 0 6px; padding-left: 20px; }
  .chat-md li { margin: 2px 0; }
  .chat-md code { background: rgba(255,255,255,.09); padding: 1px 5px; border-radius: 5px;
    font-size: 13px; font-family: ui-monospace, Menlo, Consolas, monospace; }
  .chat-md pre { background: #0D0D0D; color: #E6E6EA; padding: 11px 13px;
    border-radius: 10px; overflow-x: auto; margin: 6px 0;
    box-shadow: inset 0 0 0 1px rgba(255,255,255,.08); }
  .chat-md pre code { background: none; padding: 0; color: inherit; }
  .chat-md a { color: #6EA8F2; text-decoration: none; }
  .chat-md a:hover { text-decoration: underline; }
  .chat-md table { border-collapse: collapse; margin: 6px 0; font-size: 13px; }
  .chat-md th, .chat-md td { border: 1px solid rgba(255,255,255,.14); padding: 4px 8px; }
  .chat-md blockquote { margin: 6px 0; padding: 2px 10px;
    border-left: 3px solid rgba(255,255,255,.18); color: #B4B4B4; }
  .chat-md h1, .chat-md h2, .chat-md h3 { margin: 8px 0 4px; font-size: 15px; font-weight: 650; }
  ::-webkit-scrollbar { width: 8px; }
  ::-webkit-scrollbar-thumb { background: rgba(255,255,255,.14); border-radius: 8px;
    border: 2px solid transparent; background-clip: content-box; }
</style>
