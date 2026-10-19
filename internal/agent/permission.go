// permission.go 把 exec 审批策略翻译成 vortexagent 全局权限层（PermissionChecker）
// 规则，并把框架的 ApprovalUI 审批问询桥接到管理后台的审批队列（approval.Queue）：
//
//   - 模式映射：permissions.mode（全局权限模式）优先——fullAccess→框架
//     full_access（全部放行，deny 黑名单仍拦截）；其余按 tools.exec.mode
//     auto→whitelist（仅规则命中可执行）、confirm→confirm（规则命中直行，
//     其余进网页审批）、off→confirm（此时 exec_command 不注册，其余工具经
//     直通规则放行，模式无实际作用）。acceptEdits 不改 Checker（exec 照旧），
//     只经审批队列的编辑免审标记放行 write/edit/mcp_edit 的内部审批。
//   - allow 规则：安全工具直通名单（裸工具名）+ exec 白名单（exec_command:<条目>，
//     由框架命令匹配器做 shell 语义段级校验）+ MCP 策略 + permissions.allow。
//   - deny 规则：毁灭性命令黑名单（exec_command:<pattern>，任何模式下命中即拒）
//   - permissions.deny。
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	vxagent "github.com/capyflow/vortexagent/agent"
	vxexec "github.com/capyflow/vortexagent/tools/exec"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
)

// execToolName 是框架 exec 工具在 Registry 中的名字（权限规则以它引用）。
const execToolName = vxexec.ToolName

// passThroughTools 是与危险操作审批无关的工具：权限层直接放行，保持历史行为
// （安全工具免审批直行；write_file/edit_file/mcp_edit 的变更操作审批仍由各自
// 内部走 approval.Queue，保留 auto 直执 / confirm 入队与操作详情语义）。
// 新增工具后必须同步此名单（guard 测试 TestRegisteredToolsArePassThrough 兜底），
// 否则该工具的调用会落入 confirm 审批流程，网页会频繁弹审批卡片。
var passThroughTools = []string{
	"schedule_task", "list_tasks", "cancel_task", "update_task", "pause_task", "resume_task",
	"add_todo", "list_todos", "complete_todo", "delete_todo", "update_todo",
	"add_note", "list_notes", "update_note", "delete_note", "pin_entry",
	"get_weather",
	"remember", "recall", "forget", "get_time",
	"speak_now", "display_now", "play_audio", "play_effect",
	"read_file", "list_files", "write_file", "edit_file",
	"mcp_edit",
}

// NewPermissionChecker 按配置组装框架权限 Checker。规则格式错误返回错误，
// 调用方应视为致命（Build 快失败），不带残缺策略运行。
//
// 模式决策（permissions.mode 全局权限模式优先，参照 zcode 多档权限）：
//   - fullAccess → 框架 full_access：全部放行（deny 黑名单仍强制拦截）；
//   - 其余按 exec 模式映射：auto → whitelist（仅规则命中可执行）、
//     confirm/off → confirm（off 时 exec_command 不注册，其余工具经直通规则放行）。
func NewPermissionChecker(cfg *config.Config, q *approval.Queue) (*vxagent.Checker, error) {
	mode := vxagent.ModeConfirm
	switch {
	case cfg.Permissions.BypassesAll():
		mode = vxagent.ModeFullAccess
	case cfg.Tools.Exec.Mode == string(approval.ModeAuto):
		mode = vxagent.ModeWhitelist
	}

	allow := append([]string{}, passThroughTools...)
	if cfg.Tools.Exec.Mode != string(approval.ModeOff) {
		for _, entry := range cfg.Tools.Exec.Allowlist {
			allow = append(allow, execToolName+":"+entry)
		}
	}
	if cfg.Permissions.MCP != "confirm" { // 默认 allow：MCP 工具免审批（历史行为）
		allow = append(allow, "mcp_*")
	}
	allow = append(allow, cfg.Permissions.Allow...)

	var deny []string
	for _, p := range approval.DenyPatterns() {
		deny = append(deny, execToolName+":"+p)
	}
	deny = append(deny, cfg.Permissions.Deny...)

	c, err := vxagent.NewChecker(vxagent.PermissionConfig{
		Mode:  mode,
		Allow: allow,
		Deny:  deny,
	}, approvalUI(q))
	if err != nil {
		return nil, err
	}
	// 显式接线 exec_command 的命令匹配器：让规则中的参数模式按 shell 语义生效。
	// 与 vxagent.New 的自动收集（collectPermissionMatchers）幂等——注册的是同一对
	// 匹配器；显式注册使 Checker 不依赖 Agent 的构建顺序即可独立工作。
	c.RegisterToolMatcher(execToolName, vxexec.CommandMatcher, vxexec.CommandDenyMatcher)
	return c, nil
}

// approvalUI 把框架的审批问询桥接到审批队列：confirm 模式下未放行的工具调用
// 进入管理后台审批卡片，批准后执行（remember 表示用户选择了"批准并记住"）。
// 其余模式不会走到这里：whitelist 未命中直接拒绝，不询问。
func approvalUI(q *approval.Queue) vxagent.ApprovalUI {
	return func(ctx context.Context, info vxagent.CallInfo) vxagent.Decision {
		summary, detail := describeCall(info)
		trace.Record(ctx, "approval", "%s 未放行，进入审批队列（超时 %s）", info.Tool, q.Timeout())
		trace.Record(ctx, "approval", "待审批：%s", summary)
		remember, err := q.RequestRemember(info.Tool, summary, detail)
		if err != nil {
			trace.Record(ctx, "approval", "%s 未获批准：%v", info.Tool, err)
			return vxagent.Decision{Allowed: false}
		}
		if remember {
			trace.Record(ctx, "approval", "%s 已批准并记住（本进程内相同参数免审批）", info.Tool)
		} else {
			trace.Record(ctx, "approval", "%s 已批准，执行", info.Tool)
		}
		return vxagent.Decision{Allowed: true, Remember: remember}
	}
}

// describeCall 生成审批卡片的摘要与详情：exec_command 展示命令全文，
// 其余工具展示参数 JSON（截断防撑爆卡片）。
func describeCall(info vxagent.CallInfo) (summary, detail string) {
	if info.Tool == execToolName {
		cmd, _ := info.Args["command"].(string)
		return "执行命令：" + logx.Truncate(cmd, 120), "命令：" + cmd
	}
	b, err := json.Marshal(info.Args)
	if err != nil {
		return info.Tool, fmt.Sprintf("%v", info.Args)
	}
	return info.Tool, logx.Truncate(string(b), 800)
}
