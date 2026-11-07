import { createApp } from 'vue'
import MobileApp from './mobile/MobileApp.vue'
import { initTheme } from './theme.js'
import './style.css'
import './mobile/mobile.css'

// 移动端入口（/mobile）：与桌面共用全局样式（消息气泡/时间线/markdown 全套
// 类都定义在 style.css），mobile.css 只做数字端版式覆盖；主题机制同源。
initTheme() // 挂载前定好主题（mobile.html 已有内联预设，这里兜底）
createApp(MobileApp).mount('#root')

// 离线壳：服务器不在线时，浏览器直连会甩一张 ERR_CONNECTION_REFUSED 报错页。
// 这里的 SW 把页面壳缓存下来，断网后照样打得开，由前端渲染「服务器不在线」。
// 作用域限定 /mobile——桌面端（/、/palette 等）不受影响。
// 注册失败（不支持 SW、非安全上下文、文件不存在）一律忽略：不影响正常使用。
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/mobile-sw.js', { scope: '/mobile' }).catch(() => {})
  })
}
