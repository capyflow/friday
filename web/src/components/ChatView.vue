<script setup>
// 聊天视图：SSE 流式对话、欢迎态（逐字标题/轮播副标题/建议 chips）、
// Codex 风 composer（加号弹窗/模式 chip/圆形发送键）、图片附件、更多菜单。
// 本组件在登录期间常驻（App 里 v-show），切到别的视图不会打断进行中的回复。
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { gsap, animate, bubbleIn, popIn } from '../anim.js'
import { api } from '../api.js'
import { renderMD } from '../md.js'
import { store, toast } from '../store.js'

const messages = reactive([])      // { role: 'user'|'bot'|'err', text, images?: string[], streaming?: bool }
const input = ref('')
const busy = ref(false)
const menuOpen = ref(false)
const popOpen = ref(false)
const mode = ref(null)             // null | 'plan' | 'goal'
const scroller = ref(null)
const inputEl = ref(null)
const fileInput = ref(null)
const moreWrap = ref(null)
const popWrap = ref(null)
const titleEl = ref(null)
const rotatorEl = ref(null)
const pendingImages = reactive([]) // { file, url }
const SIZE_LIMIT = 5 * 1024 * 1024

const MODE_TEXT = {
  plan: ['计划模式', '描述你的任务，小五会先输出计划、经你确认后再执行…'],
  goal: ['目标模式', '描述你的目标，成果定义越清晰效果越好…'],
}
const SUGGESTS = ['10 分钟后提醒我喝水', '记个待办：买牛奶', '上海今天天气怎么样', '每天早上 8 点叫我起床']
const PHRASES = ['提醒、待办、天气，都可以交给我', '说一句话，就能帮你安排好',
                 '本地运行，数据只属于你', '到点了，我会主动提醒你']

const welcome = computed(() => messages.length === 0)
const sendDisabled = computed(() => busy.value || (!input.value.trim() && !pendingImages.length))
const placeholder = computed(() => (mode.value ? MODE_TEXT[mode.value][1] : '和小五说点什么…'))

/* ---------- 滚动 ---------- */
function nearBottom() {
  const el = scroller.value
  return !el || el.scrollHeight - el.scrollTop - el.clientHeight < 80
}
function stick(force) {
  const el = scroller.value
  if (!el) return
  if (force || nearBottom()) el.scrollTop = el.scrollHeight
}
function popLastBubble() {
  nextTick(() => {
    const el = scroller.value
    const last = el && el.querySelector('#msgList .mrow:last-child')
    if (last) bubbleIn(last)
  })
}

/* ---------- 欢迎态动效 ---------- */
let rotatorStop = null
function playWelcome() {
  if (!welcome.value) return
  nextTick(() => {
    // 标题逐字
    const el = titleEl.value
    let chars = []
    if (el) {
      const text = el.textContent
      el.textContent = ''
      for (const ch of text) {
        const s = document.createElement('span')
        s.className = 'ch'
        s.textContent = ch
        el.appendChild(s)
        chars.push(s)
      }
    }
    if (animate) {
      gsap.set(chars, { yPercent: 110, opacity: 0 })
      gsap.fromTo('.welcome-logo', { scale: .4, opacity: 0, y: 12 },
        { scale: 1, opacity: 1, y: 0, duration: .8, ease: 'back.out(1.7)', clearProps: 'transform' })
      gsap.to(chars, { yPercent: 0, opacity: 1, duration: .6,
        stagger: .055, ease: 'back.out(1.6)', delay: .28 })
      gsap.fromTo('.welcome-sub', { y: 12, opacity: 0 },
        { y: 0, opacity: 1, duration: .5, delay: .5, ease: 'power2.out', clearProps: 'opacity,transform' })
      gsap.fromTo('.suggest', { y: 18, opacity: 0, scale: .94 },
        { y: 0, opacity: 1, scale: 1, duration: .45, stagger: .07, delay: .58,
          ease: 'back.out(1.5)', clearProps: 'opacity,transform' })
    }
    // 副标题打字机轮播
    let idx = 0
    const play = () => {
      if (!welcome.value) return
      const phrase = PHRASES[idx++ % PHRASES.length]
      if (rotatorEl.value) rotatorEl.value.textContent = phrase
      if (animate) {
        const st = { len: 0 }
        gsap.timeline()
          .to(st, { len: phrase.length, duration: phrase.length * .06, ease: 'none',
            onUpdate: () => { if (rotatorEl.value) rotatorEl.value.textContent = phrase.slice(0, Math.round(st.len)) } })
          .to({}, { duration: 1.8 })
          .to(st, { len: 0, duration: phrase.length * .03, ease: 'none',
            onUpdate: () => { if (rotatorEl.value) rotatorEl.value.textContent = phrase.slice(0, Math.round(st.len)) } })
          .to({}, { duration: .3, onComplete: play })
      } else {
        rotatorStop = setTimeout(play, 2800)
      }
    }
    play()
  })
}
watch(welcome, v => {
  if (v) {
    playWelcome()
  } else if (rotatorStop) {
    clearTimeout(rotatorStop); rotatorStop = null
  }
})

