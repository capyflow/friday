//go:build !windows

// pty_unix.go 是 Unix 侧的 PTY 实现：creack/pty 负责 openpt/挂载 pts，
// 进程组由它一并设好，所以收尾要按组杀（只杀 shell 会留下它起的 vim/npm）。
package terminal

import (
	"syscall"
	"time"

	"github.com/creack/pty"
	"os"
	"os/exec"
)

func startPTY(cmd *exec.Cmd, cols, rows int) (*os.File, error) {
	return pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

func resizePTY(f *os.File, cols, rows int) error {
	return pty.Setsize(f, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// terminatePTY 先对进程组发 SIGTERM，等 done（wait 协程的退出信号）最多一秒，
// 到点还活着就 SIGKILL 整组。done 带缓冲，收不到也不阻塞调用方。
func terminatePTY(cmd *exec.Cmd, done <-chan Exit) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid // pty.Start 把子进程放进自己的进程组，组号等于它的 pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}
