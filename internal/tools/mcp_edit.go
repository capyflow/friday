package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/mcp"
	"github.com/capyflow/friday/internal/trace"
)

// mcpEditMu 串行化本工具对 mcp.servers 的"读取-修改-写回"（管理员在网页同时
// 编辑时以先完成者为准，与 admin 侧的 mcpUpdateMu 各自独立，最终一致性可接受）。
var mcpEditMu sync.Mutex

// McpEditTool 让 agent 在对话中对 MCP 服务器做增删改查：写回 config.yaml 并经
// Manager.Apply 热生效（新增拉起 / 移除停止 / 变更重启，工具清单变化自动重建
// Agent），下一轮对话即可直接使用新工具，无需重启服务。
// 变更类操作（add/update/remove）在 confirm 审批模式下先经网页批准
// （同 write_file 的内部审批模式）；查询类（list/get）与 restart 直行。
type McpEditTool struct {
	Manager    *mcp.Manager    // MCP 服务器管理器（nil 时工具返回未启用错误）
	Config     *config.Config  // 运行配置（读写 mcp.servers 快照）
	ConfigPath string          // config.yaml 路径（写回）
	Approver   *approval.Queue // confirm 模式下变更操作入网页审批（nil 时跳过审批）
}

func (t *McpEditTool) Name() string { return "mcp_edit" }

func (t *McpEditTool) Description() string {
	return "管理 MCP 服务器（增删改查）：接入新的外部工具/数据源、查看已接入的服务器与连接状态、" +
		"修改或移除已有配置。本地命令服务器用 command/args/env，远程服务器用 url/headers（鉴权放 headers）。" +
		"变更保存后立即热生效，无需重启；confirm 审批模式下变更需用户在网页批准。"
}

func (t *McpEditTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{"list", "get", "add", "update", "remove", "restart"},
				"description": "list=列出全部（含连接状态）；get=单台详情与工具清单；add=新增；" +
					"update=修改（只传要改的字段，其余保留）；remove=移除；restart=重连",
			},
			"name":    map[string]any{"type": "string", "description": "服务器名（get/update/remove/restart 必填；决定工具名前缀 mcp_<name>_）"},
			"command": map[string]any{"type": "string", "description": "本地 stdio 服务器的启动命令（如 npx），与 url 二选一"},
			"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "command 的启动参数，如 [\"-y\",\"@modelcontextprotocol/server-filesystem\",\"/tmp\"]"},
			"env":     map[string]any{"type": "object", "description": "command 的额外环境变量，如 {\"API_KEY\":\"sk-...\"}"},
			"url":     map[string]any{"type": "string", "description": "远程服务器的 Streamable HTTP 地址（http:// 或 https:// 开头）"},
			"headers": map[string]any{"type": "object", "description": "url 的请求头（鉴权等），如 {\"Authorization\":\"Bearer xxx\"}"},
		},
		"required": []string{"action"},
	}
}

func (t *McpEditTool) Call(ctx context.Context, args map[string]any) (string, error) {
	if t == nil || t.Manager == nil || t.Config == nil || t.ConfigPath == "" {
		return "", fmt.Errorf("MCP 管理未启用")
	}
	action, _ := args["action"].(string)
	switch action {
	case "list":
		return t.list()
	case "get":
		return t.get(stringArg(args, "name"))
	case "add":
		return t.add(ctx, args)
	case "update":
		return t.update(ctx, args)
	case "remove":
		return t.remove(ctx, stringArg(args, "name"))
	case "restart":
		return t.restart(ctx, stringArg(args, "name"))
	default:
		return "", fmt.Errorf("未知 action %q（可选 list/get/add/update/remove/restart）", action)
	}
}

// ---------- 查 ----------

// list 返回全部服务器概要（配置 + 连接状态合并视图，JSON）。
func (t *McpEditTool) list() (string, error) {
	servers := t.Config.Snapshot().Mcp.Servers
	status := map[string]mcp.Status{}
	for _, st := range t.Manager.StatusList() {
		status[st.Name] = st
	}
	type row struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		Target    string `json:"target"`
		Running   bool   `json:"running"`
		Tools     int    `json:"tools"`
		Restarts  int    `json:"restarts"`
		LastError string `json:"lastError,omitempty"`
	}
	out := make([]row, 0, len(servers))
	for _, sv := range servers {
		r := row{Name: sv.Name, Target: sv.URL}
		if sv.URL == "" {
			r.Type = "stdio"
			r.Target = strings.TrimSpace(sv.Command + " " + strings.Join(sv.Args, " "))
		} else {
			r.Type = "http"
		}
		if st, ok := status[sv.Name]; ok {
			r.Running, r.Tools, r.Restarts, r.LastError = st.Running, st.Tools, st.Restarts, st.LastError
		}
		out = append(out, r)
	}
	b, _ := json.Marshal(map[string]any{"servers": out})
	return string(b), nil
}

