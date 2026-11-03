package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// TestScratchDirUnderTmp 临时工作目录要落在本平台的临时目录下（macOS/Linux 是 /tmp，
// Windows 是 %TEMP%），系统会定期清理那里的旧文件；权限收紧到只有本人能进——
// 里面是 agent 写的脚本与中间产物。
func TestScratchDirUnderTmp(t *testing.T) {
	dir := scratchDir()
	if dir == "" {
		t.Fatal("临时工作目录应创建成功")
	}
	base := config.TempDirBase()
	if !strings.HasPrefix(dir, base) {
		t.Errorf("临时工作目录应在 %s 下，实际 %s", base, dir)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		t.Fatalf("临时工作目录应存在: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Errorf("临时工作目录权限应为 0700，实际 %o", perm)
	}
}

// TestExecCommandRunsInScratchDir 守住「脚本不再落在数据目录根下」：
// exec 的工作目录是临时工作目录，相对路径写出的文件跟着落在那儿。
func TestExecCommandRunsInScratchDir(t *testing.T) {
	tool := NewExecCommandTool(5, scratchDir())
	out, err := tool.Call(context.Background(), map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("执行 pwd 失败: %v", err)
	}
	if !strings.Contains(out, filepath.Base(scratchDir())) {
		t.Errorf("命令应在临时工作目录里执行，输出 %q", out)
	}
	probe := "probe-scratch.txt"
	if _, err := tool.Call(context.Background(), map[string]any{"command": "echo hi > " + probe}); err != nil {
		t.Fatalf("相对写入失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(scratchDir(), probe)); err != nil {
		t.Errorf("相对写出的文件应落在临时工作目录: %v", err)
	}
	os.Remove(filepath.Join(scratchDir(), probe))
}

// TestRegisterToolsUsesScratchDir 守住接线本身：注册进 Registry 的 exec_command
// 必须以临时工作目录为 cwd（否则脚本又落在数据目录根下）。
func TestRegisterToolsUsesScratchDir(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Exec.Mode = string(approval.ModeAuto)
	r := vxagent.NewRegistry()
	if err := RegisterTools(r, Deps{Config: cfg}); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	tv, ok := r.Get(execToolName)
	if !ok {
		t.Fatalf("注册表里应有 %s", execToolName)
	}
	exec, ok := tv.(*ExecCommandTool)
	if !ok {
		t.Fatalf("%s 应是本项目的包装类型，实际 %T", execToolName, tv)
	}
	if exec.WorkDir() != scratchDir() {
		t.Errorf("exec 工作目录应为临时工作目录 %s，实际 %q", scratchDir(), exec.WorkDir())
	}
}

// TestSystemPromptTempDirRules 提示词里必须写清临时目录约定，
// 否则 agent 还会把脚本写在数据目录根下、跑完也不删。
func TestSystemPromptTempDirRules(t *testing.T) {
	p := systemPrompt(&config.Config{})
	for _, want := range []string{"/tmp", "%TEMP%", "mktemp -d", "跑完自己删掉", "临时目录"} {
		if !strings.Contains(p, want) {
			t.Errorf("系统提示词应包含 %q 这条临时目录约定", want)
		}
	}
}
