// 移动端「服务器地址」：归一化 + 扫码结果解析 + 登录页的地址行接线。
//
// 覆盖三件容易出错的事：
//   ① 手输地址的各种形态（IP:端口 / 带 scheme / 带路径）归一化成同一个 origin；
//   ② 扫到的二维码区分「桌面端配对码」（带 pair 的一次性码）与「普通地址」——
//      前者扫码即登录，后者只是换服务器；
//   ③ 登录页上的地址行：显示当前服务器、扫码图标、地址改了会跳到新服务器。
import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { normalizeAddr, parseScan, addrLabel, addrToOrigin } from '../src/mobile/server.js'
import MobileLogin from '../src/mobile/MobileLogin.vue'

const apiMock = vi.hoisted(() => vi.fn())
vi.mock('../src/api.js', () => ({ api: apiMock }))
vi.mock('../src/store.js', () => ({ store: { loggedIn: false, username: '' }, toast: vi.fn() }))

describe('移动端服务器地址', () => {
  it('归一化各种手输形态：缺 scheme 补 https、缺端口补 45679', () => {
    expect(normalizeAddr('192.168.10.242:45679')).toEqual({
      ok: true, origin: 'https://192.168.10.242:45679', host: '192.168.10.242', port: '45679',
    })
    expect(normalizeAddr('https://192.168.10.242:45679').origin).toBe('https://192.168.10.242:45679')
    // 缺端口 → 补局域网门默认端口
    expect(normalizeAddr('192.168.10.242').origin).toBe('https://192.168.10.242:45679')
    // 带路径的完整链接也能吃
    expect(normalizeAddr('https://1.2.3.4:45679/mobile').origin).toBe('https://1.2.3.4:45679')
  })

  it('非法输入判失败：空、非 http(s)、垃圾串', () => {
    expect(normalizeAddr('').ok).toBe(false)
    expect(normalizeAddr('ftp://x/y').ok).toBe(false)
    expect(normalizeAddr('::::').ok).toBe(false)
  })

  it('没写 scheme 时沿用当前页面的协议（http 调试口不该被改成 https）', () => {
    // 本机调试跑 http://localhost:45678，手输同址不该被判成「换了服务器」
    expect(normalizeAddr('127.0.0.1:45678', 'http:').origin).toBe('http://127.0.0.1:45678')
    expect(normalizeAddr('127.0.0.1:45678', 'https:').origin).toBe('https://127.0.0.1:45678')
    // 写了 scheme 就以写的为准
    expect(normalizeAddr('http://127.0.0.1:45678', 'https:').origin).toBe('http://127.0.0.1:45678')
  })

  it('扫到桌面端配对码：认出一���性码与指纹（扫码即登录）', () => {
    const p = parseScan('https://192.168.10.242:45679/mobile?pair=ABC23456&fp=' + 'a'.repeat(64))
    expect(p.ok).toBe(true)
    expect(p.kind).toBe('pair')
    expect(p.origin).toBe('https://192.168.10.242:45679')
    expect(p.pair).toBe('ABC23456')
    expect(p.fp).toBe('a'.repeat(64))
    expect(p.url).toContain('pair=ABC23456')
  })

  it('扫到纯地址：当作普通服务器地址，不是一次性配对码', () => {
    expect(parseScan('192.168.10.242:45679').kind).toBe('addr')
    expect(parseScan('192.168.10.242:45679').origin).toBe('https://192.168.10.242:45679')
    // 有 path 但没有 pair → 只是地址
    expect(parseScan('https://192.168.10.242:45679/mobile').kind).toBe('addr')
  })

  it('空/垃圾扫码内容判失败', () => {
    expect(parseScan('').ok).toBe(false)
    expect(parseScan('not a url at all').ok).toBe(false)
  })

  it('addrToOrigin：用户填什么端口就是什么端口，不替他补 45679', () => {
    // 配了域名的机器走反代，端口由用户自己决定，我们不添
    expect(addrToOrigin('friday.home.arpa', 'https:')).toBe('https://friday.home.arpa')
    expect(addrToOrigin('friday.home.arpa:8443', 'https:')).toBe('https://friday.home.arpa:8443')
    expect(addrToOrigin('192.168.10.242', 'https:')).toBe('https://192.168.10.242')
    // 缺 scheme 时按当前页面的协议补一个
    expect(addrToOrigin('127.0.0.1:45678', 'http:')).toBe('http://127.0.0.1:45678')
    expect(addrToOrigin('127.0.0.1:45678', 'https:')).toBe('https://127.0.0.1:45678')
    // 写了 scheme 就以写的为准；非法输入返回 null
    expect(addrToOrigin('http://127.0.0.1:45678', 'https:')).toBe('http://127.0.0.1:45678')
    expect(addrToOrigin('')).toBe(null)
    expect(addrToOrigin('ftp://x/y')).toBe(null)
    expect(addrToOrigin('::::')).toBe(null)
  })

  it('addrLabel：origin 显示成 host:port', () => {
    expect(addrLabel('https://192.168.10.242:45679')).toBe('192.168.10.242:45679')
  })
})

describe('移动端登录页的服务器地址行', () => {
  beforeEach(() => {
    apiMock.mockReset()
    apiMock.mockImplementation(async (method, path) => {
      if (method === 'POST' && path === '/api/login') return { username: 'aaron' }
      if (path === '/api/setup/status') return { accountRequired: false }
      throw new Error('unexpected ' + method + ' ' + path)
    })
  })

  it('登录表单是三个带标签的字段：服务器地址 / 用户名 / 密码', async () => {
    const w = mount(MobileLogin, { props: { setupStatus: { accountRequired: false } } })
    await flushPromises()
    // 三个字段各有可见标签（不是只靠 placeholder）
    const labels = w.findAll('.m-field-label').map(l => l.text())
    expect(labels).toEqual(['服务器地址', '用户名', '密码'])
    // 地址输入框末尾跟着扫码图标
    expect(w.find('.m-input-row .m-input').exists()).toBe(true)
    expect(w.find('.m-input-act').exists()).toBe(true)
    // 账号密码输入框
    expect(w.find('#m-auth-user').exists()).toBe(true)
    expect(w.find('#m-auth-pass').exists()).toBe(true)
    w.unmount()
  })

  it('地址输入框默认显示当前服务器，扫码图标可点', async () => {
    const w = mount(MobileLogin, { props: { setupStatus: { accountRequired: false } } })
    await flushPromises()
    const addr = w.find('#m-srv-addr')
    expect(addr.exists()).toBe(true)
    expect(addr.element.value).toBe(location.host)
    // 扫码按钮触发扫码页
    await w.find('.m-input-act').trigger('click')
    expect(w.find('.m-scan').exists()).toBe(true)
    w.unmount()
  })

  it('首装未建号时给指引卡，不摆表单（还没到连哪台电脑那一步）', async () => {
    const w = mount(MobileLogin, { props: { setupStatus: { accountRequired: true } } })
    await flushPromises()
    expect(w.find('.m-auth-setup').exists()).toBe(true)
    expect(w.find('#m-auth-user').exists()).toBe(false)
    w.unmount()
  })
})
