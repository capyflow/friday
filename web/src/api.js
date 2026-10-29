// 与 Go 后端的 HTTP 约定：所有接口返回 { body: ... } 包装；403（body.auth=true）
// 表示 token 缺失/过期/被吊销，前端清掉本地 token 并回登录页。
import { toast, store } from './store.js'
import { authHeaders, setToken } from './auth.js'

// authFailed 统一处理登录失效：清 token、回登录页，并在已登录状态下提示一次。
export function authFailed() {
  setToken('')
  if (store.loggedIn) {
    store.loggedIn = false
    toast('登录已过期，请重新登录', true)
  }
}

// isAuthFailure 判断响应是否登录失效（服务端用 auth 标记与其它 403 区分）。
export function isAuthFailure(res, data) {
  return res.status === 403 && (!data || !data.body || data.body.auth === true)
}

export async function api(method, url, body) {
  const headers = authHeaders()
  let payload
  if (body instanceof FormData) {
    payload = body // 上传：浏览器自动生成 multipart 头，不能手工设 Content-Type
  } else if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  let res
  try {
    res = await fetch(url, { method, headers, body: payload })
  } catch (e) {
    throw new Error('无法连接服务器，请确认小五服务正在运行')
  }
  let data = null
  try { data = await res.json() } catch (e) { /* 非 JSON 响应 */ }
  const serverMsg = data && data.body && data.body.error
  if (isAuthFailure(res, data)) {
    authFailed()
    throw new Error(serverMsg || '登录已过期，请重新登录')
  }
  if (!res.ok) {
    const err = new Error(serverMsg || ('请求失败（HTTP ' + res.status + '）'))
    // 安装类接口会另带命令输出：挂到 error 上，调用方决定是否展开显示。
    if (data && data.body && data.body.output) err.output = data.body.output
    throw err
  }
  // 登录/首装接口直接下发 token：统一在这里落盘，各页面不用各自处理。
  if (data && data.body && data.body.token) setToken(data.body.token)
  return data.body
}

// 轨迹接口历史原因不带 { body } 包装之外的错误结构，直接取 body。
export async function traceAPI(path) {
  const res = await fetch(path, { headers: authHeaders() })
  if (res.status === 403) {
    authFailed()
    throw new Error('未登录或登录已过期')
  }
  return (await res.json()).body
}
