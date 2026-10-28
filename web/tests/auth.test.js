import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, authFailed, traceAPI } from '../src/api.js'
import { authHeaders, getToken, mediaURL, setToken } from '../src/auth.js'
import { store, toastState } from '../src/store.js'

function json(status, body) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

beforeEach(() => {
  store.loggedIn = false
  store.username = ''
  toastState.text = ''
  toastState.show = false
  localStorage.clear()
})
afterEach(() => { vi.unstubAllGlobals() })

describe('登录凭据本地保存', () => {
  it('登录/首装响应里的 token 自动落盘，之后请求带 Bearer 头', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(json(200, { body: { username: 'owner', token: 'jwt-1' } }))
      .mockResolvedValueOnce(json(200, { body: { ok: true } }))
    vi.stubGlobal('fetch', fetchMock)

    await api('POST', '/api/login', { username: 'owner', password: 'secret' })
    expect(getToken()).toBe('jwt-1')

    await api('GET', '/api/status')
    const headers = fetchMock.mock.calls[1][1].headers
    expect(headers.Authorization).toBe('Bearer jwt-1')
  })

  it('退出或失效后清空 token', () => {
    setToken('jwt-2')
    authFailed()
    expect(getToken()).toBe('')
    expect(authHeaders().Authorization).toBeUndefined()
  })
})

describe('403 登录失效处理', () => {
  it('带 auth 标记的 403 清 token、回登录页并提示', async () => {
    setToken('expired')
    store.loggedIn = true
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(403, {
      body: { error: '登录已失效，请重新登录', auth: true },
    })))
    await expect(api('GET', '/api/status')).rejects.toThrow('登录已失效')
    expect(getToken()).toBe('')
    expect(store.loggedIn).toBe(false)
    expect(toastState.text).toContain('登录已过期')
  })

  it('不带 auth 标记的 403（如远程首装拒绝）不当登录失效', async () => {
    setToken('still-valid')
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(403, {
      body: { error: '首次设置仅允许本机访问' },
    })))
    await expect(api('POST', '/api/setup', {})).rejects.toThrow('仅允许本机')
    expect(getToken()).toBe('still-valid')
  })

  it('轨迹接口 403 同样清 token 并抛错', async () => {
    setToken('expired')
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(403, { body: {} })))
    await expect(traceAPI('/api/traces')).rejects.toThrow('登录')
    expect(getToken()).toBe('')
  })
})

describe('图片地址携带 token', () => {
  it('只给 /api/upload/ 追加 query token，blob 等地址保持原样', () => {
    setToken('jwt-3')
    expect(mediaURL('/api/upload/abc.png')).toBe('/api/upload/abc.png?token=jwt-3')
    expect(mediaURL('/api/upload/abc.png?x=1')).toBe('/api/upload/abc.png?x=1&token=jwt-3')
    expect(mediaURL('blob:http://localhost/xyz')).toBe('blob:http://localhost/xyz')
  })

  it('未登录时不动地址', () => {
    expect(mediaURL('/api/upload/abc.png')).toBe('/api/upload/abc.png')
  })
})

// 桌面壳的原生冷备：hydrateToken 是模块级 memoized，用例各自 resetModules
// 拿全新实例，避免上一次回填的 Promise 串味。
describe('桌面原生冷备回填（__TAURI__）', () => {
  async function freshAuth({ native = '', local = '' } = {}) {
    vi.resetModules()
    localStorage.clear()
    if (local) localStorage.setItem('friday.authToken', local)
    const calls = []
    const invoke = vi.fn(async (cmd, args) => {
      calls.push({ cmd, args })
      if (cmd === 'auth_get_token') return native || null
      return null
    })
    vi.stubGlobal('__TAURI__', { core: { invoke } })
    const mod = await import('../src/auth.js')
    return { mod, calls, invoke }
  }

  it('原生有 token、本地为空：回填到 localStorage', async () => {
    const { mod, calls } = await freshAuth({ native: 'native.jwt.tok' })
    await mod.hydrateToken()
    expect(mod.getToken()).toBe('native.jwt.tok')
    expect(calls.map(c => c.cmd)).toEqual(['auth_get_token'])
  })

  it('原生为空、本地有 token：升级首启把本地推给原生层', async () => {
    const { mod, calls } = await freshAuth({ local: 'local.jwt.tok' })
    await mod.hydrateToken()
    expect(mod.getToken()).toBe('local.jwt.tok')
    expect(calls).toContainEqual({ cmd: 'auth_set_token', args: { token: 'local.jwt.tok' } })
  })

  it('两边不一致：以原生为准覆盖 localStorage', async () => {
    const { mod } = await freshAuth({ native: 'native.jwt.new', local: 'local.jwt.old' })
    await mod.hydrateToken()
    expect(mod.getToken()).toBe('native.jwt.new')
  })

  it('回填 memoized：多次调用只打一次 IPC', async () => {
    const { mod, invoke } = await freshAuth({ native: 'native.jwt.tok' })
    await Promise.all([mod.hydrateToken(), mod.hydrateToken()])
    await mod.hydrateToken()
    expect(invoke.mock.calls.filter(c => c[0] === 'auth_get_token')).toHaveLength(1)
  })

  it('setToken 写穿原生层：登录落盘、退出清盘', async () => {
    const { mod, calls } = await freshAuth()
    mod.setToken('new.jwt.tok')
    expect(calls).toContainEqual({ cmd: 'auth_set_token', args: { token: 'new.jwt.tok' } })
    mod.setToken('')
    expect(calls).toContainEqual({ cmd: 'auth_clear_token', args: undefined })
    expect(mod.getToken()).toBe('')
  })

  it('浏览器直访（无 __TAURI__）：回填与写穿全部降级为 no-op', async () => {
    vi.resetModules()
    localStorage.clear()
    vi.stubGlobal('__TAURI__', undefined)
    const mod = await import('../src/auth.js')
    await expect(mod.hydrateToken()).resolves.toBeUndefined()
    mod.setToken('plain.jwt.tok')
    expect(mod.getToken()).toBe('plain.jwt.tok')
  })
})
