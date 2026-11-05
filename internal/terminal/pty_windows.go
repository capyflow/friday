//go:build windows

// pty_windows.go 是 Windows 侧的占位：交互式伪终端要接 ConPTY，而 creack/pty
// 不支持 Windows。这里明确报错而不是退回"无 PTY 的管道"——后者跑不了 vim/top、
// 也会被 shell 当成非交互会话（提示符与颜色都没有），界面里看着像坏了。
package terminal

import (
	"errors"
	"os"
	"os/exec"
)

var errUnsupported = errors.New(
	"Windows 的内置终端还没做：需要接 ConPTY（当前只支持 macOS/Linux）。" +
		"这版请先用系统终端；界面里这个入口在 Windows 上会一直报这句")

func startPTY(cmd *exec.Cmd, cols, rows int) (*os.File, error) { return nil, errUnsupported }

func resizePTY(f *os.File, cols, rows int) error { return errUnsupported }

func terminatePTY(cmd *exec.Cmd, done <-chan Exit) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
