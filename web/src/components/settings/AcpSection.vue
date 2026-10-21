<script setup>
// 设置 → 智能体：ACP agent 清单与运行参数。保存写回 config.yaml 并热生效
// （配置变化的 agent 回收进程，上下文由下次打开对话时 session/load 尝试恢复）。
// 「测试」做一次握手诊断：拉起进程 → initialize → 立即回收，最常见的"没登录"
// 由此变成一句话报错（带 stderr 尾部）。
import { onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'

const cfg = reactive({
  enabled: false, maxConcurrent: 2, idleKillSeconds: 600,
  turnTimeoutSeconds: 300, handshakeTimeoutSeconds: 10, showThought: true,
  agents: [],
})
const busy = ref(false)
const catalog = ref([])
const installing = ref('') // 正在安装的目录条目 id
const msg = reactive({ text: '', cls: '' })
let timer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 5000)
}

function blankAgent() {
  return { name: '', command: '', argsText: '', envText: '', workspace: '',
           enabled: true, turnTimeoutSeconds: 0, degradeResume: false, version: '', testing: false }
}
function toAgent(a) {
  return {
    name: (a.name || '').trim(),
    command: (a.command || '').trim(),
    args: (a.argsText || '').split(',').map(s => s.trim()).filter(Boolean),
    env: parseEnv(a.envText),
    workspace: (a.workspace || '').trim(),
    enabled: a.enabled,
    turnTimeoutSeconds: Math.max(0, Math.floor(Number(a.turnTimeoutSeconds) || 0)),
    degradeResume: a.degradeResume,
    version: a.version || '',   // 安装时登记的版本戳（M-4），保存时原样带回
  }
}
function parseEnv(text) {
  const env = {}
  for (const line of (text || '').split('\n')) {
    const i = line.indexOf('=')
    if (i > 0) env[line.slice(0, i).trim()] = line.slice(i + 1).trim()
  }
  return env
}
function envText(env) {
  return Object.entries(env || {}).map(([k, v]) => `${k}=${v}`).join('\n')
}

const refreshing = ref(false)
async function loadCatalog(refresh) {
  try {
    const r = await api('GET', '/api/acp/catalog' + (refresh ? '?refresh=1' : ''))
    catalog.value = r.catalog || []
  } catch { /* 目录加载失败不阻塞主配置 */ }
}
async function refreshCatalog() {
  refreshing.value = true
  try {
    await loadCatalog(true)
    show('官方目录已刷新', true)
  } finally { refreshing.value = false }
}

// 安装目录条目：后端装完会自动注册进清单（写回配置并热生效），
// 这里把返回的 agent 模板同步进本地表单，列表即刻可见。
async function installAgent(entry) {
  installing.value = entry.id
  try {
    const r = await api('POST', '/api/acp/install', { id: entry.id })
    await loadCatalog()
    if (r.agent) {
      const existing = cfg.agents.find(a => a.name === r.agent.name)
      const installed = {
        command: r.agent.command,
        argsText: (r.agent.args || []).join(', '), version: r.agent.version || '',
      }
      if (existing) Object.assign(existing, installed)
      else cfg.agents.push({ ...blankAgent(), ...installed, name: r.agent.name,
        envText: envText(r.agent.env) })
    }
    show(r.message || '安装完成', true)
  } catch (e) {
    show('安装失败：' + e.message, false)
  } finally { installing.value = '' }
}

// 把目录模板追加进 agent 清单；同名已存在则只提示。
function appendTemplate(entry) {
  if (!entry.template) return
  if (cfg.agents.some(a => a.name === entry.template.name)) {
    show(`清单里已有名为「${entry.template.name}」的 agent，未重复添加`, true)
    return
  }
  const t = entry.template
  // 只有模板命令就是被探测的 CLI 时才回填绝对路径；npx/uvx 适配器模板要保留启动器。
  const command = t.command === entry.check ? (entry.path || t.command) : t.command
  cfg.agents.push({
    ...blankAgent(), name: t.name,
    command,
    argsText: (t.args || []).join(', '),
  })
}

async function load() {
  loadCatalog()
  try {
    const r = await api('GET', '/api/acp/config')
    const a = r.acp || {}
    Object.assign(cfg, {
      enabled: !!a.enabled,
      maxConcurrent: a.maxConcurrent ?? 2,
      idleKillSeconds: a.idleKillSeconds ?? 600,
      turnTimeoutSeconds: a.turnTimeoutSeconds ?? 300,
      handshakeTimeoutSeconds: a.handshakeTimeoutSeconds ?? 10,
      showThought: a.showThought !== false,
    })
    cfg.agents = (a.agents || []).map(x => ({
      ...blankAgent(), name: x.name, command: x.command,
      argsText: (x.args || []).join(', '), envText: envText(x.env),
      workspace: x.workspace || '', enabled: x.enabled !== false,
      turnTimeoutSeconds: x.turnTimeoutSeconds || 0, degradeResume: !!x.degradeResume,
      version: x.version || '',
    }))
  } catch (e) { show(e.message, false) }
}

