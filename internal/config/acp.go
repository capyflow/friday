// ACP（Agent Client Protocol）智能体配置：外部 agent 的启动参数与运行边界。
// 形状照 McpServerConfig 对齐；管理后台「设置 → 智能体」在线编辑后经 UpdateAcp
// 写回。默认关闭（acp.enabled: false）——启用依赖用户 HOME 下有可解析的启动
// 命令与 agent 登录态，这两件事在无 TTY 环境最容易碎，须显式打开。
package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Acp 运行参数默认值（agent 条目内 0 值沿用全局）。
const (
	// AcpDefaultMaxConcurrent 同时存活的 agent 进程上限。
	AcpDefaultMaxConcurrent = 2
	// AcpDefaultIdleKillSeconds 空闲回收：最后一轮结束后保留进程的时长（秒）。
	// 回收后上下文不丢——重开 thread 经 session/load 重接。
	AcpDefaultIdleKillSeconds = 600
	// AcpDefaultTurnSeconds 单轮对话超时（秒）。实测 turn 12~209s 且方差极大。
	AcpDefaultTurnSeconds = 300
	// AcpDefaultHandshakeSeconds initialize + session/new 的握手超时（秒）。
	// gemini --acp 实测静默挂起 3 分钟，必须独立短超时。
	AcpDefaultHandshakeSeconds = 10
	// AcpPermissionTimeoutSeconds 智能体权限请求的审批等待（秒）——人可能不在
	// 屏幕前，比 exec 审批的 5 分钟长得多。
	AcpPermissionTimeoutSeconds = 30 * 60
)

// AcpConfig 是智能体层配置（config.yaml acp 段）。
type AcpConfig struct {
	// Enabled 总开关（默认关闭）。关闭时不拉起任何 agent，thread 接口拒绝新消息。
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MaxConcurrent 同时存活的 agent 进程上限（默认 2）。每个进程都会加载
	// agent 自己的 MCP server 与 skill 表，内存不可忽略。
	MaxConcurrent int `yaml:"maxConcurrent,omitempty" json:"maxConcurrent,omitempty"`
	// IdleKillSeconds 空闲回收秒数（默认 600）。0 视为默认；负数不回收。
	IdleKillSeconds int `yaml:"idleKillSeconds,omitempty" json:"idleKillSeconds,omitempty"`
	// TurnTimeoutSeconds 单轮对话超时秒数（默认 300）。
	TurnTimeoutSeconds int `yaml:"turnTimeoutSeconds,omitempty" json:"turnTimeoutSeconds,omitempty"`
	// HandshakeTimeoutSeconds 握手超时秒数（默认 10）。
	HandshakeTimeoutSeconds int `yaml:"handshakeTimeoutSeconds,omitempty" json:"handshakeTimeoutSeconds,omitempty"`
	// ShowThought 是否默认显示 agent 思考流（agent_thought_chunk，默认显示）。
	ShowThought *bool `yaml:"showThought,omitempty" json:"showThought,omitempty"`
	// Agents 已声明的 agent 清单。
	Agents []AcpAgentConfig `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// AcpAgentConfig 是一个外部 agent 的启动配置。
type AcpAgentConfig struct {
	// Name 显示名与主键（如 "codex"），thread 绑定用。
	Name string `yaml:"name" json:"name"`
	// Command 启动命令（如 npx 或绝对路径；PATH 解析失败时建议绝对路径）。
	Command string `yaml:"command" json:"command"`
	// Args 启动参数（如 ["-y", "@zed-industries/codex-acp"]）。
	Args []string `yaml:"args,omitempty" json:"args,omitempty"`
	// Env 额外环境变量（在继承进程环境的基础上追加）。
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	// Workspace 默认工作目录（可空）：创建 thread 时预填，可改。
	Workspace string `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	// Enabled 启用开关（缺省启用）。
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// TurnTimeoutSeconds 单轮超时覆盖（0 = 用全局值）。
	TurnTimeoutSeconds int `yaml:"turnTimeoutSeconds,omitempty" json:"turnTimeoutSeconds,omitempty"`
	// DegradeResume 标记该 agent 的 session/load 恢复保真度实测不可靠：
	// 重开 thread 不再尝试重接，直接新开会话并标注「上下文已重置」。
	DegradeResume bool `yaml:"degradeResume,omitempty" json:"degradeResume,omitempty"`
}

// IsEnabled 返回该 agent 是否启用（缺省启用）。
func (a AcpAgentConfig) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// Normalized 填充 0 值默认。不修改调用者。
func (c AcpConfig) Normalized() AcpConfig {
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = AcpDefaultMaxConcurrent
	}
	if c.IdleKillSeconds == 0 {
		c.IdleKillSeconds = AcpDefaultIdleKillSeconds
	}
	if c.TurnTimeoutSeconds <= 0 {
		c.TurnTimeoutSeconds = AcpDefaultTurnSeconds
	}
	if c.HandshakeTimeoutSeconds <= 0 {
		c.HandshakeTimeoutSeconds = AcpDefaultHandshakeSeconds
	}
	return c
}

