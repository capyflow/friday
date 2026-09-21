// manager.go 是 MCP 服务器的生命周期管理器：按 config 声明拉起/停止/重启
// 服务器进程，把连接到的工具以 mcp_<server>_<tool> 前缀注册进 agent 注册表；
// 进程崩溃自动重启（数秒内恢复），config 变更后 Apply 增量生效（工具随之增减）。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
)

// ToolPrefix 返回某服务器工具在注册表中的前缀（防与其他工具重名冲突）。
func ToolPrefix(server string) string {
	return "mcp_" + sanitize(server) + "_"
}

// sanitize 把服务器/工具名归一为工具名安全字符。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// ServerConfig 是一台 MCP 服务器的声明（来自 config mcp.servers[]）：
// Command 非空走 stdio（本机子进程）；URL 非空走 Streamable HTTP（远程服务器）。
// 两者必须二选一。
type ServerConfig struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
}

// Conn 是一台 MCP 服务器连接的统一抽象（stdio 子进程或远程 HTTP）。
type Conn interface {
	Tools() []Tool
	RefreshTools() error
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
	Close()
}

// Status 是一台服务器的连接状态（设置页展示）。
type Status struct {
	Name      string    `json:"name"`
	Command   string    `json:"command,omitempty"`
	URL       string    `json:"url,omitempty"`
	Running   bool      `json:"running"`
	Tools     int       `json:"tools"`
	Restarts  int       `json:"restarts"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	LastError string    `json:"lastError,omitempty"`
}

// Manager 管理全部 MCP 服务器：Apply 同步配置，RegisterTools 注册工具，
// Restart 手动重连，进程退出自动重启。
type Manager struct {
	mu       sync.Mutex
	cfgs     []ServerConfig
	clients  map[string]Conn
	status   map[string]*Status
	onChange func() // 工具清单可能变化后的重建回调（重建注册表与 Agent）
	closed   bool
}

// NewManager 创建管理器。onChange 在工具清单可能变化后被调用（可为 nil）。
func NewManager(onChange func()) *Manager {
	return &Manager{
		clients:  make(map[string]Conn),
		status:   make(map[string]*Status),
		onChange: onChange,
	}
}

// Apply 按 cfgs 同步服务器集合：新增的拉起，移除的停止，命令/参数变更的
// 重启。任一服务器连不上不阻塞其余服务器（错误记入其状态）。全部处理完
// 后调用一次 onChange（工具清单可能变化）。返回首个连接错误（供日志/提示）。
func (m *Manager) Apply(ctx context.Context, cfgs []ServerConfig) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("MCP 管理器已关闭")
	}
	desired := make(map[string]ServerConfig, len(cfgs))
	for _, c := range cfgs {
		if strings.TrimSpace(c.Name) == "" ||
			(strings.TrimSpace(c.Command) == "" && strings.TrimSpace(c.URL) == "") {
			continue // 未声明完整（缺名称，或命令与 URL 均空）的服务器跳过
		}
		desired[c.Name] = c
	}
	// 停止不再声明（或定义变更）的服务器。
	var firstErr error
	for name, cli := range m.clients {
		want, keep := desired[name]
		if !keep || !m.sameServer(m.cfgByName(name), want) {
			cli.Close()
			delete(m.clients, name)
		}
	}
	m.cfgs = cfgs
	m.mu.Unlock()

	// 启动缺失的服务器。
	for name, cfg := range desired {
		m.mu.Lock()
		_, running := m.clients[name]
		m.mu.Unlock()
		if running {
			continue
		}
		if err := m.start(ctx, cfg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.notifyChange()
	return firstErr
}

// sameServer 判断两份服务器定义是否等价（决定 Apply 时是否需要重启）。
func (m *Manager) sameServer(a, b ServerConfig) bool {
	return a.Command == b.Command && a.URL == b.URL &&
		strings.Join(a.Args, "\x00") == strings.Join(b.Args, "\x00") &&
		fmt.Sprint(a.Env) == fmt.Sprint(b.Env) &&
		fmt.Sprint(a.Headers) == fmt.Sprint(b.Headers)
}

// cfgByName 取当前配置中某服务器的定义。
func (m *Manager) cfgByName(name string) ServerConfig {
	for _, c := range m.cfgs {
		if c.Name == name {
			return c
		}
	}
	return ServerConfig{}
}

// start 拉起一台服务器并完成握手；成功登记连接，失败把错误记入状态。
func (m *Manager) start(ctx context.Context, cfg ServerConfig) error {
	name := cfg.Name
	var conn Conn
	var err error
	if cfg.URL != "" {
		conn, err = StartHTTP(name, cfg.URL, cfg.Headers)
	} else {
		conn, err = Start(name, cfg.Command, cfg.Args, cfg.Env, func() { m.handleExit(name) })
	}
	m.mu.Lock()
	st, ok := m.status[name]
	if !ok {
		st = &Status{Name: name, Command: cfg.Command, URL: cfg.URL}
		m.status[name] = st
	}
	st.Command = cfg.Command
	st.URL = cfg.URL
	if err != nil {
		st.Running = false
		st.LastError = err.Error()
		m.mu.Unlock()
		return err
	}
	m.clients[name] = conn
	st.Running = true
	st.StartedAt = time.Now()
	st.LastError = ""
	st.Tools = len(conn.Tools())
	m.mu.Unlock()
	return nil
}

// notifyChange 通知外部工具清单可能已变化。
func (m *Manager) notifyChange() {
	if m.onChange != nil {
		m.onChange()
	}
}

// handleExit 进程意外退出：记状态，后台自动重启（2 秒后重试，直至成功或管理器关闭）。
func (m *Manager) handleExit(name string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if st, ok := m.status[name]; ok {
		st.Running = false
		st.Restarts++
		st.LastError = "进程退出，等待自动重启"
	}
	delete(m.clients, name)
	cfg := m.cfgByName(name)
	m.mu.Unlock()

	if cfg.Name == "" {
		return // 配置中已移除，不重启
	}
	log.Printf("[mcp] 服务器 %s 进程退出，2 秒后自动重启", name)
	go func() {
		time.Sleep(2 * time.Second)
		for {
			m.mu.Lock()
			closed := m.closed
			_, running := m.clients[name]
			m.mu.Unlock()
			if closed || running {
				return
			}
			cur := m.cfgByName(name)
			if cur.Name == "" {
				return // 配置移除，不再重启
			}
			ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout*2)
			err := m.start(ctx, cur)
			cancel()
			if err == nil {
				log.Printf("[mcp] 服务器 %s 已自动重启", name)
				m.notifyChange()
				return
			}
			time.Sleep(3 * time.Second)
		}
	}()
}

// RegisterTools 把所有在线服务器的工具注册进注册表（名字加 mcp_<server>_ 前缀）。
func (m *Manager) RegisterTools(r *vxagent.Registry) error {
	m.mu.Lock()
	names := make([]string, 0, len(m.clients))
	for name := range m.clients {
		names = append(names, name)
	}
	sort.Strings(names)
	conns := make([]Conn, 0, len(names))
	for _, name := range names {
		conns = append(conns, m.clients[name])
	}
	m.mu.Unlock()
	for i, cli := range conns {
		for _, t := range cli.Tools() {
			schema := map[string]any{"type": "object"}
			if len(t.Schema) > 0 {
				_ = json.Unmarshal(t.Schema, &schema)
			}
			tool := &ServerTool{
				Manager:   m,
				Server:    names[i],
				Tool:      t,
				SchemaMap: schema,
			}
			if err := r.Add(tool); err != nil {
				return fmt.Errorf("注册 MCP 工具 %s: %w", tool.Name(), err)
			}
		}
	}
	return nil
}

// ServerTool 把一个 MCP 工具适配成 vxagent.Tool。
type ServerTool struct {
	Manager   *Manager
	Server    string
	Tool      Tool
	SchemaMap map[string]any
}

func (t *ServerTool) Name() string {
	return ToolPrefix(t.Server) + sanitize(t.Tool.Name)
}

func (t *ServerTool) Description() string {
	desc := t.Tool.Description
	if desc == "" {
		desc = "MCP 工具（服务器 " + t.Server + "）"
	}
	return "【MCP·" + t.Server + "】" + desc
}

func (t *ServerTool) Schema() map[string]any { return t.SchemaMap }

func (t *ServerTool) Call(ctx context.Context, args map[string]any) (string, error) {
	m := t.Manager
	m.mu.Lock()
	cli, ok := m.clients[t.Server]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("MCP 服务器 %s 未连接", t.Server)
	}
	// 转发原始工具名（服务器侧的名字，不含前缀）。
	raw := t.Tool.Name
	return cli.CallTool(ctx, raw, args)
}

// StatusList 返回全部服务器状态（按名字排序由调用方处理）。
func (m *Manager) StatusList() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.status))
	for _, cfg := range m.cfgs {
		st, ok := m.status[cfg.Name]
		if !ok {
			st = &Status{Name: cfg.Name, Command: cfg.Command}
			m.status[cfg.Name] = st
		}
		out = append(out, *st)
	}
	// 已停止但状态残留的服务器也列出（如被移除后未清状态的情形）。
	seen := map[string]bool{}
	for _, s := range out {
		seen[s.Name] = true
	}
	for name, st := range m.status {
		if !seen[name] {
			out = append(out, *st)
		}
	}
	for i := range out {
		if cli, ok := m.clients[out[i].Name]; ok {
			out[i].Running = true
			out[i].Tools = len(cli.Tools())
		} else {
			out[i].Running = false
		}
	}
	return out
}

// Restart 手动重连一台服务器：断开旧进程并按当前配置重新拉起（用于设置页重连按钮）。
func (m *Manager) Restart(name string) error {
	m.mu.Lock()
	cli, ok := m.clients[name]
	if ok {
		cli.Close()
		delete(m.clients, name)
	}
	cfg := m.cfgByName(name)
	m.mu.Unlock()
	if cfg.Name == "" {
		return fmt.Errorf("未找到 MCP 服务器 %s", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout*2)
	defer cancel()
	if err := m.start(ctx, cfg); err != nil {
		return err
	}
	m.notifyChange()
	return nil
}

// Close 停止全部服务器（程序退出时调用）。
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	clients := m.clients
	m.clients = make(map[string]Conn)
	m.mu.Unlock()
	for _, c := range clients {
		c.Close()
	}
}