async function save() {
  busy.value = true
  try {
    const payload = {
      enabled: cfg.enabled, maxConcurrent: num(cfg.maxConcurrent, 2),
      idleKillSeconds: num(cfg.idleKillSeconds, 600),
      turnTimeoutSeconds: num(cfg.turnTimeoutSeconds, 300),
      handshakeTimeoutSeconds: num(cfg.handshakeTimeoutSeconds, 10),
      showThought: cfg.showThought,
      agents: cfg.agents.map(toAgent),
    }
    const r = await api('POST', '/api/acp/config', { acp: payload })
    show(r.message || '已保存', true)
    window.dispatchEvent(new CustomEvent('friday:acp-refresh'))
  } catch (e) { show(e.message, false) } finally { busy.value = false }
}
function num(v, dft) {
  const n = Math.floor(Number(v))
  return Number.isFinite(n) && n > 0 ? n : dft
}

async function test(a) {
  a.testing = true
  try {
    const r = await api('POST', '/api/acp/test', { name: a.name })
    show((a.name || 'agent') + '：' + (r.message || '握手成功'), true)
  } catch (e) { show((a.name || 'agent') + '：' + e.message, false) } finally { a.testing = false }
}
function addAgent() { cfg.agents.push(blankAgent()) }
function removeAgent(i) { cfg.agents.splice(i, 1) }

onMounted(load)
</script>

<template>
  <div class="acp-settings" :aria-busy="busy">
    <div class="acp-global">
      <label class="acp-check">
        <input v-model="cfg.enabled" type="checkbox">
        <span>启用智能体（侧栏「智能体」页；启用前先在下方添加 agent 并点「测试」确认已登录）</span>
      </label>
      <div class="acp-grid">
        <label><span>进程并发上限</span><input v-model="cfg.maxConcurrent" type="number" min="1"></label>
        <label><span>空闲回收（秒）</span><input v-model="cfg.idleKillSeconds" type="number" min="0"></label>
        <label><span>单轮超时（秒）</span><input v-model="cfg.turnTimeoutSeconds" type="number" min="1"></label>
        <label><span>握手超时（秒）</span><input v-model="cfg.handshakeTimeoutSeconds" type="number" min="1"></label>
      </div>
      <label class="acp-check">
        <input v-model="cfg.showThought" type="checkbox">
        <span>显示 agent 思考流</span>
      </label>
    </div>

    <div class="acp-catalog">
      <div class="acp-catalog-head">安装新 agent
        <button class="btn sm" type="button" style="margin-left:8px"
                :disabled="refreshing" @click="refreshCatalog">
          {{ refreshing ? '刷新中…' : '刷新官方目录' }}</button>
      </div>
      <div class="acp-catalog-grid">
        <div v-for="c in catalog" :key="c.id" class="acp-cat-card">
          <div class="acp-cat-top">
            <b>{{ c.name }}<span v-if="c.version" class="acp-cat-ver"> v{{ c.version }}</span></b>
            <span v-if="c.supported === false" class="acp-cat-badge no">本平台无分发包</span>
            <span v-else-if="c.updateAvailable" class="acp-cat-badge warn">可升级</span>
            <span v-else-if="c.registered" class="acp-cat-badge ok">已添加</span>
            <span v-else-if="c.installed" class="acp-cat-badge ok">已安装</span>
            <span v-else class="acp-cat-badge no">未安装</span>
          </div>
          <div class="meta">{{ c.description }}</div>
          <div class="acp-cat-acts">
            <button v-if="c.supported === false" class="btn sm" type="button" disabled>不可安装</button>
            <button v-else-if="c.updateAvailable" class="btn sm primary" type="button"
                    :disabled="installing === c.id" @click="installAgent(c)">
              {{ installing === c.id ? '升级中…' : '一键升级' }}</button>
            <button v-else-if="!c.installed" class="btn sm primary" type="button"
                    :disabled="installing === c.id" @click="installAgent(c)">
              {{ installing === c.id ? '安装中…（最长 5 分钟）' : '一键安装' }}</button>
            <button v-else-if="!c.registered" class="btn sm" type="button" @click="appendTemplate(c)">添加到清单</button>
          </div>
          <div v-if="c.installed && c.path" class="meta mono acp-cat-path">{{ c.path }}</div>
        </div>
      </div>
      <div class="set-note">目录 = 内置精选 + 官方 ACP 注册表（与 Zed 同源，上游上架新 agent 点「刷新官方目录」即可见）。
      安装走各 agent 的官方分发；安装 ≠ 登录——装完用清单行的「测试」做握手诊断，未登录会在报错里给指引。</div>
    </div>

    <div class="acp-agents">
      <div v-for="(a, i) in cfg.agents" :key="i" class="acp-agent">
        <div class="acp-agent-head">
          <b>{{ a.name || '未命名 agent' }}</b>
          <span class="pm-acts">
            <button class="btn sm" type="button" :disabled="a.testing || !a.name" @click="test(a)">
              {{ a.testing ? '测试中…' : '测试' }}</button>
            <button class="btn sm danger" type="button" @click="removeAgent(i)">删除</button>
          </span>
        </div>
        <div class="acp-grid">
          <label><span>名称</span><input v-model="a.name" placeholder="codex"></label>
          <label><span>启动命令</span><input v-model="a.command" placeholder="npx 或绝对路径"></label>
          <label><span>参数（逗号分隔）</span><input v-model="a.argsText" placeholder='-y, @zed-industries/codex-acp'></label>
          <label><span>预填工作目录</span><input v-model="a.workspace" placeholder="~/projects"></label>
          <label><span>单轮超时覆盖（秒，0=全局）</span><input v-model="a.turnTimeoutSeconds" type="number" min="0"></label>
        </div>
        <label class="acp-env"><span>额外环境变量（每行 K=V）</span>
          <textarea v-model="a.envText" rows="2" placeholder="API_KEY=sk-..."></textarea></label>
        <div class="acp-checks">
          <label class="acp-check"><input v-model="a.enabled" type="checkbox"><span>启用</span></label>
          <label class="acp-check"><input v-model="a.degradeResume" type="checkbox">
            <span>会话恢复不可靠（重开对话不再尝试重接，直接新开并标注）</span></label>
        </div>
      </div>
      <button class="btn sm" type="button" @click="addAgent">+ 添加 agent</button>
    </div>

    <div class="row" style="margin-top:12px">
      <button class="btn primary" type="button" :disabled="busy" @click="save">保存并生效</button>
    </div>
    <div class="msg" :class="msg.cls">{{ msg.text }}</div>
    <div class="set-note">敏感操作经网页审批卡人工放行（30 分钟无人处理自动拒绝）。要让它真的弹卡，
      需把 agent 自己配置里的审批模式调紧为"每次询问"——否则 agent 按自身配置自审批，Friday 收不到请求。
      进程空闲后自动回收；重开对话经 session/load 重接上下文，恢复失败的 agent 勾选「会话恢复不可靠」走降级。</div>
  </div>