// get 返回单台服务器的完整配置、连接状态与已注册的工具名清单。
func (t *McpEditTool) get(name string) (string, error) {
	sv, err := t.findServer(name)
	if err != nil {
		return "", err
	}
	tools := t.Manager.ServerTools(sv.Name)
	registered := make([]string, 0, len(tools))
	for _, tl := range tools {
		registered = append(registered, mcp.ToolPrefix(sv.Name)+tl.Name)
	}
	b, _ := json.Marshal(map[string]any{
		"server": sv, "status": statusOf(t.Manager, sv.Name), "tools": registered,
	})
	return string(b), nil
}

// ---------- 增 / 改 / 删 ----------

// add 新增一台服务器：校验、查重后写回配置并热生效。
func (t *McpEditTool) add(ctx context.Context, args map[string]any) (string, error) {
	mcpEditMu.Lock()
	defer mcpEditMu.Unlock()
	sv, err := serverFromArgs(args, nil)
	if err != nil {
		return "", err
	}
	servers := t.Config.Snapshot().Mcp.Servers
	for _, old := range servers {
		if old.Name == sv.Name {
			return "", fmt.Errorf("服务器 %s 已存在（可用 action=update 修改，或先 remove 再 add）", sv.Name)
		}
	}
	detail := "新增 MCP 服务器配置:\n" + logx.Truncate(serverJSON(sv), 800)
	if err := requestMcpApproval(ctx, t.Approver, "新增", sv.Name, detail); err != nil {
		return "", err
	}
	return t.commit(ctx, append(servers, *sv), sv.Name, fmt.Sprintf("MCP 服务器 %s 已接入并热生效", sv.Name))
}

// update 修改一台服务器：只覆盖 args 中出现的字段，其余保留原值；
// command 与 url 互斥切换（提供 url 即转为远程模式，反之亦然）。
func (t *McpEditTool) update(ctx context.Context, args map[string]any) (string, error) {
	mcpEditMu.Lock()
	defer mcpEditMu.Unlock()
	name := stringArg(args, "name")
	servers := t.Config.Snapshot().Mcp.Servers
	idx := -1
	for i := range servers {
		if servers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("未找到服务器 %s（可用 action=list 查看已有名称）", name)
	}
	sv, err := serverFromArgs(args, &servers[idx])
	if err != nil {
		return "", err
	}
	sv.Name = name // 名称即主键，不允许改名
	if sameServerDef(servers[idx], *sv) {
		return fmt.Sprintf("服务器 %s 配置未变化，无需修改", name), nil
	}
	detail := fmt.Sprintf("修改前:\n%s\n修改后:\n%s", logx.Truncate(serverJSON(&servers[idx]), 400),
		logx.Truncate(serverJSON(sv), 400))
	if err := requestMcpApproval(ctx, t.Approver, "修改", name, detail); err != nil {
		return "", err
	}
	servers[idx] = *sv
	return t.commit(ctx, servers, name, fmt.Sprintf("MCP 服务器 %s 已更新并热生效（定义变更自动重连）", name))
}

// remove 移除一台服务器：写回配置并热生效（连接停止，其工具从注册表消失）。
func (t *McpEditTool) remove(ctx context.Context, name string) (string, error) {
	mcpEditMu.Lock()
	defer mcpEditMu.Unlock()
	if _, err := t.findServer(name); err != nil {
		return "", err
	}
	servers := t.Config.Snapshot().Mcp.Servers
	kept := servers[:0:0]
	for _, sv := range servers {
		if sv.Name != name {
			kept = append(kept, sv)
		}
	}
	if err := requestMcpApproval(ctx, t.Approver, "移除", name,
		"移除 MCP 服务器 "+name+"（连接将停止，工具 mcp_"+name+"_* 不再可用）"); err != nil {
		return "", err
	}
	return t.commit(ctx, kept, name, fmt.Sprintf("MCP 服务器 %s 已移除并热生效（工具不再可用）", name))
}

