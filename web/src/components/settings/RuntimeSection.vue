<script setup>
// 运行上限：一轮回答里允许的工具调用轮数（默认 100）。
// 一轮 = 模型说话 + 它调用的工具跑完，如此反复，直到模型给出不再调工具的正文。
// 触顶后本轮已完成的工具操作保留，调大上限重试即可接着做完。
import { onMounted, reactive, ref } from 'vue'
import { api } from '../../api.js'

const maxIterations = ref(100)
const limits = reactive({ min: 1, max: 500, def: 100 })
const msg = reactive({ text: '', cls: '' })
let timer = null
function show(text, ok) {
  msg.text = text; msg.cls = ok ? 'ok' : 'err'
  clearTimeout(timer); timer = setTimeout(() => { msg.text = '' }, 4000)
}

async function load() {
  try {
    const r = await api('GET', '/api/runtime')
    maxIterations.value = r.maxIterations
    limits.min = r.minIterations
    limits.max = r.maxIterationsLimit
    limits.def = r.defaultMaxIterations
  } catch (e) { show('读取运行上限失败：' + e.message, false) }
}

async function save() {
  const n = Number(maxIterations.value)
  // 0 是合法的"恢复默认"；其余只收 [min, max] 的整数。
  if (!Number.isInteger(n) || n < 0 || n > limits.max) {
    show(`轮数上限只能填 ${limits.min}-${limits.max} 的整数（填 0 恢复默认 ${limits.def}）`, false)
    return
  }
  try {
    const r = await api('POST', '/api/runtime', { maxIterations: n })
    maxIterations.value = r.maxIterations
    show(r.message || '已保存', true)
  } catch (e) { show(e.message, false) }
}

onMounted(load)
</script>

<template>
  <label class="rt-field">
    <span>单轮工具调用上限（轮）</span>
    <input v-model="maxIterations" type="number" :min="limits.min" :max="limits.max" step="1"
           aria-label="单轮工具调用上限">
  </label>
  <div class="row rt-actions">
    <button class="btn primary" type="button" @click="save">保存并热生效</button>
  </div>
  <p class="meta rt-note">
    一轮 = 模型说话 + 它调用的工具跑完。默认 {{ limits.def }} 轮——撞上限时报「工具调用超过 N 轮仍未结束」，
    本轮已完成的工具操作都保留，调大后重试即可接着做完。调得越大越不容易中断，但失控的自循环也烧得越久；
    填 0 恢复默认。
  </p>
  <div class="msg" :class="msg.cls">{{ msg.text }}</div>
</template>

<style scoped>
.rt-field { display: flex; flex-direction: column; gap: 4px; font-size: 12px; }
.rt-field input {
  font: inherit; font-size: 13px; padding: 7px 9px; border: 1px solid var(--border-strong);
  border-radius: 8px; background: var(--bg); color: var(--fg); width: 160px; }
.rt-actions { margin-top: 10px; }
.rt-note { margin: 10px 0 0; font-size: 11.5px; line-height: 1.55; }
</style>
