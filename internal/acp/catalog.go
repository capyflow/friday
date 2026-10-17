// catalog.go 是可安装 agent 的内置目录（参考 Zed 的 External Agents 安装方式）：
// 每个条目带本机安装状态探测、官方安装命令与 ACP 启动模板。目录是编译期固定的
// 可信清单——安装命令与 MCP 服务器声明同信任级别（管理员显式触发），不经 shell
// 拼接任何外部输入。
package acp

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/capyflow/friday/internal/config"
)

// CatalogEntry 是目录里的一个可安装 agent。
type CatalogEntry struct {
	// ID 目录主键（如 "opencode"）。
	ID string `json:"id"`
	// Name 显示名。
	Name string `json:"name"`
	// Description 一句话说明。
	Description string `json:"description"`
	// Check 二进制名：LookPath 探测本机是否已安装。
	Check string `json:"check"`
	// InstallCmd 官方安装命令（固定字符串，整条经 sh -c 执行——含管道的
	// 官方安装脚本需要 shell）。
	InstallCmd string `json:"installCmd"`
	// DocURL 登录/配置说明页。
	DocURL string `json:"docUrl,omitempty"`
	// Template 安装完成后推荐加入清单的 agent 模板（command 用探测到的
	// 绝对路径回填）。
	Template config.AcpAgentConfig `json:"template"`
}

// catalog 是内置目录（编译期固定，新增 agent 在此追加）。
var catalog = []CatalogEntry{
	{
		ID:          "opencode",
		Name:        "opencode",
		Description: "终端 AI 编码助手，原生 ACP 支持；通用 OpenAI 兼容渠道均可接入",
		Check:       "opencode",
		InstallCmd:  "curl -fsSL https://opencode.ai/install | bash",
		DocURL:      "https://opencode.ai/docs/",
		Template: config.AcpAgentConfig{
			Name: "opencode", Command: "opencode", Args: []string{"acp"},
		},
	},
	{
		ID:          "codex",
		Name:        "Codex（OpenAI）",
		Description: "OpenAI Codex CLI，走 ChatGPT 登录态；经官方 ACP 适配器接入",
		Check:       "codex",
		InstallCmd:  "npm install -g @openai/codex @agentclientprotocol/codex-acp",
		DocURL:      "https://github.com/openai/codex",
		Template: config.AcpAgentConfig{
			Name: "codex", Command: "npx", Args: []string{"-y", "@agentclientprotocol/codex-acp"},
		},
	},
	{
		ID:          "gemini",
		Name:        "Gemini CLI（Google）",
		Description: "Google Gemini CLI，experimental ACP 模式；需 Google 账号登录",
		Check:       "gemini",
		InstallCmd:  "npm install -g @google/gemini-cli",
		DocURL:      "https://github.com/google-gemini/gemini-cli",
		Template: config.AcpAgentConfig{
			Name: "gemini", Command: "gemini", Args: []string{"--experimental-acp"},
		},
	},
	{
		ID:          "claude-code",
		Name:        "Claude Code（Anthropic）",
		Description: "Anthropic Claude Code，经 ACP 适配器接入；需 Claude 订阅或 API key",
		Check:       "claude",
		InstallCmd:  "npm install -g @anthropic-ai/claude-code @zed-industries/claude-code-acp",
		DocURL:      "https://github.com/anthropics/claude-code",
		Template: config.AcpAgentConfig{
			Name: "claude", Command: "npx", Args: []string{"-y", "@zed-industries/claude-code-acp"},
		},
	},
}

// CatalogView 是目录条目 + 本机安装状态。
type CatalogView struct {
	CatalogEntry
	// Installed 本机是否探测到 Check 二进制。
	Installed bool `json:"installed"`
	// Path 探测到的绝对路径（未安装为空）。
	Path string `json:"path,omitempty"`
}

// Catalog 返回目录与本机安装状态。
func (m *Manager) Catalog() []CatalogView {
	out := make([]CatalogView, 0, len(catalog))
	for _, e := range catalog {
		v := CatalogView{CatalogEntry: e}
		if p, err := exec.LookPath(e.Check); err == nil {
			v.Installed = true
			v.Path = p
		}
		out = append(out, v)
	}
	return out
}

// installTimeout 是单次安装的上限（npm -g 全局安装通常 30~120s，留足余量）。
const installTimeout = 5 * time.Minute

// InstallResult 是一次安装的归集结果。
type InstallResult struct {
	// Output 安装命令输出的尾部（展示用）。
	Output string
	// Path 探测到的可执行文件绝对路径。
	Path string
	// Template 回填了绝对路径的启动模板（调用方据此注册进清单）。
	Template config.AcpAgentConfig
	// AlreadyRegistered 清单里已有同名 agent（此时调用方不必重复注册）。
	AlreadyRegistered bool
}

// Install 执行目录中指定 agent 的官方安装命令。命令是内置目录里的固定字符串，
// 经 sh -c 执行（opencode 的官方脚本是管道）；超时 5 分钟，输出截尾返回。
// 是否注册进清单由调用方决定（本方法只报告 AlreadyRegistered）。
func (m *Manager) Install(ctx context.Context, id string) (InstallResult, error) {
	for _, e := range catalog {
		if e.ID != id {
			continue
		}
		res := InstallResult{}
		if _, ok := m.cfgFn().Normalized().Agent(e.Template.Name); ok {
			res.AlreadyRegistered = true
		}
		ictx, cancel := context.WithTimeout(ctx, installTimeout)
		defer cancel()
		cmd := exec.CommandContext(ictx, "sh", "-c", e.InstallCmd)
		out, err := cmd.CombinedOutput()
		res.Output = tailRunes(string(out), 2000)
		if err != nil {
			return res, fmt.Errorf("安装命令失败: %w（输出见下）", err)
		}
		// 安装成功后探测一次：新装的二进制通常已入 PATH（脚本写的标准位置）。
		p, err := exec.LookPath(e.Check)
		if err != nil {
			return res, fmt.Errorf("安装命令已执行成功，但未在 PATH 中找到 %q——可能需要重启 Friday 或手动检查安装位置", e.Check)
		}
		res.Path = p
		res.Template = e.Template
		res.Template.Command = p // 绝对路径回填：不依赖后续会话的 PATH
		return res, nil
	}
	return InstallResult{}, fmt.Errorf("目录中没有叫 %q 的 agent", id)
}

// tailRunes 取文本末尾 n 个 rune（安装输出可能很长，展示尾部即可）。
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return string(r)
	}
	return "…（前略）\n" + string(r[len(r)-n:])
}
