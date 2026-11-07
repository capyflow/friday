<script setup>
// 移动端文件面板：单列目录浏览（点目录进、面包屑回），点文件拉预览
// （text/image/binary 三类，同桌面 /api/files/preview），binary 与超限文件给
// 下载按钮（/api/files/download，token 走查询参数——<a> 带不了请求头）。
import { ref, watch } from 'vue'
import { api } from '../api.js'
import { getToken } from '../auth.js'
import { renderMD } from '../md.js'
import { toast } from '../store.js'

const props = defineProps({
  thread: { type: Object, required: true }, // 至少有 id 与 cwd（主目录）
})

const entries = ref([])
const cur = ref('') // 当前相对路径（相对会话主目录，slash 分隔）
const root = ref('')
const loading = ref(false)
const truncated = ref(false)
const preview = ref(null) // { name, abs, loading, error, kind, mime, content, dataUrl, truncated, message, size, modTime }

watch(() => props.thread && props.thread.id, () => {
  root.value = props.thread.cwd || ''
  cur.value = ''
  preview.value = null
  loadDir('')
}, { immediate: true })

async function loadDir(rel) {
  loading.value = true
  try {
    const r = await api('GET', `/api/acp/threads/${props.thread.id}/files?path=${encodeURIComponent(rel)}`)
    entries.value = r.entries || []
    truncated.value = !!r.truncated
  } catch (e) {
    toast('读目录失败：' + e.message, true)
    entries.value = []
  } finally {
    loading.value = false
  }
}

// 目录树条目的 path 是相对主目录的 slash 路径；预览/下载要绝对路径。
const abs = rel => joinPath(root.value, rel)
function joinPath(a, b) {
  if (!a) return b
  return a.replace(/\/+$/, '') + '/' + String(b || '').replace(/^\/+/, '')
}

function enter(e) {
  cur.value = e.path
  loadDir(e.path)
}

function goCrumb(seg) {
  const segs = cur.value ? cur.value.split('/') : []
  cur.value = seg ? segs.slice(0, segs.indexOf(seg) + 1).join('/') : ''
  loadDir(cur.value)
}

const crumbs = () => (cur.value ? cur.value.split('/').filter(Boolean) : [])

async function openFile(e) {
  const a = abs(e.path)
  preview.value = { name: e.name, abs: a, loading: true, error: '', kind: '', mime: '',
    content: '', dataUrl: '', truncated: false, message: '', size: e.size || 0, modTime: e.modTime || '' }
  try {
    const r = await api('GET', '/api/files/preview?path=' + encodeURIComponent(a))
    preview.value = Object.assign(preview.value, r, { abs: a, name: e.name, loading: false })
  } catch (err) {
    preview.value.loading = false
    preview.value.error = err.message || String(err)
  }
}

const closePreview = () => { preview.value = null }

const downloadURL = () =>
  '/api/files/download?path=' + encodeURIComponent(preview.value.abs) + '&token=' + encodeURIComponent(getToken())

const fmtSize = n => {
  if (n == null) return ''
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
  return (n / 1024 / 1024).toFixed(1) + ' MB'
}
const isMD = name => /\.(md|markdown)$/i.test(name || '')
</script>

<template>
  <div class="m-pane">
    <div class="m-crumb">
      <button class="m-crumb-seg" type="button" @click="goCrumb('')">主目录</button>
      <template v-for="seg in crumbs()" :key="seg">
        <span class="m-crumb-sep">/</span>
        <button class="m-crumb-seg" type="button" @click="goCrumb(seg)">{{ seg }}</button>
      </template>
    </div>

    <div class="m-list">
      <div v-if="loading" class="m-empty">加载中…</div>
      <div v-else-if="!entries.length" class="m-empty">目录为空</div>
      <button v-for="e in entries" :key="e.path" type="button" class="m-row" @click="e.dir ? enter(e) : openFile(e)">
        <span class="m-row-main">
          <span class="m-row-title">{{ e.name }}</span>
          <span v-if="!e.dir" class="m-row-sub">{{ fmtSize(e.size) }}</span>
        </span>
        <svg v-if="e.dir" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>
        <svg v-else width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3v5h5"/><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h9l5 5v11a2 2 0 0 1-2 2z"/></svg>
      </button>
      <div v-if="truncated" class="m-empty">条目过多，只显示了一部分</div>
    </div>

    <div v-if="preview" class="m-sheet-mask" @click.self="closePreview">
      <div class="m-sheet m-preview" role="dialog" :aria-label="'预览 ' + preview.name">
        <div class="m-sheet-title">{{ preview.name }} <span v-if="preview.size" class="m-dim">{{ fmtSize(preview.size) }}</span></div>
        <div v-if="preview.loading" class="m-empty">读取中…</div>
        <div v-else-if="preview.error" class="m-empty">读取失败：{{ preview.error }}</div>
        <template v-else>
          <div v-if="preview.kind === 'text' && isMD(preview.name)" class="chat-md m-preview-body" v-html="renderMD(preview.content)"></div>
          <pre v-else-if="preview.kind === 'text'" class="m-preview-body m-pre">{{ preview.content }}<span v-if="preview.truncated" class="m-dim">（已截断，可下载看全文）</span></pre>
          <img v-else-if="preview.kind === 'image'" class="m-preview-img" :src="preview.dataUrl" :alt="preview.name">
          <div v-else class="m-empty">{{ preview.message || '二进制文件，暂不支持预览' }}</div>
        </template>
        <div class="m-sheet-acts">
          <button class="m-btn" type="button" @click="closePreview">关闭</button>
          <a class="m-btn primary" :href="downloadURL()" download>下载到手机</a>
        </div>
      </div>
    </div>
  </div>
</template>