</template>

<style scoped>
.acp-catalog { margin-bottom: 18px; }
.acp-catalog-head { font-size: 13px; font-weight: 650; margin-bottom: 8px; }
.acp-catalog-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 10px; }
.acp-cat-card { border: 1px solid var(--border); border-radius: 12px; padding: 10px 12px;
                display: flex; flex-direction: column; gap: 6px; }
.acp-cat-top { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.acp-cat-top b { font-size: 13px; }
.acp-cat-badge { font-size: 11px; padding: 2px 8px; border-radius: 999px; }
.acp-cat-badge.ok { background: rgba(22, 163, 74, .12); color: #16A34A; }
.acp-cat-badge.no { background: rgba(127, 127, 127, .14); color: var(--muted-fg); }
.acp-cat-badge.warn { background: rgba(217, 119, 6, .14); color: #D97706; }
.acp-cat-ver { font-weight: 450; color: var(--muted-fg); font-size: 11px; }
.acp-cat-acts { display: flex; gap: 8px; margin-top: 2px; }
.acp-cat-path { font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.acp-global { display: flex; flex-direction: column; gap: 10px; margin-bottom: 16px; }
.acp-check { display: flex; align-items: center; gap: 8px; font-size: 13px; cursor: pointer; }
.acp-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 10px; }
.acp-grid label, .acp-env { display: flex; flex-direction: column; gap: 4px; font-size: 12px; }
.acp-grid input, .acp-env textarea {
  font: inherit; font-size: 13px; padding: 7px 9px; border: 1px solid var(--border-strong);
  border-radius: 8px; background: var(--bg); color: var(--fg); }
.acp-env { margin-top: 8px; }
.acp-agents { display: flex; flex-direction: column; gap: 10px; align-items: flex-start; }
.acp-agent { width: 100%; border: 1px solid var(--border); border-radius: 12px; padding: 12px 14px;
             display: flex; flex-direction: column; gap: 10px; }
.acp-agent-head { display: flex; align-items: center; justify-content: space-between; }
.acp-agent-head b { font-size: 13px; }
.acp-checks { display: flex; gap: 18px; flex-wrap: wrap; }
</style>
