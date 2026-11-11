// exectool.go 提供 exec_command 工具：执行一条 shell 命令并返回输出。
//
// 与框架自带实现的两点差异，都是为了"能真的在项目里干活"：
//
//  1. 工作目录 = 本次会话的工作目录。之前命令一律落在进程临时目录，于是
//     go test ./...、npm test、pytest 在项目里根本跑不起来——每条命令都得自己 cd
//     一次，模型很容易忘，忘了就是在空目录里跑出一个"没有测试文件"的假绿。
//     会话没指定目录时才回落到临时目录。
//  2. 支持显式 cwd 参数（给"去别处跑一条命令"用），以及超长输出的截断标注。
//
// 权限规则（白名单/毁灭性黑名单）不在这里判：框架在调用前拿 PermissionMatchers
// 声明的匹配器去问 Checker，这里透传即可，语义与框架自带工具完全一致。
package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	vxagent "github.com/capyflow/vortexagent/agent"
	vxexec "github.com/capyflow/vortexagent/tools/exec"

	"github.com/capyflow/friday/internal/workdir"
)

const (
	// execMaxOutputBytes 单路输出（stdout/stderr）的硬上限。
	execMaxOutputBytes = 100 * 1024
	// execMaxOutputChars 工具结果进入会话历史前的截断上限（字符）。
	execMaxOutputChars = 8000
	// execMaxTimeout 模型可请求的最大超时。
	execMaxTimeout = 10 * time.Minute
	// execDefaultTimeout 模型没传 timeout 时的默认超时。
	execDefaultTimeout = 30 * time.Second
)

// ExecCommandTool 执行 shell 命令。
type ExecCommandTool struct {
	defaultTimeout int    // 配置的默认超时秒数（<=0 用默认 30s）
	workDir        string // 没有会话目录时的回落目录（进程临时目录）
}

// NewExecCommandTool 创建 exec_command 工具。workDir 是命令在没有会话目录时的
// 工作目录：传空则继承进程当前目录（桌面模式下即数据目录）；这里传本次运行的
// 临时工作目录（系统临时目录下），相对路径写出来的脚本与中间产物就落在那儿，
// 而不是数据目录根下。
func NewExecCommandTool(defaultTimeout int, workDir string) *ExecCommandTool {
	return &ExecCommandTool{defaultTimeout: defaultTimeout, workDir: workDir}
}

// WorkDir 返回回落的工作目录（会话目录优先，见 dirFor）。
func (t *ExecCommandTool) WorkDir() string { return t.workDir }

func (t *ExecCommandTool) Name() string { return vxexec.ToolName }

func (t *ExecCommandTool) Description() string {
	return "在设备上执行一条 shell 命令并返回输出。默认在工作目录（本次会话选的项目目录）里执行，" +
		"没指定目录时在临时目录里执行；可用 cwd 参数指定别处。默认超时 30 秒，可通过 timeout 调整" +
		"（上限 10 分钟）。命令受权限规则约束：白名单内直接执行；confirm 审批模式下其他命令需用户在" +
		"管理后台批准后才会执行；rm -rf 等毁灭性命令一律拦截。"
}

func (t *ExecCommandTool) Overview() string { return "执行命令" }

func (t *ExecCommandTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "要执行的 shell 命令",
			},
			"timeout": map[string]any{
				"type":        "number",
				"description": "超时时间（秒），默认 30 秒",
			},
			"cwd": map[string]any{
				"type":        "string",
				"description": "工作目录，留空=本次会话的工作目录",
			},
		},
		"required": []string{"command"},
	}
}

// PermissionMatchers 透传权限自声明。collectPermissionMatchers 按接口扫描
// Registry，包装器不转发的话 exec_command 的 allow/deny 规则将失效。
func (t *ExecCommandTool) PermissionMatchers() (vxagent.ArgMatcher, vxagent.ArgMatcher) {
	return vxexec.CommandMatcher, vxexec.CommandDenyMatcher
}

// 编译期断言：包装器满足权限自描述接口，匹配器才能被自动接线。
var _ vxagent.PermissionMatcherProvider = (*ExecCommandTool)(nil)

// dirFor 决定这条命令的工作目录：显式 cwd 优先，其次本次会话的工作目录，
// 最后才回落到临时目录。
func (t *ExecCommandTool) dirFor(ctx context.Context, args map[string]any) (string, error) {
	if cwd, _ := args["cwd"].(string); strings.TrimSpace(cwd) != "" {
		abs, err := filepath.Abs(strings.TrimSpace(cwd))
		if err != nil {
			return "", fmt.Errorf("cwd 无法解析: %w", err)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("cwd 不存在或不可访问: %s", abs)
		}
		if !st.IsDir() {
			return "", fmt.Errorf("cwd 不是目录: %s", abs)
		}
		return abs, nil
	}
	if dir := workdir.From(ctx); dir != "" {
		return dir, nil
	}
	return t.workDir, nil
}

// timeoutFor 决定这条命令的超时：模型显式传了就用模型的，没传就用配置的默认值
// （默认超时在这里注入回 args，下游与轨迹看到的口径一致）。
func (t *ExecCommandTool) timeoutFor(args map[string]any) time.Duration {
	if _, ok := args["timeout"]; !ok && t.defaultTimeout > 0 {
		args["timeout"] = float64(t.defaultTimeout)
	}
	timeout := execDefaultTimeout
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
	}
	if timeout > execMaxTimeout {
		timeout = execMaxTimeout
	}
	return timeout
}

func (t *ExecCommandTool) Call(ctx context.Context, args map[string]any) (string, error) {
	command, _ := args["command"].(string)
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("command 不能为空")
	}
	dir, err := t.dirFor(ctx, args)
	if err != nil {
		return "", err
	}
	timeout := t.timeoutFor(args)

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var result strings.Builder
	if stdout.Len() > 0 {
		result.WriteString(truncateUTF8(stdout.String(), execMaxOutputBytes))
	}
	if stderr.Len() > 0 {
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		result.WriteString("stderr: ")
		result.WriteString(truncateUTF8(stderr.String(), execMaxOutputBytes))
	}
	// 源头节流：进入历史的最终结果按上限截断。完整输出若已归档可经 expand_chunk 回读。
	out := truncateWithMarker(result.String(), execMaxOutputChars)

	if runErr != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return out, fmt.Errorf("命令执行超时（%v，工作目录 %s）", timeout, dir)
		}
		return out, fmt.Errorf("命令执行失败（工作目录 %s）: %w", dir, runErr)
	}
	if out == "" {
		return "命令执行成功（无输出）", nil
	}
	return out, nil
}

// truncateUTF8 按字节上限截断并回退到完整的 rune 边界，避免把多字节字符切成乱码。
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n... (输出已截断)"
}

// truncateWithMarker 按 rune 上限截断并标注原始长度（max<0 不限制）。
//
// 保留头尾而不是只留头：命令输出的关键信息通常在末尾（测试失败用例、编译错误、
// 报错栈），只留开头等于把最该看的那段丢掉。中间省略的部分明确标注长度，让模型
// 知道"还有内容被折叠了"，必要时可以缩小输出再跑一次。
func truncateWithMarker(s string, max int) string {
	if max < 0 {
		return s
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	head := max * 2 / 5
	tail := max - head
	middle := len(rs) - head - tail
	return fmt.Sprintf("%s\n[…中间省略 %d 字符，完整输出共 %d 字符]\n%s",
		string(rs[:head]), middle, len(rs), string(rs[len(rs)-tail:]))
}
