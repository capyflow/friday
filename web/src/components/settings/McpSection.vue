<script setup>
// MCP 服务器：一台一张卡（与设置 → 工具同一套卡片外壳），头部只留身份与状态，
// 点这一行（图标 / 名称 / 摘要）就摊开表单，再点一次收回；右上角「更多」是另一件事，
// 管的是「对这台服务器动手」——重连、删除都收在那儿，卡片收起时也能用。新增走弹窗表单。
// 展开区底部只留「取消 / 保存」，靠右；保存即热生效，所以没有整页的「保存并生效」——
// 每张卡的「保存」只提交这一张（其余按上次保存的值原样带上），改 A 不会把 B 没保存的草稿一起写进去。
// 远程服务器可选两种鉴权：静态请求头（手工填 token）或 OAuth（在展开的表单里点
// 「授权」去浏览器登录服务商；已授权后这个按钮变「重新授权」，另有「断开授权」）。
import { onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'
import { toast } from '../../store.js'
import { openExternal } from '../../open-url.js'
import { copyText } from '../../clipboard.js'
import McpFields from './McpFields.vue'
import EmptyState from '../art/EmptyState.vue'

// 授权状态 → 徽标文案（沿用状态页既有配色：run=琥珀色待办）。
const AUTH_BADGE = {
  required: { text: '待授权', cls: 'run' },
  authorizing: { text: '等待授权…', cls: 'run' },
  authorized: { text: '已授权', cls: 'ok' },
  error: { text: '授权失败', cls: 'bad' },
}
// 令牌落库后后端自己重启这台服务器（握手最长 handshakeTimeout×2 ≈ 20s），
// 前端按 1.5s 一轮再等 16 轮，等到状态里 running 为真才收尾刷新。
const AUTH_CONNECT_WAIT_TICKS = 16

const saved = ref([])            // 上次保存的整份配置（后端原样），逐卡保存按它叠加
const servers = reactive([])     // 与 saved 同序的工作副本（多带一个 open）
const statusByName = ref({})
const msg = reactive({ text: '', cls: '' })
const saving = ref(false)
const dlg = reactive({ open: false, row: draft(), saving: false, error: '' })
let timer = null
const authPolls = new Map()      // 服务器名 → 轮询定时器（授权在浏览器里完成后自动收尾）
const inFlight = ref({})         // 服务器名 → 已点「授权」且仍在等回调（按钮立即置灰，不等后端状态回传）
const authURLs = ref({})         // 服务器名 → 进行中流程的授权地址（自动打开失败时复制用）
const menuFor = ref(-1)          // 开着「更多」菜单的卡片下标：一次只开一个，菜单项按行号定位

function draft() {
  return { type: 'stdio', name: '', target: '', argsStr: '', extraStr: '', auth: 'header' }
}
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 4000)
}
function toRow(x, open = false) {
  const isHTTP = x.type ? x.type === 'http' : !!(x.url || '').trim()
  return {
    type: isHTTP ? 'http' : 'stdio',
    name: x.name || '',
    target: isHTTP ? (x.url || '') : (x.command || ''),
    argsStr: (x.args || []).join(' '),
    auth: isHTTP ? (x.auth === 'oauth' ? 'oauth' : 'header') : 'header',
    extraStr: isHTTP
      ? Object.entries(x.headers || {}).map(([k, v]) => k + ': ' + v).join(', ')
      : Object.entries(x.env || {}).map(([k, v]) => k + '=' + v).join(', '),
    open,
  }
}
// 工作副本 → 后端配置行（与 config.McpServerConfig 同形）
function payloadOf(r) {
  const row = { type: r.type, name: r.name.trim(), command: '', args: [], env: {}, url: '', headers: {}, auth: '' }
  if (r.type === 'http') {
    row.url = r.target.trim()
    row.auth = r.auth === 'oauth' ? 'oauth' : 'header'
    r.extraStr.split(',').map(s => s.trim()).filter(Boolean).forEach(pair => {
      const idx = pair.indexOf(':')
      if (idx > 0) row.headers[pair.slice(0, idx).trim()] = pair.slice(idx + 1).trim()
    })
  } else {
    row.command = r.target.trim()
    row.args = r.argsStr.trim().split(/\s+/).filter(Boolean)
    r.extraStr.split(',').map(s => s.trim()).filter(Boolean).forEach(pair => {
      const idx = pair.indexOf('=')
      if (idx > 0) row.env[pair.slice(0, idx).trim()] = pair.slice(idx + 1).trim()
    })
  }
  return row
}
async function statusOnce() {
  const list = (await api('GET', '/api/mcp')).servers || []
  statusByName.value = Object.fromEntries(list.map(x => [x.name, x]))
  return list
}
async function load() {
  closeMenu()   // 整组列表要换，旧行号不能再指着东西
  let list = []
  try { list = (await api('GET', '/api/mcp/config')).servers || [] }
  catch (e) { toast('MCP 配置加载失败：' + e.message, true) }
  saved.value = list
  // 重新拉配置（首屏、授权收尾）不该把用户正开着的那张卡合上；没名字的保持摊开。
  const opened = new Set(servers.filter(x => x.open).map(x => x.name))
  servers.splice(0, servers.length, ...list.map(x => toRow(x, !x.name || opened.has(x.name))))
  try {
    const st = await statusOnce()
    // 页面刷新后仍有进行中的授权：接着轮询，第一次查询顺带把授权链接拿回来（复制兜底）。
    for (const s of st) {
      if (s.name && s.authState === 'authorizing' && !authPolls.has(s.name)) pollAuth(s.name)
    }
  } catch (e) { /* 状态缺失按离线展示 */ }
}
// 配置只有全量这一个写入口，所以「保存这一张卡」＝上次保存的列表里换掉第 i 项。
// 别张卡没保存的草稿原样留在界面上，不会被顺手写进去。
async function postConfig(list) {
  const r = await api('POST', '/api/mcp/config', { servers: list })
  // 存一份副本：list 这个数组会跟着请求体被调用方/测试观察到，不能在这儿往上加东西
  saved.value = list.map(x => ({ ...x }))
  try { await statusOnce() } catch (e) { /* 状态缺失按离线展示 */ }
  return r
}
function status(x) { return statusByName.value[x.name] || {} }
function state(x) {
  const st = status(x)
  if (!x.name) return { text: '—', running: false, tools: 0, lastError: '' }
  return { running: !!st.running, tools: st.tools ?? 0, lastError: st.lastError || '' }
}
// 卡片头部：徽章说清「能不能用」，摘要说清「是哪一台」——整条地址/命令收在 title 里，
// 展开表单才看全（同工作区列表不铺路径的取舍）。
function stateBadge(x) {
  const s = state(x)
  if (!x.name.trim()) return { text: '未保存', cls: '' }
  return s.running ? { text: `在线 · ${s.tools} 工具`, cls: 'ok' } : { text: '离线', cls: 'bad' }
}
function summary(x) {
  const kind = x.type === 'http' ? '远程 URL' : '本地命令'
  const target = [x.target, x.argsStr].map(v => (v || '').trim()).filter(Boolean).join(' ')
  return target ? `${kind} · ${target}` : `${kind} · 还没填${x.type === 'http' ? '地址' : '命令'}`
}
// 授权徽标：只有声明了 OAuth 的远程服务器才有状态；后端没给状态时按待授权展示。
function authBadge(x) {
  if (x.type !== 'http' || x.auth !== 'oauth') return null
  return AUTH_BADGE[status(x).authState || 'required'] || AUTH_BADGE.required
}
function isAuthorized(x) { return status(x).authState === 'authorized' }
function authMessage(x) { return status(x).authMessage || '' }

