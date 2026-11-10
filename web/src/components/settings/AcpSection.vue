<script setup>
// 设置 → 智能体：ACP agent 清单与运行参数。保存写回 config.yaml 并热生效
// （配置变化的 agent 回收进程，上下文由下次打开对话时 session/load 尝试恢复）。
// 「测试」做一次握手诊断：拉起进程 → initialize → 立即回收，最常见的"没登录"
// 由此变成一句话报错（带 stderr 尾部）。
import { onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'
import SelectBox from '../SelectBox.vue'

// 每会话独立进程是三态：auto = 按 agent 判定（CodeBuddy 默认独占）；
// on = 全部独占；off = 全部共享。SelectBox 只收字符串，落盘时转回 bool/null。
const isoOptions = [
  { value: 'auto', label: '按 agent 判定（CodeBuddy 自动独占）' },
  { value: 'on', label: '全部独占' },
  { value: 'off', label: '全部共享' },
]
const isoAgentOptions = [
  { value: 'auto', label: '跟随全局' },
  { value: 'on', label: '独占' },
  { value: 'off', label: '共享' },
]
function boolToTri(v) { return v === true ? 'on' : v === false ? 'off' : 'auto' }
function triToBool(v) { return v === 'on' ? true : v === 'off' ? false : null }

const cfg = reactive({
  enabled: false, idleKillSeconds: 600,
  handshakeTimeoutSeconds: 10, sessionTimeoutSeconds: 60, showThought: true,
  // auto = 按 agent 判定（CodeBuddy 默认独占）；on/off 全局覆盖。
  oneSessionPerProcess: 'auto',
  // 工具门面（facade）：外部 agent 建会话时挂上的 MCP server，能调哪些 Friday
  // 工具由这里的全局清单 + 逐 agent 的 allowedFacadeTools 决定。可编辑态随
  // /api/acp/config 保存；运行态（地址/计数/调用记录）只读，来自 /api/acp/facade。
  facade: { enabled: true, disableAuth: false, tools: [] },
  agents: [],
})
const busy = ref(false)
const catalog = ref([])
// 后端会在工作区清单里注入一台内置 friday（不来自 config.yaml）。本页的可编辑清单
// 不收它，但要说一句——不然「工作区里能选、设置里找不到」看着像漏配。
const hasBuiltin = ref(false)
const installing = ref('') // 正在安装的目录条目 id
const installOutput = ref('') // 安装失败时的命令输出（后端 output 字段）
const msg = reactive({ text: '', cls: '' })
let timer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 5000)
}

// ---------- 工具门面 ----------

// facadeStatus 是运行态快照（url/计数/活授权/最近调用）；facadeTools 是注册表
// 全目录，全局清单和逐 agent 白名单的勾选框都靠它渲染。白名单为空 = 全部工具
// （第一版口径），所以全局那组在空清单时显示为全选。
const facadeStatus = ref(null)
const facadeTools = ref([])
const facadeLoading = ref(false)

async function loadFacade() {
  facadeLoading.value = true
  try {
    const r = await api('GET', '/api/acp/facade')
    facadeStatus.value = r.status || null
    facadeTools.value = r.tools || []
  } catch { /* 门面接口失败不阻塞主配置 */ } finally { facadeLoading.value = false }
}

// 全局清单为空 = 全部工具：勾选框据此显示全选，取消某一个时先展开成全量再从
// 里面剔除（否则"取消"会变成"什么都别用"）。
function globalToolChecked(name) {
  const list = cfg.facade.tools
  return list.length === 0 || list.includes(name)
}
function toggleGlobalTool(name) {
  const list = cfg.facade.tools
  if (list.length === 0) list.push(...facadeTools.value.map(t => t.name))
  toggleIn(list, name)
}

// toggleIn 勾选/取消一个工具名（数组来自 reactive 树，原地改即可触发更新）。
function toggleIn(list, name) {
  const i = list.indexOf(name)
  if (i >= 0) list.splice(i, 1)
  else list.push(name)
}

