<script setup>
// 手机连接（设置 → 数据与系统）：生成一次性配对二维码，手机扫码即以同一内核
// 登录移动端 PWA。配对链接走 TLS 局域网门（admin.tls），未开启时接口报错引导。
// 证书信任的三步指引只在出码后展示（操作路径，不是产品说明）。
import { ref } from 'vue'
import { api } from '../../api.js'
import { toast } from '../../store.js'

const info = ref(null) // { url, qr, expiresAt, caURL }
const busy = ref(false)

async function generate() {
  if (busy.value) return
  busy.value = true
  try {
    info.value = await api('GET', '/api/pair/info')
  } catch (e) {
    toast(e.message || '生成配对码失败', true)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="pairing">
    <div class="row">
      <button class="btn primary" type="button" :disabled="busy" @click="generate">
        {{ busy ? '生成中…' : (info ? '重新生成配对码' : '生成配对二维码') }}
      </button>
    </div>
    <div v-if="info" class="pairing-box">
      <img class="pairing-qr" :src="'data:image/png;base64,' + info.qr" alt="配对二维码" width="180" height="180">
      <div class="pairing-steps">
        <p>手机与电脑连同一 Wi-Fi 后：</p>
        <ol>
          <li>浏览器先打开 <a :href="info.caURL" download="friday-ca.crt">{{ info.caURL }}</a>，安装并信任证书
            <span class="pairing-dim">（iOS：设置 → 通用 → VPN与设备管理 → 信任；Android：设置 → 安全 → 加密与凭据）</span></li>
          <li>扫上方二维码（或直接输入下方地址），自动登录</li>
        </ol>
        <code class="pairing-url">{{ info.url }}</code>
      </div>
    </div>
  </div>
</template>

<style scoped>
.pairing-box {
  display: flex;
  gap: 16px;
  align-items: flex-start;
  margin-top: 12px;
}
.pairing-qr {
  border-radius: 10px;
  background: #fff;
  padding: 6px;
  flex: none;
}
.pairing-steps p { margin: 0 0 6px; }
.pairing-steps ol { margin: 0; padding-left: 20px; }
.pairing-steps li { margin: 4px 0; }
.pairing-dim { opacity: .6; }
.pairing-url {
  display: block;
  margin-top: 8px;
  font-size: 12px;
  word-break: break-all;
  opacity: .75;
}
</style>
