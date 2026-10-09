import { fileURLToPath } from 'node:url'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'

// 主应用入口。构建产物内联为自包含 HTML，落在 internal/admin/webdist/ 供 go:embed。
// palette（桌面快捷键面板）由 vite.palette.config.js 单独二次构建到同一目录——
// 单入口下单入口内联才不会把两边的 CSS 混在一起（palette 的 body 必须保持透明）。
export default defineConfig({
  base: './',
  plugins: [vue(), viteSingleFile()],
  build: {
    // 产物直接落进 Go 包目录，供 go:embed；该目录提交占位页，
    // 未跑 npm build 时 go build 也能通过（占位页提示构建命令）。
    outDir: '../internal/admin/webdist',
    emptyOutDir: true,
  },
  server: {
    // 本地前端开发：npm run dev 起在 5173，API 代理到正在运行的 Go 服务。
    proxy: { '/api': 'http://localhost:8080' },
  },
})
