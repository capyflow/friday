<script setup>
// 设置：模型热更新 / 人设提示词 / 会话管理 / 心跳清单 / MCP 服务器 / 当前配置。
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { api } from '../api.js'
import { fmtDT } from '../format.js'
import { store, toast } from '../store.js'
import SelectBox from './SelectBox.vue'
import ProviderSettings from './ProviderSettings.vue'

const emit = defineEmits(['logout'])

const MCP_TYPE_OPTIONS = [
  { value: 'stdio', label: '本地命令' },
  { value: 'http', label: '远程 URL' },
]

const PERM_MODE_OPTIONS = [
  { value: 'confirm', label: '确认（默认）' },
  { value: 'acceptEdits', label: '自动接受编辑' },
  { value: 'fullAccess', label: '完全信任' },
]
const PERM_MODE_DESC = {
  confirm: '写文件、改 MCP 配置和白名单外的命令都会在网页弹卡片，批准后执行',
  acceptEdits: '写文件、改 MCP 配置免审批直行；白名单外的命令仍需网页批准',
  fullAccess: '全部免审批直行，仅毁灭性命令黑名单（rm -rf、shutdown 等）仍强制拦截',
}

/* ---------- 当前配置 / 模型 ---------- */
const status = ref(null)
const who = computed(() => store.username || '—')

/* ---------- 权限模式 ---------- */
const permMode = ref('confirm')
const permMsg = reactive({ text: '', cls: '' })
let permTimer = null
function showPermMsg(t, ok) {
  permMsg.text = t; permMsg.cls = ok ? 'ok' : 'err'
  clearTimeout(permTimer); permTimer = setTimeout(() => { permMsg.text = '' }, 4000)
}
async function loadPermissions() {
  try { permMode.value = (await api('GET', '/api/permissions')).mode || 'confirm' }
  catch (e) { /* 设置页仍可离线浏览 */ }
}
async function savePermissions() {
  if (permMode.value === 'fullAccess' &&
    !confirm('完全信任模式下，执行命令、写文件、改配置都不再询问（毁灭性命令仍拦截）。确定切换？')) return
  try {
    const r = await api('POST', '/api/permissions', { mode: permMode.value })
    showPermMsg(r.message || '已切换', true)
  } catch (e) { showPermMsg(e.message, false) }
}

const settingsRows = computed(() => {
  const st = status.value
  return [
    ['当前账号', who.value || '—'],
    ['LLM 模型', st && st.provider.model],
    ['API 地址', st && st.provider.baseURL],
    ['任务文件', st && st.files.tasks],
    ['待办文件', st && st.files.todos],
    ['记忆目录', st && st.files.memories],
    ['服务地址', location.host],
  ]
})

async function providerSaved() {
  try { status.value = await api('GET', '/api/status') } catch (e) { toast(e.message, true) }
  window.dispatchEvent(new CustomEvent('friday:setup-refresh'))
}

/* ---------- 人设提示词 ---------- */
const personaPresets = ref([])
const personaOptions = computed(() =>
  personaPresets.value.map(p => ({ value: p.id, label: p.name })))
const personaActive = ref('')
const personaSelect = ref('')
const personaMsg = reactive({ text: '', cls: '' })
let personaTimer = null
function showPersonaMsg(t, ok) {
  personaMsg.text = t; personaMsg.cls = ok ? 'ok' : 'err'
  clearTimeout(personaTimer); personaTimer = setTimeout(() => { personaMsg.text = '' }, 4000)
}
const curPersona = computed(() =>
  personaPresets.value.find(p => p.id === personaSelect.value) || null)
// 正文编辑落在 draft 上：切换人设/重新加载时重置，用于判断"是否改动过"
const personaDraft = ref('')
function fillPersonaDraft() { personaDraft.value = (curPersona.value && curPersona.value.prompt) || '' }
watch(personaSelect, fillPersonaDraft)
const personaMark = computed(() => !!(curPersona.value && curPersona.value.custom))
const showRevert = computed(() => !!(curPersona.value && curPersona.value.builtin && curPersona.value.custom))
const showDelete = computed(() => !!(curPersona.value && !curPersona.value.builtin))

