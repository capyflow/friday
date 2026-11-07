<script setup>
// 移动端登录页。首装建号（/api/setup）只收本机请求，手机上必然被拒——
// 账号未建时给一张"去电脑端完成首次设置"的说明卡，不摆一个注定 403 的表单。
import { ref } from 'vue'
import { api } from '../api.js'
import { store, toast } from '../store.js'

const props = defineProps({
  // store.setupStatus?.accountRequired 为 true 时展示首装指引而不是表单
  setupStatus: { type: Object, default: null },
})

const user = ref('')
const pass = ref('')
const busy = ref(false)

async function submit() {
  if (busy.value) return
  busy.value = true
  try {
    const r = await api('POST', '/api/login', { username: user.value.trim(), password: pass.value })
    store.username = r.username
    store.loggedIn = true
    pass.value = ''
    toast('登录成功，欢迎回来 👋')
  } catch (err) {
    toast(err.message || '登录失败', true)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="m-auth">
    <div class="m-auth-box">
      <div class="m-auth-logo"><span class="brand-mark"></span></div>
      <h1>小五</h1>
      <div class="m-auth-sub">手机端 · 与电脑上的 Friday 同一内核</div>

      <div v-if="props.setupStatus && props.setupStatus.accountRequired" class="m-card m-auth-setup">
        <p>还没有账号。首次设置（建号）只允许在运行 Friday 的电脑上完成：</p>
        <p>在该电脑打开 Friday 主页面，按引导建好账号后，回到这里用同一账号登录。</p>
      </div>

      <form v-else @submit.prevent="submit">
        <input v-model="user" class="m-input" aria-label="用户名" placeholder="用户名"
               autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" :disabled="busy" required>
        <input v-model="pass" class="m-input" aria-label="密码" type="password" placeholder="密码"
               autocomplete="current-password" :disabled="busy" required>
        <button class="m-btn primary block" type="submit" :disabled="busy">{{ busy ? '正在登录…' : '登 录' }}</button>
      </form>

      <div class="m-auth-foot">本地运行 · 数据仅保存在你的设备上</div>
    </div>
  </div>
</template>
