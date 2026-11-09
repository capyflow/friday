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
	// AcpDefaultIdleKillSeconds 空闲回收：最后一轮结束后保留进程的时长（秒）。
	// 回收后上下文不丢——重开 thread 经 session/load 重接。
	AcpDefaultIdleKillSeconds = 600
	// AcpDefaultHandshakeSeconds initialize 的握手超时（秒）。
	// gemini --acp 实测静默挂起 3 分钟，必须独立短超时。
	AcpDefaultHandshakeSeconds = 10
	// AcpDefaultSessionSeconds 建会话（session/new、session/load）的超时（秒）。
	// 不能沿用握手的 10s：opencode 1.18 实测光是回 available_commands_update（把
	// 全部 skill 表列一遍）就要 9.3s，session/new 紧跟着就撞线——表现成
	// 「建立会话失败: context deadline exceeded」，看着像 agent 坏了，其实是预算太小。
	AcpDefaultSessionSeconds = 60
	// AcpColdStartHandshakeSeconds 包管理器型启动命令（npx/pnpm/bunx/uvx…）首次
	// 拉起的握手预算：冷启动要联网装包，10s 基本必踩；同一 agent 成功起过一次后
	// 就回落到常规握手值（缓存见 acp 包的进程内记录）。
	AcpColdStartHandshakeSeconds = 90
	// AcpPermissionTimeoutSeconds 智能体权限请求的审批等待（秒）——人可能不在
	// 屏幕前，比 exec 审批的 5 分钟长得多。
	AcpPermissionTimeoutSeconds = 30 * 60
)

// BuiltinAgentName 是内置 Friday 在智能体清单里的名字（thread.Agent 的值）。
const BuiltinAgentName = "friday"

// BuiltinAgent 返回内置 Friday 的 agent 条目：它不来自 config.yaml，由 Normalized
// 注入，让「工作区 → 新建会话」的 agent 清单里出现 friday 这一项。Command 为空是
// 有意的——内置条目不拉起子进程，由 acp.Manager 用进程内适配器把这一轮交给内置
// Brain 跑（见 internal/acp/builtin.go）。
func BuiltinAgent() AcpAgentConfig {
	return AcpAgentConfig{Name: BuiltinAgentName, Builtin: true}
}

// FacadeDefaultTools 是工具门面的默认白名单：只读类工具。外部 agent 通过
// MCP 门面只能调用这里列出的名字；写类（write_file/edit_file/mcp_edit）、
// 命令执行（exec_command）、记忆与待办的增删改一律不进默认清单
// （docs/AGENT-TOOL-INJECTION.md §3.2）。
var FacadeDefaultTools = []string{
	"read_file", "list_files",
	"recall", "list_todos", "list_notes", "list_tasks",
	"get_time", "web_search", "web_fetch",
}

