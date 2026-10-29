import { fileURLToPath } from 'node:url'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'

// 首装向导页单独构建：紧随 palette 之后执行（npm run build 串三步），
// emptyOutDir 必须为 false，避免清掉同目录下已产出的 index.html / palette.html。
export default defineConfig({
  base: './',
  plugins: [vue(), viteSingleFile()],
  build: {
    outDir: '../internal/admin/webdist',
    emptyOutDir: false,
    rollupOptions: {
      input: fileURLToPath(new URL('./wizard.html', import.meta.url)),
    },
  },
})