// restart 手动重连一台服务器（不改动配置，无需审批）。
func (t *McpEditTool) restart(ctx context.Context, name string) (string, error) {
	if _, err := t.findServer(name); err != nil {
		return "", err
	}
	if err := t.Manager.Restart(name); err != nil {
		return "", fmt.Errorf("重连失败: %w（可用 action=get 查看状态）", err)
	}
	return t.resultWithTools(name, fmt.Sprintf("MCP 服务器 %s 已重连", name)), nil
}

// commit 把服务器列表写回 config.yaml 并经 Manager.Apply 热生效（target 是本次
// 变更的目标服务器，用于在返回消息中附上其工具清单）。Apply 结束后的 onChange
// 回调会重建 Agent 注册表，新工具下一轮对话即可见。
// 连接失败不回滚配置（错误保留在服务器状态里，修复后 restart/update 即可）。
func (t *McpEditTool) commit(ctx context.Context, servers []config.McpServerConfig, target, okMsg string) (string, error) {
	if err := t.Config.Apply(func(next *config.Config) error {
		next.Mcp.Servers = servers
		return config.UpdateMcpServers(t.ConfigPath, servers)
	}); err != nil {
		return "", fmt.Errorf("保存配置失败: %w", err)
	}
	live := make([]mcp.ServerConfig, 0, len(servers))
	for _, sv := range servers {
		live = append(live, mcp.ServerConfig{
			Name: sv.Name, Command: sv.Command, Args: sv.Args, Env: sv.Env,
			URL: sv.URL, Headers: sv.Headers,
		})
	}
	var extra string
	if err := t.Manager.Apply(ctx, live); err != nil {
		extra = "；部分服务器连接失败：" + err.Error() + "（配置已保存，修复后可 restart 或 update）"
		trace.Record(ctx, "mcp", "MCP 配置已保存但连接失败: %v", err)
	}
	return t.resultWithTools(target, okMsg) + extra + "。下一轮对话即可使用（无需重启）。", nil
}

// resultWithTools 在消息后附上某服务器当前注册的工具名清单（未连接则提示状态可查）。
func (t *McpEditTool) resultWithTools(name, msg string) string {
	out := msg
	if tools := t.Manager.ServerTools(name); len(tools) > 0 {
		names := make([]string, 0, len(tools))
		for _, tl := range tools {
			names = append(names, mcp.ToolPrefix(name)+tl.Name)
		}
		out += fmt.Sprintf("，注册工具 %d 个：%s", len(names), strings.Join(names, "、"))
	}
	return out
}

// ---------- 解析与校验 ----------

// serverFromArgs 从工具参数构造服务器定义：base 非 nil 时为 update 语义——
// 只覆盖参数中出现的字段；否则为 add 语义——command/url 必须二选一。
func serverFromArgs(args map[string]any, base *config.McpServerConfig) (*config.McpServerConfig, error) {
	sv := config.McpServerConfig{}
	if base != nil {
		sv = *base
		sv.Args = append([]string(nil), base.Args...)
		sv.Env = cloneStrMap(base.Env)
		sv.Headers = cloneStrMap(base.Headers)
	}
	name := strings.TrimSpace(stringArg(args, "name"))
	if name == "" {
		return nil, fmt.Errorf("缺少参数 name（服务器名，决定工具名前缀 mcp_<name>_）")
	}
	sv.Name = name

	cmd, hasCmd := stringOpt(args, "command")
	url, hasURL := stringOpt(args, "url")
	if hasCmd && hasURL {
		return nil, fmt.Errorf("command（本地命令）与 url（远程地址）只能提供其一")
	}
	switch {
	case hasURL:
		// 显式提供 url：转为远程模式，丢弃本地命令字段。
		url = strings.TrimSpace(url)
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("url 须以 http:// 或 https:// 开头，当前 %q", url)
		}
		sv.URL = url
		sv.Command, sv.Args, sv.Env = "", nil, nil
		if hdr, err := strMapArg(args, "headers"); err != nil {
			return nil, err
		} else if hdr != nil {
			sv.Headers = hdr
		} else if base == nil {
			sv.Headers = nil
		}
	case hasCmd:
		// 显式提供 command：转为本地模式，丢弃远程字段。
		sv.Command, sv.URL, sv.Headers = strings.TrimSpace(cmd), "", nil
		if arr, err := strSliceArg(args, "args"); err != nil {
			return nil, err
		} else if arr != nil {
			sv.Args = arr
		}
		if m, err := strMapArg(args, "env"); err != nil {
			return nil, err
		} else if m != nil {
			sv.Env = m
		}
	case base == nil:
		return nil, fmt.Errorf("command（本地命令，如 npx）与 url（远程地址）必须二选一")
	}
	// update 未切换模式时也允许单独调整当前模式的附属字段。
	if base != nil && !hasCmd && !hasURL {
		if sv.URL == "" {
			if arr, err := strSliceArg(args, "args"); err != nil {
				return nil, err
			} else if arr != nil {
				sv.Args = arr
			}
			if m, err := strMapArg(args, "env"); err != nil {
				return nil, err
			} else if m != nil {
				sv.Env = m
			}
		} else {
			if hdr, err := strMapArg(args, "headers"); err != nil {
				return nil, err
			} else if hdr != nil {
				sv.Headers = hdr
			}
		}
	}
	if sv.URL != "" && !strings.HasPrefix(sv.URL, "http://") && !strings.HasPrefix(sv.URL, "https://") {
		return nil, fmt.Errorf("url 须以 http:// 或 https:// 开头，当前 %q", sv.URL)
	}
	return &sv, nil
}

