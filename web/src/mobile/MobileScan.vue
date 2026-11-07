<script setup>
// 手机端扫码页：摄像头对准电脑端「手机连接」的二维码，扫出服务端地址（带一次性
// 配对码时扫完即自动登录）。
//
// 两条取码路径：
// ① 在 Android APP 壳里 → 交给壳的原生扫码页（zxing），不占用网页摄像头权限；
// ② 纯浏览器 → 用摄像头 + jsQR 逐帧解码（不依赖浏览器原生条码 API），
//    打不开摄像头或用户不给权限时降级为「手动输入地址」。
import { onBeforeUnmount, onMounted, ref } from 'vue'
import jsQR from 'jsqr'
import { hasNative, nativeScan } from './native.js'

const emit = defineEmits(['scanned', 'close'])

const video = ref(null)
const canvas = ref(null)
const status = ref('准备中…')
const manual = ref('')
const useNative = ref(false)
const scanning = ref(false)

let stream = null
let timer = null

onMounted(async () => {
  if (hasNative()) {
    // 壳里：原生扫码页（相机由系统权限管，体验更稳），结果回调给本页
    useNative.value = true
    status.value = '正在打开相机…'
    nativeScan(text => {
      if (!text) {
        status.value = '已取消扫码'
        return
      }
      emit('scanned', text)
    })
    return
  }
  await startCamera()
})

onBeforeUnmount(stopCamera)

async function startCamera() {
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    status.value = '这个浏览器不支持摄像头扫码，请手动输入地址'
    return
  }
  try {
    stream = await navigator.mediaDevices.getUserMedia({
      video: { facingMode: 'environment' },
      audio: false,
    })
  } catch (e) {
    status.value = '打不开摄像头（' + (e && e.message ? e.message : '权限被拒绝') + '），请手动输入地址'
    return
  }
  if (video.value) {
    video.value.srcObject = stream
    await video.value.play().catch(() => {})
  }
  status.value = '把电脑端「手机连接」的二维码放进取景框'
  scanning.value = true
  timer = setInterval(tick, 300)
}

// 逐帧解码：摄像头画面 → canvas → jsQR。
function tick() {
  const v = video.value
  const c = canvas.value
  if (!v || !c || !stream || v.readyState !== v.HAVE_ENOUGH_DATA) return
  const w = v.videoWidth
  const h = v.videoHeight
  if (!w || !h) return
  c.width = w
  c.height = h
  const ctx = c.getContext('2d')
  if (!ctx) return
  ctx.drawImage(v, 0, 0, w, h)
  const img = ctx.getImageData(0, 0, w, h)
  const code = jsQR(img.data, w, h)
  if (code && code.data) {
    emit('scanned', code.data)
  }
}

function stopCamera() {
  scanning.value = false
  if (timer) {
    clearInterval(timer)
    timer = null
  }
  if (stream) {
    stream.getTracks().forEach(t => t.stop())
    stream = null
  }
}

// 手动输入：接受地址或完整链接，与扫码走同一套解析。
function submitManual() {
  const text = manual.value.trim()
  if (!text) return
  emit('scanned', text)
}

function close() {
  stopCamera()
  emit('close')
}
</script>

<template>
  <div class="m-scan">
    <div class="m-scan-head">
      <button class="m-ico-btn" type="button" aria-label="关闭" @click="close">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m15 18-6-6 6-6"/></svg>
      </button>
      <b>扫描服务器二维码</b>
      <span class="grow"></span>
    </div>

    <div class="m-scan-body">
      <div class="m-scan-view">
        <video v-show="!useNative" ref="video" playsinline muted class="m-scan-video"></video>
        <canvas ref="canvas" class="m-scan-canvas"></canvas>
        <div v-if="useNative" class="m-scan-native">
          <svg width="52" height="52" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7V5a2 2 0 0 1 2-2h2M17 3h2a2 2 0 0 1 2 2v2M21 17v2a2 2 0 0 1-2 2h-2M7 21H5a2 2 0 0 1-2-2v-2"/><path d="M7 8h10v8H7z"/></svg>
          <p>已打开相机，请在系统相机里对准电脑端的二维码</p>
        </div>
        <div v-if="!useNative && !scanning" class="m-scan-native">
          <p>{{ status }}</p>
        </div>
      </div>
      <p class="m-scan-status">{{ status }}</p>

      <div class="m-field">
        <label class="m-field-label" for="m-scan-manual">扫不了？手动输入地址</label>
        <input id="m-scan-manual" v-model="manual" class="m-input" inputmode="url"
               autocomplete="off" autocapitalize="none" spellcheck="false"
               placeholder="192.168.10.242:45679">
        <button class="m-btn" type="button" :disabled="!manual.trim()" @click="submitManual">使用这个地址</button>
      </div>
    </div>
  </div>
</template>
