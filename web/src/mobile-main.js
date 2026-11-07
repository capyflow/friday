import { createApp } from 'vue'
import MobileApp from './mobile/MobileApp.vue'
import { initTheme } from './theme.js'
import './style.css'
import './mobile/mobile.css'

// 移动端入口（/mobile）：与桌面共用全局样式（消息气泡/时间线/markdown 全套
// 类都定义在 style.css），mobile.css 只做数字端版式覆盖；主题机制同源。
initTheme() // 挂载前定好主题（mobile.html 已有内联预设，这里兜底）
createApp(MobileApp).mount('#root')
