<script setup>
// 记忆 Wiki：目录搜索、页面阅读（Markdown 渲染）、新建/编辑/删除。
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { renderMD } from '../md.js'
import { fmtDT, srcName } from '../format.js'
import { toast } from '../store.js'
import EmptyState from './art/EmptyState.vue'

const query = ref('')
const pages = ref([])
const countLine = ref('')
const mode = ref('hint')           // hint | read | edit
const current = reactive({ id: null, title: '', meta: '', bodyHTML: '' })
const editingId = ref('')
const form = reactive({ title: '', body: '' })
const msg = reactive({ text: '', cls: '' })
const titleEl = ref(null)
let msgTimer = null

function showMsg(t, ok) {
  msg.text = t
  msg.cls = ok ? 'ok' : 'err'
  clearTimeout(msgTimer)
  msgTimer = setTimeout(() => { msg.text = '' }, 4000)
}

async function loadWiki() {
  try {
    const q = query.value.trim()
    const r = await api('GET', '/api/wiki/pages' + (q ? '?q=' + encodeURIComponent(q) : ''))
    pages.value = r.pages || []
    countLine.value = pages.value.length ? pages.value.length + ' 页' : ''
  } catch (e) { toast('记忆加载失败：' + e.message, true) }
}

async function open(id) {
  try {
    const p = (await api('GET', '/api/wiki/pages/' + encodeURIComponent(id))).page
    current.id = p.id
    current.title = p.title
    current.meta = (srcName[p.source] || p.source || '')
      + (p.source ? ' · ' : '') + '更新于 ' + fmtDT(p.updatedAt)
    current.bodyHTML = renderMD(p.body)
    mode.value = 'read'
  } catch (e) { toast('页面加载失败：' + e.message, true) }
}

async function startEdit(p) {
  // p 为 null 表示新建；否则先拉原始 markdown 再进编辑器
  if (!p) {
    editingId.value = ''
    form.title = ''
    form.body = ''
    mode.value = 'edit'
    return
  }
  try {
    if (!current.id || p.id !== current.id) await open(p.id)   // 确保正文已加载
    const page = (await api('GET', '/api/wiki/pages/' + encodeURIComponent(p.id))).page
    editingId.value = page.id
    form.title = page.title
    form.body = page.body
    mode.value = 'edit'
    titleEl.value && titleEl.value.focus()
  } catch (e) { toast(e.message, true) }
}

function newPage() {
  editingId.value = ''
  form.title = ''
  form.body = ''
  mode.value = 'edit'
  titleEl.value && titleEl.value.focus()
}

async function cancelEdit() {
  mode.value = editingId.value ? 'read' : 'hint'
}

async function save() {
  const body = { title: form.title.trim(), body: form.body }
  try {
    const r = editingId.value
      ? await api('PUT', '/api/wiki/pages/' + encodeURIComponent(editingId.value), body)
      : await api('POST', '/api/wiki/pages', body)
    toast(r.message || '已保存')
    mode.value = 'read'
    await loadWiki()
    await open(r.page.id)
  } catch (e) { showMsg(e.message, false) }
}

async function remove() {
  if (!current.id) return
  if (!confirm('确定删除这页记忆？整页内容将不可恢复。')) return
  try {
    await api('DELETE', '/api/wiki/pages/' + encodeURIComponent(current.id))
    toast('已删除')
    mode.value = 'hint'
    current.id = null
    loadWiki()
  } catch (e) { toast(e.message, true) }
}

onMounted(loadWiki)
</script>

<template>
  <section class="view" id="view-wiki">
    <div class="view-inner">
      <h2>记忆 <span class="count">{{ countLine }}</span></h2>
      <div class="wiki-layout">
        <aside class="panel wiki-toc">
          <input id="wQuery" v-model="query" placeholder="搜索记忆…" aria-label="搜索记忆页" @input="loadWiki">
          <button id="btnWikiNew" class="btn primary" type="button" @click="newPage">＋ 新建页面</button>
          <div>
            <button v-for="p in pages" :key="p.id" class="toc-item" type="button"
                    :class="{ active: p.id === current.id }" @click="open(p.id)">
              {{ p.title }}
              <span class="meta">{{ p.snippet ? p.snippet : (srcName[p.source] || p.source || '') + ' · ' + fmtDT(p.updatedAt) + ' 更新' }}</span>
            </button>
          </div>
          <EmptyState v-if="!pages.length" kind="wiki" :size="92">还没有记忆页<br>对小五说"记住…"即可创建</EmptyState>
        </aside>
        <div class="wiki-main">
          <EmptyState v-if="mode === 'hint'" kind="wikiPick" :size="96">从左侧选择一页查看</EmptyState>

          <div v-if="mode === 'read'" class="panel wiki-reader">
            <div class="wiki-head">
              <h3>{{ current.title }}</h3>
              <div class="wiki-acts">
                <button class="btn sm" type="button" @click="startEdit(current)">编辑</button>
                <button class="btn sm danger" type="button" @click="remove">删除</button>
              </div>
            </div>
            <div class="meta" id="wikiMeta">{{ current.meta }}</div>
            <div class="wiki-body" id="wikiBody" v-html="current.bodyHTML"></div>
          </div>

          <div v-if="mode === 'edit'" class="panel">
            <div class="msg" :class="msg.cls">{{ msg.text }}</div>
            <form class="form-col" @submit.prevent="save">
              <input ref="titleEl" v-model="form.title" placeholder="页面标题（主题名，如：用户偏好）" required>
              <textarea id="wBody" v-model="form.body" rows="14" placeholder="markdown 正文…" required></textarea>
              <div class="row">
                <button class="btn primary" type="submit">保存</button>
                <button class="btn" type="button" @click="cancelEdit">取消</button>
              </div>
            </form>
            <div class="set-note">保存会整体替换页面正文；小五把页面内容当作该主题的长期记忆。</div>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>
