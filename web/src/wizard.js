// 首装向导（桌面外壳专用独立页 /wizard.html）：第一步创建管理员账号（已有则跳过），
// 第二步确认数据目录——页面内展示默认位置与说明，按需弹原生选择器；确认后外壳
// 迁移本次产生的文件并以所选目录重启内核，本页跳转登录页。
// 只依赖 Tauri IPC 与内核公开接口（/api/setup*），不引主应用状态，保持可独立构建。
import { createApp, ref, computed, onMounted } from 'vue'
import './style.css'

const tauri = window.__TAURI__?.core ?? null

const app = createApp({
  setup() {
    const step = ref('loading') // loading | account | dir | applying | done
    const error = ref('')
    const username = ref('')
    const password = ref('')
    const confirmPassword = ref('')
    const currentDir = ref('')
    const pickedDir = ref('') // 空 = 使用默认位置
    const accountRequired = ref(true)
    const busy = ref(false)

    const targetDir = computed(() => pickedDir.value || currentDir.value)

    async function createAccount() {
      if (busy.value) return
      error.value = ''
      if (!username.value.trim()) { error.value = '请输入用户名'; return }
      if ([...password.value].length < 6) { error.value = '密码至少需要 6 个字符'; return }
      if (password.value !== confirmPassword.value) { error.value = '两次输入的密码不一致'; return }
      busy.value = true
      try {
        const r = await fetch('/api/setup', { method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ username: username.value.trim(), password: password.value }) })
        const body = await r.json()
        if (!r.ok) throw new Error(body?.error || 'HTTP ' + r.status)
        password.value = ''; confirmPassword.value = ''
        step.value = 'dir'
      } catch (e) { error.value = e.message } finally { busy.value = false }
    }

    async function pickDir() {
      if (!tauri || busy.value) return
      error.value = ''
      try { pickedDir.value = (await tauri.invoke('pick_data_dir_dialog')) || '' } catch (e) { error.value = String(e) }
    }

    async function applyDir() {
      if (busy.value) return
      busy.value = true
      error.value = ''
      step.value = 'applying'
      try {
        const r = await tauri.invoke('apply_data_dir', { path: pickedDir.value })
        step.value = 'done'
        setTimeout(() => { location.replace(r.addr + '/') }, 600)
      } catch (e) {
        error.value = String(e)
        step.value = 'dir'
      } finally { busy.value = false }
    }

    onMounted(async () => {
      try {
        const status = await fetch('/api/setup/status', { credentials: 'same-origin' }).then(r => r.json())
        accountRequired.value = !!status.body?.accountRequired
        if (!tauri) {
          // 无外壳（浏览器/非桌面环境）：目录步骤不可用，只剩提示。
          step.value = accountRequired.value ? 'account' : 'dir'
          error.value = '未检测到桌面外壳：数据目录确认需在 Friday 应用中进行。'
          return
        }
        const st = await tauri.invoke('kernel_state')
        currentDir.value = st.dataDir || ''
        step.value = accountRequired.value ? 'account' : 'dir'
      } catch (e) {
        step.value = 'dir'
        error.value = String(e)
      }
    })

    return { step, error, username, password, confirmPassword, currentDir, pickedDir, targetDir,
             accountRequired, busy, createAccount, pickDir, applyDir }
  },

  template: `
  <div id="authView">
    <section class="auth-box setup-box">
      <div class="brand-mark auth-logo"></div>

      <template v-if="step === 'loading'">
        <h1>正在准备初始设置…</h1>
      </template>

      <template v-else-if="step === 'account'">
        <h1>欢迎使用小五</h1>
        <p class="sub">第一步 · 创建你的管理员账号</p>
        <form class="form-col" @submit.prevent="createAccount">
          <label>用户名<input v-model="username" name="username" autocomplete="username"
            autocapitalize="none" autocorrect="off" spellcheck="false" maxlength="32" :disabled="busy" required></label>
          <label>密码<input v-model="password" name="password" type="password"
            autocomplete="new-password" minlength="6" :disabled="busy" required></label>
          <label>确认密码<input v-model="confirmPassword" name="confirmPassword" type="password"
            autocomplete="new-password" :disabled="busy" required></label>
          <p class="set-note">至少 6 个字符。账号与之后选择的数据目录一起保存。</p>
          <button class="btn primary" type="submit" :disabled="busy">{{ busy ? '正在创建…' : '创建账号，下一步' }}</button>
        </form>
      </template>

      <template v-else-if="step === 'dir'">
        <h1>选择数据目录</h1>
        <p class="sub">{{ accountRequired ? '第二步' : '初始设置' }} · 配置、账号、记忆和日志都保存在这里</p>
        <div class="dir-card">
          <div class="dir-row">
            <span class="dir-label">当前目录</span>
            <code class="dir-path" :title="currentDir">{{ currentDir || '（未知）' }}</code>
          </div>
          <div class="dir-row" v-if="pickedDir">
            <span class="dir-label">已选择</span>
            <code class="dir-path picked" :title="pickedDir">{{ pickedDir }}</code>
          </div>
          <p class="set-note">直接确认即使用当前目录；「选择其他位置」可换磁盘或文件夹——
            已有 Friday 数据的目录会被直接接管，其它非空目录会收进其下的 Friday 子目录。
            {{ accountRequired ? '刚才创建的账号会随目录一起迁移。' : '' }}</p>
        </div>
        <div class="dir-actions">
          <button class="btn" type="button" :disabled="busy" @click="pickDir">选择其他位置…</button>
          <button class="btn primary" type="button" :disabled="busy" @click="applyDir">
            {{ busy ? '正在应用…' : '使用并进入 Friday' }}</button>
        </div>
      </template>

      <template v-else-if="step === 'applying'">
        <h1>正在切换数据目录…</h1>
        <p class="sub">内核迁移数据并以新目录重启，几秒后自动进入登录页</p>
      </template>

      <template v-else-if="step === 'done'">
        <h1>设置完成</h1>
        <p class="sub">正在进入登录页…</p>
      </template>

      <p v-if="error" class="msg err" role="alert">{{ error }}</p>
    </section>
  </div>
  `,
})

app.mount('#wizard')
