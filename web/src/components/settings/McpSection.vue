<script setup>
// MCP 服务器：本地命令(stdio) / 远程 URL(http) 两种接入，保存即热生效。
// 远程服务器可选两种鉴权：静态请求头（手工填 token）或 OAuth（点「授权」在
// 浏览器登录服务商，Friday 存令牌并自动刷新，过期/被撤销时回到「待授权」）。
import { onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'
import { toast } from '../../store.js'
import { openExternal } from '../../open-url.js'
import { copyText } from '../../clipboard.js'
import SelectBox from '../SelectBox.vue'
import EmptyState from '../art/EmptyState.vue'

const TYPE_OPTIONS = [
  { value: 'stdio', label: '本地命令' },
  { value: 'http', label: '远程 URL' },
]
const AUTH_OPTIONS = [
  { value: 'header', label: '静态请求头' },
  { value: 'oauth', label: '浏览器授权' },
]
// 授权状态 → 徽标文案（dot 颜色沿用状态页既有配色：run=琥珀色待办）。
const AUTH_BADGE = {
  required: { text: '待授权', cls: 'run' },
  authorizing: { text: '等待授权…', cls: 'run' },
  authorized: { text: '已授权', cls: 'ok' },
  error: { text: '授权失败', cls: 'bad' },
}
const servers = reactive([])     // { type, name, target, argsStr, extraStr, auth }
const statusByName = ref({})
const msg = reactive({ text: '', cls: '' })
let timer = null
const authPolls = new Map()      // 服务器名 → 轮询定时器（授权在浏览器里完成后自动收起）
const inFlight = ref({})         // 服务器名 → 已点「授权」且仍在等回调（按钮立即置灰，不等后端状态回传）
const authURLs = ref({})         // 服务器名 → 进行中流程的授权地址（自动打开失败时复制用）
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 4000)
}
function toRow(x) {
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
  }
}
async function load() {
  let list = []
  try { list = (await api('GET', '/api/mcp/config')).servers || [] }
  catch (e) { toast('MCP 配置加载失败：' + e.message, true) }
  statusByName.value = {}
  try {
    const st = (await api('GET', '/api/mcp')).servers || []
    statusByName.value = Object.fromEntries(st.map(x => [x.name, x]))
    // 页面刷新后仍有进行中的授权：接着轮询，第一次查询顺带把授权链接拿回来（复制兜底）。
    for (const s of st) {
      if (s.name && s.authState === 'authorizing' && !authPolls.has(s.name)) pollAuth(s.name)
    }
  } catch (e) { /* 状态缺失按离线展示 */ }
  servers.splice(0, servers.length, ...list.map(toRow))
}
function status(x) {
  return statusByName.value[x.name] || {}
}
function state(x) {
  const st = status(x)
  if (!x.name) return { text: '—', running: false, tools: 0, lastError: '' }
  return { running: !!st.running, tools: st.tools ?? 0, lastError: st.lastError || '' }
}
// 授权徽标：只有声明了 OAuth 的远程服务器才有状态；后端没给状态时按待授权展示。
function authBadge(x) {
  if (x.type !== 'http' || x.auth !== 'oauth' || !x.name) return null
  const st = status(x)
  return AUTH_BADGE[st.authState || 'required'] || AUTH_BADGE.required
}
function authMessage(x) {
  return status(x).authMessage || ''
}
function addRow() {
  servers.push({ type: 'stdio', name: '', target: '', argsStr: '', extraStr: '', auth: 'header' })
}
function collect() {
  return servers.map(r => {
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
  })
}
async function save() {
  try {
    const r = await api('POST', '/api/mcp/config', { servers: collect() })
    show(r.message || '已保存', true)
    load()
  } catch (e) { show(e.message, false) }
}
async function reconnect(x) {
  if (!x.name) return
  try {
    await api('POST', '/api/mcp/' + encodeURIComponent(x.name) + '/reconnect', {})
    toast('已重连 ' + x.name)
    load()
  } catch (e) { toast(e.message, true) }
}
// 授权：后端做完发现与客户端注册后给出授权地址，这里交给系统浏览器（桌面外壳下
// window.open 无效，openExternal 会转走外壳命令）；轮询状态直到用户登录完成
// （后端拿到令牌后会自动重连，无需再点）。没打开浏览器就露出「复制授权链接」。
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
  const t = setInterval(async () => {
    try {
      const r = await api('GET', '/api/mcp/' + encodeURIComponent(name) + '/auth')
      setAuthURL(name, r.url || '') // 刷新后重新进入也能补回授权链接
      if (r.state === 'authorizing') return
      stopPoll(name)
      clearFlight(name)
      clearAuthURL(name)
      load()
      if (r.state === 'authorized') toast('已连接 ' + name)
      else if (r.state === 'error') toast('授权失败：' + (r.message || '请重试'), true)
    } catch (e) {
      stopPoll(name)
      clearFlight(name)
    }
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
    load()
  } catch (e) { toast(e.message, true) }
}
function removeRow(i) { servers.splice(i, 1) }

