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
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/config"
)

// authMethodTimeout 是运行期预认证（authenticate）的等待上限。个别 agent
// （qoder 1.1.x）收下请求却不回响应，只能到时放行；实测请求发出 0.5s 内
// 服务端就已生效，1.5s 留足余量。
const authMethodTimeout = 1500 * time.Millisecond

// agentProc 是一个存活中的 agent 进程与它的连接。
type agentProc struct {
	agent string // 配置名
	cwd   string // 绝对路径（会话的主目录，进程的工作目录）

	conn        *acp.ClientSideConnection
	events      chan acp.SessionNotification // bridge 出口（见 bridge.go 的线程纪律）
	loadSession bool                         // agent 是否声明 loadSession 能力
	// authMethods 是 initialize 响应里 agent 声明的认证方式（ACP 规范）。
	// 设置页的认证面板靠它列出可选的登录方式（见 auth.go）。
	authMethods []acp.AuthMethod
	// extraRoots 是 agent 声明支持 `additionalDirectories`（多目录工作区的扩展根）。
	// 没声明就只传 cwd——多出的目录不发给它，也不假装生效（前端会明说"只用主目录"）。
	extraRoots bool
	bridge     *bridge
	// builtin 标出这条连接是对接内置 Friday 的（进程内 io.Pipe，没有子进程）：
	// 报错文案与「没回音」提示要说人话（ stderr 恒为空、也谈不上"未登录"），
	// 工作区说明里的目录措辞同样不同。
	builtin bool

	stderrMu   sync.Mutex
	stderrMemo string // kill 时缓存的 stderr 尾部（文件随即删除，报错仍可引用）
	stderrFile string
	done       <-chan struct{} // 对端断开（进程退出/连接关闭）时关闭
	kill       func()          // 幂等：进程组击杀 + 管道关闭（N1）
}

// stderrTail 读取 stderr 落盘文件（或 kill 时缓存的尾部）的末尾 n 行
// （诊断"没登录"这类故障的关键证据）。
func (p *agentProc) stderrTail(n int) string {
	p.stderrMu.Lock()
	memo := p.stderrMemo
	p.stderrMu.Unlock()
	data := memo
	if memo == "" {
		if b, err := os.ReadFile(p.stderrFile); err == nil {
			data = string(b)
		}
	}
	lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
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
func spawnAgent(cfg config.AcpAgentConfig, cwd string, handshakeTimeout time.Duration,
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
	cmd.Dir = cwd
	// 包管理器拉起器（npx 等）把 npm 包缓存收进数据目录 npm-cache/（261110 用户
	// 清 ~/.npm/_npx 打瘫全部 agent 的事故）：注入放 extra 层，配置里显式给的
	// npm_config_cache 仍优先，全局环境里的旧值被覆盖。
	extra := make(map[string]string, len(cfg.Env)+1)
	if config.IsPackageLauncher(command) {
		if dir, err := config.NpmCacheDir(); err == nil {
			extra[config.NpmCacheEnvKey] = dir
		} else {
			log.Printf("[acp] agent %s 未注入数据目录 npm 缓存: %v", cfg.Name, err)
		}
	}
	for k, v := range cfg.Env {
		extra[k] = v
	}
	// env 合并而非追加：同名 key 以配置为准。直接 append 会产生重复 key，
	// libc 的 getenv 取第一个匹配——覆盖对 node 系 agent 会静默失效。
	cmd.Env = config.MergeEnv(os.Environ(), extra)
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
		cwd:        cwd,
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
		// 缓存 stderr 尾部再删文件：spawn/Send 的报错在 kill 之后仍要引用它，
		// 而临时文件不清理会随重启累积（agent 的 stderr 可能刷到很大，截尾即可）。
		p.stderrMu.Lock()
		p.stderrMemo = readTail(stderrFile, 64*1024)
		p.stderrMu.Unlock()
		_ = stdin.Close()
		sf.Close()
		_ = os.Remove(stderrFile)
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
		// 提问能力按 agent 白名单声明：CodeBuddy 要 _meta.question，
		// MiniMax 要 elicitation.form（见 interactions.go）。
		ClientCapabilities: clientCapabilitiesFor(cfg),
	})
	if err != nil {
		p.kill()
		tail := p.stderrTail(8)
		return nil, fmt.Errorf("agent %q 握手失败：%s（stderr: %s）",
			cfg.Name, classifyInitError(ictx, err, tail, p.done, cfg), firstLine(tail))
	}
	if initRes.ProtocolVersion != acp.ProtocolVersionNumber {
		p.kill()
		return nil, fmt.Errorf("agent %q 协议版本不支持：agent 应答 v%d，Friday 只讲 v%d（等待 ACP SDK 跟进）",
			cfg.Name, initRes.ProtocolVersion, acp.ProtocolVersionNumber)
	}
	// 有的 agent（qoder 1.1.x）要求客户端在 initialize 后显式 authenticate，
	// 才会在 session/new 里报完整模型清单；不调它虽能建会话，models 却是空的
	// ——前端表现成「一直拿不到模型」。它收下请求却不回响应（实测等不到 result），
	// 所以给短超时后放行：认证在服务端已生效，真正能不能干活仍由 session/new 判定。
	if method := strings.TrimSpace(cfg.AuthMethod); method != "" {
		actx, acancel := context.WithTimeout(context.Background(), authMethodTimeout)
		_, aerr := p.conn.Authenticate(actx, acp.AuthenticateRequest{MethodId: method})
		timedOut := actx.Err() != nil
		acancel()
		if aerr != nil && !timedOut {
			log.Printf("[智能体] %s 预认证 %s 失败（继续建会话）：%v", cfg.Name, method, aerr)
		}
	}
	p.loadSession = initRes.AgentCapabilities.LoadSession
	p.authMethods = initRes.AuthMethods
	// sessionCapabilities.additionalDirectories：'{}' 即表示吃这个字段（协议里
	// 没有取值，声明本身是唯一信息）。多目录工作区靠它决定要不要传扩展根。
	p.extraRoots = initRes.AgentCapabilities.SessionCapabilities.AdditionalDirectories != nil
	log.Printf("[智能体] %s 已就绪（cwd=%s，loadSession=%v，additionalDirectories=%v）",
		cfg.Name, cwd, p.loadSession, p.extraRoots)
	return p, nil
}

