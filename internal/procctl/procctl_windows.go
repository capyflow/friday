//go:build windows

package procctl

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Prepare 设 CREATE_NO_WINDOW：sidecar/GUI 进程拉起控制台程序（npx、npm、
// powershell）不闪黑窗。进程树击杀不走进程组（Windows 无 killpg），由
// Attach 的 Job Object 承担。
func Prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}

// jobObject 封装一个配置了 KILL_ON_JOB_CLOSE 的 Job Object 句柄。
type jobObject struct {
	handle windows.Handle
}

func newJobObject() (*jobObject, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 Job Object: %w", err)
	}
	// KILL_ON_JOB_CLOSE：job 句柄关闭（含 Friday 自身退出/崩溃）时内核连带
	// 终结 job 内全部进程——不留孤儿 npx/node。
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("配置 Job Object: %w", err)
	}
	return &jobObject{handle: h}, nil
}

// assign 把 pid 挂进 job。进程已退出但尚未被 Wait 收割时句柄仍有效，
// 因此必须在 Start 之后、调用方 Wait 之前完成（Windows 无 Unix 僵尸语义，
// 进程对象在仍有句柄引用期间可打开）。
func (j *jobObject) assign(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("打开子进程句柄: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(j.handle, h); err != nil {
		return fmt.Errorf("挂接 Job Object: %w", err)
	}
	return nil
}

func (j *jobObject) terminate() error { return windows.TerminateJobObject(j.handle, 1) }
func (j *jobObject) close() error     { return windows.CloseHandle(j.handle) }

// Attach 把已启动的子进程挂进 Job Object，返回整树击杀与句柄释放闭包。
// 创建或挂接失败（句柄耗尽、进程瞬亡等罕见情形）返回 err，调用方应退化
// 为 cmd.Process.Kill() 只杀直接子进程——agent 主进程必死，SSE 断开照常
// 触发，孙进程可能残留，可接受。
func Attach(cmd *exec.Cmd) (kill func() error, close func() error, err error) {
	if cmd.Process == nil {
		return nil, nil, fmt.Errorf("进程尚未启动，无法挂接 Job Object")
	}
	job, err := newJobObject()
	if err != nil {
		return nil, nil, err
	}
	if err := job.assign(cmd.Process.Pid); err != nil {
		_ = job.close()
		return nil, nil, err
	}
	return job.terminate, job.close, nil
}
