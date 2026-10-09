import { createApp } from 'vue'
import App from './App.vue'
import { initTheme } from './theme.js'
import './style.css'

initTheme() // 挂载前定好主题（index.html 已有内联预设，这里兜底）
createApp(App).mount('#root')
