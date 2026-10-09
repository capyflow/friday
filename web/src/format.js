// 展示格式化工具（与原单文件版一致）。
export function fmtDT(s) { return s ? s.replace('T', ' ').substring(5, 16) : '—' }

export function fmtClock(s) {
  const d = new Date(s)
  const p = n => (n < 10 ? '0' : '') + n
  return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds())
}

export function fmtNum(n) { return Number(n || 0).toLocaleString('en-US') }

export function prettyJSON(s) {
  try { return JSON.stringify(JSON.parse(s), null, 2) } catch (_) { return s }
}

export function upStr(sec) {
  return sec >= 86400 ? Math.floor(sec / 86400) + '天' + Math.floor(sec % 86400 / 3600) + '小时'
    : sec >= 3600 ? Math.floor(sec / 3600) + '小时' + Math.floor(sec % 3600 / 60) + '分'
    : Math.floor(sec / 60) + '分' + sec % 60 + '秒'
}

export const kindName = { once: '单次', repeat: '多次', cron: '循环', agent: '智能' }
export const srcName = { user: '用户', agent: '主动', admin: '后台', migrate: '迁移' }
export const EV_TITLE = { llm_in: 'LLM 输入', llm_out: 'LLM 输出', tool_req: '请求工具', tool: '工具结果', answer: '回答', error: '错误', task: '任务' }
