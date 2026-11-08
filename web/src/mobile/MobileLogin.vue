<script setup>
// 移动端登录页。三个字段自上而下：服务器地址 → 用户名 → 密码。
//
// ① 首装建号（/api/setup）只收本机请求，手机上必然被拒——账号未建时给一张
//    "去电脑端完成首次设置"的说明卡，不摆一个注定 403 的表单。
// ② 服务器地址：默认当前连着的服务器（APP 里由壳提供，浏览器里取 location.host）。
//    末尾的扫码图标扫电脑端「手机连接」的二维码即可换电脑——扫到带一次性配对码
//    的码扫码即登录，纯地址的则换服务器。地址改了要整页跳到新服务器：网页的接口
//    调用是同源相对路径，跨源调不动，只能换 origin。
// ③ 用户名/密码：标准账号密码登录。
import { computed, onMounted, ref } from 'vue'
import { api } from '../api.js'
import { store, toast } from '../store.js'
import MobileScan from './MobileScan.vue'
import MobileServerField from './MobileServerField.vue'
import { addrToOrigin, parseScan } from './server.js'
import { connectTo, currentOrigin } from './native.js'

const props = defineProps({
  // store.setupStatus?.accountRequired 为 true 时展示首装指引而不是表单
  setupStatus: { type: Object, default: null },
})

const user = ref('')
const pass = ref('')
const busy = ref(false)
const scanning = ref(false)
// 密码明文开关（对齐常见客户端：输错时能看见自己敲的字符）
const showPass = ref(false)

// 地址输入框里的值：host:port 形态（用户看到的），默认当前服务器。
const addr = ref('')

onMounted(() => {
  const cur = currentOrigin()
  // currentOrigin 是 origin（https://host:port），输入框只显示 host:port
  try {
    const u = new URL(cur)
    addr.value = u.host
  } catch {
    addr.value = ''
  }
})

// 解析输入框里的地址 → origin。空/非法返回 null。
// 端口由用户自己定：填了就用，没填就不补（配了域名的机器走反代，不需要端口）。
// 注意：origin 已含端口（http://h:45679），别再拼一次端口。
const parsed = computed(() => addrToOrigin(addr.value, location.protocol === 'http:' ? 'http:' : 'https:'))

// 输入的地址与当前服务器不同 → 需要先换服务器（换 origin 会丢登录态，得重登）。
const switching = computed(() => parsed.value && parsed.value !== currentOrigin())

async function submit() {
  if (busy.value) return
  // 地址填的是别的服务器：先整页跳过去（带一次性配对码的码在扫码那步已经处理过了）
  if (switching.value) {
    busy.value = true
    toast('正在切换到 ' + parsed.value + ' …')
    connectTo(parsed.value + '/mobile')
    return
  }
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

// 扫码结果：带一次性配对码的直接开那个链接（开完网页自己 redeem 换 JWT 登录），
// 纯地址的填进地址框（自己点登录切换）。
function onScanned(text) {
  scanning.value = false
  const p = parseScan(text, location.protocol === 'http:' ? 'http:' : 'https:')
  if (!p.ok) {
    toast('这个二维码里没认出来是服务器地址', true)
    return
  }
  if (p.kind === 'pair') {
    connectTo(p.url)
    return
  }
  addr.value = p.host + ':' + p.port
  toast('已填入服务器地址，确认后点登录')
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

      <form v-else class="m-auth-form" @submit.prevent="submit">
        <MobileServerField v-model="addr" :disabled="busy" @scan="scanning = true" />

        <div class="m-field">
          <label class="m-field-label" for="m-auth-user">用户名</label>
          <input id="m-auth-user" v-model="user" class="m-input m-input-icon" aria-label="用户名" placeholder="输入用户名"
                 autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" :disabled="busy" required>
        </div>

        <div class="m-field">
          <label class="m-field-label" for="m-auth-pass">密码</label>
          <div class="m-input-row">
            <input id="m-auth-pass" v-model="pass" class="m-input m-input-icon" aria-label="密码"
                   :type="showPass ? 'text' : 'password'" placeholder="输入密码"
                   autocomplete="current-password" :disabled="busy" required>
            <button class="m-input-act" type="button" :aria-label="showPass ? '隐藏密码' : '显示密码'"
                    :disabled="busy" @click="showPass = !showPass">
              <svg v-if="!showPass" width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/></svg>
              <svg v-else width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3l18 18"/><path d="M10.6 10.7a2 2 0 0 0 2.7 2.7"/><path d="M9.9 5.2A9.7 9.7 0 0 1 12 5c6.4 0 10 7 10 7a17.6 17.6 0 0 1-2.7 3.5M6.6 6.6A17.3 17.3 0 0 0 2 12s3.6 7 10 7a9.6 9.6 0 0 0 4.2-.9"/></svg>
            </button>
          </div>
        </div>

        <button class="m-btn primary block" type="submit" :disabled="busy">
          {{ busy ? (switching ? '正在切换服务器…' : '正在登录…') : '登 录' }}
        </button>
        <p v-if="switching" class="m-auth-note">
          服务器地址与当前不同，登录会先切到那台电脑（切换后需重新登录）。
        </p>
      </form>

      <div class="m-auth-foot">本地运行 · 数据仅保存在你的设备上</div>
    </div>

    <MobileScan v-if="scanning" @scanned="onScanned" @close="scanning = false" />
  </div>
</template>
