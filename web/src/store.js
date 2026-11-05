// 全局状态：登录态 / 当前视图 / 使用模式 / toast / 弹窗。轻量 reactive，无需 pinia。
import { reactive } from 'vue'
import { getPref, setPref } from './prefs.js'

// 使用模式存在本地（跟主题一个通道）：这是"我现在要日常对话还是要干活"的视图
// 选择，不改变 agent 的能力，也不同步给后端——后端只按请求里的 session 归属会话。
const MODE_KEY = 'friday-mode'
export const MODE_LABELS = [
  { id: 'life', label: '生活', hint: '生活模式：一条主会话，提醒、待办、闲聊都接在同一个上下文里' },
  { id: 'work', label: '工作', hint: '工作模式：聊天可以开多条会话（各一份上下文），智能体工作区也在这里' },
]

export const store = reactive({
  loggedIn: false,
  username: '',
  view: 'chat',
  mode: getPref(MODE_KEY) === 'work' ? 'work' : 'life',
  settingsOpen: false,
  settingsSection: '',  // 下次打开设置时定位的 section id，一次性：SettingsModal 读走即清零
  setupStatus: null,
  unread: 0,
  traceFilter: '',   // 调用轨迹的会话过滤（如 goal-<id> / plan-<id>，从自动化页跳转带入）
  acpOpenThread: '', // 运行看板 → 工作区：要打开哪条 ACP 会话（消费后清零）
  // 应用内全屏（专注对话）：隐藏左侧全局导航、页面吃满窗口，Esc 由我们自己处理。
  // 不持久化——每次启动都从普通布局开始，免得忘了自己处在哪种模式。
  fullscreen: false,
})

// setMode 切使用模式并记住它。无效值原样返回 false，不把状态写脏。
export function setMode(mode) {
  if (mode !== 'life' && mode !== 'work') return false
  store.mode = mode
  setPref(MODE_KEY, mode)
  return true
}

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
