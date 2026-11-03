// scratch.go 管 agent 的临时工作目录：进程启动时建一个系统临时目录下的独占目录，
// exec_command 的相对路径与中间产物默认落这儿，而不是数据目录根下（桌面模式下
// 进程 cwd 就是数据目录，脚本会直接堆在那儿）。
//
// 临时目录按平台分：macOS/Linux 是 /tmp，Windows 是系统临时目录（%TEMP%）。
// 系统会定期清理那里的旧文件：macOS 的 /usr/libexec/tmp_cleaner 由 launchd 每天
// 0 点跑一遍，清掉 /tmp 下 3 天没访问过的文件（子目录里的文件一并清），空目录随后删除。
package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/config"
)

const (
	// scratchPrefix 临时工作目录名前缀：自己认得，也便于清理旧目录。
	scratchPrefix = "friday-work-"
	// scratchStaleAfter 旧目录多久算「上次留下的」——超过一天就可以清，
	// 免得把别的实例正在用的目录删了。
	scratchStaleAfter = 24 * time.Hour
)

var (
	scratchOnce sync.Once
	scratchPath string
)

// scratchDir 返回本次进程的临时工作目录（懒创建，整进程只建一次）。
// 父目录按平台来：macOS/Linux 是 /tmp，Windows 是系统临时目录（%TEMP%）。
// 建不出来（临时目录不可写等）时返回空串：调用方退回进程当前目录，功能不受影响。
func scratchDir() string {
	scratchOnce.Do(func() {
		sweepStaleScratch()
		dir, err := os.MkdirTemp(config.TempDirBase(), scratchPrefix)
		if err != nil {
			return
		}
		scratchPath = dir
	})
	return scratchPath
}

// sweepStaleScratch 清掉上次运行留下的 Friday 临时目录。只按名字前缀 + 存在时长
// 判断：正在跑的实例目录是刚建的，不会被误删。
func sweepStaleScratch() {
	for _, base := range config.TempDirs() {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), scratchPrefix) {
				continue
			}
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < scratchStaleAfter {
				continue
			}
			os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}
