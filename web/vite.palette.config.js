import { fileURLToPath } from 'node:url'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'

// palette（桌面快捷键面板）单独构建：紧随主应用构建之后执行（npm run build 串两步），
// emptyOutDir 必须为 false，避免清掉同目录下已产出的 index.html。
export default defineConfig({
  base: './',
  plugins: [vue(), viteSingleFile()],
  build: {
    outDir: '../internal/admin/webdist',
    emptyOutDir: false,
    rollupOptions: {
      input: fileURLToPath(new URL('./palette.html', import.meta.url)),
    },
  },
})
