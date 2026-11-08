<script setup>
// 「服务器地址」输入字段：标签 + 输入框 + 末尾的扫码图标。
//
// 登录页与「服务器不在线」卡片共用同一个字段（同一套地址语义、同一处扫码入口），
// 避免两个页面各写一份、行为却对不上。
//
// 地址值是 host:port 形态（用户看到的样子）；scheme 由调用方所在页面决定
// （局域网门走 https，本机调试走 http）。
import { computed } from 'vue'

const props = defineProps({
  // host:port 形态的地址
  modelValue: { type: String, default: '' },
  label: { type: String, default: '服务器地址' },
  placeholder: { type: String, default: '例如 192.168.10.242:45679' },
  disabled: { type: Boolean, default: false },
})
const emit = defineEmits(['update:modelValue', 'scan'])

const value = computed({
  get: () => props.modelValue,
  set: v => emit('update:modelValue', v),
})
</script>

<template>
  <div class="m-field">
    <label class="m-field-label" for="m-srv-addr">{{ label }}</label>
    <div class="m-input-row">
      <input id="m-srv-addr" v-model="value" class="m-input m-input-icon" inputmode="url"
             aria-label="服务器地址" :placeholder="placeholder"
             autocomplete="off" autocapitalize="none" autocorrect="off" spellcheck="false"
             :disabled="disabled">
      <button class="m-input-act" type="button" aria-label="扫描服务器二维码"
              :disabled="disabled" @click="emit('scan')">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7V5a2 2 0 0 1 2-2h2M17 3h2a2 2 0 0 1 2 2v2M21 17v2a2 2 0 0 1-2 2h-2M7 21H5a2 2 0 0 1-2-2v-2"/><path d="M7 8h10v8H7z"/></svg>
      </button>
    </div>
  </div>
</template>
