// catalog.go 是可安装 agent 的目录（M-3）：内置精选条目 + 官方 ACP 注册表
// （registry.go，与 Zed 同源同 schema，上游上架新 agent 无需发版）。每个条目带
// 本机安装状态、注册状态与版本对比；安装命令是固定可信清单——安装命令与 MCP
// 服务器声明同信任级别（管理员显式触发），不经 shell 拼接任何外部输入。
package acp

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
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
	{
		// zcode 没有原生 ACP 模式（官方还在 feature request 阶段），接入面是
		// 社区适配器 zcode-acp-server：它桥接 zcode 的 headless app-server
		// （ZCode 协议）并对外讲标准 ACP，自己负责拉起 zcode（PATH、ZCODE_BIN
		// 或桌面 App 内置 zcode.cjs 均可），登录态复用 ~/.zcode。所以这里
		// Check 探测的是适配器本身而不是 zcode CLI，模板命令与 Check 相同，
		// 安装后回填绝对路径即可直接启动。
		// 注意 macOS 的坑：适配器兜底用系统 node 跑桌面 App 内置 zcode.cjs，
		// 而 node:sqlite 在 node 23 上过不了会话库迁移（Migration transaction
		// ownership mismatch），需自建 Electron 内嵌 Node 的 zcode shim 并在
		// 该 agent 的 Env 里设 ZCODE_BIN 指向它（2026-10-20 实测通）。
		ID:          "zcode",
		Name:        "ZCode（智谱）",
		Description: "智谱 GLM 编码 agent，经社区 ACP 适配器接入；复用 ZCode 登录态。macOS 常需 ZCODE_BIN 指向 Electron Node shim，见文档",
		Check:       "zcode-acp-server",
		InstallCmd:  "npm install -g zcode-acp-server",
		DocURL:      "https://github.com/william0wang/zcode-acp",
		Template: config.AcpAgentConfig{
			Name: "zcode", Command: "zcode-acp-server",
		},
	},
}

// CatalogView 是目录条目 + 本机状态。
type CatalogView struct {
	CatalogEntry
	// Installed 本机是否探测到 Check 二进制（仅内置条目有 Check；registry 条目
	// 以 Registered 为准——注册即视为已安装）。
	Installed bool `json:"installed"`
	// Path 探测到的可执行文件绝对路径（未安装为空）。
	Path string `json:"path,omitempty"`
	// Source 条目来源：builtin（内置精选）| registry（官方注册表）。
	Source string `json:"source"`
	// Version 官方条目的当前版本（内置条目为空）。
	Version string `json:"version,omitempty"`
	// LatestVersion npm registry 的 latest 真值（仅 npx 分发条目；registry 条目
	// 常滞后于 npm——实测 minimax-code registry=0.2.7 时 npm 已 0.6.12）。
	LatestVersion string `json:"latestVersion,omitempty"`
	// Registered 清单（config.acp.agents）里是否已有同名 agent。
	Registered bool `json:"registered"`
	// UpdateAvailable 已注册且有比登记版本戳更新的可用版本（分段数字比较，
	// 只报升级不报降级——手动升过级、registry 条目滞后的用户不再被误报）。
	UpdateAvailable bool `json:"updateAvailable,omitempty"`
	// Supported binary 分发是否覆盖当前平台（false 时禁用安装）。
	Supported bool `json:"supported"`
}

