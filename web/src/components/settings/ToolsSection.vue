<script setup>
// 联网搜索配置（设置 → 扩展 → 工具）：每个工具一张插件式卡片（图标 + 名称/
// 描述 + 当前状态徽章），点「配置」展开表单。保存即生效（工具每次调用读运行
// 时快照，无需重启），测试按钮真搜一次验配置。后续加工具 = 加一张卡片。
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'
import SelectBox from '../SelectBox.vue'

const PROVIDERS = [
  { value: 'bing', label: '必应（免费，无需 Key）' },
  { value: 'tavily', label: 'Tavily（免费 1000 次/月）' },
  { value: 'brave', label: 'Brave（免费 2000 次/月）' },
  { value: 'bocha', label: '博查（按次付费，中文优）' },
]
const providerLabel = v => (PROVIDERS.find(p => p.value === v) || PROVIDERS[0]).label

const provider = ref('bing')
const apiKey = ref('')
const msg = reactive({ text: '', cls: '' })
const testing = ref(false)
const open = ref(false)
let timer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 6000)
}

const needsKey = computed(() => provider.value !== 'bing')
const currentBadge = computed(() =>
  needsKey.value ? `已接入：${providerLabel(provider.value)}` : '免费·必应')

async function load() {
  try {
    const r = await api('GET', '/api/tools/search')
    provider.value = r.provider || 'bing'
    apiKey.value = r.apiKey || ''
  } catch { /* 服务暂不可达时展示默认档 */ }
}
async function save() {
  try {
    const r = await api('POST', '/api/tools/search', { provider: provider.value, apiKey: apiKey.value })
    show(r.message || '已保存并生效', true)
  } catch (e) { show(e.message, false) }
}
async function test() {
  testing.value = true
  try {
    const r = await api('POST', '/api/tools/search/test', { query: '今日要闻' })
    show((r.message || '').split('\n').slice(0, 6).join('\n'), true)
  } catch (e) { show('测试失败：' + e.message, false) }
  finally { testing.value = false }
}

// 切到免密钥档（必应）时清掉上家留下的 Key，别让它跟着提交到后端
function pickProvider(v) {
  provider.value = v
  if (v === 'bing') apiKey.value = ''
}

onMounted(load)
</script>

<template>
  <div class="svc-list">
    <!-- 联网搜索 -->
    <div class="svc-card">
      <div class="svc-head">
        <span class="svc-icon" aria-hidden="true">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor"
               stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
            <circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/>
            <path d="M8 11h6M11 8v6"/>
          </svg>
        </span>
        <div class="svc-info">
          <b>联网搜索
            <span class="svc-badge" :class="{ ok: needsKey }">{{ currentBadge }}</span>
          </b>
          <div class="svc-desc">为小五提供实时网络信息（新闻、时效性事实、文档）。默认必应免费可用；
            接入 Tavily / Brave / 博查可获得更高的结果质量。</div>
        </div>
        <span class="svc-head-acts">
          <button class="btn sm" type="button" :aria-expanded="open ? 'true' : 'false'"
                  @click="open = !open">{{ open ? '收起' : '配置' }}</button>
        </span>
      </div>
      <div v-show="open" class="svc-body">
        <div class="tool-form">
          <label>
            <span class="meta">搜索供应商</span>
            <SelectBox :model-value="provider" :options="PROVIDERS" aria-label="搜索供应商"
                       style="min-width:220px" @update:model-value="pickProvider" />
          </label>
          <label class="grow">
            <span class="meta">API Key</span>
            <input v-model="apiKey" type="password" autocomplete="off" :disabled="!needsKey"
                   :placeholder="needsKey ? '填入该供应商的 API Key' : '免密钥，无需填写'">
          </label>
        </div>
        <div class="svc-acts">
          <button class="btn primary sm" type="button" @click="save">保存并生效</button>
          <button class="btn sm" type="button" :disabled="testing" @click="test">
            {{ testing ? '搜索中…' : '测试（真搜一次）' }}</button>
        </div>
        <div class="msg" :class="msg.cls" style="white-space:pre-line">{{ msg.text }}</div>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* 卡片外壳（.svc-*）在 style.css 里与 MCP 服务器共用；这里只留本节的字段排布。 */
.tool-form { display: flex; gap: 10px; flex-wrap: wrap; align-items: flex-end; }
.tool-form label { display: flex; flex-direction: column; gap: 4px; font-size: 12px; }
.tool-form label.grow { flex: 1; min-width: 220px; }
.tool-form input {
  font: inherit; font-size: 13px; padding: 7px 9px; border: 1px solid var(--border-strong);
  border-radius: 8px; background: var(--bg); color: var(--fg); }
/* 供应商下拉的外观在 style.css 的 .tool-form .selbox-trigger 里（scoped 够不到子组件内部） */
</style>