/* ---------- 逐卡保存 / 取消 / 删除 ---------- */
function rowError(r, i) {
  if (!r.name.trim()) return '先给这台服务器起个名字'
  if (!r.target.trim()) return r.type === 'http' ? '填上远程地址' : '填上本地命令'
  if (saved.value.some((x, j) => j !== i && (x.name || '').trim() === r.name.trim())) {
    return `已经有叫「${r.name.trim()}」的服务器`
  }
  return ''
}
async function saveRow(i) {
  const row = servers[i]
  const err = rowError(row, i)
  if (err) { show(err, false); return }
  saving.value = true
  try {
    const r = await postConfig(saved.value.map((x, j) => (j === i ? payloadOf(row) : x)))
    row.open = false
    show(r.message || `已保存 ${row.name.trim()}`, true)
  } catch (e) { show(e.message, false) }
  finally { saving.value = false }
}
function cancelRow(i) {
  Object.assign(servers[i], toRow(saved.value[i]))
  servers[i].open = false
}
async function removeRow(i) {
  const name = (servers[i].name || '').trim()
  saving.value = true
  try {
    await postConfig(saved.value.filter((_, j) => j !== i))
    saved.value.splice(i, 1)
    servers.splice(i, 1)
    show(name ? `已删除 ${name}，配置即刻生效` : '已删除这条配置', true)
  } catch (e) { show(e.message, false) }
  finally { saving.value = false }
}

