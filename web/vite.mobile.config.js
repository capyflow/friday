import { fileURLToPath } from 'node:url'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'

// 移动端入口单独构建：紧随 wizard 之后执行（npm run build 串四步），
// emptyOutDir 必须为 false，避免清掉同目录下已产出的 index/palette/wizard。
// public/（PWA 清单与图标）由 vite 统一拷贝进产物目录，各入口构建都会覆盖，
// 内容相同无影响。
export default defineConfig({
  base: './',
  plugins: [vue(), viteSingleFile()],
  build: {
    outDir: '../internal/admin/webdist',
    emptyOutDir: false,
    rollupOptions: {
      input: fileURLToPath(new URL('./mobile.html', import.meta.url)),
    },
  },
})
