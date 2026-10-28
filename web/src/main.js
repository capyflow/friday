import { createApp } from 'vue'
import App from './App.vue'
import { initTheme } from './theme.js'
import { hydratePrefs } from './prefs.js'
import './style.css'

// 桌面端先回填偏好（端口回退/WebView 数据被清后 localStorage 为空，从壳的
// 数据目录补），再定主题挂载——避免首帧用错主题；浏览器直访/回填失败照常挂载。
hydratePrefs()
  .catch(() => {})
  .finally(() => {
    initTheme() // 挂载前定好主题（index.html 已有内联预设，这里兜底）
    createApp(App).mount('#root')
  })
