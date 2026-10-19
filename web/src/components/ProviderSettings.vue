<script setup>
// 模型服务（模型库）：多条模型档案（地址/密钥/上下文窗口/最大输出/输入格式），
// radio 选激活，全量保存即热生效。密钥不回显——留空表示保留该条目已存密钥。
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'

// 最大输出 token 的兜底值：留空即按它提交（与后端 config.DefaultMaxTokens 同口径）。
// 推理模型的思考也计入这份预算，给小了会出现「只有思考、没有正文」。
const DEFAULT_MAX_TOKENS = 4096

const emit = defineEmits(['saved'])

const models = reactive([])
const activeName = ref('')   // radio 选中的条目（保存时生效）
const appliedName = ref('')  // 加载时已在生效的条目（「生效中」徽标）
const globalWindow = ref(0)  // context.windowTokens 全局覆盖（0 = 未设置，优先级最高）
const busy = ref(false)
const msg = reactive({ text: '', cls: '' })
let msgTimer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(msgTimer); msgTimer = setTimeout(() => { msg.text = '' }, 4000)
}

function toRow(x) {
  return {
    name: x.name || '',
    baseURL: x.baseURL || '',
    model: x.model || '',
    apiKey: '',
    window: x.contextWindow ? String(x.contextWindow) : '',
    maxTokens: x.maxTokens ? String(x.maxTokens) : '',
    image: !x.inputs || x.inputs.includes('image'),
    keySet: !!x.keySet,
    testing: false,
    testMsg: null,
    _open: false,
  }
}
async function load() {
  let data
  try { data = await api('GET', '/api/provider/models') }
  catch (e) { show('模型库加载失败：' + e.message, false); return }
  globalWindow.value = data.globalWindowTokens || 0
  appliedName.value = data.active || ''
  activeName.value = data.active || ''
  models.splice(0, models.length, ...(data.models || []).map(toRow))
}
function addRow() {
  models.push({
    name: '', baseURL: '', model: '', apiKey: '', window: '', maxTokens: String(DEFAULT_MAX_TOKENS),
    image: true, keySet: false, testing: false, testMsg: null, _open: true,
  })
}
function hostOf(x) {
  try { return new URL(x.baseURL).host } catch { return ''
  }
}
function removeRow(i) {
  const name = models[i].name
  models.splice(i, 1)
  if (name && activeName.value === name) activeName.value = models[0]?.name || ''
}
// 收成非负整数：空/NaN/负数 → 0（由调用方决定兜底）。
function intOf(v) {
  const n = typeof v === 'number' ? v : Number(String(v).trim())
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0
}
function collect() {
  return models.map(r => ({
    name: r.name.trim(),
    baseURL: r.baseURL.trim(),
    model: r.model.trim(),
    apiKey: r.apiKey.trim(),
    contextWindow: intOf(r.window), // 0 = 按模型名自动识别
    maxTokens: intOf(r.maxTokens) || DEFAULT_MAX_TOKENS, // 留空按默认提交
    inputs: r.image ? ['text', 'image'] : ['text'],
  }))
}
function validate(rows) {
  const seen = new Set()
  for (const r of rows) {
    if (!r.name) return '每个模型都需要一个名称'
    if (seen.has(r.name)) return '模型名称重复：' + r.name
    seen.add(r.name)
    try {
      const url = new URL(r.baseURL)
      if (!['http:', 'https:'].includes(url.protocol)) return `模型 ${r.name} 的 API 地址须为 http(s)://`
    } catch { return `模型 ${r.name} 的 API 地址无效` }
    if (!r.model) return `模型 ${r.name} 缺少模型名`
  }
  if (!rows.some(r => r.name === activeName.value)) return '请选择一个要生效的模型'
  return ''
}
async function save() {
  if (busy.value) return
  const rows = collect()
  const err = validate(rows)
  if (err) { show(err, false); return }
  busy.value = true
  try {
    const r = await api('POST', '/api/provider/models', { active: activeName.value, models: rows })
    show(r.message || '已保存并生效', true)
    emit('saved')
    load()
  } catch (e) { show(e.message, false) }
  finally { busy.value = false }
}
async function test(x) {
  if (x.testing) return
  x.testMsg = null
  const baseURL = x.baseURL.trim(), model = x.model.trim()
  try { new URL(baseURL) } catch { x.testMsg = { ok: false, text: '请先填写有效的 API 地址' }; return }
  if (!model) { x.testMsg = { ok: false, text: '请先填写模型名' }; return }
  x.testing = true
  try {
    const r = await api('POST', '/api/provider/test', {
      baseURL, model, apiKey: x.apiKey.trim(), checkVision: x.image,
    })
    x.testMsg = {
      ok: !!r.ok,
      text: r.text?.message || '连接未通过，请检查配置后重试',
      vision: x.image ? (r.vision?.message || '') : '',
      visionBad: r.vision?.status === 'unsupported',
    }
  } catch (e) { x.testMsg = { ok: false, text: e.message } }
  finally { x.testing = false }
}

