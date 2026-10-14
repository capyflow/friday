<script setup>
// MCP 服务器：本地命令(stdio) / 远程 URL(http) 两种接入，保存即热生效。
import { onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'
import { toast } from '../../store.js'
import SelectBox from '../SelectBox.vue'

const TYPE_OPTIONS = [
  { value: 'stdio', label: '本地命令' },
  { value: 'http', label: '远程 URL' },
]

const servers = reactive([])     // { type, name, target, argsStr, extraStr }
const statusByName = ref({})
const msg = reactive({ text: '', cls: '' })
let timer = null
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
  } catch (e) { /* 状态缺失按离线展示 */ }
  servers.splice(0, servers.length, ...list.map(toRow))
}
function state(x) {
  const st = statusByName.value[x.name] || {}
  if (!x.name) return { text: '—', running: false, tools: 0, lastError: '' }
  return { running: !!st.running, tools: st.tools ?? 0, lastError: st.lastError || '' }
}
function addRow() {
  servers.push({ type: 'stdio', name: '', target: '', argsStr: '', extraStr: '' })
}
function collect() {
  return servers.map(r => {
    const row = { type: r.type, name: r.name.trim(), command: '', args: [], env: {}, url: '', headers: {} }
    if (r.type === 'http') {
      row.url = r.target.trim()
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
function removeRow(i) { servers.splice(i, 1) }

onMounted(load)
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
        <span class="grow"></span>
        <button class="btn sm" @click="reconnect(x)">重连</button>
        <button class="btn sm danger" @click="removeRow(i)">删除</button>
      </div>
      <label class="mr-field"><span>{{ x.type === 'http' ? '远程地址' : '本地命令' }}</span>
        <input v-model="x.target" :placeholder="x.type === 'http' ? 'http(s)://…' : '如 npx / python3'">
      </label>
      <label v-if="x.type === 'stdio'" class="mr-field"><span>参数</span>
        <input v-model="x.argsStr" placeholder="空格分隔">
      </label>
      <label class="mr-field">
        <span>{{ x.type === 'http' ? '请求头' : '环境变量' }}</span>
        <input v-model="x.extraStr" :placeholder="x.type === 'http' ? 'KEY: VALUE，逗号分隔（鉴权等）' : 'KEY=VALUE，逗号分隔'">
      </label>
      <div v-if="x.name && !state(x).running && state(x).lastError" class="mr-error meta">{{ state(x).lastError }}</div>
    </div>
  </div>
  <div v-else class="empty">未配置 MCP 服务器——点击"添加服务器"开始</div>
  <div class="row" style="margin-top:10px">
    <button class="btn" type="button" @click="addRow">＋ 添加服务器</button>
    <button class="btn primary" type="button" @click="save">保存并生效</button>
  </div>
  <div class="msg" :class="msg.cls">{{ msg.text }}</div>
  <div class="set-note">两种接入方式二选一：<b>本地命令</b>（Friday 拉起本机子进程，崩溃自动重启）
    或<b>远程 URL</b>（连接托管 MCP 服务，Streamable HTTP，可带鉴权请求头）。
    保存后立即热生效并写回 config.yaml，无需重启服务。</div>
</template>
