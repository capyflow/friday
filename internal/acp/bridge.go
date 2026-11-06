// bridge.go 实现 acp.Client 的 9 个稳定方法，是 SDK 回调与 Friday 内部的转接层。
//
// 线程纪律（实测硬约束，见 docs/ACP-AGENTS.md C6）：
//   - SessionUpdate 在 SDK 的单 goroutine 串行通知队列上被调用，**只允许入队**，
//     任何阻塞都会冻住该连接上所有 session 的事件流并拖死 turn——因此这里只
//     向带缓冲的 channel 投递，缓冲满即丢弃并计数（消费端是聚合器，工作量极小，
//     丢弃只可能发生在 turn 已结束后的残留场景）。
//   - RequestPermission 走 SDK 的每请求独立 goroutine（connection.go 对入站请求
//     逐个 go handleInbound），**允许阻塞**等人点审批卡——这正是 D7 的承载通道。
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"

	acp "github.com/coder/acp-go-sdk"
)

// eventBuffer 是 SessionUpdate 的入队缓冲。一轮实测 raw update 153 条，
// 聚合器消费极快；4096 足以覆盖最坏情况并留出余量。
const eventBuffer = 4096

// permissionHook 处理一次权限请求（阻塞直至用户决定或超时）。
// 返回给 agent 的响应由实现方构造。
type permissionHook func(ctx context.Context, r acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)

// elicitationHook 处理 elicitation/create（MiniMax 的 ask_user 走这里）：
// 表单字段被翻译成问答卡，答复按 requestedSchema 的形状回填。
type elicitationHook func(ctx context.Context, form acp.UnstableCreateElicitationForm) (acp.UnstableCreateElicitationResponse, error)

// extensionHook 处理 agent 发来的扩展方法（目前是 CodeBuddy 的
// `_codebuddy.ai/question`）。返回原样序列化给 agent。
type extensionHook func(ctx context.Context, method string, params json.RawMessage) (any, error)

// bridge 是一条 agent 连接的回调实现。
type bridge struct {
	events  chan acp.SessionNotification
	dropped atomic.Int64 // 因缓冲满被丢弃的 update 条数（日志哨兵）

	onPermission  permissionHook  // 为 nil 时一律答复 cancelled
	onElicitation elicitationHook // 为 nil 时一律答复 cancelled
	onExtension   extensionHook   // 为 nil 时未知方法一律 method not found
}

func newBridge(onPermission permissionHook) *bridge {
	return &bridge{events: make(chan acp.SessionNotification, eventBuffer), onPermission: onPermission}
}

// SessionUpdate 只入队（C6 红线）。
func (b *bridge) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	select {
	case b.events <- n:
	default:
		b.dropped.Add(1)
	}
	return nil
}

// RequestPermission 转交审批钩子（可长时间阻塞——独立 goroutine，不占通知队列）。
func (b *bridge) RequestPermission(ctx context.Context, r acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	if b.onPermission == nil {
		return cancelledPermission(), nil
	}
	return b.onPermission(ctx, r)
}

// UnstableCreateElicitation 转交表单提问钩子（同样可长时间阻塞）。
// 没有钩子或不是 form 形态时回 cancelled，agent 会把它当成"用户没答"。
func (b *bridge) UnstableCreateElicitation(ctx context.Context, r acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	if r.Form == nil || b.onElicitation == nil {
		return cancelledElicitation(), nil
	}
	return b.onElicitation(ctx, *r.Form)
}

// HandleExtensionMethod 实现 SDK 的扩展方法分发。未知方法必须回
// method not found，别把协议错误吞成空结果。
func (b *bridge) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if b.onExtension == nil {
		return nil, acp.NewMethodNotFound(method)
	}
	return b.onExtension(ctx, method, params)
}

// cancelledPermission 构造"取消"答复：agent 视为用户未授权，不重试。
func cancelledPermission() acp.RequestPermissionResponse {
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Cancelled: &acp.RequestPermissionOutcomeCancelled{},
	}}
}

// selectedPermission 构造"选中某选项"答复。
func selectedPermission(optionID acp.PermissionOptionId) acp.RequestPermissionResponse {
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: optionID},
	}}
}

// cancelledElicitation 构造"取消"答复：agent 视为用户没有回答。
func cancelledElicitation() acp.UnstableCreateElicitationResponse {
	return acp.UnstableCreateElicitationResponse{
		Cancel: &acp.UnstableCreateElicitationCancel{Action: "cancel"},
	}
}

// fs/terminal 五桩：Friday 不声明这些能力（实测两家 agent 一次都没调），被调即回错。
// 不可作为路径白名单，也不可原样展示 locations（C11）。

func (b *bridge) ReadTextFile(_ context.Context, r acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("friday: fs 能力未启用")
}

func (b *bridge) WriteTextFile(_ context.Context, r acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("friday: fs 能力未启用")
}

func (b *bridge) CreateTerminal(_ context.Context, r acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("friday: terminal 能力未启用")
}

func (b *bridge) KillTerminal(_ context.Context, r acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.New("friday: terminal 能力未启用")
}

func (b *bridge) TerminalOutput(_ context.Context, r acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.New("friday: terminal 能力未启用")
}

func (b *bridge) ReleaseTerminal(_ context.Context, r acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.New("friday: terminal 能力未启用")
}

func (b *bridge) WaitForTerminalExit(_ context.Context, r acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.New("friday: terminal 能力未启用")
}
