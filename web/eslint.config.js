import js from '@eslint/js'
import vue from 'eslint-plugin-vue'

export default [
  js.configs.recommended,
  ...vue.configs['flat/essential'],
  {
    files: ['src/**/*.{js,vue}', 'tests/**/*.js', '*.config.js'],
    languageOptions: {
      globals: Object.fromEntries([
        'window', 'document', 'location', 'navigator', 'localStorage', 'matchMedia', 'fetch',
        'URL', 'File', 'FormData', 'Blob', 'HTMLCanvasElement', 'createImageBitmap',
        'CustomEvent', 'TextDecoder', 'TextEncoder', 'AbortController', 'Response',
        'ReadableStream', 'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval',
        'requestAnimationFrame', 'cancelAnimationFrame', 'console', 'performance', 'confirm',
      ].map(name => [name, 'readonly'])),
    },
    rules: {
      'no-empty': ['error', { allowEmptyCatch: true }],
      'no-unused-vars': ['error', { argsIgnorePattern: '^_', caughtErrors: 'none', varsIgnorePattern: '^_' }],
      // 现有 Markdown 展示由 md.js 统一清洗；禁止 v-html 会误报其调用点。
      'vue/no-v-html': 'off',
      'vue/multi-word-component-names': 'off',
    },
  },
  {
    // 仅测试可读文件系统（校验插画 kind 与调用点对齐）；src 仍只允许浏览器全局。
    files: ['tests/**/*.js'],
    languageOptions: { globals: { process: 'readonly' } },
  },
]