/* ---------- 新增：弹窗表单 ---------- */
function openAdd() {
  dlg.row = draft()
  dlg.error = ''
  dlg.open = true
}
function closeAdd() {
  if (dlg.saving) return
  dlg.open = false
}
async function saveAdd() {
  const err = rowError(dlg.row, -1)
  if (err) { dlg.error = err; return }
  dlg.saving = true
  try {
    const payload = payloadOf(dlg.row)
    await postConfig([...saved.value.map(x => ({ ...x })), payload])
    servers.push(toRow(payload))
    dlg.open = false
    show(`已添加 ${payload.name}，配置即刻生效`, true)
  } catch (e) { dlg.error = e.message }
  finally { dlg.saving = false }
}

/* ---------- 重连与授权 ---------- */
async function reconnect(x) {
  if (!x.name) return
  try {
    await api('POST', '/api/mcp/' + encodeURIComponent(x.name) + '/reconnect', {})
    toast('已重连 ' + x.name)
    await load()
  } catch (e) { toast(e.message, true) }
}
// 授权：后端做完发现与客户端注册后给出授权地址，这里交给系统浏览器（桌面外壳下
// window.open 无效，openExternal 会转走外壳命令）。没打开浏览器就露出「复制授权链接」。
async function authorize(x) {
  if (!x.name) return
  stopPoll(x.name)
  inFlight.value = { ...inFlight.value, [x.name]: true }
  try {
    const r = await api('POST', '/api/mcp/' + encodeURIComponent(x.name) + '/authorize', {})
    const url = r.url || ''
    if (url) {
      setAuthURL(x.name, url)
      const ok = await openExternal(url)
      if (!ok) show('没能自动打开浏览器——点「复制授权链接」贴到浏览器里完成', false)
      else show(r.message || '请在浏览器里完成授权', true)
    } else {
      show(r.message || '请在浏览器里完成授权', true)
    }
    pollAuth(x.name)
  } catch (e) {
    clearFlight(x.name)
    toast(e.message, true)
  }
}
function setAuthURL(name, url) {
  if (!url) return
  authURLs.value = { ...authURLs.value, [name]: url }
}
function clearAuthURL(name) {
  if (!authURLs.value[name]) return
  const next = { ...authURLs.value }
  delete next[name]
  authURLs.value = next
}
function authLink(x) { return authURLs.value[x.name] || '' }
async function copyAuthLink(x) {
  const url = authLink(x)
  if (!url) return
  try {
    await copyText(url)
    show('授权链接已复制，粘到浏览器里打开即可', true)
  } catch (e) { toast('复制失败：' + (e?.message || e), true) }
}
function pollAuth(name) {
  let waited = 0
  const t = setInterval(async () => {
    let r
    try {
      r = await api('GET', '/api/mcp/' + encodeURIComponent(name) + '/auth')
    } catch (e) {
      stopPoll(name)
      clearFlight(name)
      return
    }
    setAuthURL(name, r.url || '') // 刷新后重新进入也能补回授权链接
    if (r.state === 'authorizing') return
    // 「已授权」不等于「已连上」：令牌刚落库时后端还在自动重启这台服务器，这时候收尾
    // 刷新只会显示离线，害得人再点一次重连。再等几轮，直到状态里 running 为真。
    if (r.state === 'authorized' && waited < AUTH_CONNECT_WAIT_TICKS) {
      const up = await statusOnce().then(l => l.some(x => x.name === name && x.running)).catch(() => false)
      if (!up) { waited++; return }
    }
    stopPoll(name)
    clearFlight(name)
    clearAuthURL(name)
    await load()
    if (r.state === 'authorized') {
      if (statusByName.value[name]?.running) toast('已连接 ' + name)
      else toast('已授权，但连接还没起来——右上角「更多」里点「重连」', true)
    } else if (r.state === 'error') toast('授权失败：' + (r.message || '请重试'), true)
  }, 1500)
  authPolls.set(name, t)
}
function clearFlight(name) {
  const next = { ...inFlight.value }
  delete next[name]
  inFlight.value = next
}
// 授权按钮的"进行中"判定：点了就该立刻不可重复点，不依赖后端状态回传时机。
function waitingAuth(x) {
  return !!inFlight.value[x.name] || status(x).authState === 'authorizing'
}
function stopPoll(name) {
  const t = authPolls.get(name)
  if (t) { clearInterval(t); authPolls.delete(name) }
}
async function disconnect(x) {
  if (!x.name) return
  try {
    const r = await api('POST', '/api/mcp/' + encodeURIComponent(x.name) + '/disconnect', {})
    toast(r.message || '已断开授权')
    await load()
  } catch (e) { toast(e.message, true) }
}

