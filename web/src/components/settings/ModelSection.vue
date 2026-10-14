<script setup>
// 模型服务：baseURL / 模型名 / 密钥，保存后热生效并刷新全局设置状态。
import { onMounted, ref } from 'vue'
import { api } from '../../api.js'
import { toast } from '../../store.js'
import ProviderSettings from '../ProviderSettings.vue'

const status = ref(null)

async function load() {
  try { status.value = await api('GET', '/api/status') } catch (e) { /* 服务暂不可达时仍可填写 */ }
}
async function saved() {
  try { status.value = await api('GET', '/api/status') } catch (e) { toast(e.message, true) }
  window.dispatchEvent(new CustomEvent('friday:setup-refresh'))
}

onMounted(load)
</script>

<template>
  <ProviderSettings :status="status" @saved="saved" />
</template>
