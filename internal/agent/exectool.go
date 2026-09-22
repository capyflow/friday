// exectool.go 把框架的 exec_command 工具包装为本项目的工具实例：透传全部接口
// （含权限自声明 PermissionMatchers——Agent 创建时据此把命令匹配器自动接线进
// 权限 Checker），并补充一项能力：模型未传 timeout 时注入配置的默认超时
// （框架工具固定默认 30 秒）。
package agent

import (
	"context"

	vxagent "github.com/capyflow/vortexagent/agent"
	vxexec "github.com/capyflow/vortexagent/tools/exec"
)

// ExecCommandTool 包装框架 exec_command：注入配置默认超时，其余行为透传。
type ExecCommandTool struct {
	inner          *vxexec.ExecTool
	defaultTimeout int // 配置的默认超时秒数（<=0 用框架默认 30s）
}

// NewExecCommandTool 创建 exec_command 工具。工作目录为进程当前目录
// （框架侧 cmd.Dir 留空即继承，与旧自研 exec 行为一致）。
func NewExecCommandTool(defaultTimeout int) *ExecCommandTool {
	return &ExecCommandTool{inner: vxexec.NewExecTool(""), defaultTimeout: defaultTimeout}
}

func (t *ExecCommandTool) Name() string { return t.inner.Name() }

func (t *ExecCommandTool) Description() string {
	return "在设备上执行一条 shell 命令并返回输出。默认超时 30 秒，可通过 timeout 调整（上限 10 分钟）。" +
		"命令受权限规则约束：白名单内直接执行；confirm 审批模式下其他命令需用户在管理后台批准后才会执行；" +
		"rm -rf 等毁灭性命令一律拦截。"
}

func (t *ExecCommandTool) Overview() string { return t.inner.Overview() }

func (t *ExecCommandTool) Schema() map[string]any { return t.inner.Schema() }

// PermissionMatchers 透传权限自声明。collectPermissionMatchers 按接口扫描
// Registry，包装器不转发的话 exec_command 的 allow/deny 规则将失效。
func (t *ExecCommandTool) PermissionMatchers() (vxagent.ArgMatcher, vxagent.ArgMatcher) {
	return t.inner.PermissionMatchers()
}

// 编译期断言：包装器满足权限自描述接口，匹配器才能被自动接线。
var _ vxagent.PermissionMatcherProvider = (*ExecCommandTool)(nil)

func (t *ExecCommandTool) Call(ctx context.Context, args map[string]any) (string, error) {
	if _, ok := args["timeout"]; !ok && t.defaultTimeout > 0 {
		args["timeout"] = float64(t.defaultTimeout)
	}
	return t.inner.Call(ctx, args)
}
