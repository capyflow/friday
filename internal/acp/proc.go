// proc.go 是一个 agent 子进程的生命周期：LookPath 预检 → 进程组启动 →
// initialize 握手（独立短超时，C7）→ 存活监视 → 进程组击杀（N1）。
//
// 不做崩溃自动重启：进程重拉即会话丢失，静默续跑比直接失败更坏
// （与 docs/ACP-AGENTS-P1.md D1 同一判断）；崩溃后由 Manager 在下一轮
// Send 时经 session/load 尝试重接，失败走「上下文已重置」降级。
package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/config"
)

// agentProc 是一个存活中的 agent 进程与它的连接。
type agentProc struct {
	agent     string // 配置名
	workspace string // 绝对路径

	conn        *acp.ClientSideConnection
	events      chan acp.SessionNotification // bridge 出口（见 bridge.go 的线程纪律）
	loadSession bool                         // agent 是否声明 loadSession 能力
	bridge      *bridge

	stderrFile string
	done       <-chan struct{} // 对端断开（进程退出/连接关闭）时关闭
	kill       func()          // 幂等：进程组击杀 + 管道关闭（N1）
}

// stderrTail 读取 stderr 落盘文件的末尾 n 行（诊断"没登录"这类故障的关键证据）。
func (p *agentProc) stderrTail(n int) string {
	data, err := os.ReadFile(p.stderrFile)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = sanitizeLog(l)
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, " | ")
}

// sanitizeLog 过滤日志中的敏感片段：agent 的握手报文会携带用户 skill 全表与
// 本机绝对路径（C13/N5），落 Friday 日志前截断并抹掉 HOME 前缀。
func sanitizeLog(s string) string {
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

// spawnAgent 拉起 agent 子进程并完成 initialize 握手。onExit 在对端断开时被
// 调用一次（进程崩溃或 kill 之后）。onPermission 为权限请求钩子（可 nil）。
func spawnAgent(cfg config.AcpAgentConfig, workspace string, handshakeTimeout time.Duration,
	onPermission permissionHook, onExit func()) (*agentProc, error) {

	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		return nil, fmt.Errorf("agent %q 未配置启动命令", cfg.Name)
	}
	for _, part := range append([]string{command}, cfg.Args...) {
		if strings.ContainsAny(part, "\x00\r\n") {
			return nil, fmt.Errorf("agent %q 的命令/参数包含非法控制字符", cfg.Name)
		}
	}
	bin, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("agent %q 启动命令不可用: %w（无 TTY 环境下 npx 未必可解析，建议写绝对路径）", cfg.Name, err)
	}

	cmd := exec.Command(bin, cfg.Args...)
	cmd.Dir = workspace
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// N1：独立进程组——kill 时连 npx→node→agent 自 spawn 的工具子进程一起带走。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdin 管道: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 stdout 管道: %w", err)
	}
	stderrFile := filepath.Join(os.TempDir(), fmt.Sprintf("friday-acp-%s-%s.stderr.log",
		sanitizeName(cfg.Name), randHex(4)))
	sf, err := os.Create(stderrFile)
	if err != nil {
		return nil, fmt.Errorf("创建 stderr 落盘文件: %w", err)
	}
	cmd.Stderr = sf

	if err := cmd.Start(); err != nil {
		sf.Close()
		return nil, fmt.Errorf("启动 agent %q: %w", cfg.Name, err)
	}
	// 收尸 goroutine：kill 之后不留 zombie（与 Done() 的断开通知各自独立）。
	go func() { _ = cmd.Wait() }()

	p := &agentProc{
		agent:      cfg.Name,
		workspace:  workspace,
		events:     nil, // 在 bridge 创建后填充
		stderrFile: stderrFile,
	}
	b := newBridge(onPermission)
	p.bridge = b
	p.events = b.events
	p.conn = acp.NewClientSideConnection(b, stdin, stdout)
	p.done = p.conn.Done()

	killed := false
	p.kill = func() {
		if killed {
			return
		}
		killed = true
		if cmd.Process != nil {
			// 杀整个进程组（负 pid）；进程组已消散时忽略错误，直接 Kill 兜底。
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		}
		_ = stdin.Close()
		sf.Close()
	}
	go func() {
		<-p.done
		p.kill() // 对端自己退出时也要收尾（关管道、关文件）
		if onExit != nil {
			onExit()
		}
	}()

	ictx, icancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer icancel()
	initRes, err := p.conn.Initialize(ictx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "friday", Version: "1.0"},
		// 不声明 fs/terminal 能力（实测两家 agent 一次都没调；桩实现见 bridge.go）。
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
	})
	if err != nil {
		p.kill()
		_, _ = cmd.Process.Wait()
		return nil, fmt.Errorf("agent %q initialize 失败: %w（常见原因：未登录/命令不对；stderr: %s）",
			cfg.Name, err, firstLine(p.stderrTail(6)))
	}
	p.loadSession = initRes.AgentCapabilities.LoadSession
	log.Printf("[智能体] %s 已就绪（workspace=%s，loadSession=%v）", cfg.Name, workspace, p.loadSession)
	return p, nil
}

// firstLine 取多行文本的第一行（错误消息里只留一句 stderr）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '|'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "agent"
	}
	return b.String()
}

func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// drainEvents 清空事件通道中的残留（session/load 的重放事件不入当轮聚合：
// 投影里已有这段对话，重复入栈会造成时间线重复）。SDK 的请求响应带
// notificationWatermark——LoadSession 返回时重放已全部入队，此处只需清通道。
func (p *agentProc) drainEvents() {
	for {
		select {
		case <-p.events:
		default:
			return
		}
	}
}

// alive 报告连接是否仍然存活。
func (p *agentProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