// Catalog 返回合并后的目录（内置精选在前，官方条目按注册表顺序跟随）。
// refresh=false 时官方目录走缓存（TTL 1h，过期自动重拉，失败回退过期缓存）。
func (m *Manager) Catalog(ctx context.Context, refresh bool) []CatalogView {
	registered := map[string]config.AcpAgentConfig{}
	for _, a := range m.cfgFn().Normalized().Agents {
		registered[a.Name] = a
	}

	out := make([]CatalogView, 0, len(catalog)+8)
	seen := map[string]bool{}
	// curLatest 是当前条目的可用最新版（npm 真值优先，registry 滞后时取两者较大）；
	// add() 用它做有方向的升级判断。
	curLatest := ""
	add := func(e CatalogEntry, source string) {
		if seen[e.ID] {
			return
		}
		seen[e.ID] = true
		v := CatalogView{
			CatalogEntry: e,
			Source:       source,
			Version:      e.Template.Version,
			Supported:    true,
		}
		if e.Check != "" {
			if p, err := lookPathFn(e.Check); err == nil {
				v.Installed = true
				v.Path = p
			}
		}
		if a, ok := registered[e.Template.Name]; ok {
			v.Registered = true
			avail := e.Template.Version
			if compareVersions(curLatest, avail) > 0 {
				avail = curLatest // npm 真值比 registry 条目新（常态）
			}
			v.UpdateAvailable = avail != "" &&
				(a.Version == "" || compareVersions(avail, a.Version) > 0)
			if e.Check == "" { // registry 条目：注册即已安装
				v.Installed = true
				v.Path = a.Command
			}
		}
		out = append(out, v)
	}

	// 内置精选（编译期固定，同 id 覆盖官方条目）。
	for _, e := range catalog {
		add(e, "builtin")
	}
	// 官方注册表（拉取失败静默降级为仅内置）。npx 条目顺带查 npm latest 真值：
	// registry 的 Npx.Package 自带版本号（"@scope/name@1.2.3"），要剥出纯包名
	// 再问 npm——带版本查 latest 会被 npm 以 HTTP 400 拒掉。
	entries := m.registryIndex(ctx, refresh)
	pkgs := make([]string, 0, len(entries))
	pkgSeen := map[string]bool{}
	entryPkg := map[string]string{} // entryID → 纯包名
	for _, re := range entries {
		if re.Distribution.Npx == nil || re.Distribution.Npx.Package == "" {
			continue
		}
		name, _ := npxPackageToken([]string{re.Distribution.Npx.Package})
		if name == "" || pkgSeen[name] {
			continue
		}
		pkgSeen[name] = true
		entryPkg[re.ID] = name
		pkgs = append(pkgs, name)
	}
	npmVer := m.npmLatest(ctx, pkgs, refresh)
	for _, re := range entries {
		if seen[re.ID] {
			continue
		}
		ce, supported := re.toCatalogEntry(m.registryBase)
		if !supported && len(re.Distribution.Binary) == 0 {
			continue // 无任何分发的条目不可安装，不展示
		}
		ce.ID = re.ID // 保留官方 id 作为安装主键
		if ce.Name == "" {
			ce.Name = re.Name
		}
		curLatest = npmVer[entryPkg[re.ID]] // binary 分发条目不在表里，自然为空
		add(ce, "registry")
		out[len(out)-1].LatestVersion = curLatest
		if !supported {
			out[len(out)-1].Supported = false
		}
	}
	return out
}

// installTimeout 是单次安装的上限（npm -g 全局安装通常 30~120s，留足余量）。
const installTimeout = 5 * time.Minute

// acpBaseDirFn 可被测试替换：registry 缓存与 binary 安装目录的父目录
// （<数据目录>/acp，与 acp-sessions 同族；内核已 chdir 到数据目录，用相对路径）。
var acpBaseDirFn = defaultAcpBaseDir

func defaultAcpBaseDir() (string, error) {
	return "acp", nil
}

