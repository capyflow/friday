<script setup>
// 待办清单：增/勾选完成/删除，与 agent 工具共用一份存储。
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api.js'
import { fmtDT } from '../format.js'
import { toast } from '../store.js'

const text = ref('')
const todos = ref([])
const msg = reactive({ text: '', cls: '' })
let msgTimer = null

function showMsg(t, ok) {
  msg.text = t
  msg.cls = ok ? 'ok' : 'err'
  clearTimeout(msgTimer)
  msgTimer = setTimeout(() => { msg.text = '' }, 4000)
}

const countLine = ref('')
const pending = computed(() => todos.value.filter(t => !t.done).length)

async function loadTodos() {
  try {
    todos.value = (await api('GET', '/api/todos')).todos || []
    countLine.value = todos.value.length
      ? pending.value + ' 项待完成 / 共 ' + todos.value.length + ' 条' : ''
  } catch (e) { toast('待办加载失败：' + e.message, true) }
}

async function submit() {
  try {
    await api('POST', '/api/todos', { text: text.value.trim() })
    text.value = ''
    loadTodos()
  } catch (e) { showMsg(e.message, false) }
}

async function toggle(t, done) {
  try { await api('POST', '/api/todos/' + t.id, { done }); loadTodos() }
  catch (e) { showMsg(e.message, false) }
}

async function remove(t) {
  if (!confirm('确定删除待办 ' + t.id + '？')) return
  try { await api('DELETE', '/api/todos/' + t.id); toast('已删除'); loadTodos() }
  catch (e) { showMsg(e.message, false) }
}

onMounted(loadTodos)
</script>

<template>
  <section class="view" id="view-todo">
    <div class="view-inner">
      <h2>待办 <span class="count">{{ countLine }}</span></h2>
      <div class="panel">
        <div class="panel-title">新增待办</div>
        <div class="msg" :class="msg.cls">{{ msg.text }}</div>
        <form @submit.prevent="submit">
          <input v-model="text" class="w1" placeholder="新增待办，如：周三前交房租" required>
          <button class="btn primary" type="submit">添加</button>
        </form>
        <table v-if="todos.length">
          <thead><tr><th style="width:36px"></th><th>内容</th><th>记录于</th><th></th></tr></thead>
          <tbody>
            <tr v-for="t in todos" :key="t.id">
              <td><input type="checkbox" :checked="t.done" :aria-label="'标记完成 ' + t.text" @change="toggle(t, $event.target.checked)"></td>
              <td :class="{ done: t.done }">{{ t.text }}</td>
              <td class="meta">{{ fmtDT(t.createdAt) }}</td>
              <td><button class="btn sm danger" @click="remove(t)">删除</button></td>
            </tr>
          </tbody>
        </table>
        <div v-else class="empty">待办清单是空的，对小五说"记个待办：买牛奶"，或直接在上方添加</div>
      </div>
    </div>
  </section>
</template>
