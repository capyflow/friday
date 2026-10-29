<script setup>
// 人设提示词：预设选择 + 正文编辑 + 应用热生效；自定义改动可恢复内置或删除。
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { api } from '../../api.js'
import SelectBox from '../SelectBox.vue'

const presets = ref([])
const options = computed(() => presets.value.map(p => ({ value: p.id, label: p.name })))
const active = ref('')
const select = ref('')
const msg = reactive({ text: '', cls: '' })
let timer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 4000)
}
const cur = computed(() => presets.value.find(p => p.id === select.value) || null)
// 正文编辑落在 draft 上：切换人设/重新加载时重置，用于判断"是否改动过"
const draft = ref('')
function fillDraft() { draft.value = (cur.value && cur.value.prompt) || '' }
watch(select, fillDraft)
const isCustomMark = computed(() => !!(cur.value && cur.value.custom))
const showRevert = computed(() => !!(cur.value && cur.value.builtin && cur.value.custom))
const showDelete = computed(() => !!(cur.value && !cur.value.builtin))

async function load(selectId) {
  try {
    const r = await api('GET', '/api/persona')
    presets.value = r.presets || []
    active.value = r.active || 'default'
    select.value = presets.value.some(p => p.id === (selectId || active.value))
      ? (selectId || active.value) : 'default'
    fillDraft()
    return true
  } catch (e) { show(e.message, false); return false }
}
// 收集当前全部自定义预设（发后端整体替换用）
function customs() {
  return presets.value.filter(x => x.custom)
    .map(x => ({ id: x.id, name: x.name, tagline: x.tagline || '', prompt: x.prompt || '' }))
}
async function apply() {
  const p = cur.value
  if (!p) return
  const promptText = draft.value.trim()
  if (!promptText) { show('提示词正文不能为空', false); return }
  const body = { active: p.id }
  if (promptText !== (p.prompt || '').trim()) {
    const list = customs()
    const hit = list.find(x => x.id === p.id)
    if (hit) hit.prompt = promptText
    else list.push({ id: p.id, name: p.name, tagline: p.tagline || '', prompt: promptText })
    body.presets = list
  }
  try {
    const r = await api('POST', '/api/persona', body)
    show(r.message || '已应用', true)
    await load(p.id)
  } catch (e) { show(e.message, false) }
}
async function add() {
  const name = (window.prompt('给新自定义人设起个名字（如：毒舌损友）') || '').trim()
  if (!name) return
  const list = customs()
  list.push({ id: 'custom-' + Date.now(), name, tagline: '自定义人设', prompt: '' })
  try {
    await api('POST', '/api/persona', { active: active.value, presets: list })
    show('已创建「' + name + '」，写好提示词后点"应用并热生效"', true)
    await load(list[list.length - 1].id)
  } catch (e) { show(e.message, false) }
}
async function revert() {
  const p = cur.value
  if (!p || !p.builtin || !p.custom) return
  if (!confirm('恢复「' + p.name + '」为内置提示词？当前自定义文案将被丢弃。')) return
  try {
    const r = await api('POST', '/api/persona', { active: active.value, presets: customs().filter(x => x.id !== p.id) })
    show(r.message || '已恢复内置', true)
    await load(p.id)
  } catch (e) { show(e.message, false) }
}
async function remove() {
  const p = cur.value
  if (!p || p.builtin) return
  if (!confirm('删除自定义人设「' + p.name + '」？')) return
  const next = active.value === p.id ? 'default' : active.value
  try {
    const r = await api('POST', '/api/persona', { active: next, presets: customs().filter(x => x.id !== p.id) })
    show(r.message || '已删除', true)
    await load(next)
  } catch (e) { show(e.message, false) }
}

onMounted(() => load())
</script>

<template>
  <div class="row">
    <SelectBox v-model="select" :options="options" aria-label="选择人设" style="min-width:132px"></SelectBox>
    <span class="meta">{{ cur ? (cur.tagline || '') : '' }}</span>
    <span v-if="isCustomMark" class="meta">已自定义</span>
  </div>
  <textarea id="personaPrompt" v-model="draft" rows="6" class="ta-plain"
    style="margin-top:10px" placeholder="人设正文：描述这个性格下小五怎么说话、怎么关心人"></textarea>
  <div class="row" style="margin-top:8px">
    <button class="btn primary" type="button" @click="apply">应用并热生效</button>
    <button class="btn" type="button" @click="add">＋ 新增自定义</button>
    <button v-if="showRevert" class="btn" type="button" @click="revert">恢复内置</button>
    <button v-if="showDelete" class="btn danger" type="button" @click="remove">删除自定义</button>
  </div>
  <div class="msg" :class="msg.cls">{{ msg.text }}</div>
</template>
