import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import FilePreviewDrawer from '../src/components/FilePreviewDrawer.vue'
import { api } from '../src/api.js'

vi.mock('../src/api.js', () => ({ api: vi.fn() }))

const ZIP = '/Users/aaron/build/photos-release.zip'

let wrapper

beforeEach(() => {
  api.mockReset()
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = null
})

function mockPreview(status) {
  api.mockImplementation(async (method, url) => {
    if (method === 'GET' && url.startsWith('/api/files/preview')) {
      return { path: ZIP, name: 'photos-release.zip', size: 287000000, kind: 'binary',
               message: '二进制文件，暂不支持预览', canOpen: status.canOpen }
    }
    if (method === 'POST' && url === '/api/files/open') return { message: '已用系统默认应用打开 photos-release.zip' }
    return {}
  })
}

async function openDrawer() {
  wrapper = mount(FilePreviewDrawer)
  await wrapper.vm.open(ZIP)
  await flushPromises()
  return wrapper
}

describe('文件预览抽屉 · 预览不了的类型', () => {
  it('本机访问：给「用默认应用打开」与「下载到本地」两条出路', async () => {
    mockPreview({ canOpen: true })
    const w = await openDrawer()
    const btn = w.findAll('button').find(b => b.text().includes('用默认应用打开'))
    expect(btn).toBeTruthy()
    const link = w.find('a[download]')
    expect(link.text()).toContain('下载到本地')
    expect(link.attributes('href')).toContain(encodeURIComponent(ZIP))
  })

  it('点「用默认应用打开」把路径交给后端', async () => {
    mockPreview({ canOpen: true })
    const w = await openDrawer()
    const btn = w.findAll('button').find(b => b.text().includes('用默认应用打开'))
    await btn.trigger('click')
    await flushPromises()
    const call = api.mock.calls.find(([method, url]) => method === 'POST' && url === '/api/files/open')
    expect(call).toBeTruthy()
    expect(call[2]).toEqual({ path: ZIP })
  })

  it('远程访问：不给「用默认应用打开」，只留下载（服务器上的文件对远端没用）', async () => {
    mockPreview({ canOpen: false })
    const w = await openDrawer()
    expect(w.findAll('button').find(b => b.text().includes('用默认应用打开'))).toBeFalsy()
    expect(w.find('a[download]').text()).toContain('下载到本地')
    expect(w.text()).toContain('下载到本地后用系统默认应用打开')
  })
})
