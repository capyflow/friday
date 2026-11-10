package acp

// tool_adapter.go 把 Friday 的 vxagent.Tool 适配成 MCP 形态：门面不重复实现
// 任何工具逻辑，只做名字白名单过滤与 schema/参数的形状转换（外部 agent 的
// tools/call ──▶ Registry.Call）。方向与 internal/mcp/manager.go 的包装器
// （外部 MCP 工具 ──▶ vxagent.Registry）正好相反。

import (
	vxagent "github.com/capyflow/vortexagent/agent"
)

// facadeTool 是白名单内单个工具的 MCP tools/list 条目。
type facadeTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// facadeToolsFromRegistry 返回 MCP 工具清单（按工具名字典序）。allowed 为空
// 表示不过滤——注册表里的全部工具都给出（第一版口径）。reg 为 nil（启动早期
// 注册表未就绪）返回 nil——外部 agent 下轮重建会话时会拿到完整清单。
// schema 为空的工具兜底成空 object，避免 schema 缺失让对方 agent 的校验直接
// 拒掉整个 server。
func facadeToolsFromRegistry(reg *vxagent.Registry, allowed []string) []facadeTool {
	if reg == nil {
		return nil
	}
	allow := map[string]bool{}
	for _, n := range allowed {
		allow[n] = true
	}
	out := make([]facadeTool, 0, len(allowed))
	for _, name := range reg.Names() { // Names 已按字典序排序，输出顺序确定
		if len(allow) > 0 && !allow[name] {
			continue
		}
		t, ok := reg.Get(name)
		if !ok {
			continue
		}
		schema := t.Schema()
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, facadeTool{
			Name:        name,
			Description: t.Description(),
			InputSchema: schema,
		})
	}
	return out
}

// resolveFacadeTool 返回白名单内的工具实例（白名单为空 = 全部工具）；注册表
// 没有该名字时返回 false（调用方翻译成「未知工具」的 JSON-RPC error）。
func resolveFacadeTool(reg *vxagent.Registry, allowed []string, name string) (vxagent.Tool, bool) {
	if reg == nil {
		return nil, false
	}
	if len(allowed) > 0 {
		found := false
		for _, n := range allowed {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return reg.Get(name)
}
