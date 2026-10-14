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
const DESC = {
  confirm: '写文件、改 MCP 配置和白名单外的命令都会在网页弹卡片，批准后执行',
  acceptEdits: '写文件、改 MCP 配置免审批直行；白名单外的命令仍需网页批准',
  fullAccess: '全部免审批直行，仅毁灭性命令黑名单（rm -rf、shutdown 等）仍强制拦截',
}

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
  } catch (e) { show(e.message, false) }
}

onMounted(load)
</script>

<template>
  <div class="row">
    <SelectBox v-model="mode" :options="OPTIONS" aria-label="权限模式" style="min-width:150px"></SelectBox>
    <button class="btn primary" type="button" @click="save">切换并热生效</button>
  </div>
  <div class="field-desc">{{ DESC[mode] }}</div>
  <div class="msg" :class="msg.cls">{{ msg.text }}</div>
  <div class="set-note">参照 zcode 的多档权限设计：<b>确认</b>（默认）危险操作逐次网页批准，可"批准并记住"；
    <b>自动接受编辑</b>日常改文件/改配置不打扰，跑命令仍把关；<b>完全信任</b>全部直行（黑名单仍拦截），
    适合本机私人使用。切换立即热生效并写回 config.yaml。</div>
</template>