onMounted(load)
</script>

<template>
  <div class="provider-settings" :aria-busy="busy">
    <div class="pm-list">
      <div v-for="(x, i) in models" :key="i" class="pm-row" :class="{ open: x._open }">
        <!-- 摘要行：默认只看这一行，点开才展开编辑（zcode 式收拢） -->
        <div class="pm-head" role="button" tabindex="0" :aria-expanded="x._open"
             :aria-label="'模型 ' + (x.name || '未命名')" @click="x._open = !x._open" @keydown.enter="x._open = !x._open">
          <input v-model="activeName" type="radio" name="active-model" :value="x.name" class="pm-radio"
                 :title="activeName === x.name ? '保存后此模型生效' : '设为生效模型'"
                 :aria-label="'使用模型 ' + (x.name || '未命名')" @click.stop>
          <span class="pm-name" :class="{ faint: !x.name }">{{ x.name || '未命名' }}</span>
          <span class="pm-meta mono">{{ x.model || '模型名未填' }}<template v-if="hostOf(x)"> · {{ hostOf(x) }}</template></span>
          <span class="pm-badges">
            <span v-if="x.image" class="pm-badge">图片</span>
            <span v-if="x.keySet" class="pm-badge">密钥</span>
          </span>
          <span v-if="x.name && x.name === appliedName" class="pm-live"><span class="dot ok"></span>生效中</span>
          <span class="pm-acts">
            <button class="btn sm" type="button" :disabled="x.testing" @click.stop="test(x)">{{ x.testing ? '测试中…' : '测试' }}</button>
            <button class="btn sm danger" type="button" :disabled="busy" @click.stop="removeRow(i)">删除</button>
          </span>
          <svg class="pm-chev" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor"
               stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg>
        </div>
        <!-- 编辑区：展开才可见 -->
        <div v-show="x._open" class="pm-body">
          <label class="mr-field"><span>API 地址</span>
            <input v-model="x.baseURL" type="url" autocomplete="url" @input="x.testMsg = null" placeholder="https://api.example.com/v1">
          </label>
          <div class="pm-grid">
            <label class="mr-field"><span>显示名</span>
              <input v-model="x.name" @input="x.testMsg = null" placeholder="如 step-5-preview" aria-label="模型显示名">
            </label>
            <label class="mr-field"><span>模型名</span>
              <input v-model="x.model" autocomplete="off" @input="x.testMsg = null" placeholder="服务商提供的模型名称">
            </label>
          </div>
          <div class="pm-grid">
            <label class="mr-field"><span>API 密钥</span>
              <input v-model="x.apiKey" type="password" autocomplete="new-password" @input="x.testMsg = null"
                     :placeholder="x.keySet ? '已配置，留空保留' : '填写该服务的 API Key'">
            </label>
            <label class="mr-field"><span>上下文窗口</span>
              <input v-model="x.window" type="number" min="0" step="1" inputmode="numeric" @input="x.testMsg = null" placeholder="留空自动识别">
            </label>
          </div>
          <div class="pm-grid">
            <label class="mr-field"><span>最大输出 token</span>
              <input v-model="x.maxTokens" type="number" min="0" step="1" inputmode="numeric"
                     aria-label="最大输出 token" @input="x.testMsg = null"
                     :placeholder="`留空按 ${DEFAULT_MAX_TOKENS}`">
            </label>
            <label class="pm-check self-center" title="支持图片输入时聊天页才开放图片上传">
              <input v-model="x.image" type="checkbox" @change="x.testMsg = null"> 支持图片输入
            </label>
          </div>
          <div v-if="x.testMsg" class="pm-test meta" :class="{ err: !x.testMsg.ok }" role="status">
            {{ x.testMsg.text }}
            <span v-if="x.testMsg.vision" :class="{ err: x.testMsg.visionBad }">；{{ x.testMsg.vision }}</span>
          </div>
        </div>
      </div>
    </div>
    <div class="row pm-foot">
      <button class="btn" type="button" :disabled="busy" @click="addRow">＋ 添加模型</button>
      <button class="btn primary" type="button" :disabled="busy" @click="save">{{ busy ? '正在保存…' : '保存并生效' }}</button>
    </div>
    <div class="msg" :class="msg.cls">{{ msg.text }}</div>
    <p class="set-note">点选 radio 的模型在保存后立即生效；上下文窗口留空按模型名自动识别<template v-if="globalWindow">（当前设了全局覆盖 context.windowTokens={{ globalWindow }}，优先于此处）</template>。
      最大输出 token 留空按 {{ DEFAULT_MAX_TOKENS }} 提交：思考型模型的「思考」也计入这份预算，给小了会出现只有思考、没有正文（回复中断）。</p>
  </div>
