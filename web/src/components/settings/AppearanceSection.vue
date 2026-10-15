<script setup>
// 外观：颜色主题。三档即时生效，纯本机浏览器偏好，不落后端配置。
import { onMounted, onUnmounted, ref } from 'vue'
import { clearTheme, setTheme, themePreference } from '../../theme.js'

const OPTIONS = [
  { id: 'auto', label: '跟随系统' },
  { id: 'light', label: '浅色' },
  { id: 'dark', label: '深色' },
]
const DESC = {
  auto: '按操作系统的深浅偏好自动切换，系统改了立刻跟着变',
  light: '固定浅色：白卡 + 墨色行动键',
  dark: '固定深色：中性炭灰 + 细线边框，无色相渐变与光斑',
}

const pref = ref(themePreference())
const effective = ref(document.documentElement.dataset.theme || 'dark')

function choose(id) {
  if (id === 'auto') clearTheme(); else setTheme(id)
  pref.value = id
}
// 账号菜单的快捷切换走同一个广播：那边改了，这里的选中态与生效值要跟上
function onTheme(e) {
  pref.value = themePreference()
  effective.value = e.detail || themePreference()
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
  <div class="field-desc">{{ DESC[pref] }}</div>
  <div class="set-note">当前生效：<b>{{ effective === 'dark' ? '深色' : '浅色' }}</b>。
    主题只存在这台设备的浏览器里（localStorage），不写进 config.yaml，换浏览器或设备是另一份选择；
    侧栏账号菜单里另有一个快捷切换，两边同步同一个值。</div>
</template>