// findServer 从当前配置快照中按名取服务器定义。
func (t *McpEditTool) findServer(name string) (config.McpServerConfig, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return config.McpServerConfig{}, fmt.Errorf("缺少参数 name")
	}
	for _, sv := range t.Config.Snapshot().Mcp.Servers {
		if sv.Name == name {
			return sv, nil
		}
	}
	return config.McpServerConfig{}, fmt.Errorf("未找到服务器 %s（可用 action=list 查看已有名称）", name)
}

// sameServerDef 判断两份服务器定义是否等价（与 Manager.sameServer 同口径）。
func sameServerDef(a, b config.McpServerConfig) bool {
	return a.Command == b.Command && a.URL == b.URL &&
		strings.Join(a.Args, "\x00") == strings.Join(b.Args, "\x00") &&
		fmt.Sprint(a.Env) == fmt.Sprint(b.Env) &&
		fmt.Sprint(a.Headers) == fmt.Sprint(b.Headers)
}

// requestMcpApproval 在 confirm 模式（且未开启编辑免审批）下为 MCP 变更发起
// 网页审批；auto/off 模式与 acceptEdits/fullAccess 权限模式直接放行。
func requestMcpApproval(ctx context.Context, q *approval.Queue, action, name, detail string) error {
	if q == nil || !q.InternalApprovalRequired() {
		return nil
	}
	summary := "mcp_edit " + action + " 服务器 " + name
	trace.Record(ctx, "approval", "%s，等待网页审批（超时 %s）", summary, q.Timeout())
	if err := q.Request("mcp_edit", summary, detail); err != nil {
		trace.Record(ctx, "approval", "%s 未获批准：%v", summary, err)
		return err
	}
	trace.Record(ctx, "approval", "%s 已获批准，执行", summary)
	return nil
}

// serverJSON 序列化服务器定义（审批卡片与轨迹展示用）。
func serverJSON(sv *config.McpServerConfig) string {
	b, err := json.Marshal(sv)
	if err != nil {
		return fmt.Sprintf("%+v", *sv)
	}
	return string(b)
}

// statusOf 取某服务器的连接状态（缺失时给占位）。
func statusOf(m *mcp.Manager, name string) mcp.Status {
	for _, st := range m.StatusList() {
		if st.Name == name {
			return st
		}
	}
	return mcp.Status{Name: name}
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// stringOpt 区分"未提供"（ok=false）与"提供了空串"。
func stringOpt(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// strSliceArg 把参数中的字符串数组转换为 []string；未提供返回 nil，
// 提供了非数组或含非字符串元素时报错。
func strSliceArg(args map[string]any, key string) ([]string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("参数 %s 须为字符串数组", key)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("参数 %s 须为字符串数组", key)
		}
		out = append(out, s)
	}
	return out, nil
}

// strMapArg 把参数中的字符串映射转换为 map[string]string；未提供返回 nil，
// 提供了非对象或含非字符串值时报错。
func strMapArg(args map[string]any, key string) (map[string]string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("参数 %s 须为字符串到字符串的映射", key)
	}
	out := make(map[string]string, len(m))
	for k, item := range m {
		s, ok := item.(string)
		if !ok {
			b, _ := json.Marshal(item)
			return nil, fmt.Errorf("参数 %s.%s 须为字符串，实际 %s", key, k, string(b))
		}
		out[k] = s
	}
	return out, nil
}

func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
