<script setup>
// 模型服务：模型库（多模型 / 上下文窗口 / 输入格式），保存后热生效并广播全局刷新。
import ProviderSettings from '../ProviderSettings.vue'

function saved() {
  window.dispatchEvent(new CustomEvent('friday:setup-refresh'))
}
</script>

<template>
  <ProviderSettings @saved="saved" />
</template>
