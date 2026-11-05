// Package terminal 在 app 里开一个真正的交互式终端：一条 PTY 上跑用户的 shell，
// 输出一块块搬给界面（xterm.js 渲染），键盘与窗口尺寸从界面搬回来。
//
// 为什么在内核侧而不是桌面壳侧：浏览器直访与桌面壳加载的是同一份页面，内核起 PTY
// 两处都能用，壳里一行 Rust 都不用改；反过来在 Tauri 侧做 PTY，浏览器形态就没终端了。
//
// 安全边界要说白：人在终端里敲的命令不经过 exec_command 那套白名单/黑名单/审批卡片
// ——这是"内置终端"的定义，不是实现能补的东西。所以两条硬规则：只接受本机回环地址
// 来的连接（在 admin 层判，配置也关不掉这条），以及沿用登录态（token 走查询参数，
// 浏览器的 WebSocket 加不了请求头）。远程/Docker 部署里这个功能直接不可用，不是"降级"。
//
// 生命周期：一条连接对应一个终端。连接断开就把那条 shell 所在的进程组收掉，不留孤儿
// ——刷新页面等于重开一个终端，正在跑的命令会被终止，界面上写清了这一点。同时开几个
// 由 maxSessions 兜底；内核退出时 CloseAll 统一收尾。
package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 默认上限：四个够并行看几路输出，再多就是拿本机内存换"舍不得关"。
const (
	DefaultMaxSessions = 4
	maxReadChunk       = 32 << 10 // 单次从 PTY 读的最大字节
	outputBuffer       = 64       // 输出通道缓冲；写不过来就自然回压给 PTY
)

// Config 是终端的运行参数（由 config 层映射过来，热生效走 cfgFn）。
type Config struct {
	// Enabled 关掉时连不上新终端，已开的也不受影响（内核启动时读一次）。
	Enabled bool
	// MaxSessions 同时可开的终端数（<=0 用 DefaultMaxSessions）。
	MaxSessions int
	// Shell 指定用的 shell（空则按 $SHELL → zsh → bash → sh）。
	Shell string
}

// Max 返回生效的并发上限。
func (c Config) Max() int {
	if c.MaxSessions <= 0 {
		return DefaultMaxSessions
	}
	return c.MaxSessions
}

// Exit 是终端进程的收尾结果。
type Exit struct {
	Code int
	Err  error
}

// SessionInfo 是终端会话的概要（状态页/日志用）。
type SessionInfo struct {
	ID        string    `json:"id"`
	Shell     string    `json:"shell"`
	Cwd       string    `json:"cwd"`
	StartedAt time.Time `json:"startedAt"`
}

// Session 是一个终端会话：一条 PTY + 一个 shell 进程。
//
// 输出走 out（读 PTY 的泵往里写，通道满了对 PTY 形成自然回压——跟真实终端
// 一样，客户端跟不上就让它慢下来，而不是丢字节）；进程结束走 done。
type Session struct {
	ID        string
	Shell     string
	Cwd       string
	StartedAt time.Time

	pty  *os.File
	cmd  *exec.Cmd
	out  chan []byte
	done chan Exit

	// manager 让会话自己结束时从活跃表里摘掉——上限算的是"还活着的"，
	// 跑完 `exit` 的终端不该继续占一个名额。
	manager *Manager

	closeOnce sync.Once
	closed    chan struct{} // Close 后停止把新输出塞进通道（进程已收，残余字节没意义）
}

// Output 返回输出块通道（终端字节流，按 PTY 读到的边界分块）。
func (s *Session) Output() <-chan []byte { return s.out }

// Done 返回进程退出信号（关闭即已结束）。
func (s *Session) Done() <-chan Exit { return s.done }

// Write 把键盘输入原样送进 PTY。
func (s *Session) Write(p []byte) (int, error) {
	if s.pty == nil {
		return 0, errors.New("终端已结束")
	}
	return s.pty.Write(p)
}

// Resize 同步窗口尺寸（界面列数/行数变了要立刻报，否则 TUI 程序排版会错）。
func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("终端尺寸非法：%dx%d", cols, rows)
	}
	if s.pty == nil {
		return errors.New("终端已结束")
	}
	return resizePTY(s.pty, cols, rows)
}

// Close 结束终端：先收整个进程组，再关掉 PTY。幂等。
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		terminatePTY(s.cmd, s.done)
		if s.pty != nil {
			_ = s.pty.Close()
		}
	})
	return nil
}

// Info 返回会话概要。
func (s *Session) Info() SessionInfo {
	return SessionInfo{ID: s.ID, Shell: s.Shell, Cwd: s.Cwd, StartedAt: s.StartedAt}
}

// Manager 持有全部终端会话。
type Manager struct {
	mu     sync.Mutex
	cfgFn  func() Config
	active map[string]*Session
	order  []string
}

// NewManager 创建管理器。cfgFn 每次读取当前配置（设置页改完即时生效，不重启）。
func NewManager(cfgFn func() Config) *Manager {
	return &Manager{cfgFn: cfgFn, active: map[string]*Session{}}
}