// TurnSeconds 返回某 agent 的生效单轮超时（条目覆盖优先）。
func (c AcpConfig) TurnSeconds(agent AcpAgentConfig) int {
	if agent.TurnTimeoutSeconds > 0 {
		return agent.TurnTimeoutSeconds
	}
	return c.TurnTimeoutSeconds
}

// ThoughtEnabled 返回思考流是否默认显示（缺省显示，拍板 D5）。
func (c AcpConfig) ThoughtEnabled() bool { return c.ShowThought == nil || *c.ShowThought }

// Agent 按名字取 agent 配置（未启用或不存在返回 false）。
func (c AcpConfig) Agent(name string) (AcpAgentConfig, bool) {
	for _, a := range c.Agents {
		if a.Name == name {
			return a, a.IsEnabled()
		}
	}
	return AcpAgentConfig{}, false
}

// Validate 校验智能体配置（Enabled=false 时跳过 agent 清单检查，允许空跑）。
func (c AcpConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	seen := map[string]bool{}
	for _, a := range c.Agents {
		if a.Name == "" {
			return fmt.Errorf("config: acp.agents 存在缺 name 的条目")
		}
		if seen[a.Name] {
			return fmt.Errorf("config: acp.agents 条目 name 重复: %q", a.Name)
		}
		seen[a.Name] = true
		if a.Command == "" {
			return fmt.Errorf("config: acp.agents[%s].command 不能为空", a.Name)
		}
		if a.TurnTimeoutSeconds < 0 {
			return fmt.Errorf("config: acp.agents[%s].turnTimeoutSeconds 不能为负", a.Name)
		}
	}
	if c.MaxConcurrent < 0 || c.IdleKillSeconds < 0 ||
		c.TurnTimeoutSeconds < 0 || c.HandshakeTimeoutSeconds < 0 {
		return fmt.Errorf("config: acp 各项数值不能为负")
	}
	return nil
}

// UpdateAcp 热更新 config.yaml 中的 acp 段（整体替换该映射，保留文件其余部分
// 的注释与结构；acp 段不存在时追加）。cfg 须已通过 Validate。以 yaml.Node 方式
// 编辑，临时文件 + rename 原子写回。
func UpdateAcp(path string, cfg AcpConfig) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()
	data, err := readFileLocked(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("解析配置失败: %w", err)
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("配置文件为空")
	}
	root := doc.Content[0]

	section := &yaml.Node{Kind: yaml.MappingNode}
	strPair := func(m *yaml.Node, k, v string) {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"})
	}
	intPair := func(m *yaml.Node, k string, v int) {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Value: strconv.Itoa(v), Tag: "!!int"})
	}
	boolPair := func(m *yaml.Node, k string, v bool) {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Value: boolYAML(v), Tag: "!!bool"})
	}
	boolPair(section, "enabled", cfg.Enabled)
	if cfg.MaxConcurrent > 0 {
		intPair(section, "maxConcurrent", cfg.MaxConcurrent)
	}
	if cfg.IdleKillSeconds > 0 {
		intPair(section, "idleKillSeconds", cfg.IdleKillSeconds)
	}
	if cfg.TurnTimeoutSeconds > 0 {
		intPair(section, "turnTimeoutSeconds", cfg.TurnTimeoutSeconds)
	}
	if cfg.HandshakeTimeoutSeconds > 0 {
		intPair(section, "handshakeTimeoutSeconds", cfg.HandshakeTimeoutSeconds)
	}
	if cfg.ShowThought != nil && !*cfg.ShowThought {
		boolPair(section, "showThought", false)
	}
	if len(cfg.Agents) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, a := range cfg.Agents {
			item := &yaml.Node{Kind: yaml.MappingNode}
			strPair(item, "name", a.Name)
			strPair(item, "command", a.Command)
			if len(a.Args) > 0 {
				argsSeq := &yaml.Node{Kind: yaml.SequenceNode}
				for _, arg := range a.Args {
					argsSeq.Content = append(argsSeq.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Value: arg, Tag: "!!str"})
				}
				item.Content = append(item.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "args"}, argsSeq)
			}
			if len(a.Env) > 0 {
				envMap := &yaml.Node{Kind: yaml.MappingNode}
				keys := sortedKeys(a.Env)
				for _, k := range keys {
					strPair(envMap, k, a.Env[k])
				}
				item.Content = append(item.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "env"}, envMap)
			}
			if a.Workspace != "" {
				strPair(item, "workspace", a.Workspace)
			}
			if a.Enabled != nil && !*a.Enabled {
				boolPair(item, "enabled", false)
			}
			if a.TurnTimeoutSeconds > 0 {
				intPair(item, "turnTimeoutSeconds", a.TurnTimeoutSeconds)
			}
			if a.DegradeResume {
				boolPair(item, "degradeResume", true)
			}
			seq.Content = append(seq.Content, item)
		}
		section.Content = append(section.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "agents"}, seq)
	}
	setMappingNode(root, "acp", section)

	return writeFileLocked(path, &doc)
}

func boolYAML(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// readFileLocked / writeFileLocked 是 Update* 家族共用的读写骨架：
// 读取在 configFileMu 保护下进行；写入临时文件 + rename 原子替换。
func readFileLocked(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	return data, nil
}

func writeFileLocked(path string, doc *yaml.Node) error {
	out, err := marshalNode(doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	return os.Rename(tmp, path)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