// loginHintFor 给出该 agent 的登录指引：配置名优先，命令基名兜底。
// 指引表与登录命令在 auth.go（认证面板与报错文案共用一份口径）。
func loginHintFor(cfg config.AcpAgentConfig) string {
	return loginGuideFor(cfg).hint
}

// authKeywords：stderr/错误文本命中即提示「疑似未登录」。
var authKeywords = []string{
	"login", "log in", "sign in", "signin", "401", "unauthorized", "unauthorised",
	"api key", "apikey", "not authenticated", "not logged in", "认证", "未登录",
}

func looksLikeAuthProblem(texts ...string) bool {
	joined := strings.ToLower(strings.Join(texts, " "))
	for _, kw := range authKeywords {
		if strings.Contains(joined, kw) {
			return true
		}
	}
	return false
}

// classifyInitError 把握手失败归成用户可行动的类别（M-2）；疑似未登录时附上
// 该 agent 的登录指引（M-1）。done 是连接断开信号——进程在握手期间退出是
// 与「活着但不说话」（超时）完全不同的故障，线索全在 stderr。
// ictx 是 initialize 的超时上下文：SDK 会把超时包成不带 Unwrap 链的
// *RequestError（errors.Is 认不出 DeadlineExceeded），只能看 ctx 本身。
func classifyInitError(ictx context.Context, err error, stderr string, done <-chan struct{}, cfg config.AcpAgentConfig) string {
	select {
	case <-done:
		if looksLikeAuthProblem(stderr) {
			return "进程提前退出，疑似未登录：" + loginHintFor(cfg)
		}
		return "进程提前退出（命令秒退——检查命令与参数能否独立运行）"
	default:
	}
	if ictx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "握手超时（agent 无响应——常见于 npx 首次下载、静默挂起或卡在交互提示）；" +
			"npx 型命令首次拉起已按冷启动预算放宽，仍不够可在设置 → 智能体把它的「握手超时」调大"
	}
	if looksLikeAuthProblem(err.Error(), stderr) {
		return "疑似未登录：" + loginHintFor(cfg)
	}
	return "initialize 被拒绝（协议层错误）"
}

// firstLine 取多行文本的第一行（错误消息里只留一句 stderr）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '|'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// readTail 读取文件末尾至多 max 字节（诊断文件可能被刷到很大，截尾即可）。
func readTail(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if size > max {
		if _, err := f.Seek(-max, io.SeekEnd); err != nil {
			return ""
		}
		size = max
	}
	buf := make([]byte, size)
	n, _ := io.ReadFull(f, buf)
	return string(buf[:n])
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
