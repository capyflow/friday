// 服务器地址的解析与归一化：登录页的「服务器地址」输入框、扫码结果、以及
// 「当前连的是哪台电脑」三处共用一套规则，避免各写各的。
//
// 三种输入都要吃：
//   ① IP:端口             —— 手输（例 192.168.10.242:45679）
//   ② https://IP:端口      —— 手输带 scheme
//   ③ 完整链接            —— 桌面端「手机连接」二维码里的
//      https://IP:端口/mobile?pair=一次性码&fp=证书指纹
//
// 局域网门走自签 CA，所以默认补 https://；端口缺省补 admin.tls 的默认 45679。

// 局域网门默认端口（config.yaml 的 admin.tls.port 默认值）。
export const DEFAULT_PORT = '45679'

// 归一化地址输入 → { ok, origin, host, port }；只认 http/https，其余判非法。
//
// schemeHint：用户没写 scheme 时用哪个协议。当前页面是 http（本机调试走
// http://localhost:45678）就沿用 http，否则默认 https（局域网门走自签 CA 的 https）。
// 写错了 scheme 会导致「地址没改却提示要切换」或跳到打不开的协议上。
export function normalizeAddr(raw, schemeHint) {
  const s = String(raw || '').trim()
  if (!s) return { ok: false, reason: 'empty' }
  const hint = schemeHint === 'http:' ? 'http:' : 'https:'
  let u
  try {
    u = new URL(s.includes('://') ? s : hint + '//' + s)
  } catch {
    return { ok: false, reason: 'invalid' }
  }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') return { ok: false, reason: 'scheme' }
  if (!u.hostname) return { ok: false, reason: 'invalid' }
  const port = u.port || (u.protocol === 'https:' ? DEFAULT_PORT : '80')
  return { ok: true, origin: u.protocol + '//' + u.hostname + ':' + port, host: u.hostname, port }
}

// 解析扫码结果：桌面端配对码（kind='pair'）与普通地址（kind='addr'）。
// 配对码自带一次性 pair 码与证书指纹——扫它等于「换电脑 + 直接登录」。
export function parseScan(text, schemeHint) {
  const s = String(text || '').trim()
  if (!s) return { ok: false, reason: 'empty' }
  if (s.includes('://')) {
    let u
    try {
      u = new URL(s)
    } catch {
      return { ok: false, reason: 'invalid' }
    }
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return { ok: false, reason: 'scheme' }
    if (!u.hostname) return { ok: false, reason: 'invalid' }
    const port = u.port || (u.protocol === 'https:' ? DEFAULT_PORT : '80')
    const origin = u.protocol + '//' + u.hostname + ':' + port
    const pair = u.searchParams.get('pair') || ''
    const fp = u.searchParams.get('fp') || ''
    // 桌面端二维码的路径是 /mobile；带 pair 才是配对码，其它链接按地址算。
    const isPair = /\/mobile(\/|$)/.test(u.pathname) && !!pair
    return { ok: true, kind: isPair ? 'pair' : 'addr', origin, host: u.hostname, port, pair, fp, url: s }
  }
  const n = normalizeAddr(s, schemeHint)
  if (!n.ok) return n
  return { ok: true, kind: 'addr', origin: n.origin, host: n.host, port: n.port }
}

// 用户手输的地址 → origin。**不补端口**：用户自己配了域名就按域名的默认端口
// 走（反代一般已经把 45679 转进去了），没配就自己把端口写上——端口由用户决定，
// 我们不替他添。缺 scheme 时按当前页面的协议补一个。
export function addrToOrigin(raw, schemeHint) {
  const s = String(raw || '').trim()
  if (!s) return null
  const hint = schemeHint === 'http:' ? 'http:' : 'https:'
  try {
    const u = new URL(s.includes('://') ? s : hint + '//' + s)
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return null
    if (!u.hostname) return null
    return u.origin // origin 原样保留用户写的端口；没写端口就是没有
  } catch {
    return null
  }
}

// origin → 给用户看的 host:port（输入框里显示的形态）。
export function addrLabel(origin) {
  const m = /^[a-z]+:\/\/([^/]+)/i.exec(String(origin || ''))
  return m ? m[1] : String(origin || '')
}
