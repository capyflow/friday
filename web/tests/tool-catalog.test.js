import { readFileSync, readdirSync, statSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import { DEFAULT_ICON, TOOL_LABELS, toolDisplay, toolIcon } from '../src/tool-catalog.js'

// 工具行收起态要直接说清是哪个工具：认识的给中文名与专属图标，不认识的原样显示工具名。
// 名称来源有两头——Friday 自己的（internal/**/*.go）与 ACP / MCP 那些我们改不动的——
// 所以目录放前端，但 Friday 那一半要有对账，免得工具改名后这里悄悄失配。

const INTERNAL = path.resolve(process.cwd(), '../internal')
function goFiles(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const p = path.join(dir, name)
    if (statSync(p).isDirectory()) goFiles(p, out)
    else if (name.endsWith('.go')) out.push(p)
  }
  return out
}
const goSource = goFiles(INTERNAL).map(f => readFileSync(f, 'utf8')).join('\n')

describe('工具展示目录', () => {
  it('自己 agent 的工具：认识的名给中文名与它自己的图标', () => {
    expect(toolDisplay('delete_todo', true)).toEqual({ label: '删除待办', icon: toolIcon('check') })
    expect(toolDisplay('read_file', true).label).toBe('读取文件')
    expect(toolDisplay('web_search', true).label).toBe('联网搜索')
    expect(toolDisplay('get_time', true).label).toBe('看时间')
    // 大小写不敏感（MCP 服务器给的工具名可能是驼峰）
    expect(toolDisplay('MCP_Notion_Search', true).label).toBe('MCP · Notion')
  })

  // 外部 agent 的工具名改不动也猜不准：照直显示 + 扳手，别拿我们的字典去套它的名字。
  it('外部 agent（ACP）的工具：不查目录，原样显示名字配扳手', () => {
    expect(toolDisplay('Bash', false).label).toBe('Bash')
    expect(toolDisplay('Bash', false).icon).toEqual(toolIcon(DEFAULT_ICON))
    // 名字撞上了也不给中文名：codex 自己也有 read_file，那不是我们的 read_file
    expect(toolDisplay('read_file', false).label).toBe('read_file')
    expect(toolDisplay('mcp_notion_search_pages', false).label).toBe('mcp_notion_search_pages')
  })

  it('不认识的照直显示工具名，前导退回默认扳手，不空着', () => {
    const d = toolDisplay('some_agent_tool', true)
    expect(d.label).toBe('some_agent_tool')
    expect(d.icon).toEqual(toolIcon(DEFAULT_ICON))
    // 名字没给也不能出现空行
    expect(toolDisplay('', true).label).toBe('工具调用')
    expect(toolDisplay(undefined, false).label).toBe('工具调用')
  })

  // 后端命名口径：internal/mcp/manager.go 的 ToolPrefix = "mcp_" + 服务器 + "_"
  it('自己 agent 挂的 MCP 工具说清是哪台服务器在动', () => {
    expect(toolDisplay('mcp_notion_search_pages', true).label).toBe('MCP · notion')
    expect(toolDisplay('mcp_notion_search_pages', true).icon).toEqual(toolIcon('plug'))
    // 我们自己的配置工具优先按目录走，不被 MCP 前缀规则吃掉
    expect(toolDisplay('mcp_edit', true).label).toBe('配置 MCP')
  })

  it('图标都是 24 网格的描边路径，每个条目都指向存在的图标', () => {
    for (const [name, [label, icon]] of Object.entries(TOOL_LABELS)) {
      expect(label, `${name} 缺展示名`).toBeTruthy()
      const paths = toolIcon(icon)
      expect(paths.length, `${name} 的图标 ${icon} 没有路径`).toBeGreaterThan(0)
      for (const d of paths) expect(d, `${name} 的图标路径为空`).toMatch(/^[A-Za-z]/)
    }
  })

  // 目录只收 Friday 自己的工具，所以每个键都该在 Go 源码里出现。工具改名而这里没跟上，
  // 表现是那一行悄悄退回原始名——没人会当回事，故在此对账。
  it('目录里的工具名都是源码里真有的工具', () => {
    const stale = Object.keys(TOOL_LABELS).filter(k => !goSource.includes(`"${k}"`))
    expect(stale).toEqual([])
  })
})
