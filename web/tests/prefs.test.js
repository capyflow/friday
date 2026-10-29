import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// prefs.js 的 hydrate 是模块级 memoized，用例各自 resetModules 拿全新实例。
async function freshPrefs({ native = null } = {}) {
  vi.resetModules()
  localStorage.clear()
  const calls = []
  const invoke = vi.fn(async (cmd, args) => {
    calls.push({ cmd, args })
    if (cmd === 'prefs_get') return native
    return null
  })
  vi.stubGlobal('__TAURI__', { core: { invoke } })
  const mod = await import('../src/prefs.js')
  return { mod, calls, invoke }
}

beforeEach(() => { localStorage.clear() })
afterEach(() => { vi.unstubAllGlobals() })

describe('偏好原生冷备（桌面壳）', () => {
  it('本地为空时从原生快照补齐缺失项', async () => {
    const { mod, calls } = await freshPrefs({
      native: { 'friday-theme': 'light', 'friday-notifications-enabled': '1' },
    })
    await mod.hydratePrefs()
    expect(mod.getPref('friday-theme')).toBe('light')
    expect(mod.getPref('friday-notifications-enabled')).toBe('1')
    expect(calls.map(c => c.cmd)).toEqual(['prefs_get'])
  })

  it('本地已有值时以本地为准，不被原生快照覆盖', async () => {
    const { mod } = await freshPrefs({ native: { 'friday-theme': 'light' } })
    localStorage.setItem('friday-theme', 'dark')
    await mod.hydratePrefs()
    expect(mod.getPref('friday-theme')).toBe('dark')
  })

  it('原生快照里的 token 与非 friday- 键一律忽略', async () => {
    const { mod } = await freshPrefs({
      native: { 'friday.authToken': 'leak.jwt.tok', other: 'x' },
    })
    await mod.hydratePrefs()
    expect(mod.getPref('friday.authToken')).toBe(null)
    expect(mod.getPref('other')).toBe(null)
  })

  it('setPref 写穿原生层：快照包含偏好、排除 token', async () => {
    const { mod, calls } = await freshPrefs()
    localStorage.setItem('friday.authToken', 'secret.jwt.tok')
    mod.setPref('friday-theme', 'dark')
    await Promise.resolve()
    await Promise.resolve()
    const save = calls.find(c => c.cmd === 'prefs_save')
    expect(save).toBeTruthy()
    expect(save.args.prefs).toEqual({ 'friday-theme': 'dark' })
    expect(mod.getPref('friday-theme')).toBe('dark')
  })

  it('removePref 清本地并同步落盘', async () => {
    const { mod, calls } = await freshPrefs()
    mod.setPref('friday-theme', 'light')
    await Promise.resolve()
    await Promise.resolve()
    mod.removePref('friday-theme')
    await Promise.resolve()
    await Promise.resolve()
    expect(mod.getPref('friday-theme')).toBe(null)
    const saves = calls.filter(c => c.cmd === 'prefs_save')
    expect(saves[saves.length - 1].args.prefs).toEqual({})
  })

  it('同一轮事件循环内的多次写合并成一次落盘', async () => {
    const { mod, calls } = await freshPrefs()
    mod.setPref('friday-theme', 'dark')
    mod.setPref('friday-notifications-enabled', '1')
    await Promise.resolve()
    await Promise.resolve()
    expect(calls.filter(c => c.cmd === 'prefs_save')).toHaveLength(1)
  })
})

describe('偏好浏览器降级（无 __TAURI__）', () => {
  it('只写 localStorage，不触发原生调用', async () => {
    vi.resetModules()
    localStorage.clear()
    vi.stubGlobal('__TAURI__', undefined)
    const mod = await import('../src/prefs.js')
    await expect(mod.hydratePrefs()).resolves.toBe(false)
    mod.setPref('friday-theme', 'light')
    expect(mod.getPref('friday-theme')).toBe('light')
    mod.removePref('friday-theme')
    expect(mod.getPref('friday-theme')).toBe(null)
  })
})