/* ---------- 历史 ---------- */
async function loadHistory() {
  let list = []
  try {
    list = (await api('GET', '/api/chat/history?session=web')).messages || []
  } catch (e) { /* 拉取失败按空会话处理 */ }
  messages.splice(0, messages.length, ...list.map(m => ({
    role: m.role === 'user' ? 'user' : 'bot', text: m.text,
  })))
  if (messages.length) nextTick(() => stick(true))
}

/* ---------- 附件：归一化压缩 + 预览 + 上传 ---------- */
function removePreview(p) {
  const i = pendingImages.indexOf(p)
  if (i >= 0) { URL.revokeObjectURL(p.url); pendingImages.splice(i, 1) }
}
function clearPendingImages() {
  pendingImages.forEach(p => URL.revokeObjectURL(p.url))
  pendingImages.splice(0, pendingImages.length)
}

// normalizeImage 把任意图片统一转为 JPEG：≤5MB 保持原尺寸仅转格式；
// 超过 5MB 的叠加降采样阶梯（降尺寸 → 降质量）压到上限以内。
async function normalizeImage(file) {
  let bitmap = null
  try { bitmap = await createImageBitmap(file, { imageOrientation: 'from-image' }) } catch (_) {}
  if (!bitmap) {
    try { bitmap = await createImageBitmap(file) } catch (_) { return null }
  }
  const draw = async (maxDim, quality) => {
    const scale = Math.min(1, maxDim / Math.max(bitmap.width, bitmap.height))
    const w = Math.max(1, Math.round(bitmap.width * scale))
    const h = Math.max(1, Math.round(bitmap.height * scale))
    const canvas = document.createElement('canvas')
    canvas.width = w; canvas.height = h
    const ctx = canvas.getContext('2d')
    ctx.fillStyle = '#fff'
    ctx.fillRect(0, 0, w, h)
    ctx.drawImage(bitmap, 0, 0, w, h)
    return new Promise(r => canvas.toBlob(r, 'image/jpeg', quality))
  }
  const toJPG = blob => new File([blob], file.name.replace(/\.\w+$/, '') + '.jpg', { type: 'image/jpeg' })
  const attempts = file.size > SIZE_LIMIT
    ? [[2048, .85], [2048, .7], [2048, .55], [1536, .7], [1280, .6], [1024, .5]]
    : [[Infinity, .9], [2048, .85], [2048, .7], [1280, .6]]
  for (const [maxDim, q] of attempts) {
    const blob = await draw(maxDim, q)
    if (blob && blob.size <= SIZE_LIMIT) {
      return { file: toJPG(blob), down: file.size > SIZE_LIMIT,
               note: (file.size / 1048576).toFixed(1) + 'MB → ' + (blob.size / 1048576).toFixed(1) + 'MB' }
    }
  }
  return null
}

async function onFilesPicked(ev) {
  for (const f of ev.target.files) {
    if (pendingImages.length >= 4) { toast('一条消息最多 4 张图片', true); break }
    if (!f.type.startsWith('image/')) { toast('仅支持图片文件', true); continue }
    const r = await normalizeImage(f)
    if (!r) { toast('图片无法解码，已跳过', true); continue }
    if (r.down) toast('图片过大，已降采样：' + r.note)
    pendingImages.push({ file: r.file, url: URL.createObjectURL(r.file) })
  }
  ev.target.value = ''
}

async function uploadImages() {
  const ids = []
  for (const p of pendingImages) {
    const fd = new FormData()
    fd.append('file', p.file)
    const res = await fetch('/api/upload', { method: 'POST', body: fd })
    const data = await res.json()
    if (!res.ok) throw new Error((data.body && data.body.error) || ('上传失败 HTTP ' + res.status))
    ids.push(data.body.id)
  }
  return ids
}

/* ---------- 加号弹窗 / 模式 ---------- */
function setPop(open) {
  if (open === popOpen.value) return
  popOpen.value = open
  if (open) nextTick(() => { const el = document.getElementById('addPopover'); if (el) popIn(el) })
}
function setMode(m) {
  mode.value = mode.value === m ? null : m
}
function fill(text) {
  input.value = text
  nextTick(() => { inputEl.value && inputEl.value.focus(); autoGrow() })
}

