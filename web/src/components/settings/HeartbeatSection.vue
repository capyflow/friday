<script setup>
// 心跳清单：HEARTBEAT.md 正文在线编辑，保存后下轮心跳生效。
import { onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'

const content = ref('')
const path = ref('')
const msg = reactive({ text: '', cls: '' })
let timer = null
function show(t, ok) {
  msg.text = t; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 4000)
}

async function load() {
  try {
    const r = await api('GET', '/api/heartbeat')
    content.value = r.content || ''
    path.value = r.path + (r.note || '')
  } catch (e) { show(e.message, false) }
}
async function save() {
  try {
    await api('POST', '/api/heartbeat', { content: content.value })
    show('已保存，下轮心跳生效', true)
  } catch (e) { show(e.message, false) }
}

onMounted(load)
</script>

<template>
  <textarea id="hbContent" v-model="content" rows="6" class="ta-plain"
    placeholder="每轮心跳把这份清单交给小五自主检查，如：&#10;- 检查 notes.txt 里的提醒是否需要汇报&#10;- 若用户超过一天没互动，写一句问候"></textarea>
  <div class="row" style="margin-top:8px">
    <button class="btn primary" type="button" @click="save">保存清单</button>
    <span class="meta">{{ path }}</span>
  </div>
  <div class="msg" :class="msg.cls">{{ msg.text }}</div>
</template>
