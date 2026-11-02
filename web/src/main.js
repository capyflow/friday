import { createApp } from 'vue'
import App from './App.vue'
import { initTheme } from './theme.js'
import { hydratePrefs } from './prefs.js'
import './style.css'

// 桌面外壳（Tauri）：标题栏是 Overlay（内容铺到窗口顶部），红绿灯浮在侧栏左上角，
// 所以要把应用头部让出那一段并接管拖拽；浏览器直访不加这个标记，版式不变。
if (window.__TAURI_INTERNALS__ || window.__TAURI__) {
  document.documentElement.dataset.shell = 'desktop'
}

// 桌面端先回填偏好（端口回退/WebView 数据被清后 localStorage 为空，从壳的
// 数据目录补），再定主题挂载——避免首帧用错主题；浏览器直访/回填失败照常挂载。
hydratePrefs()
  .catch(() => {})
  .finally(() => {
    initTheme() // 挂载前定好主题（index.html 已有内联预设，这里兜底）
    createApp(App).mount('#root')
  })