/* ---------- 发送（SSE 流式） ---------- */
function autoGrow() {
  const el = inputEl.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = Math.min(el.scrollHeight, 140) + 'px'
}
function onInputKeydown(e) {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); sendChat() }
}

async function sendChat(presetText) {
  const raw = typeof presetText === 'string' ? presetText : input.value.trim()
  if (mode.value && raw) {
    // 计划/目标模式：以轻量前缀把意图带给小五
    const tag = mode.value === 'plan' ? '【计划】' : '【目标】'
    raw = tag + raw
    mode.value = null
  }
  if ((!raw && !pendingImages.length) || busy.value) return
  setPop(false)
  busy.value = true
  input.value = ''
  autoGrow()

  let ids = []
  const images = pendingImages.map(p => p.url)
  try { ids = pendingImages.length ? await uploadImages() : [] }
  catch (e) {
    toast(e.message, true)
    busy.value = false
    return
  }

  messages.push({ role: 'user', text: raw, images })
  clearPendingImages()
  messages.push({ role: 'typing' })
  popLastBubble()
  nextTick(() => stick(true))

  const typingIdx = () => messages.findIndex(m => m.role === 'typing')
  const end = () => {
    const i = typingIdx()
    if (i >= 0) messages.splice(i, 1)
    busy.value = false
    nextTick(() => inputEl.value && inputEl.value.focus())
  }

  fetch('/api/chat', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ message: raw, attachments: ids, session: 'web' }),
  }).then(res => {
    if (res.status === 401) {
      store.loggedIn = false
      toast('登录已过期，请重新登录', true)
      end()
      return
    }
    if (!res.ok || !res.body) throw new Error('HTTP ' + res.status)
    const reader = res.body.getReader()
    const decoder = new TextDecoder('utf-8')
    let buf = ''
    let bot = null
    let acc = ''
    const handle = ev => {
      let data
      try { data = JSON.parse(ev) } catch (_) { return }
      if (data.error) {
        if (bot) { bot.streaming = false }
        messages.push({ role: 'err', text: data.error })
        popLastBubble()
        end()
        return
      }
      if (data.delta) {
        const i = typingIdx()
        if (i >= 0) messages.splice(i, 1)
        if (!bot) {
          bot = reactive({ role: 'bot', text: '', streaming: true })
          messages.push(bot)
        }
        acc += data.delta
        bot.text = acc
        nextTick(() => stick())
      }
      if (data.done) {
        if (bot) {
          bot.streaming = false
          if (!bot.text) bot.text = '……'
        } else {
          messages.push({ role: 'bot', text: '……' })
        }
        nextTick(() => stick())
        end()
        if (store.view === 'trace') window.dispatchEvent(new CustomEvent('friday:trace-refresh'))
      }
    }
    const pump = () => reader.read().then(r => {
      if (r.done) {
        if (bot) bot.streaming = false
        end()
        return
      }
      buf += decoder.decode(r.value, { stream: true })
      let idx
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const chunk = buf.slice(0, idx)
        buf = buf.slice(idx + 2)
        if (chunk.startsWith('data:')) handle(chunk.slice(5).trim())
      }
      return pump()
    })
    return pump()
  }).catch(e => {
    messages.push({ role: 'err', text: '连接不上小五（' + e.message + '），请稍后再试。' })
    popLastBubble()
    end()
  })
}

/* ---------- 更多菜单 ---------- */
function onDocClick(e) {
  if (menuOpen.value && moreWrap.value && !moreWrap.value.contains(e.target)) menuOpen.value = false
  if (popOpen.value && popWrap.value && !popWrap.value.contains(e.target)) setPop(false)
}
function onKeydown(e) {
  if (e.key === 'Escape') { menuOpen.value = false; setPop(false) }
}
onMounted(() => {
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKeydown)
  loadHistory()
})
onUnmounted(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKeydown)
  if (rotatorStop) clearTimeout(rotatorStop)
})

function goTraces() { menuOpen.value = false; store.view = 'trace' }
async function clearContext() {
  menuOpen.value = false
  try {
    const r = await api('POST', '/api/context/clear', { session: 'web' })
    messages.splice(0, messages.length)
    toast(r.message || '上下文已清除')
  } catch (e) { toast(e.message, true) }
}
</script>