// Start 起一个终端：cwd 为空用内核的工作目录，shell 按配置解析。
// 报错都说清下一步（上限满了先关一个、目录不存在就换一个、Windows 走不了 ConPTY）。
func (m *Manager) Start(cols, rows int, cwd string) (*Session, error) {
	cfg := m.cfgFn()
	if !cfg.Enabled {
		return nil, errors.New("内置终端未开启（config.yaml 里 terminal.enabled 设为 true 后重启）")
	}
	if cols <= 0 || rows <= 0 {
		return nil, fmt.Errorf("终端初始尺寸非法：%dx%d（界面还没量到尺寸？刷新一次）", cols, rows)
	}
	shell, err := resolveShell(cfg.Shell)
	if err != nil {
		return nil, err
	}
	dir, err := resolveCwd(cwd)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(shell)
	cmd.Dir = dir
	// TERM 必须是 xterm 系，否则界面里跑 vim/top 会当成 dumb terminal；
	// 其余环境照搬内核进程，用户自己的 PATH/HOME/别名配置才照常生效。
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	m.mu.Lock()
	if len(m.active) >= cfg.Max() {
		n := len(m.active)
		max := cfg.Max()
		m.mu.Unlock()
		return nil, fmt.Errorf("已经开了 %d 个终端，上限 %d：先关掉一个再开（上限见 config.yaml 的 terminal.maxSessions）", n, max)
	}
	m.mu.Unlock()

	ptyFile, err := startPTY(cmd, cols, rows)
	if err != nil {
		return nil, fmt.Errorf("启动终端失败: %w", err)
	}
	s := &Session{
		ID:        newID(),
		manager:   m,
		Shell:     shell,
		Cwd:       dir,
		StartedAt: time.Now(),
		pty:       ptyFile,
		cmd:       cmd,
		out:       make(chan []byte, outputBuffer),
		done:      make(chan Exit, 1),
		closed:    make(chan struct{}),
	}
	m.mu.Lock()
	m.active[s.ID] = s
	m.order = append(m.order, s.ID)
	m.mu.Unlock()

	go s.pumpOutput()
	go s.wait()
	log.Printf("[终端] 开一个终端 %s（shell=%s，目录=%s，%dx%d）", s.ID, shell, dir, cols, rows)
	return s, nil
}

// forget 把结束的会话移出活跃表（上限按活跃数算）。
func (m *Manager) forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.active[id]; !ok {
		return
	}
	delete(m.active, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

// Config 返回当前生效的运行参数（状态接口用，免得调用方再拼一遍 cfgFn）。
func (m *Manager) Config() Config { return m.cfgFn() }

// Count 返回当前活跃终端数。
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// List 返回活跃终端概要。
func (m *Manager) List() []SessionInfo {
	m.mu.Lock()
	ids := append([]string{}, m.order...)
	m.mu.Unlock()
	out := make([]SessionInfo, 0, len(ids))
	for _, id := range ids {
		m.mu.Lock()
		s := m.active[id]
		m.mu.Unlock()
		if s != nil {
			out = append(out, s.Info())
		}
	}
	return out
}

// CloseAll 收掉全部终端（内核退出时调用，避免留下挂在已关 PTY 上的进程组）。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.active))
	for _, s := range m.active {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}

// pumpOutput 把 PTY 输出搬到通道；通道满了就等（回压给 PTY，不丢字节）。
// 进程退出时 PTY 读会报错（Linux 上是 EIO），按结束处理。
func (s *Session) pumpOutput() {
	defer close(s.out)
	buf := make([]byte, maxReadChunk)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			chunk := append([]byte{}, buf[:n]...)
			select {
			case <-s.closed:
				return // 已经关掉这个终端，残余输出没人要了
			case s.out <- chunk:
			}
		}
		if err != nil {
			return
		}
	}
}

// wait 等 shell 退出，把结果交出去并从管理器摘掉；PTY 由 pumpOutput 收尾。
func (s *Session) wait() {
	err := s.cmd.Wait()
	exit := Exit{}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit.Code = ee.ExitCode()
		} else {
			exit.Err = err
		}
	}
	s.done <- exit
	if m := s.manager; m != nil {
		m.forget(s.ID)
	}
	log.Printf("[终端] 终端 %s 结束（code=%d err=%v）", s.ID, exit.Code, exit.Err)
}

// resolveShell 定下用哪个 shell：配置指定 → $SHELL → 常见三个里第一个存在的。
// 配置里写了个不存在的 shell 要直说，别悄悄换一个——用户会以为自己在 zsh 里跑。
func resolveShell(cfg string) (string, error) {
	if p := strings.TrimSpace(cfg); p != "" {
		if executable(p) {
			return p, nil
		}
		return "", fmt.Errorf("配置里的 shell %q 不可执行（留空则按 $SHELL 走）", p)
	}
	if env := strings.TrimSpace(os.Getenv("SHELL")); env != "" && executable(env) {
		return env, nil
	}
	for _, cand := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if executable(cand) {
			return cand, nil
		}
	}
	return "", errors.New("本机找不到可用的 shell（/bin/sh 都没有）")
}

func executable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// resolveCwd 定下终端的起始目录：空 = 内核工作目录；给了就必须存在且是目录。
func resolveCwd(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ".", nil
		}
		return wd, nil
	}
	if cwd == "~" || strings.HasPrefix(cwd, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("展开主目录失败: %w（改用绝对路径）", err)
		}
		cwd = filepath.Join(home, strings.TrimPrefix(cwd, "~"))
	}
	abs, err := filepath.Abs(filepath.Clean(cwd))
	if err != nil {
		return "", fmt.Errorf("目录路径无法解析: %q", cwd)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("终端起始目录不存在：%s", abs)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s 不是目录", abs)
	}
	return abs, nil
}

func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return "t" + hex.EncodeToString(b[:])
}
