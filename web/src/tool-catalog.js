// 工具调用的展示口径：时间线收起态就该说清「这是哪个工具」，不用点开才知道。
// 只作用于 Friday 自己 agent 的工具调用（ChatView 那条链路，kind=tool）：这些名字是我们
// 自己的，定义在 internal/tools/*.go 的 Name()。ACP agent 的工具名是外部进程给的（codex 的
// Bash、opencode 的 read…），我们既改不动也猜不准它下一步叫什么，所以那条链路一律照直显示
// 原始名 + 默认扳手（TimelineBlocks 的 ownTools 决定走不走这份目录）。
// 图标是 24 网格的描边路径（与全站 14px 描边图标同族），圆用两段圆弧写进 path。

export const DEFAULT_ICON = 'wrench'

const ICONS = {
  wrench: ['M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z'],
  file: ['M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z', 'M14 2v6h6'],
  // 新建与改写分两个图标：文档里带个加号是「写一份新的」，铅笔是「改现有的」
  'file-plus': ['M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z', 'M14 2v6h6', 'M12 12v6', 'M9 15h6'],
  pen: ['M12 20h9', 'M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4z'],
  folder: ['M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z'],
  search: ['M4 11a7 7 0 1 0 14 0a7 7 0 1 0 -14 0', 'm21 21-4.3-4.3'],
  globe: ['M4 12a8 8 0 1 0 16 0a8 8 0 1 0 -16 0', 'M3.6 9h16.8', 'M3.6 15h16.8', 'M12 4a12 12 0 0 1 0 16a12 12 0 0 1 0 -16'],
  terminal: ['m5 8 4 4-4 4', 'M13 16h6'],
  check: ['M9 11l3 3L22 4', 'M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11'],
  note: ['M4 19.5A2.5 2.5 0 0 1 6.5 17H20', 'M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z'],
  clock: ['M6 12a6 6 0 1 0 12 0a6 6 0 1 0 -12 0', 'M12 8.5v3.5l2.5 2'],
  weather: ['M17.5 19a4.5 4.5 0 0 0 .5-9 6 6 0 0 0-11.6 1.5A3.5 3.5 0 0 0 6.5 19z'],
  memory: ['M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3z', 'M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6', 'M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3'],
  plug: ['M9 2v6', 'M15 2v6', 'M6 8h12v3a6 6 0 0 1-12 0z', 'M12 17v5'],
  speaker: ['M11 5 6 9H3v6h3l5 4z', 'M15.5 8.5a5 5 0 0 1 0 7', 'M18.5 5.5a9 9 0 0 1 0 13'],
}

// 名字一律按小写匹配：Friday 用 snake_case，ACP 各家有 Bash / read / Read 混着写的。
// 导出只为让用例能扫一遍目录里有没有改名后没跟上的条目。
export const TOOL_LABELS = {
  // 文件与代码（键一律是 internal 里 Name() 返回的那个名字）
  read_file: ['读取文件', 'file'],
  write_file: ['写入文件', 'file-plus'],
  edit_file: ['修改文件', 'pen'],
  list_files: ['查看目录', 'folder'],
  // 命令
  exec_command: ['执行命令', 'terminal'],
  // 待办 / 笔记 / 任务
  add_todo: ['新建待办', 'check'],
  list_todos: ['查待办', 'check'],
  update_todo: ['改待办', 'check'],
  complete_todo: ['完成待办', 'check'],
  delete_todo: ['删除待办', 'check'],
  add_note: ['记一笔', 'note'],
  list_notes: ['查笔记', 'note'],
  update_note: ['改笔记', 'note'],
  delete_note: ['删笔记', 'note'],
  list_tasks: ['查任务', 'clock'],
  update_task: ['改任务', 'clock'],
  schedule_task: ['安排任务', 'clock'],
  pause_task: ['暂停任务', 'clock'],
  resume_task: ['继续任务', 'clock'],
  cancel_task: ['取消任务', 'clock'],
  // 网络与时间
  web_search: ['联网搜索', 'search'],
  web_fetch: ['抓取网页', 'globe'],
  get_weather: ['查天气', 'weather'],
  get_time: ['看时间', 'clock'],
  // 记忆与外部服务
  remember: ['记住', 'memory'],
  recall: ['回忆', 'search'],
  forget: ['忘记', 'memory'],
  pin_entry: ['钉住记忆', 'memory'],
  mcp_edit: ['配置 MCP', 'plug'],
  // 声音与展示
  play_audio: ['播放音频', 'speaker'],
  play_effect: ['播放音效', 'speaker'],
  speak_now: ['说一句话', 'speaker'],
  display_now: ['显示内容', 'speaker'],
}

// 自己 agent 挂的 MCP 工具：后端命名是 mcp_<服务器>_<工具>（internal/mcp/manager.go 的
// ToolPrefix），至少说清是哪台服务器在动。服务器名本身带下划线时只能截到第一段，
// 完整工具名在展开区里。
const MCP_NAME = /^mcp_([^_]+)(?:_.+)?$/i

export function toolIcon(key) {
  return ICONS[key] || ICONS[DEFAULT_ICON]
}

// toolDisplay 给一行工具调用的展示文案与图标。own=false（外部 agent）时不查目录，
// 照直显示工具名；name 缺失时退回「工具调用」，绝不空着。
export function toolDisplay(name, own = false) {
  const raw = (name || '').trim()
  if (!raw) return { label: '工具调用', icon: toolIcon(DEFAULT_ICON) }
  if (!own) return { label: raw, icon: toolIcon(DEFAULT_ICON) }
  const hit = TOOL_LABELS[raw.toLowerCase()]
  if (hit) return { label: hit[0], icon: toolIcon(hit[1]) }
  const mcp = MCP_NAME.exec(raw)
  if (mcp) return { label: `MCP · ${mcp[1]}`, icon: toolIcon('plug') }
  return { label: raw, icon: toolIcon(DEFAULT_ICON) }
}
