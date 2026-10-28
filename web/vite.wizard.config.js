import { fileURLToPath } from 'node:url'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'

// 首装向导页单独构建：紧随 palette 之后执行（npm run build 串三步），
// emptyOutDir 必须为 false，避免清掉同目录下已产出的 index.html / palette.html。
export default defineConfig({
  base: './',
  plugins: [vue(), viteSingleFile()],
  // 向导组件用运行时 template 字符串（wizard.js 内联模板），必须带上编译器的
  // vue 构建——默认入口是纯运行时，模板编译不了，页面渲染为空白。
  resolve: {
    alias: { vue: 'vue/dist/vue.esm-bundler.js' },
  },
  build: {
    outDir: '../internal/admin/webdist',
    emptyOutDir: false,
    rollupOptions: {
      input: fileURLToPath(new URL('./wizard.html', import.meta.url)),
    },
  },
})