<template>
  <section id="view-chat" class="view">
    <div id="chatHead">
      <div class="title">小五</div>
      <div ref="moreWrap" class="more-wrap">
        <button class="btn icon" aria-label="更多操作" aria-haspopup="true" @click.stop="menuOpen = !menuOpen">
          <svg width="17" height="17" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><use href="#i-more"/></svg>
        </button>
        <div v-show="menuOpen" id="moreMenu" role="menu">
          <button class="menu-item" role="menuitem" @click="goTraces">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-route"/></svg>调用轨迹
          </button>
          <button class="menu-item" role="menuitem" @click="clearContext">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-reset"/></svg>清除上下文
          </button>
        </div>
      </div>
    </div>

    <div ref="scroller" id="chatScroller">
      <!-- 欢迎态（空会话） -->
      <div v-if="welcome" class="welcome">
        <div class="welcome-logo">五</div>
        <h3 class="welcome-title" ref="titleEl">你好，我是小五</h3>
        <p class="welcome-sub">你的私人 AI 生活管家 · <span ref="rotatorEl">提醒、待办、天气，都可以交给我</span><span class="caret"></span></p>
        <div class="welcome-suggests">
          <button v-for="s in SUGGESTS" :key="s" class="suggest" @click="sendChat(s)">{{ s }}</button>
        </div>
      </div>
      <!-- 对话态 -->
      <div v-else id="msgList">
        <div v-for="(m, i) in messages" :key="i" class="mrow" :class="m.role === 'typing' ? 'bot' : m.role">
          <div v-if="m.role === 'typing'" class="bubble">
            <span class="typing"><i></i><i></i><i></i></span>
          </div>
          <div v-else class="bubble" :class="{ md: m.role === 'bot' }">
            <template v-if="m.role === 'bot'">
              <span v-html="renderMD(m.text)"></span>
              <span v-if="m.streaming" class="cursor"></span>
            </template>
            <template v-else>
              {{ m.text }}
              <img v-for="(src, j) in m.images || []" :key="j" class="chat-img" :src="src" alt="">
            </template>
          </div>
        </div>
      </div>
    </div>

    <div id="chatFoot">
      <!-- 模式 chip -->
      <div class="mode-chip" :class="{ show: !!mode }">
        <span class="mode-name">{{ mode ? MODE_TEXT[mode][0] : '' }}</span>
        <button aria-label="退出模式" @click="setMode(mode)">
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path d="M18 6 6 18M6 6l12 12"/></svg>
        </button>
      </div>
      <div id="attachPreviews">
        <div v-for="p in pendingImages" :key="p.url" class="thumb">
          <img :src="p.url" alt="">
          <button class="rm" aria-label="移除图片" @click="removePreview(p)">×</button>
        </div>
      </div>
      <div class="inputbar">
        <button class="ib-btn plus-btn" :class="{ active: popOpen }" aria-label="添加" title="添加"
                @click.stop="setPop(!popOpen)">
          <svg width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>
        </button>
        <input type="file" ref="fileInput" accept="image/*" multiple hidden @change="onFilesPicked">
        <textarea ref="inputEl" v-model="input" rows="1" :placeholder="placeholder" aria-label="聊天输入"
                  @keydown="onInputKeydown" @input="autoGrow"></textarea>
        <button class="ib-btn send-btn" aria-label="发送" title="发送" :disabled="sendDisabled" @click="sendChat()">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#i-send"/></svg>
        </button>

        <!-- 加号弹窗 -->
        <div v-show="popOpen" ref="popWrap" class="popover" id="addPopover">
          <div class="popover-title pop-anim">添加</div>
          <button class="pop-item pop-anim" @click="fileInput.click(); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/><path d="M12 10v6"/><path d="M9 13h6"/></svg></span>
            <span class="pl">添加图片</span>
          </button>
          <div class="pop-sep pop-anim"></div>
          <button class="pop-item pop-anim" :class="{ selected: mode === 'plan' }" @click="setMode('plan'); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m3 17 2 2 4-4"/><path d="m3 7 2 2 4-4"/><path d="M13 6h8"/><path d="M13 12h8"/><path d="M13 18h8"/></svg></span>
            <span class="pl">计划</span>
            <span class="pd">描述你的任务，执行前会先输出计划</span>
          </button>
          <button class="pop-item pop-anim" :class="{ selected: mode === 'goal' }" @click="setMode('goal'); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><circle cx="12" cy="12" r="6"/><circle cx="12" cy="12" r="2"/></svg></span>
            <span class="pl">目标</span>
            <span class="pd">描述你的目标，成果定义越清晰效果越好</span>
          </button>
          <div class="pop-group pop-anim">快捷创建</div>
          <button class="pop-item pop-anim" @click="fill('提醒我 '); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg></span>
            <span class="pl">定时提醒</span>
          </button>
          <button class="pop-item pop-anim" @click="fill('待办：'); setPop(false)">
            <span class="pi"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/></svg></span>
            <span class="pl">新建待办</span>
          </button>
        </div>
      </div>
    </div>
  </section>
</template>
