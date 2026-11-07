// 移动端 PWA 的离线壳：服务器不在线时，浏览器直连 /mobile 会直接甩一张
// “网页无法打开 net::ERR_CONNECTION_REFUSED” 给用户——那不是我们的界面。
//
// 这个 SW 把移动端页面壳缓存下来：断网（或电脑没开）时仍然打得开页面，
// 由前端自己渲染「服务器不在线 + 重试 + 换地址」的卡片。
//
// 缓存策略：
//   ① 页面壳（/mobile）：网络优先、失败回缓存——在线永远拿最新的，断了用上次那份；
//   ② 其它请求（接口、附件、WebSocket）：一律不缓存——接口数据带登录态且是活数据，
//      缓存它只会让用户看到别人的旧数据；断网时让请求照常失败，前端据此显示离线态。
//
// 注意：SW 只能在安全上下文（https 或 localhost）注册，且需要用户先成功打开过一次
// 页面。所以它治的是“开着 PWA 突然断线”，治不了“第一次就断线”——那仍由壳
// （Android APP）或浏览器自己的错误页兜底。

const CACHE = 'friday-mobile-v1'
const SHELL = '/mobile'

self.addEventListener('install', e => {
  e.waitUntil(
    caches.open(CACHE)
      .then(c => c.add(new Request(SHELL, { cache: 'reload' })))
      .catch(() => {}) // 壳拿不到不算安装失败：等下次联网再缓存
      .then(() => self.skipWaiting())
  )
})

self.addEventListener('activate', e => {
  e.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
      .then(() => self.clients.claim())
  )
})

self.addEventListener('fetch', e => {
  const req = e.request
  if (req.method !== 'GET') return
  const url = new URL(req.url)
  if (url.origin !== self.location.origin) return
  if (url.pathname !== SHELL) return // 只管页面壳，其余请求不碰

  e.respondWith(
    fetch(req)
      .then(res => {
        // 在线拿到新的就顺手更新缓存（断网后有最新的一份可用）
        const copy = res.clone()
        caches.open(CACHE).then(c => c.put(req, copy)).catch(() => {})
        return res
      })
      .catch(() => caches.match(req).then(hit => hit || caches.match(SHELL)))
  )
})
