// Android 壳注入的原生桥（window.FridayNative）。壳能做三件网页做不到的事：
// 记住服务端地址、切换服务端（含证书指纹信任）、调起原生扫码页。
// 浏览器直访（无壳）时这些能力不存在——调用方一律先问 hasNative()，
// 再决定走原生还是走浏览器的降级路径（整页跳转）。

// 壳里 @JavascriptInterface 的方法名（见 android/.../MainActivity.java）。
const BRIDGE = 'FridayNative'

export function hasNative() {
  try {
    return !!window[BRIDGE] && typeof window[BRIDGE].isApp === 'function' && window[BRIDGE].isApp()
  } catch {
    return false
  }
}

// 调壳的方法：壳不存在或调用异常时返回 null（不抛——网页侧统一降级）。
export function nativeCall(method, ...args) {
  try {
    const fn = window[BRIDGE] && window[BRIDGE][method]
    if (typeof fn !== 'function') return null
    return fn.apply(window[BRIDGE], args)
  } catch {
    return null
  }
}

// 连到某个服务器：APP 里交给壳换 origin 重载（证书信任与地址记忆都在壳侧，
// 配对链接的指纹随码 pin 也在壳侧做）；浏览器里没有壳，整页跳过去。
export function connectTo(url) {
  if (hasNative()) {
    return nativeCall('connectTo', url) !== null
  }
  try {
    location.assign(url)
    return true
  } catch {
    return false
  }
}

// 当前页面所在的 origin：APP 里是壳记着的地址，浏览器里是页面自身的。
export function currentOrigin() {
  if (hasNative()) {
    const v = nativeServerAddress()
    if (v) return v
  }
  return location.origin
}

// 当前连的是哪台电脑（origin）。壳里是壳记着的地址，浏览器里是当前页面地址。
export function nativeServerAddress() {
  const v = nativeCall('getServerAddress')
  return typeof v === 'string' ? v : ''
}

// 唤起壳的原生扫码页。壳扫完把结果通过 window.__fridayScanCallback 交回网页。
export function nativeScan(onResult) {
  if (!hasNative()) return false
  window.__fridayScanCallback = text => {
    window.__fridayScanCallback = null
    onResult(String(text || ''))
  }
  return nativeCall('scanQrCode') !== null
}
