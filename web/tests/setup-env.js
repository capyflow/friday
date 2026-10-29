// Node 22+ 暴露了实验性 localStorage 全局（未配 --localstorage-file 时不可用，
// 读取直接 undefined 并告警），会遮住 jsdom 环境注入的同名实现，所有依赖
// localStorage 的用例全灭。delete 掉 Node 的属性不可行（不可配置），这里直接
// 用 jsdom 的真 Storage 定义回去。
import { JSDOM } from 'jsdom'

if (typeof localStorage === 'undefined') {
  // 必须给 http origin：jsdom 对 opaque origin（默认 about:blank）禁用 localStorage。
  Object.defineProperty(globalThis, 'localStorage', {
    value: new JSDOM('', { url: 'http://localhost/' }).window.localStorage,
    configurable: true,
  })
}
