<script setup>
// 会话管理：各入口独立会话，支持单条清除上下文。
import { onMounted, ref } from 'vue'
import { api } from '../../api.js'
import { fmtDT } from '../../format.js'
import { toast } from '../../store.js'
import EmptyState from '../art/EmptyState.vue'

const sessions = ref([])

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
    <thead><tr><th>会话</th><th>消息数</th><th>最后活跃</th><th></th></tr></thead>
    <tbody>
      <tr v-for="x in sessions" :key="x.key">
        <td class="mono">{{ x.key }}</td>
        <td>{{ x.messages }}</td>
        <td class="meta">{{ fmtDT(x.updatedAt) }}</td>
        <td><button class="btn sm danger" @click="clear(x.key)">清除</button></td>
      </tr>
    </tbody>
  </table>
  <EmptyState v-else kind="session" :size="88">还没有任何会话</EmptyState>
  <div class="set-note">每个入口（网页/终端/钉钉/飞书聊天）拥有独立会话，历史落盘重启不丢；
    清除只影响对应会话。</div>
</template>