async function loadPersona(selectId) {
  try {
    const r = await api('GET', '/api/persona')
    personaPresets.value = r.presets || []
    personaActive.value = r.active || 'default'
    personaSelect.value = personaPresets.value.some(p => p.id === (selectId || personaActive.value))
      ? (selectId || personaActive.value) : 'default'
    fillPersonaDraft()
    return true
  } catch (e) { showPersonaMsg(e.message, false); return false }
}
// 收集当前全部自定义预设（发后端整体替换用）
function personaCustoms() {
  return personaPresets.value.filter(x => x.custom)
    .map(x => ({ id: x.id, name: x.name, tagline: x.tagline || '', prompt: x.prompt || '' }))
}
async function applyPersona() {
  const p = curPersona.value
  if (!p) return
  const promptText = personaDraft.value.trim()
  if (!promptText) { showPersonaMsg('提示词正文不能为空', false); return }
  const body = { active: p.id }
  if (promptText !== (p.prompt || '').trim()) {
    const customs = personaCustoms()
    const hit = customs.find(x => x.id === p.id)
    if (hit) hit.prompt = promptText
    else customs.push({ id: p.id, name: p.name, tagline: p.tagline || '', prompt: promptText })
    body.presets = customs
  }
  try {
    const r = await api('POST', '/api/persona', body)
    showPersonaMsg(r.message || '已应用', true)
    await loadPersona(p.id)
  } catch (e) { showPersonaMsg(e.message, false) }
}
async function addPersona() {
  const name = (window.prompt('给新自定义人设起个名字（如：毒舌损友）') || '').trim()
  if (!name) return
  const customs = personaCustoms()
  customs.push({ id: 'custom-' + Date.now(), name, tagline: '自定义人设', prompt: '' })
  try {
    await api('POST', '/api/persona', { active: personaActive.value, presets: customs })
    showPersonaMsg('已创建「' + name + '」，写好提示词后点"应用并热生效"', true)
    await loadPersona(customs[customs.length - 1].id)
  } catch (e) { showPersonaMsg(e.message, false) }
}
async function revertPersona() {
  const p = curPersona.value
  if (!p || !p.builtin || !p.custom) return
  if (!confirm('恢复「' + p.name + '」为内置提示词？当前自定义文案将被丢弃。')) return
  try {
    const r = await api('POST', '/api/persona',
      { active: personaActive.value, presets: personaCustoms().filter(x => x.id !== p.id) })
    showPersonaMsg(r.message || '已恢复内置', true)
    await loadPersona(p.id)
  } catch (e) { showPersonaMsg(e.message, false) }
}
async function deletePersona() {
  const p = curPersona.value
  if (!p || p.builtin) return
  if (!confirm('删除自定义人设「' + p.name + '」？')) return
  const next = personaActive.value === p.id ? 'default' : personaActive.value
  try {
    const r = await api('POST', '/api/persona',
      { active: next, presets: personaCustoms().filter(x => x.id !== p.id) })
    showPersonaMsg(r.message || '已删除', true)
    await loadPersona(next)
  } catch (e) { showPersonaMsg(e.message, false) }
}

/* ---------- 会话管理 ---------- */
const sessions = ref([])
async function loadSessions() {
  try { sessions.value = (await api('GET', '/api/sessions')).sessions || [] }
  catch (e) { toast('会话加载失败：' + e.message, true) }
}
async function clearSession(key) {
  if (!confirm('确定清除会话 ' + key + ' 的全部上下文？')) return
  try {
    await api('POST', '/api/context/clear', { session: key })
    toast('会话 ' + key + ' 已清除')
    loadSessions()
  } catch (e) { toast(e.message, true) }
}

/* ---------- 心跳清单 ---------- */
const hbContent = ref('')
const hbPath = ref('')
const hbMsg = reactive({ text: '', cls: '' })
let hbTimer = null
function showHbMsg(t, ok) {
  hbMsg.text = t; hbMsg.cls = ok ? 'ok' : 'err'
  clearTimeout(hbTimer); hbTimer = setTimeout(() => { hbMsg.text = '' }, 4000)
}
async function loadHeartbeat() {
  try {
    const r = await api('GET', '/api/heartbeat')
    hbContent.value = r.content || ''
    hbPath.value = r.path + (r.note || '')
  } catch (e) { showHbMsg(e.message, false) }
}
async function saveHeartbeat() {
  try {
    await api('POST', '/api/heartbeat', { content: hbContent.value })
    showHbMsg('已保存，下轮心跳生效', true)
  } catch (e) { showHbMsg(e.message, false) }
}

