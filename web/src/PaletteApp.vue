<script setup>
// 全局快捷键弹窗（桌面外壳 Alt+Space 创建的透明无边框窗口加载本页）。
// 与主窗口同源共享登录会话；Esc/失焦隐藏窗口，Enter 发送 /api/chat 流式提问。
// 放在 Go 侧是刻意设计（docs/DESKTOP.md）：浏览器直接访问 :port/palette 亦可用。
import { ref } from 'vue'

const q = ref('')
const answer = ref('')
const err = ref(false)
const status = ref('')
let busy = false

function hideWindow() {
  try {
    const win = window.__TAURI__ && window.__TAURI__.window && window.__TAURI__.window.getCurrentWindow()
    if (win) { win.hide(); return }
  } catch (_) { /* 外壳不可用（浏览器直接访问）时降级 */ }
  window.close()
}

function onKeydown(e) {
  if (e.key === 'Escape') { e.preventDefault(); hideWindow() }
}
function onEnter(e) {
  if (!e.isComposing) { e.preventDefault(); ask() }
}

async function ask() {
  const text = q.value.trim()
  if (!text || busy) return
  busy = true
  q.value = ''
  err.value = false
  answer.value = ''
  status.value = '思考中…'
  try {
    const res = await fetch('/api/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: text }),
    })
    if (res.status === 401) {
      err.value = true
      answer.value = '还未登录：请先打开 Friday 主窗口登录，再回来使用。'
      status.value = ''
      return
    }
    if (!res.ok || !res.body) throw new Error('HTTP ' + res.status)
    const reader = res.body.getReader()
    const decoder = new TextDecoder('utf-8')
    let buf = ''
    for (;;) {
      const r = await reader.read()
      if (r.done) break
      buf += decoder.decode(r.value, { stream: true })
      let idx
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const chunk = buf.slice(0, idx)
        buf = buf.slice(idx + 2)
        if (!chunk.startsWith('data:')) continue
        let data
        try { data = JSON.parse(chunk.slice(5).trim()) } catch (_) { continue }
        if (data.error) {
          err.value = true
          answer.value += (answer.value ? '\n' : '') + data.error
        } else if (data.delta) {
          answer.value += data.delta
          scrollAnswer()
        }
      }
    }
    status.value = ''
  } catch (e) {
    err.value = true
    answer.value = '连接不上小五（' + e.message + '）。'
  } finally {
    busy = false
  }
}

function scrollAnswer() {
  const el = document.getElementById('answer')
  if (el) el.scrollTop = el.scrollHeight
}
</script>

<template>
  <div id="panel">
    <div id="inputRow">
      <div id="logo">五</div>
      <input id="q" v-model="q" type="text" placeholder="问小五任何事，或让它安排事项…" autofocus
             autocomplete="off" spellcheck="false" @keydown="onKeydown" @keydown.enter="onEnter">
      <span id="hint">Esc 关闭</span>
    </div>
    <div id="answer" :class="{ err }">{{ answer }}</div>
    <div id="status">{{ status }}</div>
  </div>
</template>

<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  html, body { background: transparent; height: 100%; overflow: hidden; }
  #palette-root { height: 100%; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", "Segoe UI",
                 "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
    padding: 10px;
    -webkit-user-select: none; user-select: none;
  }
  /* 深色炭灰面板（参考 Cursor：无色相表面 + 细线边框），浮于桌面 */
  #panel {
    display: flex; flex-direction: column;
    height: 100%;
    border-radius: 18px;
    background: rgba(20, 20, 20, .92);
    border: 1px solid rgba(255, 255, 255, .10);
    box-shadow: 0 24px 64px -14px rgba(0, 0, 0, .7);
    backdrop-filter: blur(28px) saturate(1.4);
    -webkit-backdrop-filter: blur(28px) saturate(1.4);
    overflow: hidden;
    color: #E8E8E8;
  }
  #inputRow {
    display: flex; align-items: center; gap: 10px;
    padding: 14px 16px;
    border-bottom: 1px solid rgba(255, 255, 255, .08);
  }
  #logo { width: 30px; height: 30px; border-radius: 9px; flex: none;
    background: #E8E8E8; border: 1px solid rgba(255,255,255,.16);
    display: flex; align-items: center; justify-content: center;
    color: #141414; font-size: 14px; font-weight: 600; }
  #q {
    flex: 1; background: transparent; border: 0; outline: 0;
    color: #E8E8E8; font-size: 16px; caret-color: #6EA8F2;
  }
  #q::placeholder { color: #616161; }
  #hint { font-size: 11px; color: #616161; border: 1px solid rgba(255,255,255,.14);
    border-radius: 999px; padding: 2px 10px; flex: none; }
  #answer {
    flex: 1; overflow-y: auto; padding: 14px 18px;
    font-size: 14px; line-height: 1.7; white-space: pre-wrap; word-break: break-word;
    -webkit-user-select: text; user-select: text;
  }
  #answer:empty::before {
    content: "直接输入问题，Enter 发送；Esc 关闭。";
    color: #616161;
  }
  #answer.err { color: #FCA5A5; }
  #status { padding: 6px 18px 12px; font-size: 12px; color: #949494; }
  #status:empty { display: none; }
  ::-webkit-scrollbar { width: 8px; }
  ::-webkit-scrollbar-thumb { background: rgba(255,255,255,.14); border-radius: 8px;
    border: 2px solid transparent; background-clip: content-box; }
</style>
