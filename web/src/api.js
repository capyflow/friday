// 与 Go 后端的 HTTP 约定：所有接口返回 { body: ... } 包装；401 表示会话过期。
import { toast, store } from './store.js'

export async function api(method, url, body) {
  const headers = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  let res
  try {
    res = await fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  } catch (e) {
    throw new Error('无法连接服务器，请确认小五服务正在运行')
  }
  let data = null
  try { data = await res.json() } catch (e) { /* 非 JSON 响应 */ }
  const serverMsg = data && data.body && data.body.error
  if (res.status === 401) {
    if (store.loggedIn) {
      toast('登录已过期，请重新登录', true)
      store.loggedIn = false
    }
    throw new Error(serverMsg || '未登录或会话已过期')
  }
  if (!res.ok) throw new Error(serverMsg || ('请求失败（HTTP ' + res.status + '）'))
  return data.body
}

// 轨迹接口历史原因不带 { body } 包装之外的错误结构，直接取 body。
export async function traceAPI(path) {
  const res = await fetch(path)
  if (res.status === 401) {
    if (store.loggedIn) {
      toast('登录已过期，请重新登录', true)
      store.loggedIn = false
    }
    throw new Error('未登录')
  }
  return (await res.json()).body
}
