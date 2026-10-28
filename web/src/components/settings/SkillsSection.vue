<script setup>
// 技能：直接编辑 SKILL.md（框架格式 + Friday 的启用/触发键），保存即生效。
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'
import EmptyState from '../art/EmptyState.vue'

const TEMPLATE = `---
name: 技能名
description: 一句话说明这个技能干什么（自动匹配按它和 keywords 打分）
# model: deepseek-chat          # 可选：本轮换一个模型
# temperature: 0.4              # 可选：本轮采样温度
allowed-tools: [list_todos, get_weather]   # 可选：本轮只允许这些工具（只能收窄，不能提权）
enabled: true
auto: false                    # true 才参与自动匹配（也可用 triggers 精确触发）
triggers: ["技能名"]
keywords: ["相关词"]
priority: 10
---
写给小五的具体做法（这一段会作为技能指令注入本轮对话）。
`

const skills = ref([])
const dir = ref('')
const storeErr = ref('')
const msg = reactive({ text: '', cls: '' })
const editing = ref(null)   // { id, raw, isNew }
const saving = ref(false)
let timer = null

function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 5000)
}

const broken = computed(() => skills.value.filter(s => s.parseError))
const shadowed = computed(() => skills.value.filter(s => !s.parseError && s.note))

async function load() {
  try {
    const r = await api('GET', '/api/skills')
    skills.value = r.skills || []
    dir.value = r.dir || ''
    storeErr.value = r.error || ''
  } catch (e) { show('技能列表加载失败：' + e.message, false) }
}

async function openEditor(s) {
  try {
    const r = await api('GET', '/api/skills/' + encodeURIComponent(s.id))
    editing.value = { id: s.id, raw: r.raw, isNew: false, name: s.name }
  } catch (e) { show(e.message, false) }
}
function newSkill() { editing.value = { id: '', raw: TEMPLATE, isNew: true } }
function cancelEdit() { editing.value = null }

async function save() {
  if (!editing.value) return
  saving.value = true
  try {
    const r = editing.value.isNew
      ? await api('POST', '/api/skills', { raw: editing.value.raw })
      : await api('PUT', '/api/skills/' + encodeURIComponent(editing.value.id), { raw: editing.value.raw })
    show(r.message || '已保存并生效', true)
    editing.value = null
    await load()
  } catch (e) { show(e.message, false) } finally { saving.value = false }
}

async function toggle(s) {
  try {
    const r = await api('POST', '/api/skills/' + encodeURIComponent(s.id) + '/enabled', { enabled: !s.enabled })
    show(r.message || '已切换', true)
    await load()
  } catch (e) { show(e.message, false) }
}

async function remove(s) {
  if (!confirm('删除技能「' + (s.name || s.id) + '」？SKILL.md 文件会被移除。')) return
  try {
    await api('DELETE', '/api/skills/' + encodeURIComponent(s.id))
    show('已删除', true)
    await load()
  } catch (e) { show(e.message, false) }
}

async function reload() {
  try {
    const r = await api('POST', '/api/skills/reload', {})
    skills.value = r.skills || []
    show(r.message || '已重新扫描目录', true)
  } catch (e) { show(e.message, false) }
}

onMounted(load)
</script>

<template>
  <div class="row" style="margin-bottom:10px">
    <button class="btn primary" type="button" @click="newSkill">＋ 新建技能</button>
    <button class="btn" type="button" @click="reload">重新扫描目录</button>
    <span class="grow"></span>
    <span class="meta" v-if="dir">目录 {{ dir }}</span>
  </div>
  <div v-if="storeErr" class="msg err">{{ storeErr }}</div>
  <div class="msg skill-msg" :class="msg.cls">{{ msg.text }}</div>

  <div v-if="editing" class="skill-edit">
    <div class="se-head">
      <b>{{ editing.isNew ? '新建技能' : '编辑 ' + (editing.name || editing.id) }}</b>
      <span class="grow"></span>
      <button class="btn sm" type="button" @click="cancelEdit">取消</button>
      <button class="btn sm primary" type="button" :disabled="saving" @click="save">
        {{ saving ? '保存中…' : '保存并生效' }}
      </button>
    </div>
    <textarea v-model="editing.raw" class="se-raw" rows="14" spellcheck="false"
              aria-label="SKILL.md 内容"></textarea>
  </div>

  <div v-if="skills.length" class="skill-list">
    <div v-for="s in skills" :key="s.id" class="skill-row" :class="{ broken: !!s.parseError }">
      <div class="sr-head">
        <b>{{ s.name || s.id }}</b>
        <span class="meta">{{ s.scope === 'user' ? '本机目录' : '项目' }} · {{ s.id }}</span>
        <span v-if="s.auto" class="chip">自动匹配</span>
        <span v-if="s.model" class="chip">{{ s.model }}</span>
        <span class="grow"></span>
        <template v-if="!s.readOnly">
          <button class="btn sm" type="button" @click="toggle(s)">{{ s.enabled ? '停用' : '启用' }}</button>
          <button class="btn sm" type="button" @click="openEditor(s)">编辑</button>
          <button class="btn sm danger" type="button" @click="remove(s)">删除</button>
        </template>
        <button v-else class="btn sm" type="button" @click="openEditor(s)">查看</button>
      </div>
      <div class="meta" v-if="s.description">{{ s.description }}</div>
      <div class="meta" v-if="s.triggers && s.triggers.length">触发词：{{ s.triggers.join('、') }}</div>
      <div v-if="s.parseError" class="sr-error">{{ s.parseError }}</div>
      <div v-else-if="s.note" class="sr-note">{{ s.note }}</div>
    </div>
  </div>
  <EmptyState v-else kind="skill" :size="92">还没有技能——点"新建技能"写一份 SKILL.md，或小五的对话里用 /skill 技能名 直接点名</EmptyState>

  <div v-if="broken.length || shadowed.length" class="msg err">
    <template v-if="broken.length">{{ broken.length }} 个技能文件无效，已按原因标出（原文件保留）。</template>
    <template v-if="shadowed.length">{{ shadowed.length }} 个技能因重名未生效。</template>
  </div>
</template>
