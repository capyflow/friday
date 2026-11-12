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

// jsdom 不实现 scrollIntoView（只滚动、不返回任何东西），但键盘导航的下拉框会调它。
// 补一个空实现，免得一个与断言无关的 TypeError 把整个用例文件判红。
if (window.Element && !window.Element.prototype.scrollIntoView) {
  window.Element.prototype.scrollIntoView = function scrollIntoView() {}
}

// ---------- EventSource mock（事件驱动架构的前端测试底座） ----------
// 改造后前端不再从 POST /api/chat 的响应体里读帧，而是订阅 /api/events/stream
// 的一条 SSE 长连接（见 src/events.js）。jsdom 没实现 EventSource，Node 自带的
// 那个也不接受我们的推送，所以这里补一个可控的假实现：
//   - new EventSource(url) 时登记到全局列表，测试按创建顺序取实例；
//   - 实例暴露 onopen / onmessage / onerror 与 push(obj)：push 把对象 JSON 化后
//     当作一帧 SSE data 交给 onmessage，等价于服务端广播了一条事件；
//   - close() 真的标记关闭（前端 stop() 会调它，断了的连接不该再收到帧）。
class MockEventSource {
  constructor(url) {
    this.url = url
    this.onopen = null
    this.onmessage = null
    this.onerror = null
    this.closed = false
    // readyState 对齐真实 EventSource：CONNECTING=0 OPEN=1 CLOSED=2。
    // 前端靠它判断「连接还活着吗」——活着就不该重开一条（会占满连接池）。
    this.readyState = 1
    this.seq = 0            // 事件帧自带 seq（服务端递增），这里自动补
    MockEventSource.instances.push(this)
  }

  // push 推一帧事件。seq 默认自增；显式给了就用给的（测缺口时要手动跳号）。
  push(obj) {
    if (this.closed) return
    const frame = { seq: this.seq + 1, ...obj }
    this.seq = frame.seq
    if (this.onmessage) this.onmessage({ data: JSON.stringify(frame) })
  }

  close() {
    this.closed = true
    this.readyState = 2
  }
}
MockEventSource.instances = []

globalThis.EventSource = MockEventSource
// 测试取连接用：__esAll() 全部连接、__esLast() 最近一条、__esReset() 清空列表。
globalThis.__esAll = () => MockEventSource.instances
globalThis.__esLast = () => MockEventSource.instances[MockEventSource.instances.length - 1]
globalThis.__esReset = () => { MockEventSource.instances.length = 0 }
