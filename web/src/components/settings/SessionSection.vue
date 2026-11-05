<script setup>
// 会话管理：生活模式的主会话与心跳/计划/目标这些后台入口会话都在这一张表里，
// 支持按条清除上下文（聊天页只列主会话那一条）。
import { onMounted, ref } from 'vue'
import { api } from '../../api.js'
import { fmtDT } from '../../format.js'
import { toast } from '../../store.js'
import EmptyState from '../art/EmptyState.vue'

const sessions = ref([])
const KIND_LABEL = { life: '生活 · 主会话', internal: '后台入口' }

async function load() {
  try { sessions.value = (await api('GET', '/api/sessions')).sessions || [] }
  catch (e) { toast('会话加载失败：' + e.message, true) }
}
async function clear(key) {
  if (!confirm('确定清除会话 ' + key + ' 的全部上下文？')) return
  try {
    await api('POST', '/api/context/clear', { session: key })
    toast('会话 ' + key + ' 已清除')
    load()
  } catch (e) { toast(e.message, true) }
}

onMounted(load)
</script>

<template>
  <table v-if="sessions.length">
    <thead><tr><th>会话</th><th>归属</th><th>消息数</th><th>最后活跃</th><th></th></tr></thead>
    <tbody>
      <tr v-for="x in sessions" :key="x.key">
        <td>{{ x.title || x.key }}<br><span class="mono meta">{{ x.key }}</span></td>
        <td class="meta">{{ KIND_LABEL[x.kind] || '未标记' }}</td>
        <td>{{ x.messages }}</td>
        <td class="meta">{{ fmtDT(x.updatedAt) }}</td>
        <td><button class="btn sm danger" @click="clear(x.key)">清除</button></td>
      </tr>
    </tbody>
  </table>
  <EmptyState v-else kind="session" :size="88">还没有任何会话</EmptyState>
</template>
