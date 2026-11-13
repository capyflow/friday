<script setup>
// FilePreviewDrawer 是文件预览侧边抽屉：时间线里点文件路径 chip 后打开，
// 由后端 /api/files/preview 只读读取，按类型渲染——markdown 走同一套渲染管线
// （抽屉内命中的路径可继续点开）、代码/文本走 pre、图片内联、二进制给提示。
// Esc 或点遮罩关闭；open(path) 由父组件经 ref 调用。
import { onUnmounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { getToken } from '../auth.js'
import { toast } from '../store.js'
import { renderMD, localFileHref, joinFilePath } from '../md.js'

const openState = ref(false)
const loading = ref(false)
const error = ref('')
const file = reactive({
  path: '', name: '', size: 0, modTime: '', kind: '', mime: '',
  content: '', dataUrl: '', truncated: false, message: '', canOpen: false,
})

const isMD = () => /\.(md|markdown)$/i.test(file.name)

function fmtSize(n) {
  if (!Number.isFinite(n)) return ''
  if (n < 1024) return n + ' B'
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KB'
  return (n / 1048576).toFixed(1) + ' MB'
}

function onKey(e) { if (e.key === 'Escape') close() }

async function open(path) {
  openState.value = true
  loading.value = true
  error.value = ''
  Object.assign(file, { path: '', name: '', size: 0, modTime: '', kind: '', mime: '', content: '', dataUrl: '', truncated: false, message: '', canOpen: false })
  document.addEventListener('keydown', onKey)
  try {
    Object.assign(file, await api('GET', '/api/files/preview?path=' + encodeURIComponent(path)))
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function close() {
  openState.value = false
  document.removeEventListener('keydown', onKey)
}

// 下载到本地：/api/files/download 的鉴权走查询参数（浏览器 <a download> 带不了
// 自定义头），所以这里把 token 拼进 URL。
function downloadURL() {
  return '/api/files/download?path=' + encodeURIComponent(file.path) +
    '&token=' + encodeURIComponent(getToken() || '')
}

// 用系统默认应用打开：预览不了的类型（zip、pdf、二进制）交给系统。
// 只有本机访问才行——远程访问时 open 打开的是服务器上的文件，对远端没意义，
// 那边给「下载到本地」就够（canOpen 由后端按回环判定给出）。
async function openExternally() {
  try {
    const r = await api('POST', '/api/files/open', { path: file.path })
    toast(r.message || '已用系统默认应用打开')
  } catch (e) {
    toast(e.message || '用系统默认应用打开失败', true)
  }
}

// 抽屉里预览 markdown 时命中的文件路径 chip 同样可点开；预览正文里的文件链接
// （[文案](./b.md)）按当前文件所在目录解析相对路径，抽屉内继续点开。
function onBodyClick(e) {
  const chip = e.target.closest?.('.file-chip')
  if (chip?.dataset.path) { open(chip.dataset.path); return }
  const a = e.target.closest?.('a[href]')
  if (!a) return
  const href = localFileHref(a.getAttribute('href'))
  if (href === null) return
  const dir = file.path ? file.path.slice(0, file.path.lastIndexOf('/')) : ''
  const abs = href.startsWith('/') ? href : joinFilePath(dir, href)
  if (!abs) return
  e.preventDefault()
  open(abs)
}

onUnmounted(() => document.removeEventListener('keydown', onKey))
defineExpose({ open, close })
</script>

<template>
  <div v-if="openState">
    <div class="fp-mask" @click="close"></div>
    <aside class="fp-panel" role="dialog" aria-modal="true" aria-label="文件预览">
      <header class="fp-head">
        <div class="fp-title">
          <span class="fp-name">{{ file.name || '文件预览' }}</span>
          <span class="fp-meta">{{ fmtSize(file.size) }}<template v-if="file.modTime"> · {{ new Date(file.modTime).toLocaleString() }}</template></span>
        </div>
        <button class="btn icon" type="button" aria-label="关闭预览" title="关闭" @click="close">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
        </button>
      </header>
      <div class="fp-path" :title="file.path">{{ file.path }}</div>
      <div v-if="loading" class="fp-status">读取中…</div>
      <div v-else-if="error" class="fp-status is-error">{{ error }}</div>
      <div v-else-if="file.kind === 'image'" class="fp-body">
        <img class="fp-img" :src="file.dataUrl" :alt="file.name">
      </div>
      <div v-else-if="file.kind === 'text'" class="fp-body">
        <div v-if="file.truncated" class="fp-notice">文件较大，仅显示前 512 KB</div>
        <div v-if="isMD()" class="fp-md chat-md" @click="onBodyClick" v-html="renderMD(file.content, { fileChips: true })"></div>
        <pre v-else class="fp-code">{{ file.content }}</pre>
      </div>
      <!-- 预览不了的类型：给两条出路——本机用系统默认应用打开，远程就下载到本地 -->
      <div v-else class="fp-status fp-unsupported">
        <p class="fp-unsupported-txt">{{ file.message || '暂不支持预览' }}</p>
        <div class="fp-acts">
          <button v-if="file.canOpen" class="btn sm primary" type="button" @click="openExternally">
            用默认应用打开</button>
          <a class="btn sm" :href="downloadURL()" :download="file.name">下载到本地</a>
        </div>
        <p class="meta fp-unsupported-hint">
          {{ file.canOpen ? '也可以下载到本地再打开' : '远程访问：下载到本地后用系统默认应用打开' }}</p>
      </div>
    </aside>
  </div>
</template>
