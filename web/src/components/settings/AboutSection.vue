<script setup>
// 关于与配置：运行时生效的配置快照 + 退出登录。
import { computed, onMounted, ref } from 'vue'
import { api } from '../../api.js'
import { store } from '../../store.js'

const emit = defineEmits(['logout'])
const status = ref(null)

const rows = computed(() => {
  const st = status.value
  return [
    ['当前账号', store.username || '—'],
    ['LLM 模型', st && st.provider.model],
    ['API 地址', st && st.provider.baseURL],
    ['任务文件', st && st.files.tasks],
    ['待办文件', st && st.files.todos],
    ['记忆目录', st && st.files.memories],
    ['服务地址', location.host],
  ]
})

onMounted(async () => {
  try { status.value = await api('GET', '/api/status') } catch (e) { /* 服务暂不可达时展示空位 */ }
})
</script>

<template>
  <table><tbody id="settingsRows">
    <tr v-for="([k, v]) in rows" :key="k">
      <td class="meta">{{ k }}</td>
      <td class="mono">{{ v == null ? '—' : v }}</td>
    </tr>
  </tbody></table>
  <div class="set-note">管理员账号通过启动配置设置：环境变量 FRIDAY_ADMIN_USERNAME / FRIDAY_ADMIN_PASSWORD
    或 config.yaml 的 admin.username / admin.password，修改后重启服务生效。</div>
  <div class="mt22">
    <button class="btn danger" @click="emit('logout')">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-out"/></svg>退出登录
    </button>
  </div>
</template>