/* ---------- 「更多」菜单：对这台服务器动手 ---------- */
function toggleMenu(i) { menuFor.value = menuFor.value === i ? -1 : i }
function closeMenu() { if (menuFor.value >= 0) menuFor.value = -1 }
// 菜单项点完就收：重连、删除都是一次性动作，留着只会挡住下面那张卡。
// 下标先对一遍：菜单开着时列表被别处刷新过，就不该再照着旧行号动手。
async function menuReconnect(x, i) {
  if (menuFor.value !== i) return
  closeMenu()
  await reconnect(x)
}
async function menuRemove(i) {
  if (menuFor.value !== i) return
  closeMenu()
  await removeRow(i)
}
// 点外面收起：菜单和它的触发按钮同挂在 .ws-menu-wrap（工作区列表那套行内菜单外壳）里，
// 判外侧用 closest——在菜单里挪一下鼠标不算离开。
function onDocPointerdown(e) {
  if (menuFor.value < 0) return
  // closest 只有元素才有：target 落在 document/window 上时按「点外面」算
  if (!e.target?.closest?.('.ws-menu-wrap')) closeMenu()
}
function onDocKeydown(e) {
  if (e.key !== 'Escape' || menuFor.value < 0) return
  // 只收菜单，别把整层设置弹窗一起关掉：本组件的监听先于 SettingsModal 注册，
  // 同节点上的监听要用 stopImmediatePropagation 才拦得住。
  e.stopImmediatePropagation()
  closeMenu()
}

onMounted(() => {
  load()
  document.addEventListener('pointerdown', onDocPointerdown)
  document.addEventListener('keydown', onDocKeydown)
})
onBeforeUnmount(() => {
  clearTimeout(timer); authPolls.forEach(t => clearInterval(t)); authPolls.clear()
  document.removeEventListener('pointerdown', onDocPointerdown)
  document.removeEventListener('keydown', onDocKeydown)
})
</script>

