<script setup>
import { onMounted, ref } from 'vue'
import { api } from '../api.js'
import { store, toast, showModal } from '../store.js'
import { authIn } from '../anim.js'

const user = ref('')
const pass = ref('')
const busy = ref(false)

onMounted(authIn)

async function submit() {
  if (busy.value) return
  busy.value = true
  try {
    const r = await api('POST', '/api/login', { username: user.value.trim(), password: pass.value })
    store.username = r.username
    store.loggedIn = true
    pass.value = ''
    window.dispatchEvent(new CustomEvent('friday:setup-refresh'))
    toast('登录成功，欢迎回来 👋')
  } catch (err) { showModal(err.message) }
  finally { busy.value = false }
}
</script>

<template>
  <div id="authView">
    <div class="auth-box">
      <div class="auth-logo">五</div>
      <h1>小五</h1>
      <div class="sub">你的私人 AI 生活管家</div>
      <form @submit.prevent="submit">
        <div class="field">
          <input id="loginUser" v-model="user" aria-label="用户名" placeholder="用户名" autocomplete="username" :disabled="busy" required>
        </div>
        <div class="field">
          <input id="loginPass" v-model="pass" aria-label="密码" type="password" placeholder="密码" autocomplete="current-password" :disabled="busy" required>
        </div>
        <button class="btn primary" type="submit" :disabled="busy">{{ busy ? '正在登录…' : '登 录' }}</button>
      </form>
      <div class="auth-foot">
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
        本地运行 · 数据仅保存在你的设备上
      </div>
    </div>
  </div>
</template>
