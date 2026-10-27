// manager.go 是 MCP 服务器的生命周期管理器：按 config 声明拉起/停止/重启
// 服务器进程，把连接到的工具以 mcp_<server>_<tool> 前缀注册进 agent 注册表；
// 进程崩溃自动重启（数秒内恢复），config 变更后 Apply 增量生效（工具随之增减）。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
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
	// Auth 远程服务器的鉴权方式：空或 header 用 headers 静态头；oauth 走框架的
	// 通用 MCP OAuth 引擎（发现 + 动态注册 + PKCE + loopback 回调 + 令牌刷新）。
	Auth string `yaml:"auth"`
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
	// Auth 配置声明的鉴权方式（空=静态头）。
	Auth string `json:"auth,omitempty"`
	// AuthState OAuth 授权状态（AuthState* 常量）；非 OAuth 服务器为空。
	AuthState string `json:"authState,omitempty"`
	// AuthMessage 授权侧的可读说明（失败原因或下一步动作，不含令牌）。
	AuthMessage string `json:"authMessage,omitempty"`
}

// Manager 管理全部 MCP 服务器：Apply 同步配置，RegisterTools 注册工具，
// Restart 手动重连，进程退出自动重启。
type Manager struct {
	mu       sync.Mutex
	cfgs     []ServerConfig
	clients  map[string]Conn
	status   map[string]*Status
	oauth    *OAuth // 远程服务器的 OAuth 会话服务（未开启为 nil）
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

// EnableOAuth 开启远程服务器的 OAuth 鉴权：会话落盘到 path（0600，含
// client_secret 与 refresh_token），授权成功后自动重连该服务器，工具随即出现，
// 用户不必再点一次「重连」。启动时调用一次（见 cmd/robot/main.go）。
func (m *Manager) EnableOAuth(path string) {
	m.SetOAuth(NewOAuth(path))
}

// SetOAuth 注入授权服务（测试可传内存实现或 fake 服务商的客户端）。
func (m *Manager) SetOAuth(svc *OAuth) {
	if svc != nil {
		svc.SetAuthorizedHandler(func(name string) {
			if err := m.Restart(name); err != nil {
				log.Printf("[MCP] 服务器 %s 授权后重连失败: %v", name, err)
				return
			}
			log.Printf("[MCP] 服务器 %s 授权完成，已连接", name)
		})
	}
	m.mu.Lock()
	m.oauth = svc
	m.mu.Unlock()
}

// applyAuthConfig 把服务器定义同步给 OAuth 服务（鉴权方式、资源地址）。
// 未开启 OAuth 服务时只做一件事：声明了 oauth 的服务器直接记为不可用，
// 免得用户在设置页配了却静默连不上。
func (m *Manager) applyAuthConfig(cfg ServerConfig) string {
	m.mu.Lock()
	svc := m.oauth
	m.mu.Unlock()
	authMode := strings.TrimSpace(cfg.Auth)
	if authMode == "" {
		authMode = AuthHeader
	}
	if svc == nil {
		return authMode
	}
	svc.Configure(cfg.Name, authMode, cfg.URL)
	return authMode
}

// Authorize 为一条声明了 OAuth 的远程服务器起一次浏览器授权，返回要打开的
// 授权地址（由前端交给系统浏览器，后台不猜桌面环境怎么开）。
func (m *Manager) Authorize(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	svc := m.oauth
	cfg := m.cfgByName(name)
	m.mu.Unlock()
	if svc == nil {
		return "", fmt.Errorf("未启用 MCP 授权存储（OAuth 不可用）")
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return "", fmt.Errorf("未找到 MCP 服务器 %s", name)
	}
	if cfg.URL == "" {
		return "", fmt.Errorf("服务器 %s 是本地命令，不需要授权", name)
	}
	if authMode := strings.TrimSpace(cfg.Auth); authMode != AuthOAuth {
		return "", fmt.Errorf("服务器 %s 的鉴权方式不是 OAuth（当前：%s）", name, orDefault(authMode, "静态请求头"))
	}
	return svc.Authorize(ctx, name)
}

// AuthState 返回一台服务器的 OAuth 授权状态与说明（非 OAuth 服务器为空串）。
func (m *Manager) AuthState(name string) (state, message string) {
	m.mu.Lock()
	svc := m.oauth
	m.mu.Unlock()
	if svc == nil {
		return "", ""
	}
	return svc.StateOf(name)
}

// AuthURL 返回一台服务器进行中的浏览器授权地址（没有进行中流程时为空串）。
func (m *Manager) AuthURL(name string) string {
	m.mu.Lock()
	svc := m.oauth
	m.mu.Unlock()
	if svc == nil {
		return ""
	}
	return svc.AuthURL(name)
}

// Disconnect 撤销并清除一台服务器的 OAuth 会话（令牌本地删除 + 服务商侧撤销）。
// 断开后连接随之失效，状态回到「待授权」。
func (m *Manager) Disconnect(ctx context.Context, name string) error {
	m.mu.Lock()
	svc := m.oauth
	cfg := m.cfgByName(name)
	cli, running := m.clients[name]
	if running {
		cli.Close()
		delete(m.clients, name)
	}
	st := m.status[name]
	m.mu.Unlock()
	if svc == nil {
		return fmt.Errorf("未启用 MCP 授权存储（OAuth 不可用）")
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("未找到 MCP 服务器 %s", name)
	}
	if running {
		m.notifyChange()
	}
	if st != nil {
		m.mu.Lock()
		st.Running = false
		st.LastError = "已断开授权"
		m.mu.Unlock()
	}
	return svc.Disconnect(ctx, name)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
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
	var dropped []string
	for name, cli := range m.clients {
		want, keep := desired[name]
		if !keep || !m.sameServer(m.cfgByName(name), want) {
			cli.Close()
			delete(m.clients, name)
		}
	}
	for name := range m.status {
		if _, keep := desired[name]; !keep {
			dropped = append(dropped, name)
		}
	}
	m.cfgs = cfgs
	oauth := m.oauth
	m.mu.Unlock()

	// 已移除的服务器清掉授权侧状态与会话（令牌不留孤本）。
	if oauth != nil {
		for _, name := range dropped {
			oauth.Forget(name)
		}
	}

	// 同步鉴权声明（必须先于拉起：Provider 依据它决定是否注入令牌）。
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m.applyAuthConfig(desired[name])
	}

	// 启动缺失的服务器（按名字顺序，日志与错误归属稳定）。
	for _, name := range names {
		m.mu.Lock()
		_, running := m.clients[name]
		m.mu.Unlock()
		if running {
			continue
		}
		if err := m.start(ctx, desired[name]); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.notifyChange()
	return firstErr
}

// sameServer 判断两份服务器定义是否等价（决定 Apply 时是否需要重启）。
// 鉴权方式也算定义的一部分：header ↔ oauth 来回切都要重连才生效。
func (m *Manager) sameServer(a, b ServerConfig) bool {
	return a.Command == b.Command && a.URL == b.URL &&
		strings.Join(a.Args, "\x00") == strings.Join(b.Args, "\x00") &&
		fmt.Sprint(a.Env) == fmt.Sprint(b.Env) &&
		fmt.Sprint(a.Headers) == fmt.Sprint(b.Headers) &&
		a.Auth == b.Auth
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
		m.mu.Lock()
		svc := m.oauth
		m.mu.Unlock()
		var tokens TokenProvider
		if svc != nil {
			tokens = svc.Provider(name) // 未声明 OAuth 的服务器返回 nil
		}
		conn, err = StartHTTPAuth(name, cfg.URL, cfg.Headers, tokens)
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
	st.Auth = normalizeAuth(cfg.Auth)
	if err != nil {
		st.Running = false
		if errors.Is(err, ErrMCPAuthRequired) {
			// 令牌缺失/失效不是"连接失败"，而是等用户去授权：给可操作的提示。
			st.LastError = "需要授权：点「授权」在浏览器里登录服务商"
		} else {
			st.LastError = err.Error()
		}
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

// normalizeAuth 归一配置里的鉴权方式（空值等同 header）。
func normalizeAuth(auth string) string {
	if strings.TrimSpace(auth) == "" {
		return AuthHeader
	}
	return strings.TrimSpace(auth)
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
	svc := m.oauth
	out := make([]Status, 0, len(m.status))
	for _, cfg := range m.cfgs {
		st, ok := m.status[cfg.Name]
		if !ok {
			st = &Status{Name: cfg.Name, Command: cfg.Command, URL: cfg.URL}
			m.status[cfg.Name] = st
		}
		st.Auth = normalizeAuth(cfg.Auth)
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
	m.mu.Unlock()
	// 授权状态来自 OAuth 服务（另一把锁），在管理器锁外取，避免交叉持锁。
	if svc != nil {
		for i := range out {
			state, msg := svc.StateOf(out[i].Name)
			out[i].AuthState, out[i].AuthMessage = state, msg
		}
	}
	return out
}

// ServerTools 返回某在线服务器当前提供的工具清单（未连接返回 nil）。
func (m *Manager) ServerTools(name string) []Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cli, ok := m.clients[name]; ok {
		return cli.Tools()
	}
	return nil
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
