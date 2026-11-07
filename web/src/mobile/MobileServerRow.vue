<script setup>
// 移动端「服务器地址」输入行：地址输入框 + 紧跟其后的扫码图标。
//
// 用途：登录页要连哪台电脑上的 Friday。输入框默认是当前页面所在的服务器
// （APP 里由壳提供，浏览器里取 location.host）——多数时候不用改；换电脑 /
// 换 IP 时可以手输，或点末尾的扫码图标扫电脑端「手机连接」的二维码。
//
// 地址改了以后要整页跳到新服务器——网页的接口调用是同源相对路径，跨源调不动，
// 只能换 origin（APP 里由壳换，浏览器里整页跳）。
import { computed, ref, watch } from 'vue'
import { addrLabel, normalizeAddr } from './server.js'
import { currentOrigin, hasNative, nativeServerAddress } from './native.js'

// 用户没写 scheme 时沿用当前页面的协议：局域网门是 https，本机调试是 http。
const schemeHint = () => (location.protocol === 'http:' ? 'http:' : 'https:')

const props = defineProps({
  // 当前连着的服务器（origin 或 host:port）。空则用页面自身的地址。
  current: { type: String, default: '' },
})
const emit = defineEmits(['scan', 'switch'])

const draft = ref('')
const editing = ref(false)

// 默认展示当前服务器地址；点一下才可编辑（免得每次登录都要动它）。
const shown = computed(() => {
  const base = props.current || (hasNative() ? nativeServerAddress() : location.host)
  return addrLabel(base)
})

// 输入框里的值与当前服务器不同 → 给出「连过去」的出口。
const dirty = computed(() => {
  const n = normalizeAddr(draft.value, schemeHint())
  return n.ok && n.origin !== currentOrigin()
})

watch(() => props.current, () => {
  if (!editing.value) draft.value = shown.value
}, { immediate: true })

function startEdit() {
  draft.value = shown.value
  editing.value = true
}

// 提交新地址：归一化后交给上层（APP 走壳，浏览器整页跳）。
function commit() {
  const n = normalizeAddr(draft.value, schemeHint())
  if (!n.ok) return
  editing.value = false
  draft.value = shown.value
  if (n.origin === currentOrigin()) return
  emit('switch', n.origin)
}
</script>

<template>
  <div class="m-srv">
    <div class="m-srv-row">
      <input v-model="draft" class="m-input m-srv-input" :class="{ editing }" inputmode="url"
             aria-label="服务器地址" :readonly="!editing"
             autocomplete="off" autocapitalize="none" spellcheck="false"
             placeholder="192.168.10.242:45679"
             @focus="startEdit" @keyup.enter="commit">
      <button class="m-srv-scan" type="button" aria-label="扫描服务器二维码" @click="emit('scan')">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7V5a2 2 0 0 1 2-2h2M17 3h2a2 2 0 0 1 2 2v2M21 17v2a2 2 0 0 1-2 2h-2M7 21H5a2 2 0 0 1-2-2v-2"/><path d="M7 8h10v8H7z"/></svg>
      </button>
    </div>
    <button v-if="dirty" class="m-srv-go" type="button" @click="commit">
      连到 {{ draft.trim() }}
    </button>
  </div>
</template>