</template>

<style scoped>
.provider-settings label { display: block; }
/* 行卡片与 .mcp-row 同规格但独立类名；默认收成一行摘要，展开才是表单（zcode 式收拢） */
.pm-list { display: flex; flex-direction: column; gap: 8px; }
.pm-row { border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-subtle);
  padding: 3px 10px; }
html[data-theme="dark"] .pm-row { background: rgba(255,255,255,.03); }
.pm-head { display: flex; align-items: center; gap: 10px; min-height: 40px; cursor: pointer;
  border-radius: var(--radius-sm); }
.pm-head:hover { background: var(--bg-muted); }
html[data-theme="dark"] .pm-head:hover { background: rgba(255,255,255,.05); }
.pm-radio { flex: none; width: 14px; height: 14px; accent-color: var(--primary); cursor: pointer; }
.pm-name { flex: none; font-size: 13.5px; font-weight: 600; white-space: nowrap; }
.pm-name.faint { color: var(--faint-fg); font-weight: 400; }
.pm-meta { flex: 1; min-width: 0; font-size: 12px; color: var(--faint-fg);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pm-badges { display: inline-flex; gap: 4px; flex: none; }
.pm-badge { font-size: 11px; color: var(--muted-fg); background: var(--bg-muted);
  border-radius: 999px; padding: 2px 8px; }
.pm-live { flex: none; display: inline-flex; align-items: center; gap: 4px;
  font-size: 12px; font-weight: 600; white-space: nowrap; }
.pm-acts { display: inline-flex; gap: 6px; flex: none; opacity: 0; transition: opacity var(--tr); }
.pm-row:hover .pm-acts, .pm-row:focus-within .pm-acts, .pm-row.open .pm-acts { opacity: 1; }
@media (hover: none) { .pm-acts { opacity: 1; } }
.pm-chev { flex: none; color: var(--faint-fg); transition: transform var(--tr); }
.pm-row.open .pm-chev { transform: rotate(180deg); }
.pm-body { padding: 6px 2px 10px; display: grid; gap: 10px; }
.pm-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
@media (max-width: 640px) { .pm-grid { grid-template-columns: 1fr; } }
.pm-check { display: flex !important; align-items: center; gap: 8px; font-size: 13px;
  color: var(--muted-fg); cursor: pointer; }
.pm-check.self-center { align-self: center; }
.pm-foot { margin-top: 14px; }
.pm-foot .btn.primary { margin-left: auto; }
.pm-test { padding: 0; }
.pm-test.err, .pm-test .err { color: var(--danger); }
</style>
