<script setup>
import { ref } from 'vue'
import { api } from '../api.js'

// 首次设置只负责创建管理员账号：模型配置留在「设置 → 模型」里，
// 不在引导中追问（那需要用户先有服务商地址与密钥，卡在门口反而更慢）。
defineProps({ status: { type: Object, required: true } })
const emit = defineEmits(['account-created'])
const username = ref('')
const password = ref('')
const confirmPassword = ref('')
const error = ref('')
const busy = ref(false)

async function createAccount() {
  if (busy.value) return
  error.value = ''
  if (!username.value.trim()) { error.value = '请输入用户名'; return }
  if ([...password.value].length < 6) { error.value = '密码至少需要 6 个字符'; return }
  if (password.value !== confirmPassword.value) { error.value = '两次输入的密码不一致'; return }
  busy.value = true
  try {
    const response = await api('POST', '/api/setup', { username: username.value.trim(), password: password.value })
    password.value = ''; confirmPassword.value = ''
    emit('account-created', response)
  } catch (err) { error.value = err.message }
  finally { busy.value = false }
}
</script>

<template>
  <div id="authView">
    <section class="auth-box setup-box" :aria-busy="busy">
      <div class="auth-logo">五</div>
      <h1>欢迎使用小五</h1>
      <p class="sub">先创建你的管理员账号</p>
      <form v-if="status.localSetupAllowed" class="form-col" @submit.prevent="createAccount">
        <label>用户名<input v-model="username" name="username" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" maxlength="32" :disabled="busy" required></label>
        <label>密码<input v-model="password" name="password" type="password" autocomplete="new-password" minlength="6" :disabled="busy" required></label>
        <label>确认密码<input v-model="confirmPassword" name="confirmPassword" type="password" autocomplete="new-password" :disabled="busy" required></label>
        <p class="set-note">使用至少 6 个字符的独立密码。账号仅在成功保存后创建。</p>
        <button class="btn primary" type="submit" :disabled="busy">{{ busy ? '正在创建…' : '创建账号' }}</button>
      </form>
      <div v-else class="set-note" role="status">请在运行 Friday 的设备上打开本机地址完成首次设置。远程或 Docker 部署请通过 FRIDAY_ADMIN_USERNAME 和 FRIDAY_ADMIN_PASSWORD 预置管理员凭据，然后重启服务。</div>
      <p v-if="error" class="msg err" role="alert">{{ error }}</p>
    </section>
  </div>
</template>

<style scoped>
.setup-box { width: min(100%, 480px); box-sizing: border-box; text-align: left; }
.setup-box h1, .setup-box .sub { text-align: center; }
.setup-box label { display: block; }
.setup-box label > input { width: 100%; min-height: 44px; box-sizing: border-box; display: block; margin-top: 6px; }
.setup-box .btn { min-height: 44px; }
</style>