onMounted(load)
onBeforeUnmount(() => { clearTimeout(timer); authPolls.forEach(t => clearInterval(t)); authPolls.clear() })
</script>

<template>
  <div v-if="servers.length" class="mcp-list">
    <div v-for="(x, i) in servers" :key="i" class="mcp-row">
      <div class="mr-head">
        <SelectBox v-model="x.type" :options="TYPE_OPTIONS" compact aria-label="接入方式" style="width:116px"></SelectBox>
        <input v-model="x.name" class="mr-name" placeholder="名称" aria-label="服务器名称">
        <span class="mr-state">
          <template v-if="!x.name"><span class="meta">未命名</span></template>
          <template v-else-if="state(x).running"><span class="dot ok"></span>在线 · {{ state(x).tools }} 工具</template>
          <template v-else><span class="dot bad"></span>离线</template>
        </span>
        <span v-if="authBadge(x)" class="mr-state"><span class="dot" :class="authBadge(x).cls"></span>{{ authBadge(x).text }}</span>
        <span class="grow"></span>
        <button v-if="x.type === 'http' && x.auth === 'oauth'" class="btn sm primary"
                :disabled="!x.name || waitingAuth(x)" @click="authorize(x)">
          {{ waitingAuth(x) ? '等待授权…' : '授权' }}
        </button>
        <button v-if="x.type === 'http' && x.auth === 'oauth' && status(x).authState === 'authorized'"
                class="btn sm" @click="disconnect(x)">断开授权</button>
        <button class="btn sm" @click="reconnect(x)">重连</button>
        <button class="btn sm danger" @click="removeRow(i)">删除</button>
      </div>
      <label class="mr-field"><span>{{ x.type === 'http' ? '远程地址' : '本地命令' }}</span>
        <input v-model="x.target" :placeholder="x.type === 'http' ? 'http(s)://…' : '如 npx / python3'">
      </label>
      <label v-if="x.type === 'stdio'" class="mr-field"><span>参数</span>
        <input v-model="x.argsStr" placeholder="空格分隔">
      </label>
      <label v-if="x.type === 'http'" class="mr-field"><span>鉴权方式</span>
        <SelectBox v-model="x.auth" :options="AUTH_OPTIONS" compact aria-label="鉴权方式"></SelectBox>
      </label>
      <label class="mr-field">
        <span>{{ x.type === 'http' ? (x.auth === 'oauth' ? '附加请求头' : '请求头') : '环境变量' }}</span>
        <input v-model="x.extraStr"
               :placeholder="x.type === 'http'
                 ? (x.auth === 'oauth' ? '一般留空，OAuth 令牌由 Friday 管理' : 'KEY: VALUE，逗号分隔（鉴权等）')
                 : 'KEY=VALUE，逗号分隔'">
      </label>
      <div v-if="x.name && authMessage(x) && authBadge(x)?.cls !== 'ok'" class="mr-error meta">{{ authMessage(x) }}</div>
      <div v-else-if="x.name && !state(x).running && state(x).lastError" class="mr-error meta">{{ state(x).lastError }}</div>
      <div v-if="waitingAuth(x) && authLink(x)" class="mr-authlink meta">
        浏览器没弹出？<button class="btn sm" type="button" @click="copyAuthLink(x)">复制授权链接</button>
        <span>贴到浏览器里打开即可，完成后这里会自动变成「已授权」</span>
      </div>
    </div>
  </div>
  <EmptyState v-else kind="mcp" :size="92">未配置 MCP 服务器——点击"添加服务器"开始</EmptyState>
  <div class="row" style="margin-top:10px">
    <button class="btn" type="button" @click="addRow">＋ 添加服务器</button>
    <button class="btn primary" type="button" @click="save">保存并生效</button>
  </div>
  <div class="msg" :class="msg.cls">{{ msg.text }}</div>
  <div class="set-note">两种接入方式二选一：<b>本地命令</b>（Friday 拉起本机子进程，崩溃自动重启）
    或<b>远程 URL</b>（连接托管 MCP 服务，Streamable HTTP）。远程服务器的鉴权可选
    <b>静态请求头</b>（自己填 token）或<b>浏览器授权</b>：点「授权」在浏览器里登录服务商，
    令牌由 Friday 存在数据目录里并自动刷新，失效时会重新显示「待授权」。
    保存后立即热生效并写回 config.yaml，无需重启服务。</div>
</template>
