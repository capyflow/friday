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
	"sync"

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
	"web_search", "web_fetch",
	"remember", "recall", "forget", "get_time",
	"speak_now", "display_now", "play_audio", "play_effect",
	"read_file", "list_files", "write_file", "edit_file", "edit_files",
	"move_file", "delete_file",
	"search_code", "find_files",
	"git_status", "git_diff", "git_log",
	"read_skill",
	"delegate_task",
	"mcp_edit",
}

// checkerMode 把「全局权限模式 + tools.exec 策略」翻成框架权限层的执行模式。
// 单独抽出来是因为切换权限模式不再重建 Agent（重建要等本轮对话的读锁，对话中
// 点切换会把请求挂住），只对活着的 Checker 做一次 SetMode——两处必须同一口径。
func checkerMode(cfg *config.Config) vxagent.Mode {
	switch {
	case cfg.Permissions.BypassesAll():
		return vxagent.ModeFullAccess
	case cfg.Tools.Exec.Mode == string(approval.ModeAuto):
		return vxagent.ModeWhitelist
	default: // confirm / off：off 时 exec_command 不注册，模式无实际作用
		return vxagent.ModeConfirm
	}
}

// liveCheckers 记下每条审批队列当前生效的权限 Checker（每次 Build 都新建一个，
// 后建的覆盖先建的）。切换权限模式靠它 SetMode，不重建整个 Agent。
var liveCheckers sync.Map // *approval.Queue → *vxagent.Checker

// ApplyPermissionModeNow 把 cfg 当前的权限模式落到活 Checker 上，返回是否落成了
// （没经过 NewPermissionChecker 建过 Checker 时返回 false，调用方退回重建 Agent）。
//
// Checker 的锁贯穿整次判定（含网页审批问询），所以 SetMode 放后台做一次：有审批
// 卡片正等人答时，新档位在那张卡答完之后跟上，而不是把这个 HTTP 请求挂在那儿。
// 配置快照与编辑免审标记在调用方已同步改好，界面读到的是新值。
func ApplyPermissionModeNow(cfg *config.Config, q *approval.Queue) bool {
	v, ok := liveCheckers.Load(q)
	if !ok {
		return false
	}
	m := checkerMode(cfg.Snapshot())
	go v.(*vxagent.Checker).SetMode(m)
	return true
}

// NewPermissionChecker 按配置组装框架权限 Checker。规则格式错误返回错误，
// 调用方应视为致命（Build 快失败），不带残缺策略运行。
//
// 模式决策（permissions.mode 全局权限模式优先，参照 zcode 多档权限）：
//   - fullAccess → 框架 full_access：全部放行（deny 黑名单仍强制拦截）；
//   - 其余按 exec 模式映射：auto → whitelist（仅规则命中可执行）、
//     confirm/off → confirm（off 时 exec_command 不注册，其余工具经直通规则放行）。
func NewPermissionChecker(cfg *config.Config, q *approval.Queue) (*vxagent.Checker, error) {
	mode := checkerMode(cfg)

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
	if q != nil {
		liveCheckers.Store(q, c) // 记下「这条队列当前用的 Checker」，切档靠它
	}
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
		remember, err := q.RequestRememberCtx(ctx, info.Tool, summary, detail)
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

// ToolGuard 返回一个"按当前权限层判定一次工具调用"的函数，供 MCP 工具门面在
// 执行外部 agent 的 tools/call 前调用。
//
// 为什么要显式判定：权限 Checker 挂在框架的 Agent.execTool 上（vortexagent
// agent/ask.go），而门面是直接调 Tool.Call 的——不判定就等于绕过 deny 黑名单
// （毁灭性命令）与 exec 审批。门面里的调用没有 Brain 的上下文，所以这里按
// 队列当前生效的 Checker 走同一套规则；confirm 档会同步等网页审批，门面自身
// 的 12s 超时到点即取消审批请求（不会在 agent 放弃后继续执行）。
func ToolGuard(q *approval.Queue) func(context.Context, string, map[string]any) (bool, string) {
	return func(ctx context.Context, tool string, args map[string]any) (bool, string) {
		v, ok := liveCheckers.Load(q)
		if !ok { // 还没建过 Checker（Brain 未就绪）：不额外拦，保持旧行为
			return true, ""
		}
		verdict, reason := v.(*vxagent.Checker).Check(ctx, vxagent.CallInfo{Tool: tool, Args: args})
		return verdict == vxagent.VerdictAllow, reason
	}
}
