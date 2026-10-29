<script setup>
// 外观：颜色主题。三档即时生效，纯本机浏览器偏好，不落后端配置。
import { onMounted, onUnmounted, ref } from 'vue'
import { clearTheme, setTheme, themePreference } from '../../theme.js'

const OPTIONS = [
  { id: 'auto', label: '跟随系统' },
  { id: 'light', label: '浅色' },
  { id: 'dark', label: '深色' },
]

const pref = ref(themePreference())

function choose(id) {
  if (id === 'auto') clearTheme(); else setTheme(id)
  pref.value = id
}
// 账号菜单的快捷切换走同一个广播：那边改了，这里的选中态要跟上
function onTheme(e) {
  pref.value = e.detail || themePreference()
}
onMounted(() => window.addEventListener('friday:theme', onTheme))
onUnmounted(() => window.removeEventListener('friday:theme', onTheme))
</script>

<template>
  <div class="row">
    <button v-for="o in OPTIONS" :key="o.id" class="btn" type="button"
            :class="{ primary: pref === o.id }" :aria-pressed="pref === o.id"
            @click="choose(o.id)">{{ o.label }}</button>
  </div>
</template>
