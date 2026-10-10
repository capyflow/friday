<script setup>
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { api } from '../api.js'

const props = defineProps({ status: { type: Object, default: null } })
const emit = defineEmits(['saved'])
const form = reactive({ baseURL: '', model: '', apiKey: '', checkVision: false })
const busy = ref('')
const error = ref('')
const savedMessage = ref('')
const result = ref(null)
let active = true
let generation = 0

function target(value) {
  try {
    const url = new URL(value.trim())
    if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) return ''
    return url.href.replace(/\/+$/, '')
  } catch { return '' }
}
const canKeepKey = computed(() => props.status?.apiKeyConfigured && target(form.baseURL) && target(form.baseURL) === target(props.status?.provider?.baseURL || ''))
const keyPlaceholder = computed(() => canKeepKey.value ? '已配置，留空保留当前密钥' : '填写该服务的 API Key')
const fingerprint = computed(() => JSON.stringify([form.baseURL.trim(), form.model.trim(), form.apiKey.trim(), form.checkVision]))

watch(() => props.status, status => {
  if (!status) return
  form.baseURL = status.provider?.baseURL || ''
  form.model = status.provider?.model || ''
}, { immediate: true })
watch(fingerprint, () => { generation++; result.value = null; savedMessage.value = ''; error.value = '' }, { flush: 'sync' })
onBeforeUnmount(() => { active = false; generation++ })

function validate() {
  if (!target(form.baseURL)) return '请输入有效的 HTTP 或 HTTPS API 地址'
  if (!form.model.trim()) return '请输入模型名'
  if (!form.apiKey.trim() && !canKeepKey.value) return '请填写该 API 地址的 API Key'
  return ''
}
function requestBody() {
  return { baseURL: form.baseURL.trim(), model: form.model.trim(), apiKey: form.apiKey.trim(), checkVision: form.checkVision }
}
async function test() {
  if (busy.value) return
  error.value = validate()
  result.value = null
  if (error.value) return
  const version = generation
  busy.value = 'test'
  try {
    const response = await api('POST', '/api/provider/test', requestBody())
    if (active && version === generation) result.value = response
  } catch (err) {
    if (active && version === generation) error.value = err.message
  } finally { if (active) busy.value = '' }
}
async function save() {
  if (busy.value) return
  error.value = validate()
  if (error.value) return
  const version = generation
  busy.value = 'save'
  try {
    const response = await api('POST', '/api/provider', requestBody())
    if (!active) return
    if (version === generation) {
      form.apiKey = ''
      result.value = null
      savedMessage.value = response.message || '模型配置已保存'
    }
    emit('saved')
  } catch (err) {
    if (active && version === generation) error.value = err.message
  } finally { if (active) busy.value = '' }
}
</script>

<template>
  <div class="provider-settings" :aria-busy="!!busy">
    <form class="form-col" @submit.prevent="save">
      <label>API 地址（baseURL）
        <input v-model="form.baseURL" name="baseURL" type="url" autocomplete="url" placeholder="https://api.example.com/v1" :disabled="!!busy" required>
      </label>
      <label>模型名（model）
        <input v-model="form.model" name="model" autocomplete="off" placeholder="服务商提供的模型名称" :disabled="!!busy" required>
      </label>
      <label>API 密钥
        <input v-model="form.apiKey" name="apiKey" type="password" autocomplete="new-password" :placeholder="keyPlaceholder" :disabled="!!busy" :required="!canKeepKey">
      </label>
      <label class="vision-option"><input v-model="form.checkVision" name="checkVision" type="checkbox" :disabled="!!busy"> 同时测试图片理解</label>
      <div class="row provider-actions">
        <button class="btn" type="button" :disabled="!!busy" @click="test">{{ busy === 'test' ? '正在测试…' : '测试连接' }}</button>
        <button class="btn primary" type="submit" :disabled="!!busy">{{ busy === 'save' ? '正在保存…' : '保存并生效' }}</button>
      </div>
    </form>
    <p v-if="error" class="msg err" role="alert">{{ error }}</p>
    <div v-if="result" class="provider-result" role="status" aria-live="polite">
      <p :class="result.ok ? 'ok' : 'err'">{{ result.text?.message || '连接未通过，请检查配置后重试' }}</p>
      <p class="meta" :class="{ err: result.vision?.status === 'unsupported' }">{{ result.vision?.message }}</p>
    </div>
    <p v-if="savedMessage" class="msg ok" role="status">{{ savedMessage }}</p>
    <p class="set-note">测试会向模型服务发送少量文本{{ form.checkVision ? '和一张测试图片' : '' }}，不会保存配置。保存后立即生效。更换 API 地址时请填写该服务的密钥。</p>
  </div>
</template>

<style scoped>
.provider-settings label { display: block; }
.provider-settings label > input:not([type="checkbox"]) { display: block; width: 100%; margin-top: 6px; min-height: 44px; box-sizing: border-box; }
.vision-option { display: flex !important; align-items: center; gap: 8px; min-height: 44px; cursor: pointer; }
.provider-actions { flex-wrap: wrap; gap: 10px; }
.provider-actions .btn { min-height: 44px; }
.provider-result { padding: 10px 0; }
.provider-result p { margin: 4px 0; }
</style>
