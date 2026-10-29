<script setup>
// 权限模式：三档热切换，毁灭性命令黑名单在任何一档都仍拦截。
import { onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'
import SelectBox from '../SelectBox.vue'

const OPTIONS = [
  { value: 'confirm', label: '确认（默认）' },
  { value: 'acceptEdits', label: '自动接受编辑' },
  { value: 'fullAccess', label: '完全信任' },
]

const mode = ref('confirm')
const msg = reactive({ text: '', cls: '' })
let timer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 4000)
}

async function load() {
  try { mode.value = (await api('GET', '/api/permissions')).mode || 'confirm' }
  catch (e) { /* 服务暂不可达时展示默认档 */ }
}
async function save() {
  if (mode.value === 'fullAccess' &&
    !confirm('完全信任模式下，执行命令、写文件、改配置都不再询问（毁灭性命令仍拦截）。确定切换？')) return
  try {
    const r = await api('POST', '/api/permissions', { mode: mode.value })
    show(r.message || '已切换', true)
    // 通知聊天输入框的权限 chip 即时同步（ChatView 监听 friday:perm）
    window.dispatchEvent(new CustomEvent('friday:perm', { detail: mode.value }))
  } catch (e) { show(e.message, false) }
}

onMounted(load)
</script>

<template>
  <div class="row">
    <SelectBox v-model="mode" :options="OPTIONS" aria-label="权限模式" style="min-width:150px"></SelectBox>
    <button class="btn primary" type="button" @click="save">切换并热生效</button>
  </div>
  <div class="msg" :class="msg.cls">{{ msg.text }}</div>
</template>
