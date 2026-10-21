// manager.go 是智能体对话空间的核心：thread（对话线程）的生命周期、投影落盘、
// agent 进程的拉起与空闲回收、session/load 重接与降级、权限审批桥接、以及
// 一轮 turn 的编排。
//
// 设计要点（docs/ACP-AGENTS-P1.md §二 已拍板决策）：
//   - thread 创建时绑定 agent + 真实工作目录，之后不可切换（D1）；
//   - 进程空闲 10 分钟回收，上下文经 session/load 重接（D3/N2），降级显式标注；
//   - request_permission 经 approval.Queue 强制入队人工审批，30 分钟超时（D7/N3）；
//   - 每 thread 一进程一连接，同一 thread 的 turn 串行（C3 结构性满足）；
//   - 投影落盘只记历史不恢复执行：重启后 running 一律转 interrupted（D3）。
package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/usage"
)

// thread 状态。orphaned 不落盘——绑定的 agent 被删除是读时计算的展示态。
const (
	StateIdle        = "idle"
	StateRunning     = "running"
	StateInterrupted = "interrupted"
	StateOrphaned    = "orphaned"
)

// Entry 是投影对话记录里的一条消息。
type Entry struct {
	Role    string    `json:"role"` // user | assistant | note
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Images  int       `json:"images,omitempty"` // 随消息发送的图片数
	In      int64     `json:"in,omitempty"`
	Out     int64     `json:"out,omitempty"`
	Ms      int64     `json:"ms,omitempty"`
	Stopped string    `json:"stopped,omitempty"` // end_turn | cancelled | timeout | error…
}