const OUTCOMES = { ok: '成功', error: '失败', timeout: '超时', denied: '拒绝' }
function outcomeText(o) { return OUTCOMES[o] || o }
function shortTime(iso) {
  // RFC3339（本地时区偏移）截 HH:MM:SS；列表只求一眼可辨先后。
  return String(iso || '').slice(11, 19)
}

// 卡片默认收起：折叠态只留名称 + 启动命令摘要 + 测试/删除，展开才给全字段。
// open 是纯前端状态（toAgent 不带它，不会写回 config.yaml）。
function blankAgent() {
  return { name: '', command: '', argsText: '', envText: '', workspace: '', open: false,
           enabled: true, handshakeTimeoutSeconds: 0, sessionTimeoutSeconds: 0,
           authMethod: '', degradeResume: false, oneSessionPerProcess: 'auto', version: '', testing: false, facadeTools: [] }
}
// launchText 折叠态摘要：按真实启动形态拼一行（命令 + 空格分隔的参数），
// 一眼看出是哪个 agent、怎么起的。
function launchText(a) {
  const args = (a.argsText || '').split(',').map(s => s.trim()).filter(Boolean)
  return [a.command, ...args].filter(Boolean).join(' ') || '未填启动命令'
}
function toAgent(a) {
  return {
    name: (a.name || '').trim(),
    command: (a.command || '').trim(),
    args: (a.argsText || '').split(',').map(s => s.trim()).filter(Boolean),
    env: parseEnv(a.envText),
    // 工作目录不在卡片上填（每个对话建 thread 时自己选目录），但这里原样带回，
    // 免得手写在 config.yaml 里的 acp.agents[].workspace 被一次保存抹掉。
    workspace: (a.workspace || '').trim(),
    enabled: a.enabled,
    handshakeTimeoutSeconds: Math.max(0, Math.floor(Number(a.handshakeTimeoutSeconds) || 0)),
    sessionTimeoutSeconds: Math.max(0, Math.floor(Number(a.sessionTimeoutSeconds) || 0)),
    // 握手后要显式调用的 ACP 认证方法 id（如 qoder 的 qodercli-login）。
    // 不填就保持旧行为：只握手、不认证。
    authMethod: (a.authMethod || '').trim(),
    degradeResume: a.degradeResume,
    // 三态：auto = 跟随全局/按 agent 判定；on = 独占；off = 共享。
    oneSessionPerProcess: triToBool(a.oneSessionPerProcess),
    version: a.version || '',   // 安装时登记的版本戳（M-4），保存时原样带回
    // Friday 工具白名单逐 agent 覆盖：空数组 = 用全局清单（后端不落盘）。
    allowedFacadeTools: (a.facadeTools || []).filter(Boolean),
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

// compareVer 分段比较版本号（与后端 internal/acp 的 compareVersions 同口径）：
// 按 "." 切分逐段比数字，段数不齐右侧补 0；非数字段退化为字符串比较。
// 返回 -1/0/1。
function compareVer(a, b) {
  const as = String(a || '').split('.')
  const bs = String(b || '').split('.')
  for (let i = 0; i < Math.max(as.length, bs.length); i++) {
    const av = as[i] ?? '0'
    const bv = bs[i] ?? '0'
    const an = /^\d+$/.test(av) ? Number(av) : null
    const bn = /^\d+$/.test(bv) ? Number(bv) : null
    if (an !== null && bn !== null) {
      if (an !== bn) return an < bn ? -1 : 1
      continue
    }
    if (av !== bv) return av < bv ? -1 : 1
  }
  return 0
}

// upgradeHint 升级徽标的「v旧 → v新」注记：旧取清单里登记的版本戳（没有就退
// 回目录条目版本），新取后端给的最终可用版本 latestVersion（没有就退回目录
// 版本）。
//
// 只有「新版本严格高于旧版本」才画箭头：后端的 latestVersion 与
// updateAvailable 已同源，但数据来自缓存/npm，理论上仍可能拿到比登记版本更
// 旧的号——那时显示成「v2.180.0 → v2.179.2」就是明晃晃的降级提示，比不显示
// 更糟（2026-11-06 实测）。
function upgradeHint(c) {
  const to = c.latestVersion || c.version
  const reg = cfg.agents.find(a => a.name === (c.template?.name || c.id))
  const from = reg?.version || c.version
  if (!to || !from) return ''
  if (compareVer(to, from) <= 0) return ''
  return `（v${from} → v${to}）`
}

// 安装目录条目：后端装完会自动注册进清单（写回配置并热生效），
// 这里把返回的 agent 模板同步进本地表单，列表即刻可见。
async function installAgent(entry) {
  installing.value = entry.id
  installOutput.value = ''
  try {
    const r = await api('POST', '/api/acp/install', { id: entry.id })
    await loadCatalog()
    if (r.agent) {
      const existing = cfg.agents.find(a => a.name === r.agent.name)
      const installed = {
        command: r.agent.command, open: true,
        argsText: (r.agent.args || []).join(', '), version: r.agent.version || '',
      }
      if (existing) Object.assign(existing, installed)
      else cfg.agents.push({ ...blankAgent(), ...installed, name: r.agent.name,
        envText: envText(r.agent.env) })
    }
    show(r.message || '安装完成', true)
  } catch (e) {
    installOutput.value = e.output || ''
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
    ...blankAgent(), open: true, name: t.name,
    command,
    argsText: (t.args || []).join(', '),
  })
}

async function load() {
  loadCatalog()
  loadFacade()
  try {
    const r = await api('GET', '/api/acp/config')
    const a = r.acp || {}
    Object.assign(cfg, {
      enabled: !!a.enabled,
      idleKillSeconds: a.idleKillSeconds ?? 600,
      handshakeTimeoutSeconds: a.handshakeTimeoutSeconds ?? 10,
      sessionTimeoutSeconds: a.sessionTimeoutSeconds ?? 60,
      showThought: a.showThought !== false,
      oneSessionPerProcess: boolToTri(a.oneSessionPerProcess),
      // 后端回的是归一化后的值：tools 为空会补默认只读清单，勾选框直接以它为准；
      // 全部取消再保存即回到默认（落盘时与默认相同就不写 facade 段）。
      facade: {
        enabled: a.facade?.enabled !== false,
        disableAuth: !!a.facade?.disableAuth,
        tools: [...(a.facade?.tools || [])],
      },
    })
    const list = a.agents || []
    hasBuiltin.value = list.some(x => x.builtin)
    cfg.agents = list.filter(x => !x.builtin).map(x => ({
      ...blankAgent(), name: x.name, command: x.command,
      argsText: (x.args || []).join(', '), envText: envText(x.env),
      workspace: x.workspace || '', enabled: x.enabled !== false,
      degradeResume: !!x.degradeResume,
      oneSessionPerProcess: boolToTri(x.oneSessionPerProcess),
      handshakeTimeoutSeconds: x.handshakeTimeoutSeconds || 0,
      sessionTimeoutSeconds: x.sessionTimeoutSeconds || 0,
      authMethod: x.authMethod || '',
      version: x.version || '',
      facadeTools: [...(x.allowedFacadeTools || [])],
    }))
  } catch (e) { show(e.message, false) }
}

async function save() {
  busy.value = true
  try {
    const payload = {
      enabled: cfg.enabled,
      idleKillSeconds: num(cfg.idleKillSeconds, 600),
      handshakeTimeoutSeconds: num(cfg.handshakeTimeoutSeconds, 10),
      sessionTimeoutSeconds: num(cfg.sessionTimeoutSeconds, 60),
      showThought: cfg.showThought,
      oneSessionPerProcess: triToBool(cfg.oneSessionPerProcess),
      facade: {
        enabled: cfg.facade.enabled,
        disableAuth: cfg.facade.disableAuth,
        tools: [...cfg.facade.tools],
      },
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
function addAgent() { cfg.agents.push({ ...blankAgent(), open: true }) }
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
        <label><span>空闲回收（秒）</span><input v-model="cfg.idleKillSeconds" type="number" min="0"></label>
        <label><span>握手超时（秒）</span><input v-model="cfg.handshakeTimeoutSeconds" type="number" min="1"></label>
        <label><span>建会话超时（秒）</span><input v-model="cfg.sessionTimeoutSeconds" type="number" min="1"
                title="session/new 与 session/load 的等待上限：skill 表多的 agent 首启很慢，10 秒建不出会话"></label>
        <label><span>每会话独立进程</span>
          <SelectBox v-model="cfg.oneSessionPerProcess" :options="isoOptions"
                     aria-label="每会话独立进程（全局）"></SelectBox></label>
      </div>
      <p class="meta acp-coldnote">npx 这类「每次现拉包」的启动命令，第一次拉起会自动按 90 秒冷启动预算等；起过一次之后回到上面的握手值。</p>
      <label class="acp-check">
        <input v-model="cfg.showThought" type="checkbox">
        <span>显示 agent 思考流</span>
      </label>
    </div>

    <div class="acp-facade">
      <div class="acp-catalog-head">Friday 工具门面
        <button class="btn sm" type="button" style="margin-left:8px"
                :disabled="facadeLoading" @click="loadFacade">
          {{ facadeLoading ? '刷新中…' : '刷新状态' }}</button>
      </div>
      <p class="meta">外部 agent（minimax / codex…）建会话时挂上一台 MCP server，即可直接调用勾选的 Friday 工具。
        默认只给只读清单；写文件、执行命令属审批类工具，不能经门面暴露——那类需求 agent 用自己的工具走审批卡。</p>
      <div class="acp-checks">
        <label class="acp-check"><input v-model="cfg.facade.enabled" type="checkbox"><span>启用门面</span></label>
        <label class="acp-check"><input v-model="cfg.facade.disableAuth" type="checkbox">
          <span>关闭鉴权（仅当某家 agent 不转发 Authorization 时用）</span></label>
      </div>
      <div v-if="facadeStatus" class="acp-facade-status meta">
        <span v-if="facadeStatus.url" class="mono">{{ facadeStatus.url }}</span>
        <span>tools/list {{ facadeStatus.lists }} 次 · 调用 {{ facadeStatus.calls }} 次 · 拒绝 {{ facadeStatus.rejects }} 次</span>
        <span v-if="facadeStatus.grants?.length">活授权：{{ facadeStatus.grants.map(g => g.agent || '（关鉴权）').join('、') }}</span>
      </div>
      <div class="acp-facade-tools">
        <label v-for="t in facadeTools" :key="t.name" class="acp-check">
          <input type="checkbox" :checked="globalToolChecked(t.name)"
                 @change="toggleGlobalTool(t.name)">
          <span>{{ t.name }}</span>
        </label>
        <p v-if="!facadeTools.length" class="meta">工具目录未就绪（Friday 大脑还没起工具），稍后点「刷新状态」。</p>
      </div>
      <p class="meta acp-coldnote">留空 = 全部工具（默认）；取消其中某几个才生成白名单。逐 agent 可在其卡片里单独覆盖（不勾 = 沿用这里的全局清单）。</p>
      <table v-if="facadeStatus?.records?.length" class="acp-facade-records" aria-label="最近调用">
        <thead><tr><th>时间</th><th>agent</th><th>工具</th><th>结果</th><th>详情</th></tr></thead>
        <tbody>
          <tr v-for="(r, i) in facadeStatus.records" :key="i">
            <td class="mono">{{ shortTime(r.at) }}</td>
            <td>{{ r.agent || '（关鉴权）' }}</td>
            <td class="mono">{{ r.tool }}</td>
            <td><span class="acp-outcome" :class="r.outcome">{{ outcomeText(r.outcome) }}</span></td>
            <td class="meta">{{ r.detail }}</td>
          </tr>
        </tbody>
      </table>
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
            <span v-else-if="c.updateAvailable" class="acp-cat-badge warn">可升级{{ upgradeHint(c) }}</span>
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
    </div>

    <div class="acp-agents">
      <p v-if="hasBuiltin" class="meta acp-builtin-hint">
        工作区的「新建会话」里还多一台 <b>friday（内置）</b>：一轮对话直接交给内置大脑，
        模型与权限用本页上方的全局设置。它是代码给的，不写进 config.yaml，所以不在下面这份可编辑清单里。
      </p>
      <div v-for="(a, i) in cfg.agents" :key="i" class="acp-agent">
        <div class="acp-agent-head">
          <button class="acp-agent-toggle" type="button" :aria-expanded="a.open ? 'true' : 'false'"
                  :aria-controls="`acp-agent-body-${i}`" @click="a.open = !a.open">
            <span class="acp-agent-caret" :class="{ open: a.open }" aria-hidden="true">›</span>
            <b>{{ a.name || '未命名 agent' }}</b>
            <span v-if="!a.enabled" class="acp-agent-tag">已停用</span>
            <span v-if="a.version" class="acp-agent-tag">v{{ a.version }}</span>
            <span v-if="a.facadeTools?.length" class="acp-agent-tag"
                  :title="`可调用 Friday 工具：${a.facadeTools.join('、')}`">Friday 工具 ×{{ a.facadeTools.length }}</span>
            <span v-if="!a.open" class="meta mono acp-agent-sum" :title="launchText(a)">{{ launchText(a) }}</span>
          </button>
          <span class="pm-acts">
            <button class="btn sm" type="button" :disabled="a.testing || !a.name" @click="test(a)">
              {{ a.testing ? '测试中…' : '测试' }}</button>
            <button class="btn sm danger" type="button" @click="removeAgent(i)">删除</button>
          </span>
        </div>
        <div v-if="a.open" :id="`acp-agent-body-${i}`" class="acp-agent-body">
          <div class="acp-grid">
            <label><span>名称</span><input v-model="a.name" placeholder="codex"></label>
            <label><span>启动命令</span><input v-model="a.command" placeholder="npx 或绝对路径"></label>
            <label><span>参数（逗号分隔）</span><input v-model="a.argsText" placeholder='-y, @zed-industries/codex-acp'></label>
            <label><span>握手超时覆盖（秒，0=全局）</span><input v-model="a.handshakeTimeoutSeconds" type="number" min="0"></label>
            <label><span>建会话超时覆盖（秒，0=全局）</span><input v-model="a.sessionTimeoutSeconds" type="number" min="0"
                    title="该 agent 首启列 skill 表/初始化很慢时调大这一项，不必动全局"></label>
            <label><span>预认证方法（可选）</span><input v-model="a.authMethod" placeholder="qodercli-login"
                    title="握手后显式调用的 ACP authenticate 方法 id，留空则不调用。qoder 不调它时模型清单为空"></label>
          </div>
          <label class="acp-env"><span>额外环境变量（每行 K=V）</span>
            <textarea v-model="a.envText" rows="2" placeholder="API_KEY=sk-..."></textarea></label>
          <div class="acp-checks">
            <label class="acp-check"><input v-model="a.enabled" type="checkbox"><span>启用</span></label>
            <label class="acp-check"><input v-model="a.degradeResume" type="checkbox">
              <span>会话恢复不可靠（重开对话不再尝试重接，直接新开并标注）</span></label>
            <label class="acp-check"><span>每会话独立进程</span>
              <SelectBox v-model="a.oneSessionPerProcess" :options="isoAgentOptions" compact
                         aria-label="每会话独立进程"></SelectBox></label>
          </div>
          <div class="acp-facade-agent">
            <span class="acp-facade-agent-label">Friday 工具白名单（不勾 = 沿用全局清单）</span>
            <div class="acp-facade-tools">
              <label v-for="t in facadeTools" :key="t.name" class="acp-check">
                <input type="checkbox" :checked="a.facadeTools.includes(t.name)"
                       @change="toggleIn(a.facadeTools, t.name)">
                <span>{{ t.name }}</span>
              </label>
            </div>
          </div>
        </div>
      </div>
      <button class="btn sm" type="button" @click="addAgent">+ 添加 agent</button>
    </div>

    <div class="row" style="margin-top:12px">
      <button class="btn primary" type="button" :disabled="busy" @click="save">保存并生效</button>
    </div>
    <div class="msg" :class="msg.cls">{{ msg.text }}</div>
    <pre v-if="installOutput" class="acp-install-output" aria-label="安装命令输出">{{ installOutput }}</pre>
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
.acp-coldnote { margin: 8px 0 0; font-size: 11.5px; line-height: 1.55; }
.acp-grid label, .acp-env { display: flex; flex-direction: column; gap: 4px; font-size: 12px; }
.acp-grid input, .acp-env textarea {
  font: inherit; font-size: 13px; padding: 7px 9px; border: 1px solid var(--border-strong);
  border-radius: 8px; background: var(--bg); color: var(--fg); }
.acp-env { margin-top: 8px; }
.acp-agents { display: flex; flex-direction: column; gap: 10px; align-items: flex-start; }
.acp-builtin-hint { width: 100%; max-width: 76ch; margin: 0; font-size: 11.5px; line-height: 1.55; }
.acp-agent { width: 100%; border: 1px solid var(--border); border-radius: 12px; padding: 12px 14px;
             display: flex; flex-direction: column; gap: 10px; }
.acp-agent-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
.acp-agent-head b { font-size: 13px; }
/* 折叠开关：整行标题即按钮（名称 + 摘要 + 版本/停用标记），点一下展开全字段。 */
.acp-agent-toggle { flex: 1; min-width: 0; display: flex; align-items: center; gap: 8px;
  padding: 2px 0; border: 0; background: none; color: inherit; font: inherit;
  text-align: left; cursor: pointer; }
.acp-agent-caret { color: var(--muted-fg); font-size: 15px; line-height: 1;
  transition: transform .12s ease; }
.acp-agent-caret.open { transform: rotate(90deg); }
.acp-agent-tag { flex: none; font-size: 11px; padding: 1px 7px; border-radius: 999px;
  background: rgba(127, 127, 127, .14); color: var(--muted-fg); }
.acp-agent-sum { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.acp-agent-body { display: flex; flex-direction: column; gap: 10px; }
.acp-checks { display: flex; gap: 18px; flex-wrap: wrap; }
/* 工具门面卡片：状态行、勾选网格、调用记录表。 */
.acp-facade { display: flex; flex-direction: column; gap: 10px; margin-bottom: 18px;
              border: 1px solid var(--border); border-radius: 12px; padding: 12px 14px; }
.acp-facade-status { display: flex; gap: 14px; flex-wrap: wrap; margin: 0; font-size: 11.5px; }
.acp-facade-tools { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
                    gap: 6px 14px; }
.acp-facade-records { width: 100%; border-collapse: collapse; font-size: 12px; }
.acp-facade-records th { text-align: left; font-weight: 600; font-size: 11px;
                         color: var(--muted-fg); padding: 4px 8px 4px 0; }
.acp-facade-records td { padding: 4px 8px 4px 0; border-top: 1px solid var(--border);
                         vertical-align: top; }
.acp-outcome { font-size: 11px; padding: 1px 8px; border-radius: 999px; }
.acp-outcome.ok { background: rgba(22, 163, 74, .12); color: #16A34A; }
.acp-outcome.error, .acp-outcome.timeout { background: rgba(217, 119, 6, .14); color: #D97706; }
.acp-outcome.denied { background: rgba(127, 127, 127, .14); color: var(--muted-fg); }
/* agent 卡片里的逐 agent 白名单。 */
.acp-facade-agent { display: flex; flex-direction: column; gap: 6px; }
.acp-facade-agent-label { font-size: 12px; color: var(--muted-fg); }
.acp-install-output { margin: 8px 0 0; padding: 10px 12px; max-height: 240px; overflow: auto;
  border: 1px solid var(--border); border-radius: 10px; background: var(--bg-subtle);
  font: 11.5px/1.55 ui-monospace, SFMono-Regular, Menlo, monospace; white-space: pre-wrap;
  word-break: break-word; color: var(--muted-fg); }
</style>
