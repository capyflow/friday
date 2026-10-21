package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Diagnostic 是一条预检诊断。Source 为 "tsc"（类型/语法）、"lint"（facade 规则）或 "host"。
type Diagnostic struct {
	Source  string `json:"source"`
	Rule    string `json:"rule,omitempty"`
	Code    string `json:"code,omitempty"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

func (d Diagnostic) String() string {
	tag := d.Code
	if d.Rule != "" {
		tag = d.Rule
	}
	if d.Line == 0 {
		return fmt.Sprintf("[%s] %s", tag, d.Message)
	}
	return fmt.Sprintf("%d:%d [%s] %s", d.Line, d.Column, tag, d.Message)
}

// DiagnosticsText 把诊断拼成给人看的一段话（聊天里直接回给用户）。
func DiagnosticsText(list []Diagnostic) string {
	if len(list) == 0 {
		return ""
	}
	lines := make([]string, 0, len(list))
	for _, d := range list {
		lines = append(lines, "  "+d.String())
	}
	return strings.Join(lines, "\n")
}

// ArgSpec 是流程声明的一个入参。
type ArgSpec struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Default     any    `json:"default,omitempty"`
}

// Meta 是脚本头部 `// friday-workflow` 元数据块的解析结果。
type Meta struct {
	Description string             `json:"description"`
	WhenToUse   string             `json:"whenToUse"`
	Args        map[string]ArgSpec `json:"args"`
}

// ArgNames 按字典序返回入参名（宿主返回的是 JSON 对象，Go map 无序）。
func (m *Meta) ArgNames() []string {
	if m == nil || len(m.Args) == 0 {
		return nil
	}
	names := make([]string, 0, len(m.Args))
	for n := range m.Args {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// AskSite 是一个 ask 调用点及其合成出的 JSON Schema；Raw = 无类型参数（取子代理文本）。
type AskSite struct {
	Index    int             `json:"index"`
	Raw      bool            `json:"raw"`
	TypeName string          `json:"typeName,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}

// CheckResult 是 `wf-host check` 的产出：确认卡所需的一切，加上诊断。
type CheckResult struct {
	OK          bool         `json:"ok"`
	Script      string       `json:"script"`
	Meta        *Meta        `json:"meta,omitempty"`
	Phases      []string     `json:"phases"`
	Commands    []string     `json:"commands"`
	Agents      []string     `json:"agents"`
	AskSites    []AskSite    `json:"askSites"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	TypeScript  string       `json:"typescript,omitempty"`
}

// Summary 是确认卡顶部的一句话概述。
func (r *CheckResult) Summary() string {
	return fmt.Sprintf("%d 个阶段 · 命令集：%s · 具名子代理 %d 个",
		len(r.Phases), joinOr(r.Commands, "、", "无"), len(r.Agents))
}

func joinOr(items []string, sep, whenEmpty string) string {
	if len(items) == 0 {
		return whenEmpty
	}
	return strings.Join(items, sep)
}

// Check 对脚本做静态预检：类型检查 + lint + 元数据/命令集/schema 提取。
//
// 宿主退出码 0 = 无诊断，10 = 有诊断（两者都有可解析的 JSON），其余是宿主自身失败。
// 调用方必须在 OK=false 时**不执行任何东西**——这就是"诊断意味着什么都没跑"那条不变量的落点。
func (h *Host) Check(ctx context.Context, scriptPath string) (*CheckResult, error) {
	abs, err := filepath.Abs(scriptPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("脚本读不到：%w", err)
	}

	stdout, stderr, runErr := h.runOnce(ctx, "check", abs)
	payload := lastJSONLine(stdout)
	if len(payload) == 0 || payload[0] != '{' {
		return nil, fmt.Errorf("宿主 check 没有产出 JSON：%s", trimTail(stderr))
	}
	res := &CheckResult{Script: abs}
	if err := json.Unmarshal(payload, res); err != nil {
		return nil, fmt.Errorf("宿主 check 输出无法解析：%w（stderr：%s）", err, trimTail(stderr))
	}
	if res.Diagnostics == nil {
		res.Diagnostics = []Diagnostic{}
	}
	if !res.OK && exitCode(runErr) > 10 {
		res.Diagnostics = append(res.Diagnostics, Diagnostic{Source: "host", Message: "宿主异常退出：" + trimTail(stderr)})
	}
	return res, nil
}

// exitCode 取子进程退出码；没有退出错误时视为 0，其它错误（启动失败）返回 -1。
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		return ee.ExitCode()
	}
	return -1
}
