<script setup>
// MCP 服务器的字段表单：卡片展开态与「添加服务器」弹窗共用这一份定义，
// 两边字段、占位文案、校验口径不会走岔。row 是用 v-model 传进来的（不是普通 prop）：
// 草稿对象归调用方所有，子组件只改它的字段，何时落盘（保存 / 取消）由调用方决定。
import SelectBox from '../SelectBox.vue'

const row = defineModel('row', { type: Object, required: true })
defineProps({
  disabled: { type: Boolean, default: false },
})

const TYPE_OPTIONS = [
  { value: 'stdio', label: '本地命令' },
  { value: 'http', label: '远程 URL' },
]
const AUTH_OPTIONS = [
  { value: 'header', label: '静态请求头' },
  { value: 'oauth', label: '浏览器授权' },
]
</script>

<template>
  <label class="mr-field"><span>接入方式</span>
    <SelectBox v-model="row.type" :options="TYPE_OPTIONS" aria-label="接入方式" :disabled="disabled"></SelectBox>
  </label>
  <label class="mr-field"><span>名称</span>
    <input v-model="row.name" placeholder="如 notion / fs" aria-label="服务器名称" :disabled="disabled">
  </label>
  <label class="mr-field"><span>{{ row.type === 'http' ? '远程地址' : '本地命令' }}</span>
    <input v-model="row.target" :disabled="disabled"
           :placeholder="row.type === 'http' ? 'http(s)://…' : '如 npx / python3'">
  </label>
  <label v-if="row.type === 'stdio'" class="mr-field"><span>参数</span>
    <input v-model="row.argsStr" placeholder="空格分隔" :disabled="disabled">
  </label>
  <label v-if="row.type === 'http'" class="mr-field"><span>鉴权方式</span>
    <SelectBox v-model="row.auth" :options="AUTH_OPTIONS" aria-label="鉴权方式" :disabled="disabled"></SelectBox>
  </label>
  <label class="mr-field">
    <span>{{ row.type === 'http' ? (row.auth === 'oauth' ? '附加请求头' : '请求头') : '环境变量' }}</span>
    <input v-model="row.extraStr" :disabled="disabled"
           :placeholder="row.type === 'http'
             ? (row.auth === 'oauth' ? '一般留空，OAuth 令牌由 Friday 管理' : 'KEY: VALUE，逗号分隔（鉴权等）')
             : 'KEY=VALUE，逗号分隔'">
  </label>
</template>
