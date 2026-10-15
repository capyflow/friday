// 全局状态：登录态 / 当前视图 / toast / 弹窗。轻量 reactive，无需 pinia。
import { reactive } from 'vue'

export const store = reactive({
  loggedIn: false,
  username: '',
  view: 'chat',
  settingsOpen: false,
  settingsSection: '',  // 下次打开设置时定位的 section id，一次性：SettingsModal 读走即清零
  setupStatus: null,
  unread: 0,
  traceFilter: '',   // 调用轨迹的会话过滤（如 goal-<id> / plan-<id>，从自动化页跳转带入）
})

export const toastState = reactive({ text: '', err: false, show: false })
let toastTimer
export function toast(text, isErr) {
  toastState.text = text
  toastState.err = !!isErr
  toastState.show = true
  clearTimeout(toastTimer)
  toastTimer = setTimeout(() => { toastState.show = false }, 2200)
}

export const modalState = reactive({ show: false, text: '' })
export function showModal(text) { modalState.text = text; modalState.show = true }
export function hideModal() { modalState.show = false }