/* ---------- MCP 服务器 ---------- */
const mcpServers = reactive([])     // { type, name, target, argsStr, extraStr }
const mcpStatusByName = ref({})
const mcpMsg = reactive({ text: '', cls: '' })
let mcpTimer = null
function showMcpMsg(t, ok) {
  mcpMsg.text = t; mcpMsg.cls = ok ? 'ok' : 'err'
  clearTimeout(mcpTimer); mcpTimer = setTimeout(() => { mcpMsg.text = '' }, 4000)
}
function mcpRow(x) {
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
async function loadMCP() {
  let servers = []
  try { servers = (await api('GET', '/api/mcp/config')).servers || [] }
  catch (e) { toast('MCP 配置加载失败：' + e.message, true) }
  mcpStatusByName.value = {}
  try {
    const st = (await api('GET', '/api/mcp')).servers || []
    mcpStatusByName.value = Object.fromEntries(st.map(x => [x.name, x]))
  } catch (e) { /* 状态缺失按离线展示 */ }
  mcpServers.splice(0, mcpServers.length, ...servers.map(mcpRow))
}
function mcpStatus(x) {
  const st = mcpStatusByName.value[x.name] || {}
  if (!x.name) return { text: '—', running: false, tools: 0, lastError: '' }
  return { running: !!st.running, tools: st.tools ?? 0, lastError: st.lastError || '' }
}
function addMcpRow() {
  mcpServers.push({ type: 'stdio', name: '', target: '', argsStr: '', extraStr: '' })
}
function collectMcpServers() {
  return mcpServers.map(r => {
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
async function saveMCP() {
  try {
    const r = await api('POST', '/api/mcp/config', { servers: collectMcpServers() })
    showMcpMsg(r.message || '已保存', true)
    loadMCP()
  } catch (e) { showMcpMsg(e.message, false) }
}
async function reconnectMCP(x) {
  if (!x.name) return
  try {
    await api('POST', '/api/mcp/' + encodeURIComponent(x.name) + '/reconnect', {})
    toast('已重连 ' + x.name)
    loadMCP()
  } catch (e) { toast(e.message, true) }
}
function removeMcpRow(i) { mcpServers.splice(i, 1) }

onMounted(async () => {
  try { status.value = await api('GET', '/api/status') } catch (e) { /* 设置页仍可离线浏览 */ }
  loadSessions()
  loadHeartbeat()
  loadMCP()
  loadPersona()
  loadPermissions()
})
</script>

<template>
  <section class="view" id="view-settings">
    <div class="view-inner">
      <h2>设置</h2>

      <div class="panel">
        <div class="panel-title">模型配置</div>
        <ProviderSettings :status="status" @saved="providerSaved" />
      </div>

      <div class="panel">
        <div class="panel-title">权限模式</div>
        <div class="row">
          <SelectBox v-model="permMode" :options="PERM_MODE_OPTIONS" aria-label="权限模式" style="min-width:150px"></SelectBox>
          <button class="btn primary" type="button" @click="savePermissions">切换并热生效</button>
        </div>
        <div class="meta" style="margin-top:8px">{{ PERM_MODE_DESC[permMode] }}</div>
        <div class="msg" :class="permMsg.cls">{{ permMsg.text }}</div>
        <div class="set-note">参照 zcode 的多档权限设计：<b>确认</b>（默认）危险操作逐次网页批准，可"批准并记住"；
          <b>自动接受编辑</b>日常改文件/改配置不打扰，跑命令仍把关；<b>完全信任</b>全部直行（黑名单仍拦截），
          适合本机私人使用。切换立即热生效并写回 config.yaml。</div>
      </div>

      <div class="panel">
        <div class="panel-title">人设提示词</div>
        <div class="row">
          <SelectBox v-model="personaSelect" :options="personaOptions" aria-label="选择人设" style="min-width:132px"></SelectBox>
          <span class="meta">{{ curPersona ? (curPersona.tagline || '') : '' }}</span>
          <span v-if="personaMark" class="meta">已自定义</span>
        </div>
        <textarea id="personaPrompt" v-model="personaDraft" rows="6" class="ta-plain"
          style="margin-top:10px" placeholder="人设正文：描述这个性格下小五怎么说话、怎么关心人"></textarea>
        <div class="row" style="margin-top:8px">
          <button class="btn primary" type="button" @click="applyPersona">应用并热生效</button>
          <button class="btn" type="button" @click="addPersona">＋ 新增自定义</button>
          <button v-if="showRevert" class="btn" type="button" @click="revertPersona">恢复内置</button>
          <button v-if="showDelete" class="btn danger" type="button" @click="deletePersona">删除自定义</button>
        </div>
        <div class="msg" :class="personaMsg.cls">{{ personaMsg.text }}</div>
        <div class="set-note">人设只改变小五的性格与语气，能力和工具规则不受影响；所有人设都保留
          <b>人文关怀</b>底线（深夜关怀、天气添衣带伞、低落时的安慰）。应用后立即热生效（无需重启，
          对话上下文保持连续）并写回 config.yaml。内置人设的提示词改动会保存为同名自定义覆盖，可一键恢复内置。</div>
      </div>

      <div class="panel">
        <div class="panel-title">会话管理</div>
        <table v-if="sessions.length">
          <thead><tr><th>会话</th><th>消息数</th><th>最后活跃</th><th></th></tr></thead>
          <tbody>
            <tr v-for="x in sessions" :key="x.key">
              <td class="mono">{{ x.key }}</td>
              <td>{{ x.messages }}</td>
              <td class="meta">{{ fmtDT(x.updatedAt) }}</td>
              <td><button class="btn sm danger" @click="clearSession(x.key)">清除</button></td>
            </tr>
          </tbody>
        </table>
        <div v-else class="empty">还没有任何会话</div>
        <div class="set-note">每个入口（网页/终端/钉钉/飞书聊天）拥有独立会话，历史落盘重启不丢；
          清除只影响对应会话。</div>
      </div>

      <div class="panel">
        <div class="panel-title">心跳清单（HEARTBEAT.md）</div>
        <textarea id="hbContent" v-model="hbContent" rows="6" class="ta-plain"
          placeholder="每轮心跳把这份清单交给小五自主检查，如：&#10;- 检查 notes.txt 里的提醒是否需要汇报&#10;- 若用户超过一天没互动，写一句问候"></textarea>
        <div class="row" style="margin-top:8px">
          <button class="btn primary" type="button" @click="saveHeartbeat">保存清单</button>
          <span class="meta">{{ hbPath }}</span>
        </div>
        <div class="msg" :class="hbMsg.cls">{{ hbMsg.text }}</div>
        <div class="set-note">心跳按 config.yaml 的 heartbeat.schedule/interval 定时触发；
          agent 输出首行 [推送]/[静默] 决定是否打扰；静默时段（quietHours）内只记录不推送。</div>
      </div>

      <div class="panel">
        <div class="panel-title">MCP 服务器 <span class="count">{{ mcpServers.length ? mcpServers.length + ' 台' : '' }}</span></div>
        <table v-if="mcpServers.length">
          <thead><tr><th>名称</th><th>方式</th><th>本地命令 / 远程地址</th><th>参数</th><th>环境变量 / 请求头</th><th>状态</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="(x, i) in mcpServers" :key="i">
              <td><input v-model="x.name" placeholder="名称" style="width:88px"></td>
              <td>
                <SelectBox v-model="x.type" :options="MCP_TYPE_OPTIONS" compact style="width:116px"></SelectBox>
              </td>
              <td>
                <input v-if="x.type === 'http'" v-model="x.target" placeholder="远程地址，http(s)://…" style="width:100%; min-width:150px">
                <input v-else v-model="x.target" placeholder="本地命令，如 npx / python3" style="width:100%; min-width:150px">
              </td>
              <td>
                <input v-if="x.type === 'http'" value="" disabled placeholder="远程无需参数" style="width:100%; min-width:110px; opacity:.5">
                <input v-else v-model="x.argsStr" placeholder="空格分隔" style="width:100%; min-width:110px">
              </td>
              <td>
                <input v-if="x.type === 'http'" v-model="x.extraStr" placeholder="KEY: VALUE，逗号分隔（鉴权等）" style="width:150px">
                <input v-else v-model="x.extraStr" placeholder="KEY=VALUE，逗号分隔" style="width:150px">
              </td>
              <td class="st-cell">
                <template v-if="!x.name">—</template>
                <template v-else-if="mcpStatus(x).running"><span class="dot ok"></span>在线 · {{ mcpStatus(x).tools }} 工具</template>
                <template v-else><span class="dot bad"></span>离线</template>
                <div v-if="x.name && !mcpStatus(x).running && mcpStatus(x).lastError" class="meta">{{ mcpStatus(x).lastError }}</div>
              </td>
              <td style="white-space:nowrap">
                <button class="btn sm" @click="reconnectMCP(x)">重连</button>
                <button class="btn sm danger" @click="removeMcpRow(i)">删除</button>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-else class="empty">未配置 MCP 服务器——点击"添加服务器"开始</div>
        <div class="row" style="margin-top:10px">
          <button class="btn" type="button" @click="addMcpRow">＋ 添加服务器</button>
          <button class="btn primary" type="button" @click="saveMCP">保存并生效</button>
        </div>
        <div class="msg" :class="mcpMsg.cls">{{ mcpMsg.text }}</div>
        <div class="set-note">两种接入方式二选一：<b>本地命令</b>（Friday 拉起本机子进程，崩溃自动重启）
          或<b>远程 URL</b>（连接托管 MCP 服务，Streamable HTTP，可带鉴权请求头）。
          保存后立即热生效并写回 config.yaml，无需重启服务。</div>
      </div>

      <div class="panel">
        <div class="panel-title">当前配置</div>
        <table><tbody id="settingsRows">
          <tr v-for="([k, v]) in settingsRows" :key="k">
            <td class="meta">{{ k }}</td>
            <td class="mono">{{ v == null ? '—' : v }}</td>
          </tr>
        </tbody></table>
        <div class="set-note">管理员账号通过启动配置设置：环境变量 FRIDAY_ADMIN_USERNAME / FRIDAY_ADMIN_PASSWORD
          或 config.yaml 的 admin.username / admin.password，修改后重启服务生效。</div>
      </div>

      <button class="btn danger" @click="emit('logout')">
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-out"/></svg>退出登录
      </button>
    </div>
  </section>
</template>