// Thread 是一个智能体对话线程。
type Thread struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	Workspace string    `json:"workspace"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// SessionID 是 agent 侧会话 ID：空闲回收/Friday 重启后经 session/load 重接的凭据。
	SessionID string  `json:"sessionId,omitempty"`
	State     string  `json:"state"`
	Degraded  bool    `json:"degraded,omitempty"` // 上次重接失败过：恢复不可靠
	Messages  []Entry `json:"messages"`
	// 上下文水位：agent 最近一次 usage_update 的自报值（M-5）——窗口总量/当前
	// 占用/会话累计花费。不是本轮 in/out（那是 Entry 上的字段）；cost 是 agent
	// 自报，仅展示，不进预算闸门（C10）。
	ContextUsed int     `json:"contextUsed,omitempty"`
	ContextSize int     `json:"contextSize,omitempty"`
	ContextCost float64 `json:"contextCost,omitempty"`
}

// Image 是随消息发送的一张图片（原始字节，发送时 base64 编码进 ContentBlock）。
type Image struct {
	MIME string
	Data []byte
}

// TurnResult 是一轮对话的归集结果。
type TurnResult struct {
	Answer     string
	In, Out    int64
	Ms         int64
	StopReason string
}

// live 是一个 thread 当前挂着的进程与运行态。
type live struct {
	proc     *agentProc
	turnDone chan struct{} // 当前 turn 的结束信号（关闭后挂着的权限审批自动撤卡）
	slot     chan struct{} // 从哪个信号量槽位取得（Reload 可能换槽）
	idle     *time.Timer
}

// Manager 管理全部 thread 与存活进程。
type Manager struct {
	mu       sync.Mutex
	threads  map[string]*Thread
	order    []string
	live     map[string]*live
	locks    map[string]*sync.Mutex
	sem      chan struct{}
	lastCfg  config.AcpConfig // Reload 的差异比较基准
	path     string           // acp-threads.json
	cfgFn    func() config.AcpConfig
	approver *approval.Queue

	// registryBase 官方目录数据的基目录（$HOME/.vortex/acp）：registry-cache.json
	// 缓存与 agents/<id>/<version>/ binary 安装都在其下。空值或 registryOff=true
	// 表示只显示内置目录（基目录不可建或测试隔离）。fetchMu 串行化拉取与缓存写。
	registryBase string
	registryPath string
	registryOff  bool
	fetchMu      sync.Mutex

	// spawn 可被测试替换（fake agent 注入）。
	spawn func(cfg config.AcpAgentConfig, workspace string, handshake time.Duration,
		onPermission permissionHook, onExit func()) (*agentProc, error)
}

// NewManager 创建管理器：加载 thread 投影（重启后 running 转 interrupted，D3）。
// cfgFn 提供运行时配置快照；approver 为全局审批队列（可为 nil：权限请求自动允许）。
func NewManager(cfgFn func() config.AcpConfig, approver *approval.Queue, path string) *Manager {
	m := &Manager{
		threads:  map[string]*Thread{},
		live:     map[string]*live{},
		locks:    map[string]*sync.Mutex{},
		path:     path,
		cfgFn:    cfgFn,
		approver: approver,
	}
	if base, err := acpBaseDirFn(); err == nil {
		m.registryBase = base
		m.registryPath = filepath.Join(base, "registry-cache.json")
	} else {
		m.registryOff = true // 无 HOME 环境只保留内置目录
	}
	m.spawn = spawnAgent
	m.lastCfg = cfgFn().Normalized()
	n := m.lastCfg.MaxConcurrent
	if n <= 0 {
		n = config.AcpDefaultMaxConcurrent
	}
	m.sem = make(chan struct{}, n)
	if loaded, err := loadThreads(path); err != nil {
		log.Printf("[智能体] %v（原文件已保留，本次运行不加载历史 thread）", err)
	} else {
		for _, th := range loaded {
			if th.State == StateRunning {
				th.State = StateInterrupted // 重启不恢复执行，历史仍可见
			}
			m.threads[th.ID] = th
			m.order = append(m.order, th.ID)
		}
		if n := len(loaded); n > 0 {
			log.Printf("[智能体] 已加载 %d 个 thread（%s）", n, path)
		}
	}
	return m
}

// threadLock 返回该 thread 的串行锁（Send 全程持有，C3）。
func (m *Manager) threadLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	mu, ok := m.locks[id]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[id] = mu
	}
	return mu
}

// ---------- thread CRUD ----------

// List 返回全部 thread（含动态计算的状态）。
func (m *Manager) List() []Thread {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Thread, 0, len(m.order))
	for _, id := range m.order {
		th := m.threads[id]
		if th == nil {
			continue
		}
		cp := *th
		cp.Messages = nil
		if _, ok := m.cfgFn().Agent(th.Agent); !ok {
			cp.State = StateOrphaned
		} else if cp.State == StateRunning && m.live[id] == nil {
			cp.State = StateIdle
		}
		out = append(out, cp)
	}
	return out
}

// Get 返回单个 thread（含消息）。
func (m *Manager) Get(id string) (Thread, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	th, ok := m.threads[id]
	if !ok {
		return Thread{}, false
	}
	cp := *th
	cp.Messages = append([]Entry{}, th.Messages...)
	if _, ok := m.cfgFn().Agent(th.Agent); !ok {
		cp.State = StateOrphaned
	}
	return cp, true
}

// sessionsBaseDirFn 可被测试替换：未指定工作目录时，新建临时会话的父目录。
var sessionsBaseDirFn = defaultSessionsDir

// defaultSessionsDir 返回 $HOME/.vortex/acp-sessions——与 $HOME/.vortex/skills
// 同族的专用目录，专门存放「没选目录就开聊」的临时会话。
func defaultSessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("解析 HOME 失败: %w", err)
	}
	return filepath.Join(home, ".vortex", "acp-sessions"), nil
}

// resolveWorkspace 解析并准备工作目录：~ 展开 → 绝对路径 → 确保存在且是目录。
// 请求与 agent 默认都为空时用 fallback（空 fallback 保持报错）。
func resolveWorkspace(agentCfg config.AcpAgentConfig, workspace, fallback string) (string, error) {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		ws = strings.TrimSpace(agentCfg.Workspace)
	}
	if ws == "" {
		ws = fallback
	}
	if ws == "" {
		return "", errors.New("请填写工作目录（该目录是 agent 干活的地方，会被写入文件）")
	}
	if ws == "~" || strings.HasPrefix(ws, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("解析 HOME 失败: %w", err)
		}
		ws = filepath.Join(home, strings.TrimPrefix(ws, "~"))
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("解析工作目录失败: %w", err)
	}
	if info, err := os.Stat(abs); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("检查工作目录失败: %w", err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", fmt.Errorf("创建工作目录失败: %w", err)
		}
	} else if !info.IsDir() {
		return "", fmt.Errorf("工作目录 %s 不是目录", abs)
	}
	return abs, nil
}

// Create 新建 thread（D1：agent 与工作目录在此刻绑定，之后不可切换）。
func (m *Manager) Create(agentName, workspace, title string) (Thread, error) {
	c := m.cfgFn().Normalized()
	if !c.Enabled {
		return Thread{}, errors.New("智能体功能未开启（设置 → 智能体）")
	}
	agentCfg, ok := c.Agent(agentName)
	if !ok {
		return Thread{}, fmt.Errorf("没有叫 %q 的智能体（可在设置 → 智能体里添加）", agentName)
	}
	th := &Thread{
		ID:        randHex(8),
		Agent:     agentCfg.Name,
		Workspace: "",
		Title:     strings.TrimSpace(title),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		State:     StateIdle,
	}
	// 目录未指定（UI 点选/手输都没有）→ 回退到临时会话目录，每个会话一个
	// 子目录互不踩文件（对齐其他 agent 工具「无需先想目录即可开聊」的默认）。
	fallback := ""
	if strings.TrimSpace(workspace) == "" && strings.TrimSpace(agentCfg.Workspace) == "" {
		base, err := sessionsBaseDirFn()
		if err != nil {
			return Thread{}, err
		}
		fallback = filepath.Join(base, th.ID)
	}
	ws, err := resolveWorkspace(agentCfg, workspace, fallback)
	if err != nil {
		return Thread{}, err
	}
	th.Workspace = ws
	m.mu.Lock()
	m.threads[th.ID] = th
	m.order = append(m.order, th.ID)
	err = m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		log.Printf("[智能体] thread 落盘失败: %v", err)
	}
	log.Printf("[智能体] 新建 thread %s（agent=%s，workspace=%s）", th.ID, th.Agent, th.Workspace)
	return *th, nil
}

// Delete 删除 thread：杀掉存活进程并移除记录（含投影）。
func (m *Manager) Delete(id string) bool {
	m.mu.Lock()
	th, ok := m.threads[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.threads, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	delete(m.locks, id)
	lv := m.live[id]
	delete(m.live, id)
	_ = m.saveLocked()
	m.mu.Unlock()
	if lv != nil {
		m.release(lv, "删除 thread")
	}
	log.Printf("[智能体] 已删除 thread %s（agent=%s）", id, th.Agent)
	return true
}

// ---------- 进程生命周期 ----------

// release 归还进程占用的资源：杀进程 + 释放信号量槽。
func (m *Manager) release(lv *live, reason string) {
	if lv.idle != nil {
		lv.idle.Stop()
	}
	lv.proc.kill()
	if lv.slot != nil {
		select {
		case <-lv.slot:
		default:
		}
	}
	log.Printf("[智能体] %s 进程已回收（%s）", lv.proc.agent, reason)
}

// onProcExit 进程自己退出（崩溃）时清理 live 记录并释放槽位。
func (m *Manager) onProcExit(id string) {
	m.mu.Lock()
	lv, ok := m.live[id]
	if ok {
		delete(m.live, id)
	}
	m.mu.Unlock()
	if ok {
		m.release(lv, "进程退出")
	}
}

// killLive 主动回收某 thread 的进程（空闲回收 / 配置变更 / 删除）。
func (m *Manager) killLive(id, reason string) {
	m.mu.Lock()
	lv, ok := m.live[id]
	if ok {
		delete(m.live, id)
	}
	m.mu.Unlock()
	if ok {
		m.release(lv, reason)
	}
}

// resetIdle 设定/重置空闲回收定时器（idle<=0 表示不回收）。
func (m *Manager) resetIdle(id string) {
	idle := time.Duration(m.cfgFn().Normalized().IdleKillSeconds) * time.Second
	if idle <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lv, ok := m.live[id]
	if !ok {
		return
	}
	if lv.idle != nil {
		lv.idle.Stop()
	}
	lv.idle = time.AfterFunc(idle, func() { m.killLive(id, "空闲回收") })
}

// ensureLive 保证该 thread 有存活进程与会话，返回 live 记录与是否发生降级。
// 需要新拉进程时先取并发槽位（满了随 ctx 等待），再 spawn + 握手 +
// session/load 重接（或降级新开）。
func (m *Manager) ensureLive(ctx context.Context, th *Thread, agentCfg config.AcpAgentConfig, hooks TurnHooks) (*live, bool, error) {
	m.mu.Lock()
	if lv := m.live[th.ID]; lv != nil {
		if lv.proc.alive() {
			if lv.idle != nil {
				lv.idle.Stop()
			}
			m.mu.Unlock()
			return lv, false, nil
		}
		delete(m.live, th.ID)
		m.mu.Unlock()
		m.release(lv, "连接已断")
	} else {
		m.mu.Unlock()
	}

	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, false, errors.New("智能体进程并发已满，等待槽位时被取消")
	}

	hooks.status("正在启动 agent " + agentCfg.Name + "…")
	proc, err := m.spawn(agentCfg, th.Workspace,
		time.Duration(m.cfgFn().Normalized().HandshakeTimeoutSeconds)*time.Second,
		m.handlePermission(th.ID, agentCfg.Name),
		func() { m.onProcExit(th.ID) })
	if err != nil {
		<-m.sem
		return nil, false, err
	}
	lv := &live{proc: proc, slot: m.sem}
	m.mu.Lock()
	m.live[th.ID] = lv
	m.mu.Unlock()

	sid, degraded, serr := m.ensureSession(th, proc, agentCfg, hooks)
	if serr != nil {
		m.killLive(th.ID, "会话建立失败")
		return nil, false, serr
	}
	m.mu.Lock()
	th.SessionID = sid
	th.Degraded = degraded
	serr = m.saveLocked()
	m.mu.Unlock()
	if serr != nil {
		log.Printf("[智能体] 投影落盘失败: %v", serr)
	}
	return lv, degraded, nil
}

// ensureSession 保证进程内有可用会话：优先 session/load 重接（N2：协议要求
// agent 全量重放对话；重放事件不入当轮聚合——投影里已有这段历史），失败或
// 不可靠（DegradeResume / agent 未声明能力）则新开并报告降级。
// 返回生效的 sessionID、是否降级、错误（会话都建不起来时非 nil）。
func (m *Manager) ensureSession(th *Thread, proc *agentProc, agentCfg config.AcpAgentConfig, hooks TurnHooks) (string, bool, error) {
	handshake := time.Duration(m.cfgFn().Normalized().HandshakeTimeoutSeconds) * time.Second
	if th.SessionID != "" && proc.loadSession && !agentCfg.DegradeResume {
		hooks.status("正在重接上次会话…")
		lctx, lcancel := context.WithTimeout(context.Background(), handshake)
		_, err := proc.conn.LoadSession(lctx, acp.LoadSessionRequest{
			SessionId: acp.SessionId(th.SessionID), Cwd: proc.workspace, McpServers: []acp.McpServer{},
		})
		lcancel()
		if err == nil {
			proc.drainEvents() // 重放不入时间线（投影已有）
			log.Printf("[智能体] %s thread %s 已重接会话 %s", th.Agent, th.ID, th.SessionID)
			return th.SessionID, false, nil
		}
		log.Printf("[智能体] %s thread %s 重接会话失败（%v），新开会话", th.Agent, th.ID, err)
	} else if th.SessionID != "" && agentCfg.DegradeResume {
		log.Printf("[智能体] %s 标记为 resume 不可靠，跳过重接", th.Agent)
	}
	nctx, ncancel := context.WithTimeout(context.Background(), handshake)
	res, err := proc.conn.NewSession(nctx, acp.NewSessionRequest{
		Cwd: proc.workspace, McpServers: []acp.McpServer{},
	})
	ncancel()
	if err != nil {
		return "", false, fmt.Errorf("建立会话失败: %w（常见原因：未登录；stderr: %s）",
			err, firstLine(proc.stderrTail(6)))
	}
	proc.drainEvents()
	degraded := th.SessionID != "" && string(res.SessionId) != th.SessionID
	if degraded {
		log.Printf("[智能体] %s thread %s 上下文已重置（新会话 %s）", th.Agent, th.ID, res.SessionId)
	}
	return string(res.SessionId), degraded, nil
}

// ---------- 权限审批（D7） ----------

// handlePermission 构造某 thread 的权限请求钩子：强制入队人工审批（无视全局
// 权限模式——这是对话空间的独立闸门），30 分钟无人处理自动答复拒绝。
// 批准选第一个 allow* 选项；拒绝选第一个 reject* 选项；都没有则答复 cancelled。
// 审批等待绑定 turn 生命周期：turn 结束（完成/超时/用户停止/进程崩溃）即撤卡
// 并按拒绝结算——卡面向的会话已不存在，留着只会得到一张对着死进程的卡。
func (m *Manager) handlePermission(threadID, agentName string) permissionHook {
	return func(_ context.Context, r acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		kind := ""
		if r.ToolCall.Kind != nil {
			kind = string(*r.ToolCall.Kind)
		}
		summary := ""
		if r.ToolCall.Title != nil {
			summary = strings.TrimSpace(*r.ToolCall.Title)
		}
		if summary == "" {
			summary = "未知操作"
		}
		detail := fmt.Sprintf("智能体 %s 请求授权（%s）", agentName, kind)
		if r.ToolCall.RawInput != nil {
			if b, err := json.Marshal(r.ToolCall.RawInput); err == nil && len(b) > 0 {
				detail += "\n" + sanitizeLog(string(b))
			}
		}
		// C11：文案来自子 agent，卡片摘要照常展示但不作为授权依据。
		log.Printf("[智能体] %s 权限请求: %s", agentName, sanitizeLog(summary))
		if m.approver == nil {
			log.Printf("[智能体] 未装配审批队列，自动允许（仅测试环境应出现）")
			return respondPermission(r, true), nil
		}
		m.mu.Lock()
		var turnDone chan struct{}
		if lv := m.live[threadID]; lv != nil {
			turnDone = lv.turnDone
		}
		m.mu.Unlock()
		if turnDone == nil {
			// 无进行中对话的权限请求（协议外时序）：按拒绝处理，不入队。
			log.Printf("[智能体] %s 在无进行中对话时请求权限，按拒绝处理", agentName)
			return respondPermission(r, false), nil
		}
		card := m.approver.RequestCancelable("智能体权限", summary, detail,
			config.AcpPermissionTimeoutSeconds*time.Second)
		go func() {
			select {
			case <-turnDone:
				card.Cancel()
			case <-card.Done():
			}
		}()
		approved, err := card.Wait()
		if err != nil {
			if !errors.Is(err, approval.ErrDenied) {
				log.Printf("[智能体] 权限审批异常（按拒绝处理）: %v", err)
			}
			return respondPermission(r, false), nil
		}
		log.Printf("[智能体] %s 权限%s: %s", agentName,
			map[bool]string{true: "已批准", false: "已拒绝"}[approved], sanitizeLog(summary))
		return respondPermission(r, approved), nil
	}
}

// respondPermission 按批准与否构造答复：选中匹配选项；没有匹配项时答复
// cancelled（agent 视为未授权）。
func respondPermission(r acp.RequestPermissionRequest, allow bool) acp.RequestPermissionResponse {
	if optID := pickOption(r.Options, allow); optID != "" {
		return selectedPermission(optID)
	}
	return cancelledPermission()
}

// pickOption 按倾向挑选项：allow 时优先 allow_once 再任意 allow*；
// deny 时优先任意 reject*。找不到返回空。
func pickOption(options []acp.PermissionOption, allow bool) acp.PermissionOptionId {
	if allow {
		for _, o := range options {
			if o.Kind == acp.PermissionOptionKindAllowOnce {
				return o.OptionId
			}
		}
		for _, o := range options {
			if strings.HasPrefix(string(o.Kind), "allow") {
				return o.OptionId
			}
		}
	} else {
		for _, o := range options {
			if strings.HasPrefix(string(o.Kind), "reject") {
				return o.OptionId
			}
		}
	}
	return ""
}

// ---------- turn 编排 ----------

// Send 发送一条消息并跑完一轮对话（同一 thread 串行，C3）。
// hooks 的回调在独立 pump goroutine 上调用（SSE 写出方保证并发安全）；
// ctx 取消（客户端断开/服务关闭）会中止本轮并向 agent 发显式取消（C8）。
func (m *Manager) Send(ctx context.Context, id, text string, imgs []Image, hooks TurnHooks) (TurnResult, error) {
	c := m.cfgFn().Normalized()
	if !c.Enabled {
		return TurnResult{}, errors.New("智能体功能未开启（设置 → 智能体）")
	}
	if strings.TrimSpace(text) == "" && len(imgs) == 0 {
		return TurnResult{}, errors.New("消息为空")
	}

	mu := m.threadLock(id)
	mu.Lock()
	defer mu.Unlock()

	m.mu.Lock()
	th, ok := m.threads[id]
	m.mu.Unlock()
	if !ok {
		return TurnResult{}, fmt.Errorf("未找到 thread %s", id)
	}
	agentCfg, avail := c.Agent(th.Agent)
	if !avail {
		return TurnResult{}, fmt.Errorf("thread 绑定的智能体 %q 已删除或停用", th.Agent)
	}

	lv, degraded, err := m.ensureLive(ctx, th, agentCfg, hooks)
	if err != nil {
		return TurnResult{}, err
	}
	if !lv.proc.alive() {
		return TurnResult{}, errors.New("agent 连接已断开，请重试")
	}

	turnTimeout := time.Duration(c.TurnSeconds(agentCfg)) * time.Second
	turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	// turn 结束信号：pump 收尾与权限审批撤卡共用（权限钩子在 SDK 的请求
	// goroutine 上读到它，感知 turn 已结束即自动撤卡）。
	turnDone := make(chan struct{})
	m.mu.Lock()
	lv.turnDone = turnDone
	th.State = StateRunning
	if th.Title == "" {
		th.Title = truncateRunes(text, 40)
	}
	m.mu.Unlock()

	// prompt 块：图片在前、文本在后（与主聊天"图片先注入"的惯例一致）。
	blocks := make([]acp.ContentBlock, 0, len(imgs)+1)
	for _, img := range imgs {
		blocks = append(blocks, acp.ImageBlock(base64.StdEncoding.EncodeToString(img.Data), img.MIME))
	}
	blocks = append(blocks, acp.TextBlock(text))

	sink := newTurnSink(hooks)
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			select {
			case ev, ok := <-lv.proc.events:
				if !ok {
					return
				}
				sink.handle(ev)
			case <-turnDone:
				for { // Prompt 返回后清空已入队残留（watermark 保证不会再有新增）
					select {
					case ev, ok := <-lv.proc.events:
						if !ok {
							return
						}
						sink.handle(ev)
					default:
						return
					}
				}
			}
		}
	}()

	started := time.Now()
	res, promptErr := lv.proc.conn.Prompt(turnCtx, acp.PromptRequest{
		SessionId: acp.SessionId(th.SessionID), Prompt: blocks,
	})
	close(turnDone)
	<-pumpDone
	sink.finish()
	elapsed := time.Since(started)

	// 超时/断开时 agent 可能仍在跑：显式取消（C8，读 stopReason 而非猜）。
	aborted := turnCtx.Err() != nil
	if aborted && promptErr != nil {
		_ = lv.proc.conn.Cancel(context.Background(), acp.CancelNotification{SessionId: acp.SessionId(th.SessionID)})
	}

	m.mu.Lock()
	lv.turnDone = nil
	current, keep := m.threads[id] // turn 进行中被删除则不再写投影
	if keep {
		current.State = StateIdle
		current.UpdatedAt = time.Now()
	}
	m.mu.Unlock()

	result := TurnResult{
		Ms: elapsed.Milliseconds(),
	}
	if res.Usage != nil { // 错误路径下 res 为零值，Usage 为 nil
		result.In = int64(res.Usage.InputTokens)
		result.Out = int64(res.Usage.OutputTokens)
	}
	var runErr error
	var errText string
	switch {
	case promptErr == nil:
		result.Answer = sink.allText()
		result.StopReason = string(res.StopReason) // cancelled 不判失败（C8）
	case aborted:
		result.Answer = sink.allText()
		result.StopReason = "timeout"
	default:
		runErr = fmt.Errorf("agent 连接中断: %v（stderr: %s）", promptErr, firstLine(lv.proc.stderrTail(4)))
		result.StopReason = "error"
		errText = runErr.Error()
	}

	if keep {
		m.mu.Lock()
		m.appendTurnLocked(current, text, len(imgs), result, degraded, aborted, promptErr != nil && !aborted, errText)
		if used, size, cost, _ := sink.context(); size > 0 {
			current.ContextUsed = used
			current.ContextSize = size
			current.ContextCost = cost
		}
		if serr := m.saveLocked(); serr != nil {
			log.Printf("[智能体] 投影落盘失败: %v", serr)
		}
		m.mu.Unlock()
	}

	// 记账：维度 acp:<agent>。金额恒缺失（配额在对面，实测 cost.amount=0），
	// 成本页按现有未知模型口径显示。
	usage.Record(usage.Call{
		Model: "acp:" + th.Agent, Session: "acp:" + id, Origin: "agent",
		In: result.In, Out: result.Out,
	})

	m.resetIdle(id)
	return result, runErr
}

// appendTurnLocked 把本轮问答追加进投影（调用方须持 m.mu）。
// degraded 为真说明本轮跑在新会话里（旧上下文丢失），插入显式标注。
func (m *Manager) appendTurnLocked(th *Thread, question string, imgs int, r TurnResult, degraded, aborted, crashed bool, errText string) {
	if degraded && len(th.Messages) > 0 {
		th.Messages = append(th.Messages, Entry{Role: "note", At: time.Now(),
			Text: "agent 进程已重建，旧会话无法恢复，上下文已重置（历史记录仍可见）"})
	}
	th.Messages = append(th.Messages, Entry{
		Role: "user", Text: question, At: time.Now(), Images: imgs,
	})
	entry := Entry{Role: "assistant", Text: r.Answer, At: time.Now(),
		In: r.In, Out: r.Out, Ms: r.Ms, Stopped: r.StopReason}
	if entry.Text == "" {
		entry.Text = "（本轮没有产出文本）"
	}
	th.Messages = append(th.Messages, entry)
	if aborted {
		reason := "超时（agent 可能仍在执行，结果不会回到本对话）"
		if r.StopReason == "cancelled" {
			reason = "被用户停止"
		}
		th.Messages = append(th.Messages, Entry{Role: "note", At: time.Now(),
			Text: "本轮对话在完成前中断：" + reason})
	}
	if crashed {
		text := "本轮失败：" + truncateRunes(errText, 160)
		text += "。下次发消息将重新拉起进程并尝试重接会话。"
		th.Messages = append(th.Messages, Entry{Role: "note", At: time.Now(), Text: text})
	}
}

// Stop 取消该 thread 正在进行的一轮：显式 session/cancel（C8），Prompt 端
// 会拿到 stopReason=cancelled 正常收尾。没有 PausedTurn 机制——agent 侧
// 会话上下文还在，用户直接发下一句即可续聊。
func (m *Manager) Stop(id string) bool {
	m.mu.Lock()
	lv, ok := m.live[id]
	sid := ""
	if th, tok := m.threads[id]; tok {
		sid = th.SessionID
	}
	m.mu.Unlock()
	if !ok || sid == "" {
		return false
	}
	return lv.proc.conn.Cancel(context.Background(), acp.CancelNotification{SessionId: acp.SessionId(sid)}) == nil
}

// Test 对指定 agent 做一次握手诊断（设置页「测试」按钮）：拉起进程、initialize、
// 立即回收，报告带 stderr 尾部的错误。最常见的故障"没登录"由此变成一句话。
func (m *Manager) Test(name string) error {
	c := m.cfgFn().Normalized()
	agentCfg, ok := c.Agent(name)
	if !ok {
		return fmt.Errorf("没有叫 %q 的智能体", name)
	}
	ws, err := os.MkdirTemp("", "friday-acp-test-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(ws)
	proc, err := m.spawn(agentCfg, ws,
		time.Duration(c.HandshakeTimeoutSeconds)*time.Second, nil, nil)
	if err != nil {
		return err
	}
	proc.kill()
	return nil
}

// Reload 配置热更新：杀掉配置变化/被删除/被停用 agent 的存活进程（上下文由
// session/load 在下次打开时尝试恢复），并发槽位按新值重建。
func (m *Manager) Reload(cfg config.AcpConfig) {
	type entry struct {
		id string
		lv *live
	}
	var toKill []entry
	m.mu.Lock()
	prev := m.lastCfg
	m.lastCfg = cfg
	for id, lv := range m.live {
		th, thOK := m.threads[id]
		if !thOK {
			delete(m.live, id)
			toKill = append(toKill, entry{id, lv})
			continue
		}
		ac, ok := cfg.Agent(th.Agent)
		old, hadOld := prev.Agent(th.Agent)
		if !ok || !ac.IsEnabled() || !hadOld || agentChanged(old, ac) {
			delete(m.live, id)
			toKill = append(toKill, entry{id, lv})
		}
	}
	n := cfg.Normalized().MaxConcurrent
	m.sem = make(chan struct{}, n)
	m.mu.Unlock()
	for _, e := range toKill {
		m.release(e.lv, "配置变更")
	}
}

// agentChanged 判断 agent 的启动参数是否变化（变化即需回收进程）。
func agentChanged(a, b config.AcpAgentConfig) bool {
	if a.Command != b.Command || a.TurnTimeoutSeconds != b.TurnTimeoutSeconds ||
		len(a.Args) != len(b.Args) || len(a.Env) != len(b.Env) {
		return true
	}
	for i := range a.Args {
		if a.Args[i] != b.Args[i] {
			return true
		}
	}
	for k, v := range a.Env {
		if b.Env[k] != v {
			return true
		}
	}
	return false
}

// Close 关闭全部存活进程（Friday 退出时调用）。
func (m *Manager) Close() {
	m.mu.Lock()
	all := make([]*live, 0, len(m.live))
	for id, lv := range m.live {
		delete(m.live, id)
		all = append(all, lv)
	}
	m.mu.Unlock()
	for _, lv := range all {
		m.release(lv, "服务关闭")
	}
}

// ---------- 持久化 ----------

type threadFile struct {
	Threads []*Thread `json:"threads"`
}

func loadThreads(path string) ([]*Thread, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 thread 文件 %s 失败: %w", path, err)
	}
	var f threadFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("thread 文件 %s 损坏: %w", path, err)
	}
	return f.Threads, nil
}

// saveLocked 落盘（调用方须持 m.mu）。写失败不吞：thread 投影是用户数据。
func (m *Manager) saveLocked() error {
	f := threadFile{Threads: make([]*Thread, 0, len(m.order))}
	for _, id := range m.order {
		if th := m.threads[id]; th != nil {
			f.Threads = append(f.Threads, th)
		}
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// ---------- 小工具 ----------

// truncateRunes 截断到 n 个 rune。
func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