// AcpConfig 是智能体层配置（config.yaml acp 段）。
type AcpConfig struct {
	// Enabled 总开关（默认关闭）。关闭时不拉起任何 agent，thread 接口拒绝新消息。
	Enabled bool `yaml:"enabled" json:"enabled"`
	// IdleKillSeconds 空闲回收秒数（默认 600）。0 视为默认；负数不回收。
	IdleKillSeconds int `yaml:"idleKillSeconds,omitempty" json:"idleKillSeconds,omitempty"`
	// HandshakeTimeoutSeconds initialize 握手超时秒数（默认 10）。
	HandshakeTimeoutSeconds int `yaml:"handshakeTimeoutSeconds,omitempty" json:"handshakeTimeoutSeconds,omitempty"`
	// SessionTimeoutSeconds 建会话（session/new、session/load）超时秒数（默认 60）。
	// 与握手分开：skill 表大的 agent（opencode 实测 9s+）在 10s 里连会话都建不出来。
	SessionTimeoutSeconds int `yaml:"sessionTimeoutSeconds,omitempty" json:"sessionTimeoutSeconds,omitempty"`
	// ShowThought 是否默认显示 agent 思考流（agent_thought_chunk，默认显示）。
	ShowThought *bool `yaml:"showThought,omitempty" json:"showThought,omitempty"`
	// Facade 工具门面配置（config.yaml acp.facade 段；nil = 默认开启 + 只读白名单）。
	Facade *AcpFacadeConfig `yaml:"facade,omitempty" json:"facade,omitempty"`
	// OneSessionPerProcess 让每个会话独占一条 agent 连接（= 一个 agent 进程）。
	// 缺省按 agent 判定：CodeBuddy 的 ACP 一条连接只有一条 prompt 渲染循环，
	// 同项目多会话共连接会把 prompt 排成队列，因此它默认独占；其余 agent 沿用
	// 「agent+目录 一进程」的复用模型。true/false 可全局覆盖。
	OneSessionPerProcess *bool `yaml:"oneSessionPerProcess,omitempty" json:"oneSessionPerProcess,omitempty"`
	// Agents 已声明的 agent 清单。
	Agents []AcpAgentConfig `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// AcpFacadeConfig 是「Friday 工具门面」的配置：把 Friday 注册表里白名单内的
// 工具以 MCP server 形态注入外部 agent 的会话（session/new 的 mcpServers），
// 见 internal/acp/facade.go。
type AcpFacadeConfig struct {
	// Enabled 门面开关（缺省开启）。关闭时 session/new 的 mcpServers 回到空数组。
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Tools 暴露给外部 agent 的工具白名单（空 = 用 FacadeDefaultTools）。
	// 各 agent 可用 allowedFacadeTools 覆盖（逐 agent 收紧或放宽）。
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	// DisableAuth 关掉门面的 Bearer token 鉴权（缺省开启鉴权）。逃生口：若某家
	// agent 不转发 session/new 里声明的自定义 header（docs/AGENT-TOOL-INJECTION.md
	// §3.5 的未实测风险），关掉鉴权让它的门面工具可用——门面只听回环，风险同级
	// 于 Admin 服务本机暴露。
	DisableAuth *bool `yaml:"disableAuth,omitempty" json:"disableAuth,omitempty"`
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
	// HandshakeTimeoutSeconds 握手超时覆盖（0 = 用全局值）：npx 型 agent 冷启动
	// 慢，可以只给它放宽，不必连别的 agent 一起放宽。
	HandshakeTimeoutSeconds int `yaml:"handshakeTimeoutSeconds,omitempty" json:"handshakeTimeoutSeconds,omitempty"`
	// SessionTimeoutSeconds 建会话超时覆盖（0 = 用全局值）。
	SessionTimeoutSeconds int `yaml:"sessionTimeoutSeconds,omitempty" json:"sessionTimeoutSeconds,omitempty"`
	// DegradeResume 标记该 agent 的 session/load 恢复保真度实测不可靠：
	// 重开 thread 不再尝试重接，直接新开会话并标注「上下文已重置」。
	DegradeResume bool `yaml:"degradeResume,omitempty" json:"degradeResume,omitempty"`
	// OneSessionPerProcess 让这个 agent 的每个会话独占一条连接（一个进程）。
	// 缺省：CodeBuddy 为 true（一条连接只有一条 prompt 渲染循环，多会话共连接
	// 会串行）；其余 agent 为 false（沿用 agent+目录 一进程的复用模型）。
	// 显式设置优先于 acp.oneSessionPerProcess 全局值。
	OneSessionPerProcess *bool `yaml:"oneSessionPerProcess,omitempty" json:"oneSessionPerProcess,omitempty"`
	// AllowedFacadeTools 该 agent 可调用的 Friday 门面工具白名单（空 = 用全局
	// acp.facade.tools）。只该比全局更紧或按需放宽；门面按 token 逐会话执行。
	AllowedFacadeTools []string `yaml:"allowedFacadeTools,omitempty" json:"allowedFacadeTools,omitempty"`
	// Version 安装时登记的版本戳（M-4）：registry 条目安装/升级时写入，目录页
	// 据此提示「有新版本」。仅元数据，不影响启动。
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// Builtin 标出这是代码内置的 agent（内置 Friday），不是用户配置的外部进程：
	// 由 Normalized 注入、不写回 config.yaml、设置页不可编辑或删除。
	// 同名配置条目存在时以配置为准（用户可以真的配一台叫 friday 的外部 agent）。
	Builtin bool `yaml:"builtin,omitempty" json:"builtin,omitempty"`
}

// IsEnabled 返回该 agent 是否启用（缺省启用）。
func (a AcpAgentConfig) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// Normalized 填充 0 值默认。不修改调用者。
func (c AcpConfig) Normalized() AcpConfig {
	// MaxConcurrent 的 0 本身就是有效值（不限），不再回填默认值。
	if c.IdleKillSeconds == 0 {
		c.IdleKillSeconds = AcpDefaultIdleKillSeconds
	}
	if c.HandshakeTimeoutSeconds <= 0 {
		c.HandshakeTimeoutSeconds = AcpDefaultHandshakeSeconds
	}
	if c.SessionTimeoutSeconds <= 0 {
		c.SessionTimeoutSeconds = AcpDefaultSessionSeconds
	}
	if c.Facade == nil {
		c.Facade = &AcpFacadeConfig{}
	}
	if len(c.Facade.Tools) == 0 {
		c.Facade.Tools = append([]string(nil), FacadeDefaultTools...)
	}
	c.Agents = withBuiltinAgent(c.Agents)
	return c
}

// FacadeEnabled 返回工具门面是否开启（缺省开启）。
func (c AcpConfig) FacadeEnabled() bool {
	return c.Facade == nil || c.Facade.Enabled == nil || *c.Facade.Enabled
}

// FacadeAuthDisabled 返回门面是否关闭鉴权（缺省开启鉴权）。
func (c AcpConfig) FacadeAuthDisabled() bool {
	return c.Facade != nil && c.Facade.DisableAuth != nil && *c.Facade.DisableAuth
}

// FacadeTools 返回门面白名单（Normalized 后必有值；未归一化的裸配置回退默认清单）。
func (c AcpConfig) FacadeTools() []string {
	if c.Facade != nil && len(c.Facade.Tools) > 0 {
		return c.Facade.Tools
	}
	return FacadeDefaultTools
}

// withBuiltinAgent 把内置 Friday 排在 agent 清单最前：工作区新建会话时它就在那里，
// 不必先装一台外部 agent 才能用。清单里已有同名条目（用户自己配的 friday）就不注入、
// 不重复，配置说了算。
//
// 注意返回的是新切片：调用者那份 Agents 的底层数组不动（Normalized 常在共享快照上跑）。
func withBuiltinAgent(in []AcpAgentConfig) []AcpAgentConfig {
	for _, a := range in {
		if a.Name == BuiltinAgentName {
			return in
		}
	}
	out := make([]AcpAgentConfig, 0, len(in)+1)
	return append(out, append([]AcpAgentConfig{BuiltinAgent()}, in...)...)
}

// WithoutBuiltin 剔除内置条目（写回 config.yaml 前用）：内置 Friday 是代码给的，
// 不该落到用户的配置文件里——那里每一条都该是「要拉起的外部进程」。
func (c AcpConfig) WithoutBuiltin() AcpConfig {
	out := make([]AcpAgentConfig, 0, len(c.Agents))
	for _, a := range c.Agents {
		if a.Builtin {
			continue
		}
		out = append(out, a)
	}
	c.Agents = out
	return c
}

// HandshakeSeconds 返回某 agent 的生效握手超时（条目覆盖优先）。
func (c AcpConfig) HandshakeSeconds(agent AcpAgentConfig) int {
	if agent.HandshakeTimeoutSeconds > 0 {
		return agent.HandshakeTimeoutSeconds
	}
	return c.HandshakeTimeoutSeconds
}

// SessionSeconds 返回某 agent 的生效建会话超时（条目覆盖优先）。
func (c AcpConfig) SessionSeconds(agent AcpAgentConfig) int {
	if agent.SessionTimeoutSeconds > 0 {
		return agent.SessionTimeoutSeconds
	}
	return c.SessionTimeoutSeconds
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
		if a.Command == "" && !a.Builtin {
			return fmt.Errorf("config: acp.agents[%s].command 不能为空", a.Name)
		}
		if a.HandshakeTimeoutSeconds < 0 {
			return fmt.Errorf("config: acp.agents[%s].handshakeTimeoutSeconds 不能为负", a.Name)
		}
		if a.SessionTimeoutSeconds < 0 {
			return fmt.Errorf("config: acp.agents[%s].sessionTimeoutSeconds 不能为负", a.Name)
		}
	}
	if c.IdleKillSeconds < 0 ||
		c.HandshakeTimeoutSeconds < 0 || c.SessionTimeoutSeconds < 0 {
		return fmt.Errorf("config: acp 各项数值不能为负")
	}
	if c.Facade != nil {
		for _, t := range c.Facade.Tools {
			if t == "" {
				return fmt.Errorf("config: acp.facade.tools 存在空工具名")
			}
		}
	}
	for _, a := range c.Agents {
		for _, t := range a.AllowedFacadeTools {
			if t == "" {
				return fmt.Errorf("config: acp.agents[%s].allowedFacadeTools 存在空工具名", a.Name)
			}
		}
	}
	return nil
}

// UpdateAcp 热更新 config.yaml 中的 acp 段（整体替换该映射，保留文件其余部分
// 的注释与结构；acp 段不存在时追加）。cfg 须已通过 Validate。以 yaml.Node 方式
// 编辑，临时文件 + rename 原子写回。
func UpdateAcp(path string, cfg AcpConfig) error {
	// 内置条目（Normalized 注入的 friday）不落盘：设置页保存的是整份清单，
	// 不剔除就会把代码给的内置 agent 写进用户的 config.yaml（而它没有 command）。
	cfg = cfg.WithoutBuiltin()
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
	if cfg.IdleKillSeconds > 0 {
		intPair(section, "idleKillSeconds", cfg.IdleKillSeconds)
	}
	if cfg.HandshakeTimeoutSeconds > 0 {
		intPair(section, "handshakeTimeoutSeconds", cfg.HandshakeTimeoutSeconds)
	}
	if cfg.SessionTimeoutSeconds > 0 {
		intPair(section, "sessionTimeoutSeconds", cfg.SessionTimeoutSeconds)
	}
	if cfg.ShowThought != nil && !*cfg.ShowThought {
		boolPair(section, "showThought", false)
	}
	// 工具门面：只在偏离代码默认时落盘（显式关闭、白名单与 FacadeDefaultTools
	// 不同、或显式关鉴权），默认形态不写进 config.yaml，与「0 值沿用全局」的段落
	// 风格一致。
	if cfg.Facade != nil {
		fac := &yaml.Node{Kind: yaml.MappingNode}
		if cfg.Facade.Enabled != nil && !*cfg.Facade.Enabled {
			boolPair(fac, "enabled", false)
		}
		if cfg.Facade.DisableAuth != nil && *cfg.Facade.DisableAuth {
			boolPair(fac, "disableAuth", true)
		}
		if len(cfg.Facade.Tools) > 0 && !sameStrings(cfg.Facade.Tools, FacadeDefaultTools) {
			fac.Content = append(fac.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "tools"}, strSeqNode(cfg.Facade.Tools))
		}
		if len(fac.Content) > 0 {
			section.Content = append(section.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "facade"}, fac)
		}
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
			if len(a.AllowedFacadeTools) > 0 {
				item.Content = append(item.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "allowedFacadeTools"},
					strSeqNode(a.AllowedFacadeTools))
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
			if a.HandshakeTimeoutSeconds > 0 {
				intPair(item, "handshakeTimeoutSeconds", a.HandshakeTimeoutSeconds)
			}
			if a.SessionTimeoutSeconds > 0 {
				intPair(item, "sessionTimeoutSeconds", a.SessionTimeoutSeconds)
			}
			if a.DegradeResume {
				boolPair(item, "degradeResume", true)
			}
			if a.Version != "" {
				strPair(item, "version", a.Version)
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

// strSeqNode 构造字符串序列的 yaml 节点（args / facade.tools /
// allowedFacadeTools 共用）。
func strSeqNode(values []string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, v := range values {
		seq.Content = append(seq.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"})
	}
	return seq
}

// sameStrings 判断两个切片作为集合是否相同（排序后逐项比较；白名单场景下
// 重复项没有语义，顺序也不重要）。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}
