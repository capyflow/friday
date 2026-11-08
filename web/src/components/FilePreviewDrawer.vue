<script setup>
// FilePreviewDrawer 是文件预览侧边抽屉：时间线里点文件路径 chip 后打开，
// 由后端 /api/files/preview 只读读取，按类型渲染——markdown 走同一套渲染管线
// （抽屉内命中的路径可继续点开）、代码/文本走 pre、图片内联、二进制给提示。
// Esc 或点遮罩关闭；open(path) 由父组件经 ref 调用。
import { onUnmounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { renderMD, localFileHref, joinFilePath } from '../md.js'

const openState = ref(false)
const loading = ref(false)
const error = ref('')
const file = reactive({
  path: '', name: '', size: 0, modTime: '', kind: '', mime: '',
  content: '', dataUrl: '', truncated: false, message: '',
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
  Object.assign(file, { path: '', name: '', size: 0, modTime: '', kind: '', mime: '', content: '', dataUrl: '', truncated: false, message: '' })
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
      <div v-else class="fp-status">{{ file.message || '暂不支持预览' }}</div>
    </aside>
  </div>
</template>