// runInstallCommand 可被测试替换：执行一条官方安装命令（固定字符串经 sh -c）。
// 超时击杀整个进程组——CommandContext 默认只杀 sh，npm/curl 这类孙进程会残留
// （与 agent 进程的 N1 同一纪律）。
var runInstallCommand = func(ctx context.Context, installCmd string) ([]byte, error) {
	ictx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	cmd := exec.CommandContext(ictx, "sh", "-c", installCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd.CombinedOutput()
}

// InstallResult 是一次安装的归集结果。
type InstallResult struct {
	// Output 安装命令输出的尾部（展示用）。
	Output string
	// Path 探测到的可执行文件绝对路径。
	Path string
	// Template 回填了绝对路径的启动模板（调用方据此注册进清单）。
	Template config.AcpAgentConfig
	// Version 本次安装对应的版本（registry 条目才有）。
	Version string `json:"version,omitempty"`
	// AlreadyRegistered 清单里已有同名 agent（此时调用方不必重复注册）。
	AlreadyRegistered bool
}

// Install 安装目录中指定 id 的 agent：内置条目走精选安装命令，官方条目按
// distribution 分发（npx → npm -g；binary → 下载校验解压）。安装成功后返回
// 回填好的启动模板，是否注册进清单由调用方决定。
func (m *Manager) Install(ctx context.Context, id string) (InstallResult, error) {
	for _, e := range catalog {
		if e.ID == id {
			return m.installBuiltin(ctx, e)
		}
	}
	for _, re := range m.registryIndex(ctx, false) {
		if re.ID == id {
			// 已注册且启动参数带 <包名>@<版本> → 原位升级：参数形态原样保留
			//（registry 模板的整表覆写会把多 bin 包必需的自定义形态打回坏掉的
			// 旧形态，实测 minimax 0.6.x 需要 -p … mcode acp）；无版本 token
			// 的（如 codex 模板）仍走完整安装路径。
			if ag, ok := m.cfgFn().Normalized().Agent(re.ID); ok {
				if _, tokIdx := npxPackageToken(ag.Args); tokIdx >= 0 && re.Distribution.Npx != nil {
					return m.upgradeNpx(ctx, ag)
				}
			}
			return m.installRegistryAgent(ctx, re)
		}
	}
	return InstallResult{}, fmt.Errorf("目录中没有叫 %q 的 agent", id)
}

// installBuiltin 内置精选条目的安装：命令为内置固定串，超时 5 分钟，输出截尾
// 返回；随后探测 Check 二进制回填绝对路径。
func (m *Manager) installBuiltin(ctx context.Context, e CatalogEntry) (InstallResult, error) {
	res := InstallResult{}
	if _, ok := m.cfgFn().Normalized().Agent(e.Template.Name); ok {
		res.AlreadyRegistered = true
	}
	out, err := runInstallCommand(ctx, e.InstallCmd)
	res.Output = tailRunes(string(out), 2000)
	if err != nil {
		return res, fmt.Errorf("安装命令失败: %w（输出见下）", err)
	}
	// 安装成功后探测一次：新装的二进制通常已入 PATH（脚本写的标准位置）。
	p, err := lookPathFn(e.Check)
	if err != nil {
		return res, fmt.Errorf("安装命令已执行成功，但未在 PATH 中找到 %q——可能需要重启 Friday 或手动检查安装位置", e.Check)
	}
	res.Path = p
	res.Template = templateWithPath(e.Template, e.Check, p)
	return res, nil
}

// installRegistryAgent 官方条目的安装：npx → npm -g 安装后锚定 npx 绝对路径；
// binary → 下载校验解压（registry.go）。版本随模板返回（M-4 版本戳）。
func (m *Manager) installRegistryAgent(ctx context.Context, re registryEntry) (InstallResult, error) {
	res := InstallResult{Version: re.Version}
	if _, ok := m.cfgFn().Normalized().Agent(re.ID); ok {
		res.AlreadyRegistered = true
	}
	ce, supported := re.toCatalogEntry(m.registryBase)
	switch {
	case !supported && len(re.Distribution.Binary) == 0:
		return res, fmt.Errorf("agent %q 没有可安装的分发", re.Name)
	case ce.Template.Command == "npx":
		out, err := runInstallCommand(ctx, ce.InstallCmd)
		res.Output = tailRunes(string(out), 2000)
		if err != nil {
			return res, fmt.Errorf("安装命令失败: %w（输出见下）", err)
		}
		p, err := lookPathFn("npx")
		if err != nil {
			return res, fmt.Errorf("安装命令已执行成功，但未在 PATH 中找到 npx——请手动检查 node 环境")
		}
		res.Path = p
		res.Template = ce.Template
		res.Template.Command = p
		return res, nil
	case supported:
		bin, out, err := installBinaryDistribution(ctx, m.registryBase, re)
		res.Output = out
		if err != nil {
			return res, err
		}
		res.Path = bin
		res.Template = ce.Template
		res.Template.Command = bin
		return res, nil
	default:
		return res, fmt.Errorf("agent %q 在本平台（%s）没有分发包", re.Name, platformKey())
	}
}

// lookPathFn 可被测试替换（启动器绝对路径解析）。
var lookPathFn = exec.LookPath

// templateWithPath 把模板命令补成可独立解析的形态：
//   - 模板命令就是被探测的二进制本身（opencode/gemini/zcode 适配器这类 CLI 形态）
//     时，回填探测到的绝对路径；
//   - 否则是启动器形态（npx/uvx 等，绝不能替换成被探测 CLI 的路径——同一条参数
//     会被当成 CLI 的子命令），锚定启动器的绝对路径：Friday 以 daemon 运行时
//     PATH 可能与安装时不同。解析失败保留原名（spawn 失败会提示写绝对路径）。
func templateWithPath(t config.AcpAgentConfig, check, path string) config.AcpAgentConfig {
	if path != "" && t.Command == check {
		t.Command = path
		return t
	}
	if p, err := lookPathFn(t.Command); err == nil {
		t.Command = p
	}
	return t
}

// tailRunes 取文本末尾 n 个 rune（安装输出可能很长，展示尾部即可）。
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return string(r)
	}
	return "…（前略）\n" + string(r[len(r)-n:])
}