<template>
  <!-- 新增入口固定在列表右上角：不管已经配了几台，都在同一个位置，不用滚到列表底下找 -->
  <div class="row mcp-bar">
    <button class="btn" type="button" @click="openAdd">＋ 添加服务器</button>
  </div>
  <div v-if="servers.length" class="svc-list">
    <div v-for="(x, i) in servers" :key="i" class="svc-card">
      <div class="svc-head">
        <!-- 整行都是开关：点图标 / 名称 / 摘要就把字段摊开，再点一次收回 -->
        <button class="svc-disclose" type="button" :aria-expanded="x.open ? 'true' : 'false'"
                :aria-controls="'mcp-body-' + i"
                :aria-label="(x.name || '未命名服务器') + (x.open ? '：收起配置' : '：展开配置')"
                @click="x.open = !x.open">
          <span class="svc-icon" aria-hidden="true">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor"
                 stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/>
              <path d="M7 7.5h.01M7 16.5h.01"/>
            </svg>
          </span>
          <span class="svc-info">
            <b>
              <span class="svc-name" :title="x.name">{{ x.name || '未命名服务器' }}</span>
              <span class="svc-badge" :class="stateBadge(x).cls">{{ stateBadge(x).text }}</span>
              <span v-if="authBadge(x)" class="svc-badge" :class="authBadge(x).cls">{{ authBadge(x).text }}</span>
            </b>
            <span class="svc-desc" :title="summary(x)">{{ summary(x) }}</span>
          </span>
          <svg class="svc-caret" :class="{ open: x.open }" width="14" height="14" viewBox="0 0 24 24" fill="none"
               stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"
               aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>
        </button>
        <!-- 「更多」= 对这台服务器动手：重连、删除收在这儿，卡片收起时也点得到。
             菜单外壳沿用工作区/智能体列表那套（.ws-menu-wrap + .ws-menu），不再新造一份样式 -->
        <span class="ws-menu-wrap">
          <button class="ib-btn ws-icon" type="button" :class="{ active: menuFor === i }"
                  :aria-label="(x.name || '未命名服务器') + ' 的更多操作'" aria-haspopup="menu"
                  :aria-expanded="menuFor === i" title="更多" @click="toggleMenu(i)">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
              <circle cx="5" cy="12" r="1.7"/><circle cx="12" cy="12" r="1.7"/><circle cx="19" cy="12" r="1.7"/>
            </svg>
          </button>
          <div v-if="menuFor === i" class="ws-menu pop-anim" role="menu">
            <button role="menuitem" type="button" :disabled="!x.name || saving"
                    @click="menuReconnect(x, i)">重连</button>
            <button role="menuitem" type="button" class="danger" :disabled="saving"
                    @click="menuRemove(i)">删除</button>
          </div>
        </span>
      </div>
      <div v-show="x.open" :id="'mcp-body-' + i" class="svc-body">
        <McpFields v-model:row="servers[i]" :disabled="saving" />
        <div v-if="x.type === 'http' && x.auth === 'oauth'" class="mr-auth">
          <span class="svc-badge" :class="authBadge(x).cls">{{ authBadge(x).text }}</span>
          <!-- 已经授权过就不再摆一个「授权」在那儿，给的是「重新授权」 -->
          <template v-if="isAuthorized(x)">
            <button class="btn sm" type="button" :disabled="waitingAuth(x)" @click="authorize(x)">重新授权</button>
            <button class="btn sm" type="button" @click="disconnect(x)">断开授权</button>
          </template>
          <button v-else class="btn sm primary" type="button"
                  :disabled="!x.name || waitingAuth(x)" @click="authorize(x)">
            {{ waitingAuth(x) ? '等待授权…' : '授权' }}
          </button>
        </div>
        <div v-if="authMessage(x) && authBadge(x)?.cls !== 'ok'" class="mr-error meta">{{ authMessage(x) }}</div>
        <div v-else-if="!state(x).running && state(x).lastError" class="mr-error meta">{{ state(x).lastError }}</div>
        <div v-if="waitingAuth(x) && authLink(x)" class="mr-authlink meta">
          浏览器没弹出？<button class="btn sm" type="button" @click="copyAuthLink(x)">复制授权链接</button>
          <span>贴到浏览器里打开即可，完成后会自动连上，不用回来点重连</span>
        </div>
        <!-- 展开区只剩这两个：取消在左、保存在右，整组靠右贴边（重连/删除已进「更多」） -->
        <div class="svc-acts">
          <span class="grow"></span>
          <button class="btn sm" type="button" :disabled="saving" @click="cancelRow(i)">取消</button>
          <button class="btn sm primary" type="button" :disabled="saving" @click="saveRow(i)">保存</button>
        </div>
      </div>
    </div>
  </div>
  <EmptyState v-else kind="mcp" :size="92">未配置 MCP 服务器——点右上角「添加服务器」开始</EmptyState>
  <div class="msg" :class="msg.cls">{{ msg.text }}</div>

  <!-- 新增服务器：弹窗表单（同自动化任务那套骨架），保存才落盘；取消 / ✕ / Esc / 点遮罩都算放弃 -->
  <Teleport to="body">
    <div v-if="dlg.open" class="dlg-mask" @click.self="closeAdd">
      <div class="dlg" role="dialog" aria-modal="true" aria-label="添加 MCP 服务器" @keydown.esc="closeAdd">
        <div class="dlg-form">
          <header class="dlg-head">
            <h3 class="dlg-title">添加 MCP 服务器</h3>
            <button type="button" class="set-close" aria-label="关闭添加服务器" @click="closeAdd">
              <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
                   stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
            </button>
          </header>
          <div class="dlg-body">
            <McpFields v-model:row="dlg.row" :disabled="dlg.saving" />
            <p class="meta mcp-dlg-hint">保存即生效：本地命令当场拉起，远程地址当场握手；
              选「浏览器授权」的，保存后再展开这张卡点「授权」。</p>
            <div v-if="dlg.error" class="mr-error meta" role="alert">{{ dlg.error }}</div>
          </div>
          <footer class="dlg-foot">
            <div class="dlg-btns">
              <button class="btn" type="button" :disabled="dlg.saving" @click="closeAdd">取消</button>
              <button class="btn primary" type="button" :disabled="dlg.saving" @click="saveAdd">
                {{ dlg.saving ? '保存中…' : '保存' }}</button>
            </div>
          </footer>
        </div>
      </div>
    </div>
  </Teleport>
</template>
