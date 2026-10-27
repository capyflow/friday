//go:build !windows

package procctl

import (
	"context"
	"os/exec"
	"syscall"
)

// Prepare 设独立进程组（Setpgid）：Attach 返回的击杀闭包以负 pid 击杀整组，
// 连 npx→node→agent 自 spawn 的孙进程一起带走（对应 acp 包的 N1 纪律）。
func Prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Attach Unix 侧进程组在 Prepare 已就位，无需额外载体：直接返回以负 pid
// 击杀进程组的闭包。组长已退出、进程组消散时返回 ESRCH，调用方保留
// cmd.Process.Kill() 兜底即可。
func Attach(cmd *exec.Cmd) (kill func() error, close func() error, err error) {
	kill = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return kill, func() error { return nil }, nil
}

// ShellCommandContext 经 sh 执行一条命令行（含管道的安装脚本需要 shell；
// 命令是目录里的固定可信字符串，不经外部输入拼接）。
func ShellCommandContext(ctx context.Context, line string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", line)
}
