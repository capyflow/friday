package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
)

const (
	// execDefaultTimeout / execMaxTimeout：命令默认 30s，上限 120s。
	execDefaultTimeout = 30 * time.Second
	execMaxTimeout     = 120 * time.Second
	// execMaxOutput 输出截断上限 32KB。
	execMaxOutput = 32 * 1024
)

// ExecTool 执行 shell 命令：auto 模式白名单内直接执行、白名单外拒绝；
// confirm 模式白名单内直接执行、白名单外进审批队列（网页批准后执行，
// 超时自动拒绝）；off 模式整体禁用。黑名单命令任何模式都拦截。
type ExecTool struct {
	Approver    *approval.Queue
	BaseTimeout int // config 提供的默认超时秒数（0 用内置 30s）
}

func (t *ExecTool) Name() string { return "exec" }

func (t *ExecTool) Description() string {
	return "在设备上执行一条 shell 命令并返回输出（/bin/sh -c）。默认超时 30 秒，" +
		"可通过 timeout 调整（上限 120 秒）。危险命令受审批机制约束：" +
		"confirm 模式下非白名单命令需用户在网页批准；off 模式禁用；rm -rf 等毁灭性命令一律拦截。"
}

func (t *ExecTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "要执行的命令，如 uname -a"},
			"timeout": map[string]any{"type": "integer", "description": "超时秒数（默认 30，上限 120）"},
		},
		"required": []string{"command"},
	}
}

func (t *ExecTool) Call(ctx context.Context, args map[string]any) (string, error) {
	q := t.Approver
	if q == nil { // 防御：未配置审批器时按 off 处理（禁用执行）
		q = approval.New(string(approval.ModeOff), nil, 0)
	}
	if q.Mode() == approval.ModeOff {
		return "", approval.ErrDisabled
	}
	command, _ := args["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("缺少参数 command")
	}
	timeout := execDefaultTimeout
	if t.BaseTimeout > 0 {
		timeout = time.Duration(t.BaseTimeout) * time.Second
		if timeout > execMaxTimeout {
			timeout = execMaxTimeout
		}
	}
	if n := toInt(args["timeout"], 0); n > 0 {
		timeout = time.Duration(n) * time.Second
		if timeout > execMaxTimeout {
			timeout = execMaxTimeout
		}
	}

	// 黑名单兜底：任何模式都拦截。
	if reason := q.Denied(command); reason != "" {
		trace.Record(ctx, "approval", "exec 黑名单拦截：%s", command)
		return "", fmt.Errorf("%s", reason)
	}

	// 白名单命中 → 直接执行；否则按模式处理。
	if !q.Allowlisted(command) {
		switch t.Approver.Mode() {
		case approval.ModeAuto:
			trace.Record(ctx, "approval", "exec 非白名单命令，auto 模式拒绝：%s", command)
			return "", fmt.Errorf("命令不在白名单（tools.exec.allowlist）中，auto 模式下拒绝执行；如需放行请联系管理员调整白名单或切换 confirm 模式")
		case approval.ModeConfirm:
			trace.Record(ctx, "approval", "exec 非白名单命令，进入审批队列（超时 %s）：%s", q.Timeout(), command)
			if err := q.Request("exec", "执行命令："+logx.Truncate(command, 120),
				"命令："+command+"\n超时："+timeout.String()); err != nil {
				trace.Record(ctx, "approval", "exec 未获批准：%v（%s）", err, command)
				return "", err
			}
			trace.Record(ctx, "approval", "exec 已获批准，执行：%s", command)
		}
	}
	return runCommand(ctx, command, timeout)
}

// runCommand 用 /bin/sh -c 执行命令，组合输出截断到 32KB。
func runCommand(ctx context.Context, command string, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", command)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if len(text) > execMaxOutput {
		text = text[:execMaxOutput] + "\n…[输出超长已截断]"
	}
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("命令超时（%s）被终止\n输出:\n%s", timeout, text)
		}
		return fmt.Sprintf("命令退出码非 0：%v\n输出:\n%s", err, text), nil
	}
	if strings.TrimSpace(text) == "" {
		return "（命令执行成功，无输出）", nil
	}
	return text, nil
}
