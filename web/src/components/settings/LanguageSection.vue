<script setup>
// 回复语言：小五按 app 设置的语言回复用户。
//
// 为什么是「设置」而不是写死在提示词里：同一套规则要同时喂给两处——内置 agent
// 写进系统提示词，ACP 外部 agent 靠 prompt 前置块带过去。两边取同一个值，改一处
// 就够；默认值是中文。
import { onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'
import SelectBox from '../SelectBox.vue'

// 预设直接写语言名而不是语言代码：提示词里嵌的是这个名字，模型认名字。
const PRESETS = ['中文', 'English', '日本語', '한국어', 'Deutsch', 'Français', 'Español']
const CUSTOM = '__custom__'
const OPTIONS = PRESETS.map(p => ({ value: p, label: p })).concat([{ value: CUSTOM, label: '自定义…' }])

const lang = ref('中文')
const custom = ref('')
const msg = reactive({ text: '', cls: '' })
let timer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 4000)
}

async function load() {
  try {
    const r = await api('GET', '/api/language')
    const v = (r.language || '中文').trim()
    if (PRESETS.includes(v)) {
      lang.value = v
      custom.value = ''
    } else {
      // 存的是自定义语言名：回填输入框，别让它显示成第一个预设。
      lang.value = CUSTOM
      custom.value = v
    }
  } catch (e) { show(e.message, false) }
}

async function save() {
  const v = lang.value === CUSTOM ? custom.value.trim() : lang.value
  if (!v) { show('语言名不能为空', false); return }
  try {
    const r = await api('POST', '/api/language', { language: v })
    show(r.message || '已保存', true)
    await load()
  } catch (e) { show(e.message, false) }
}

onMounted(load)
</script>

<template>
  <div class="row">
    <SelectBox v-model="lang" :options="OPTIONS" aria-label="回复语言" style="min-width:150px"></SelectBox>
    <input v-if="lang === CUSTOM" v-model="custom" type="text" placeholder="语言名，如 粤语 / Русский"
           style="width:200px" aria-label="自定义语言名">
    <button class="btn primary" type="button" @click="save">保存并热生效</button>
  </div>
  <div class="msg" :class="msg.cls">{{ msg.text }}</div>
  <div class="meta">小五（内置 agent 与工作区里的外部 agent）都按这个语言回复；只改回复语言，界面语言不变。</div>
</template>
