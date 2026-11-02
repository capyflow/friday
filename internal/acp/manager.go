// manager.go 是智能体对话空间的核心：工作区与 thread（会话）的生命周期、投影落盘、
// agent 进程的拉起与空闲回收、session/load 重接与降级、权限审批桥接、以及
// 一轮 turn 的编排。
//
// 设计要点（docs/ACP-AGENTS-P1.md §二 已拍板决策 + 261029 工作区改造）：
//   - 页面主体是「工作区」：一组目录（第一个是主目录）+ 挂在它下面的会话；
//   - 会话创建时绑定 agent + 主目录，之后不可切换（D1，换 = 再建一个会话）；
//     工作区其余目录经 `additionalDirectories` 下发（agent 声明支持才生效，见 workspace.go）；
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
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/usage"
)

// thread 状态。orphaned 不落盘——绑定的 agent 被删除是读时计算的展示态。
const (
	StateIdle    = "idle"
	StateRunning = "running"
	// StateQueued 是「消息已收下，但在等并发槽位」：不标出来的话，用户在时间线
	// 与运行看板都看不到任何东西，只能猜它是不是死了（V-3）。
	StateQueued      = "queued"
	StateInterrupted = "interrupted"
	StateOrphaned    = "orphaned"
)

// stopKillGrace 是用户点停止后等 agent 自报 cancelled 的宽限；超时仍未收尾
// 就强杀进程组（取消只是通知，agent 可能不执行）。测试会临时改小。
var stopKillGrace = 8 * time.Second

// timeoutKillGrace 是单轮超时后的强杀宽限：先 session/cancel，留一点时间让 agent
// 自己停下；一条连接承载多个会话（M-6），不该为了一轮立刻把同组别的会话一起打断。
// 宽限到点仍脏（且没有别的轮次在跑）才回收连接；否则拖到下次使用/空闲回收。测试会改小。
var timeoutKillGrace = 10 * time.Second

// turnSilenceNotice 是"这一轮收工后多久没有任何 update 就先说一声"的静默阈值：
// 单轮上限是 300 秒，干等五分钟不出一句话，用户只会以为界面坏了。测试会改小。
var turnSilenceNotice = 30 * time.Second

// Entry 是投影对话记录里的一条消息。
type Entry struct {
	Role    string    `json:"role"` // user | assistant | note
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Images  int       `json:"images,omitempty"` // 随消息发送的图片数（旧数据只有数量）
	Files   []string  `json:"files,omitempty"`  // 图片的上传 ID，前端经 /api/upload/:id 回显原图
	In      int64     `json:"in,omitempty"`
	Out     int64     `json:"out,omitempty"`
	Ms      int64     `json:"ms,omitempty"`
	Stopped string    `json:"stopped,omitempty"` // end_turn | cancelled | timeout | error…
	// Changes 是这一轮的改动摘要（不含逐文件 diff；详情按 turnId 单独取）。
	Changes *TurnChanges `json:"changes,omitempty"`
}

// Thread 是工作区里的一个会话（协议层叫 thread：一个 agent 会话 + 一条投影）。
type Thread struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	Agent       string `json:"agent"`
	// Cwd 是这个会话锚定的主目录（session/new 的 cwd）：创建时从工作区的目录里
	// 选定，之后不可切换（D1 的纪律没变）；工作区后来增删的目录只影响扩展根。
	Cwd       string    `json:"cwd"`
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
	// CapChoices 记用户手动选过的能力值（capID → value：模型/思考强度/审批模式等）。
	// agent 侧不一定把这些存进会话（重开后常回默认），所以 Friday 记一份"意图"，
	// 每次（重）建立会话后按它重放一遍；见 reapplyChoices。
	CapChoices map[string]string `json:"capChoices,omitempty"`
	// BriefedSession / BriefedDirs 记"哪次会话、按哪份目录清单已经跟 agent 交代过"。
	// ACP 没有系统提示词，agent 只能看到 cwd 与扩展根、并不知道工作区的成员是谁；
	// 所以每个（新/重开的）会话的第一轮会随消息附带一段工作区说明，这两个字段
	// 用来避免同一会话里重复注入、目录变了则重新交代一遍。
	BriefedSession string `json:"briefedSession,omitempty"`
	BriefedDirs    string `json:"briefedDirs,omitempty"`
	// Handoff 是上一个会话留下的交接摘要：建立下一轮时作为前置文本发给
	// agent，送达成功后清空（失败保留，重试还能带上）。
	Handoff string `json:"handoff,omitempty"`
	// PolicyApplied / PolicyNote 记录工作区默认审批档位在本会话的实际落地情况：
	// Applied 为空表示没有下发过；Note 说明为什么没下发（agent 没报审批模式等）。
	PolicyApplied string `json:"policyApplied,omitempty"`
	PolicyNote    string `json:"policyNote,omitempty"`
	// PolicyValue / PolicyValueName 是真正下发给 agent 的审批模式取值与它的显示名；
	// PolicyFallback 标出这次是「退让命中」（agent 没有语义等价档）。只说「已下发」不够——
	// codex 上「每次都问」只能落到 workspace-write，用户得看到实际跑的是哪一档。
	PolicyValue     string `json:"policyValue,omitempty"`
	PolicyValueName string `json:"policyValueName,omitempty"`
	PolicyFallback  bool   `json:"policyFallback,omitempty"`
	// TurnStartedAt 是当前这一轮的开始时刻（运行看板用；空闲/结束时清零）。
	TurnStartedAt time.Time `json:"turnStartedAt,omitempty"`
}

// UnmarshalJSON 兼容 261029 之前的投影：那时会话直接带 `workspace`（目录路径）、
// 没有 workspaceId。读进来把目录搬到 `cwd`，落盘时只写新结构（工作区由
// NewManager 的迁移补齐）。
func (t *Thread) UnmarshalJSON(b []byte) error {
	type alias Thread
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	if a.Cwd == "" {
		var legacy struct {
			Workspace string `json:"workspace"`
		}
		if err := json.Unmarshal(b, &legacy); err == nil {
			a.Cwd = legacy.Workspace
		}
	}
	*t = Thread(a)
	return nil
}

// Image 是随消息发送的一张图片（原始字节，发送时 base64 编码进 ContentBlock）。
type Image struct {
	MIME string
	Data []byte
	ID   string // 上传 ID（记录进 thread 历史供前端回显；仅供内部传递）
}

// TurnResult 是一轮对话的归集结果。
type TurnResult struct {
	Answer     string
	In, Out    int64
	Ms         int64
	StopReason string
	Changes    *TurnChanges
}

// live 是一个 thread 当前的会话与运行态。进程按「agent + 主目录」共享（procState）：
// 同工作区、同 agent 的多个会话跑在同一条连接里（M-6），所以会话能力
// （configOptions/models/modes）是**会话级**的，缓存在这里而不是进程上。
type live struct {
	ps        *procState
	proc      *agentProc
	sessionID acp.SessionId

	optMu     sync.Mutex
	opts      []acp.SessionConfigOption
	models    *acp.SessionModelState
	modeState *acp.SessionModeState

	sinkMu     sync.Mutex
	sink       *turnSink     // 进行中那一轮的事件接收器；nil = 没有轮次，事件丢弃
	turnDone   chan struct{} // 当前 turn 的结束信号（关闭后挂着的权限审批自动撤卡）
	stopTimer  *time.Timer   // 用户停止后的强杀兜底
	graceTimer *time.Timer   // 超时后的强杀宽限（先 cancel，仍不收尾才杀）
	stopping   atomic.Bool   // 用户已请求停止（强杀收尾时按 cancelled 记账）
	// dirty 表示"agent 可能还在跑上一轮"（超时后取消只是通知）。下轮开始前若
	// 还是脏的就回收这条连接，保证同一 session 不会有两轮并发（C3）。
	dirty atomic.Bool
}

// 会话级能力缓存：会话响应、config_options_update 推送、切换响应都会刷新它。
func (lv *live) setOptions(opts []acp.SessionConfigOption) {
	lv.optMu.Lock()
	lv.opts = opts
	lv.optMu.Unlock()
}

func (lv *live) getOptions() []acp.SessionConfigOption {
	lv.optMu.Lock()
	defer lv.optMu.Unlock()
	return lv.opts
}

func (lv *live) setModelState(ms *acp.SessionModelState) {
	lv.optMu.Lock()
	lv.models = ms
	lv.optMu.Unlock()
}

func (lv *live) getModelState() *acp.SessionModelState {
	lv.optMu.Lock()
	defer lv.optMu.Unlock()
	return lv.models
}

func (lv *live) setCurrentModel(value string) {
	lv.optMu.Lock()
	defer lv.optMu.Unlock()
	if lv.models != nil {
		lv.models.CurrentModelId = acp.SessionModelId(value)
	}
}

func (lv *live) setModeState(ms *acp.SessionModeState) {
	lv.optMu.Lock()
	lv.modeState = ms
	lv.optMu.Unlock()
}

func (lv *live) getModeState() *acp.SessionModeState {
	lv.optMu.Lock()
	defer lv.optMu.Unlock()
	return lv.modeState
}

func (lv *live) setCurrentMode(value string) {
	lv.optMu.Lock()
	defer lv.optMu.Unlock()
	if lv.modeState != nil {
		lv.modeState.CurrentModeId = acp.SessionModeId(value)
	}
}

// setCurrent 兜底更新某配置项的当前值：agent 在 set_config_option 的响应里
// 没回带全量 options 时，仅保 UI 一致性用。
func (lv *live) setCurrent(configID, value string) {
	lv.optMu.Lock()
	defer lv.optMu.Unlock()
	for _, o := range lv.opts {
		if o.Select != nil && string(o.Select.Id) == configID {
			o.Select.CurrentValue = acp.SessionConfigValueId(value)
		}
	}
}

func (lv *live) setSink(sink *turnSink) {
	lv.sinkMu.Lock()
	lv.sink = sink
	lv.sinkMu.Unlock()
}

func (lv *live) takeSink() *turnSink {
	lv.sinkMu.Lock()
	defer lv.sinkMu.Unlock()
	return lv.sink
}

// procState 是一条 agent 连接（一个进程）与它承载的会话。复用键 = agent 名 + 主目录：
// 进程的工作目录、MCP/skill 表与登录态都是进程级的，而会话的 cwd 必须与进程一致，
// 所以不同目录的会话不能共用一个进程。
type procState struct {
	key    string
	proc   *agentProc
	slot   chan struct{}
	idle   *time.Timer
	mu     sync.Mutex
	owners map[acp.SessionId]string // sessionId → threadID

	// 事件泵的暂停/恢复握手：一轮结束时要把"已入队但还没分发"的事件排空，
	// 否则 Prompt 返回后立刻摘接收器会丢掉结尾的文本/用量（实测会随机丢）。
	pause  chan struct{}
	ack    chan struct{}
	resume chan struct{}
}

func (ps *procState) setOwner(sid acp.SessionId, threadID string) {
	ps.mu.Lock()
	ps.owners[sid] = threadID
	ps.mu.Unlock()
}

func (ps *procState) owner(sid acp.SessionId) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.owners[sid]
}

// dropThread 摘掉某 thread 的会话归属，返回这条连接上还剩几个会话。
func (ps *procState) dropThread(threadID string) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for sid, tid := range ps.owners {
		if tid == threadID {
			delete(ps.owners, sid)
		}
	}
	return len(ps.owners)
}

func procKey(agent, cwd string) string { return agent + "\x00" + cwd }

// procHasActiveTurn 报告这条连接上是否还有轮次在跑（宽限强杀前用：
// 别为了一个超时的会话把同连接上别的会话一起打断）。
func (m *Manager) procHasActiveTurn(ps *procState) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lv := range m.live {
		if lv.ps == ps && lv.takeSink() != nil {
			return true
		}
	}
	return false
}

// Manager 管理全部工作区、thread 与存活进程。
type Manager struct {
	mu         sync.Mutex
	threads    map[string]*Thread
	order      []string
	workspaces map[string]*Workspace
	wsOrder    []string
	live       map[string]*live      // threadID → 会话与运行态
	procs      map[string]*procState // agent+cwd → 一条连接（承载多个会话）
	spawnMu    sync.Mutex            // 进程创建串行（同一 key 不并发拉两个）
	locks      map[string]*sync.Mutex
	sem        chan struct{}
	// waiting 是在等并发槽位的会话数（m.mu 保护）。有人排队时空闲连接不再守着
	// 完整的 IdleKillSeconds，而是快速让位——否则「上限 2」会变成「后面的人等 10 分钟」。
	waiting int
	// queueCancels 是排队中会话的取消句柄：这时还没有 ACP 会话可发 session/cancel，
	// 「停止」能做的就是把这次等待撤掉（threadID → cancel）。
	queueCancels map[string]context.CancelFunc
	lastCfg      config.AcpConfig // Reload 的差异比较基准
	path         string           // acp-threads.json（工作区与会话同一份投影）
	cfgFn        func() config.AcpConfig
	approver     *approval.Queue
	memory       *memory.WikiStore // 工作区项目记忆（可为 nil：不注入项目速览）
	// checks 是验证面板的运行记录与取消句柄（内存态：进程重启后「在跑」无意义）。
	checks checkRegistry
	// warmAgents 记下哪些 agent 已经成功起过一次：npx 型命令只有第一次要联网装包，
	// 冷启动预算给一次就够，之后回落到常规握手值（见 handshakeBudget）。
	warmAgents map[string]bool

	// registryBase 官方目录数据的基目录（<数据目录>/acp）：registry-cache.json
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
		} else if cp.State == StateQueued && m.queueCancels[id] == nil {
			cp.State = StateIdle // 等待方已经不在了（异常收尾）：不把「排队中」留成僵尸状态
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

// sessionsBaseDirFn 可被测试替换：没选目录时，临时工作区目录的父目录。
var sessionsBaseDirFn = defaultSessionsDir

// defaultSessionsDir 返回 <数据目录>/acp-sessions——与 acp/ 同族的专用目录，
// 专门存放「没选目录就开聊」的临时会话（内核 chdir 到数据目录后按相对路径使用）。
func defaultSessionsDir() (string, error) {
	return "acp-sessions", nil
}

// CreateSession 在工作区里新建一个会话（D1 的纪律仍在：会话与 agent、主目录在此刻
// 绑定，之后不可切换——换目录/换 agent = 再建一个会话）。cwd 留空用工作区的主目录
// （第一个目录），指定了就必须是工作区里的某个目录。
func (m *Manager) CreateSession(agentName, workspaceID, cwd, title string) (Thread, error) {
	c := m.cfgFn().Normalized()
	if !c.Enabled {
		return Thread{}, errors.New("智能体功能未开启（设置 → 智能体）")
	}
	agentCfg, ok := c.Agent(agentName)
	if !ok {
		return Thread{}, fmt.Errorf("没有叫 %q 的智能体（可在设置 → 智能体里添加）", agentName)
	}
	m.mu.Lock()
	ws := m.workspaces[workspaceID]
	var dirs []string
	if ws != nil {
		dirs = append([]string{}, ws.Dirs...)
	}
	m.mu.Unlock()
	if ws == nil {
		return Thread{}, fmt.Errorf("未找到工作区 %s（先在工作区列表里新建一个）", workspaceID)
	}
	want := strings.TrimSpace(cwd)
	if want == "" {
		want = dirs[0]
	} else if !containsStr(dirs, want) {
		abs, err := resolveDir(want, "")
		if err != nil {
			return Thread{}, err
		}
		if !containsStr(dirs, abs) {
			return Thread{}, fmt.Errorf("目录 %s 不在该工作区里（先用「添加目录」把它加进来）", abs)
		}
		want = abs
	}
	th := &Thread{
		ID:          randHex(8),
		WorkspaceID: ws.ID,
		Agent:       agentCfg.Name,
		Cwd:         want,
		Title:       strings.TrimSpace(title),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		State:       StateIdle,
	}
	m.mu.Lock()
	m.threads[th.ID] = th
	m.order = append(m.order, th.ID)
	ws.UpdatedAt = time.Now()
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		log.Printf("[智能体] thread 落盘失败: %v", err)
	}
	log.Printf("[智能体] 新建会话 %s（工作区=%s，agent=%s，主目录=%s）", th.ID, ws.Name, th.Agent, th.Cwd)
	return *th, nil
}

// Handoff 从一条会话生成交接摘要，并在同一工作区/agent/主目录下新开一条
// “续”会话。ACP 没有压缩接口，这里不假装能原地压缩：旧会话原样保留，
// 新会话下一轮把摘要作为前置文本发给 agent（送达成功才清空）。
func (m *Manager) Handoff(id string) (Thread, error) {
	mu := m.threadLock(id)
	mu.Lock()
	defer mu.Unlock()

	m.mu.Lock()
	th, ok := m.threads[id]
	if !ok {
		m.mu.Unlock()
		return Thread{}, fmt.Errorf("未找到会话 %s", id)
	}
	if th.State == StateRunning || len(th.Messages) == 0 {
		m.mu.Unlock()
		if th.State == StateRunning {
			return Thread{}, errors.New("这一轮还在跑，等它结束再交接")
		}
		return Thread{}, errors.New("这条会话还没有可交接的对话内容")
	}
	summary := handoffSummary(th.Messages)
	title := th.Title
	if title == "" {
		title = "上一个会话"
	}
	workspaceID, agent, cwd := th.WorkspaceID, th.Agent, th.Cwd
	choices := make(map[string]string, len(th.CapChoices))
	for k, v := range th.CapChoices {
		choices[k] = v
	}
	m.mu.Unlock()

	nt, err := m.CreateSession(agent, workspaceID, cwd, title+" · 续")
	if err != nil {
		return Thread{}, err
	}
	m.mu.Lock()
	if cur := m.threads[nt.ID]; cur != nil {
		cur.Handoff = summary
		if len(choices) > 0 {
			cur.CapChoices = choices
		}
		cur.Messages = append(cur.Messages, Entry{Role: "note", At: time.Now(),
			Text: "已从上一个会话交接上下文；摘要会随下一轮发给 agent。\n\n" + truncateRunes(summary, 800)})
		if err := m.saveLocked(); err != nil {
			log.Printf("[智能体] 交接摘要落盘失败: %v", err)
		}
	}
	m.mu.Unlock()
	log.Printf("[智能体] 会话 %s 已交接为新会话 %s（摘要 %d 字）", id, nt.ID, len([]rune(summary)))
	got, ok := m.Get(nt.ID)
	if !ok {
		return Thread{}, errors.New("续会话创建后读取失败")
	}
	return got, nil
}

// handoffSummary 从会话投影整理交接摘要。只使用 Friday 看得见的消息（用户原话、
// agent 最终回答与系统备注），不做模型调用；标注“可能遗漏 agent 未上报的细节”。
func handoffSummary(msgs []Entry) string {
	const maxEntries = 12
	const maxTotal = 4000
	start := len(msgs) - maxEntries
	if start < 0 {
		start = 0
	}
	var b strings.Builder
	b.WriteString("以下是上一个会话的交接摘要（由 Friday 从可见时间线整理，agent 未上报的细节可能遗漏）。请在此基础上继续，不要假设未列出的细节。\n")
	for _, e := range msgs[start:] {
		text := strings.TrimSpace(e.Text)
		if text == "" {
			continue
		}
		switch e.Role {
		case "user":
			b.WriteString("\n用户：" + truncateRunes(text, 500))
		case "assistant":
			b.WriteString("\nagent：" + truncateRunes(text, 800))
		case "note":
			b.WriteString("\n备注：" + truncateRunes(text, 300))
		}
	}
	out := b.String()
	if len([]rune(out)) > maxTotal {
		out = truncateRunes(out, maxTotal)
	}
	return out
}

// RenameThread 改会话标题：手动标题优先于首条消息派生。留空 = 回到自动
// （下一轮对话时若标题仍为空，会按那时的消息重新派生）。
func (m *Manager) RenameThread(id, title string) (Thread, error) {
	title = strings.Join(strings.Fields(strings.TrimSpace(title)), " ")
	title = truncateRunes(title, 60)
	m.mu.Lock()
	defer m.mu.Unlock()
	th := m.threads[id]
	if th == nil {
		return Thread{}, fmt.Errorf("未找到会话 %s", id)
	}
	th.Title = title
	th.UpdatedAt = time.Now()
	if err := m.saveLocked(); err != nil {
		log.Printf("[智能体] 会话改名落盘失败: %v", err)
	}
	cp := *th
	cp.Messages = append([]Entry{}, th.Messages...)
	return cp, nil
}

// Delete 删除会话：摘掉它在这条连接上的 session 归属并移除记录（含投影）。
// 进程是共享的（M-6），只有删到这条连接上最后一个会话时才回收进程。
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
	var ps *procState
	empty := true
	if lv != nil && lv.ps != nil {
		ps = lv.ps
		empty = ps.dropThread(id) == 0
	}
	_ = m.saveLocked()
	m.mu.Unlock()
	if ps != nil && empty {
		m.killProc(ps.key, "最后一个会话已删除")
	}
	if base := m.snapshotBase(); base != "" {
		_ = os.RemoveAll(filepath.Join(base, id))
	}
	log.Printf("[智能体] 已删除会话 %s（agent=%s）", id, th.Agent)
	return true
}

// NewManager 创建管理器：加载工作区与 thread 投影（重启后 running 转 interrupted，
// D3），并把 261029 之前「thread 直接带目录」的投影迁成「工作区（1 个目录）+ 会话」。
// cfgFn 提供运行时配置快照；approver 为全局审批队列（可为 nil：权限请求自动允许）。
func NewManager(cfgFn func() config.AcpConfig, approver *approval.Queue, path string) *Manager {
	m := &Manager{
		threads:    map[string]*Thread{},
		workspaces: map[string]*Workspace{},
		live:       map[string]*live{},
		procs:      map[string]*procState{},
		locks:      map[string]*sync.Mutex{},
		path:       path,
		cfgFn:      cfgFn,
		approver:   approver,
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
	if file, err := loadThreads(path); err != nil {
		log.Printf("[智能体] %v（原文件已保留，本次运行不加载历史 thread）", err)
	} else {
		migrated := 0
		for _, ws := range file.Workspaces {
			if ws == nil || ws.ID == "" || len(ws.Dirs) == 0 {
				continue
			}
			m.workspaces[ws.ID] = ws
			m.wsOrder = append(m.wsOrder, ws.ID)
		}
		for _, th := range file.Threads {
			if th == nil || th.ID == "" {
				continue
			}
			if th.State == StateRunning || th.State == StateQueued {
				th.State = StateInterrupted // 重启不恢复执行，历史仍可见（排队同理：没人替它等槽位）
			}
			// 旧投影：会话自己带目录、没有归属 → 同目录的旧 thread 收进同一个工作区
			if th.WorkspaceID == "" || m.workspaces[th.WorkspaceID] == nil {
				if th.Cwd == "" {
					log.Printf("[智能体] thread %s 没有目录也没有工作区，跳过", th.ID)
					continue
				}
				ws := m.workspaceForDirLocked(th.Cwd)
				th.WorkspaceID = ws.ID
				migrated++
			}
			m.threads[th.ID] = th
			m.order = append(m.order, th.ID)
		}
		if migrated > 0 {
			m.mu.Lock()
			err := m.saveLocked()
			m.mu.Unlock()
			if err != nil {
				log.Printf("[智能体] 工作区迁移结果落盘失败: %v", err)
			}
			log.Printf("[智能体] 已把 %d 个旧对话归入工作区（%s）", migrated, path)
		}
		if n := len(m.order); n > 0 {
			log.Printf("[智能体] 已加载 %d 个会话、%d 个工作区（%s）", n, len(m.wsOrder), path)
		}
	}
	return m
}

// ---------- 进程生命周期（一 agent+目录 一进程，承载多个会话） ----------

// procOf 返回 key 对应的存活进程（没有或已退出返回 nil）。
func (m *Manager) procOf(key string) *procState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ps := m.procs[key]; ps != nil && ps.proc.alive() {
		return ps
	}
	return nil
}

// liveOf 返回某 thread 当前的会话记录。
func (m *Manager) liveOf(threadID string) *live {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[threadID]
}

// ensureProcess 保证该 thread 需要的（agent+目录）进程在场：先取并发槽位
// （满了先在时间线上说一声再等），再 spawn + initialize。
func (m *Manager) ensureProcess(ctx context.Context, th *Thread, agentCfg config.AcpAgentConfig, hooks TurnHooks) (*procState, error) {
	key := procKey(agentCfg.Name, th.Cwd)
	if ps := m.procOf(key); ps != nil {
		m.touchProc(key)
		return ps, nil
	}
	m.spawnMu.Lock()
	defer m.spawnMu.Unlock()
	if ps := m.procOf(key); ps != nil { // 双检：等锁期间别人可能已经拉起
		m.touchProc(key)
		return ps, nil
	}
	// 槽位以“取的时候是哪个 channel”为准记账：Reload 会换掉 m.sem，若释放时去读
	// 新的那个，就会抽走别人的令牌，或把空 channel 抽干后死在阻塞式 `<-m.sem` 上
	// ——而那时 spawnMu 还握着，之后所有会话都起不了进程，只能重启（V-3）。
	m.mu.Lock()
	sem := m.sem
	m.mu.Unlock()
	if err := m.acquireSlot(ctx, th, hooks, sem); err != nil {
		return nil, err
	}
	hooks.status("正在启动 agent " + agentCfg.Name + "…")
	handshake := m.handshakeBudget(agentCfg)
	proc, err := m.spawn(agentCfg, th.Cwd, handshake,
		m.permissionHookFor(key, agentCfg.Name),
		func() { m.onProcExit(key) })
	if err != nil {
		releaseSlot(sem)
		return nil, err
	}
	m.markWarm(agentCfg.Name)
	ps := &procState{key: key, proc: proc, slot: sem, owners: map[acp.SessionId]string{},
		pause: make(chan struct{}, 1), ack: make(chan struct{}, 1), resume: make(chan struct{}, 1)}
	m.mu.Lock()
	m.procs[key] = ps
	m.mu.Unlock()
	go m.pumpProc(ps)
	return ps, nil
}

// coldStartLaunchers 是"每次都要现拉包"的启动命令：第一次跑要联网装包，
// 10 秒握手预算基本必踩（261030 验收：codex 首次 npx 冷启动握手超时，缓存热了才正常）。
var coldStartLaunchers = map[string]bool{
	"npx": true, "pnpm": true, "pnpm-dlx": true, "dlx": true, "bunx": true,
	"yarn": true, "deno": true, "uvx": true, "pipx": true, "npm": true,
}

// coldStartLauncher 判断启动命令是不是包管理器拉起器（只看 basename，绝对路径也算）。
func coldStartLauncher(command string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	if base == "pnpm" || base == "npm" || base == "yarn" || base == "deno" {
		return true // 这几个只在"用 run/dlx 现拉包"时慢，但多给一次冷启动预算没有坏处
	}
	return coldStartLaunchers[base]
}

// handshakeBudget 决定这次 initialize 给多久：包管理器型命令的首次拉起按冷启动预算给，
// 同一 agent 成功起过之后就回落到常规握手值——不必为了第一次慢把每次等待都拉长。
func (m *Manager) handshakeBudget(agentCfg config.AcpAgentConfig) time.Duration {
	c := m.cfgFn().Normalized()
	base := time.Duration(c.HandshakeSeconds(agentCfg)) * time.Second
	cold := time.Duration(config.AcpColdStartHandshakeSeconds) * time.Second
	m.mu.Lock()
	warm := m.warmAgents[agentCfg.Name]
	m.mu.Unlock()
	if warm || cold <= base || !coldStartLauncher(agentCfg.Command) {
		return base
	}
	return cold
}

// dialBudget 是"从零把一条会话拉起来"的总预算：握手（含冷启动）+ 建会话 + 排队余量。
// Options/Capabilities 这类 UI 触发的一次性拉起用它，别拿 10s 握手值去判"能力拿不到"。
func (m *Manager) dialBudget(agentCfg config.AcpAgentConfig) time.Duration {
	c := m.cfgFn().Normalized()
	return m.handshakeBudget(agentCfg) +
		time.Duration(c.SessionSeconds(agentCfg))*time.Second + 30*time.Second
}

func (m *Manager) markWarm(name string) {	m.mu.Lock()
	if m.warmAgents == nil {
		m.warmAgents = map[string]bool{}
	}
	m.warmAgents[name] = true
	m.mu.Unlock()
}

// queuedIdleGrace 是「有人在排队等槽位」时空闲连接的让位时长：不按 IdleKillSeconds// 死守（默认 10 分钟），否则上限 2 的机器上第三个会话要等一整段空闲时间才能开跑。
const queuedIdleGrace = 5 * time.Second

// acquireSlot 取一个并发槽位。满了就把会话标成「排队中」、在时间线上说一声谁占着，
// 拿到槽位或被取消后恢复状态；等待期间可被 ctx 取消（关页面/停止）。
func (m *Manager) acquireSlot(ctx context.Context, th *Thread, hooks TurnHooks, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	default:
	}
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.waiting++
	if m.queueCancels == nil {
		m.queueCancels = map[string]context.CancelFunc{}
	}
	m.queueCancels[th.ID] = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.waiting > 0 {
			m.waiting--
		}
		delete(m.queueCancels, th.ID)
		m.mu.Unlock()
		cancel()
		m.setThreadState(th, StateIdle)
	}()
	hooks.status(fmt.Sprintf("并发的智能体进程已跑满（%d/%d），排队等空闲槽位…", len(sem), cap(sem)))
	m.setThreadState(th, StateQueued)
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("智能体进程并发已满（%d/%d），等待槽位时被取消；上限可在设置 → 智能体调整", len(sem), cap(sem))
	}
}

// releaseSlot 归还一个槽位。非阻塞：计数已经被 Reload 或重复回收打乱时，
// 宁可放过一次归还也不能卡死在 spawnMu 里面。
func releaseSlot(sem chan struct{}) {
	select {
	case <-sem:
	default:
	}
}

func (m *Manager) setThreadState(th *Thread, state string) {
	m.mu.Lock()
	th.State = state
	m.mu.Unlock()
}

// permissionHookFor 把权限请求按 sessionId 路由回它所属 thread 的审批卡：
// 一条连接上可能同时挂着多个会话，不能像一进程一会话时那样固定绑一个 thread。
func (m *Manager) permissionHookFor(key, agentName string) permissionHook {
	return func(ctx context.Context, r acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		var thID string
		if ps := m.procOf(key); ps != nil {
			thID = ps.owner(r.SessionId)
		}
		if thID == "" {
			log.Printf("[智能体] %s 的权限请求找不到所属会话，按拒绝处理", agentName)
			return respondPermission(r, false), nil
		}
		return m.handlePermission(thID, agentName)(ctx, r)
	}
}

// pumpProc 是每个进程一条常驻事件泵（C6：SDK 的入站队列是单 goroutine，
// 读通道必须持续进行，否则会拖死整条连接）。事件按 sessionId 分发给对应会话
// 进行中的那一轮，同时刷新该会话的能力缓存；没有轮次接收的事件（重放、迟到
// 通知）直接丢弃。
func (m *Manager) pumpProc(ps *procState) {
	for {
		select {
		case <-ps.pause: // 某轮收尾要排空队列：先停读，等它处理完再继续
			ps.ack <- struct{}{}
			<-ps.resume
		case ev, ok := <-ps.proc.events:
			if !ok {
				return
			}
			m.routeEvent(ps, ev)
		}
	}
}

// routeEvent 把一条事件送到它所属会话：刷新该会话的能力缓存，再交给进行中的
// 那一轮；没有轮次在跑就丢弃（重放、迟到通知）。
func (m *Manager) routeEvent(ps *procState, ev acp.SessionNotification) {
	thID := ps.owner(ev.SessionId)
	if thID == "" {
		return
	}
	lv := m.liveOf(thID)
	if lv == nil {
		return
	}
	if ev.Update.ConfigOptionUpdate != nil {
		lv.setOptions(ev.Update.ConfigOptionUpdate.ConfigOptions)
	}
	if ev.Update.CurrentModeUpdate != nil {
		lv.setCurrentMode(string(ev.Update.CurrentModeUpdate.CurrentModeId))
	}
	if sink := lv.takeSink(); sink != nil {
		sink.handle(ev)
	}
}

// settleTurn 是轮次收尾的"结界"：让事件泵暂停，把已经入队、还没分发的本轮事件
// 全部处理掉，然后摘掉接收器并恢复泵。Prompt 返回只保证通知已入队（watermark），
// 不保证我们的泵已经分发——少了这一步，结尾的文本与用量会随机丢失。
func (m *Manager) settleTurn(lv *live) {
	ps := lv.ps
	ps.pause <- struct{}{}
	<-ps.ack
	for {
		select {
		case ev, ok := <-ps.proc.events:
			if !ok {
				lv.setSink(nil)
				ps.resume <- struct{}{}
				return
			}
			m.routeEvent(ps, ev)
		default:
			lv.setSink(nil)
			ps.resume <- struct{}{}
			return
		}
	}
}

// stopIdle 停掉一条连接的空闲回收计时。ps.idle 由 ps.mu 保护（armIdle 在别的
// goroutine 上写它），不加锁读会撞 -race。
func stopIdle(ps *procState) {
	ps.mu.Lock()
	if ps.idle != nil {
		ps.idle.Stop()
		ps.idle = nil
	}
	ps.mu.Unlock()
}

// releaseProc 杀进程、归还槽位，并清掉挂在这条连接上的会话记录。
func (m *Manager) releaseProc(ps *procState, reason string) {
	stopIdle(ps)
	m.mu.Lock()
	for thID, lv := range m.live {
		if lv.ps == ps {
			if lv.stopTimer != nil {
				lv.stopTimer.Stop()
			}
			if lv.graceTimer != nil {
				lv.graceTimer.Stop()
			}
			delete(m.live, thID)
		}
	}
	delete(m.procs, ps.key)
	m.mu.Unlock()
	ps.proc.kill()
	if ps.slot != nil {
		releaseSlot(ps.slot)
	}
	log.Printf("[智能体] %s 进程已回收（%s）", ps.proc.agent, reason)
}

// onProcExit 进程自己退出（崩溃）时清理连接与会话记录并归还槽位。
func (m *Manager) onProcExit(key string) {
	m.mu.Lock()
	ps := m.procs[key]
	if ps != nil {
		for thID, lv := range m.live {
			if lv.ps == ps {
				delete(m.live, thID)
			}
		}
		delete(m.procs, key)
	}
	m.mu.Unlock()
	if ps != nil {
		stopIdle(ps)
		if ps.slot != nil {
			releaseSlot(ps.slot)
		}
		log.Printf("[智能体] %s 进程已退出", ps.proc.agent)
	}
}

// killProc 主动回收一条连接（空闲回收 / 配置变更 / 建会话失败）。
func (m *Manager) killProc(key, reason string) {
	m.mu.Lock()
	ps := m.procs[key]
	m.mu.Unlock()
	if ps != nil {
		m.releaseProc(ps, reason)
	}
}

// killLive 回收某 thread 所在的那条连接（同一条连接上的其它会话会一起回收，
// 下次使用按 session/load 重接——比如本轮超时/客户端断开后的强杀）。
func (m *Manager) killLive(threadID, reason string) {
	lv := m.liveOf(threadID)
	if lv == nil || lv.ps == nil {
		return
	}
	m.killProc(lv.ps.key, reason)
}

// touchProc 停掉空闲回收计时：进程正在被用（建会话 / 一轮对话前后）。
func (m *Manager) touchProc(key string) {
	m.mu.Lock()
	ps := m.procs[key]
	m.mu.Unlock()
	if ps == nil {
		return
	}
	ps.mu.Lock()
	if ps.idle != nil {
		ps.idle.Stop()
		ps.idle = nil
	}
	ps.mu.Unlock()
}

// armIdle 一轮结束后武装空闲回收计时（以连接为单位：整条连接都没活了才回收）。
// 有会话在排队等槽位时不守完整的 IdleKillSeconds——空闲连接占着名额，对后面排队
// 的人来说就是「发了消息没反应」（V-3）。
func (m *Manager) armIdle(key string) {
	idle := time.Duration(m.cfgFn().Normalized().IdleKillSeconds) * time.Second
	if idle <= 0 {
		return
	}
	reason := "空闲回收"
	m.mu.Lock()
	ps := m.procs[key]
	waiting := m.waiting
	m.mu.Unlock()
	if ps == nil {
		return
	}
	if waiting > 0 && idle > queuedIdleGrace {
		idle = queuedIdleGrace
		reason = "空闲回收（有会话在排队等槽位）"
	}
	ps.mu.Lock()
	if ps.idle != nil {
		ps.idle.Stop()
	}
	ps.idle = time.AfterFunc(idle, func() { m.killProc(key, reason) })
	ps.mu.Unlock()
}

// ensureLive 保证该 thread 有进程与会话，返回会话记录与是否发生了降级。
func (m *Manager) ensureLive(ctx context.Context, th *Thread, agentCfg config.AcpAgentConfig, hooks TurnHooks) (*live, bool, error) {
	if lv := m.liveOf(th.ID); lv != nil && lv.proc != nil && lv.proc.alive() {
		m.touchProc(procKey(agentCfg.Name, th.Cwd))
		return lv, false, nil
	}
	ps, err := m.ensureProcess(ctx, th, agentCfg, hooks)
	if err != nil {
		return nil, false, err
	}
	lv := &live{ps: ps, proc: ps.proc}
	sid, degraded, err := m.ensureSession(ps, th, lv, agentCfg, hooks)
	if err != nil {
		// 会话建不起来：这条连接上可能一个会话都没有，回收进程免得白占槽位
		if ps.dropThread(th.ID) == 0 {
			m.killProc(ps.key, "会话建立失败")
		}
		return nil, false, err
	}
	lv.sessionID = acp.SessionId(sid)
	m.mu.Lock()
	m.live[th.ID] = lv
	// 会话 id 与降级标记写回投影：重开时靠它 session/load、Stop 也按它取消
	th.SessionID = sid
	th.Degraded = degraded
	if degraded {
		// 会话是重开的新会话：旧上下文已丢，旧水位不再适用（等 agent 下次自报）
		th.ContextUsed, th.ContextSize, th.ContextCost = 0, 0, 0
	}
	if serr := m.saveLocked(); serr != nil {
		log.Printf("[智能体] 投影落盘失败: %v", serr)
	}
	m.mu.Unlock()
	m.touchProc(ps.key)
	return lv, degraded, nil
}

// ensureSession 保证该会话在连接里有可用的 ACP session：优先 session/load 重接
// （N2：协议要求 agent 全量重放对话；重放事件此时没有接收轮次，由事件泵丢弃），
// 失败或不可靠（DegradeResume / agent 未声明能力）则新开并报告降级。
func (m *Manager) ensureSession(ps *procState, th *Thread, lv *live, agentCfg config.AcpAgentConfig, hooks TurnHooks) (string, bool, error) {
	proc := ps.proc
	// 建会话单独给预算：skill 表大的 agent（opencode 1.18 实测光列命令就 9.3s）
	// 在握手的 10s 里连会话都建不出来，报出来像"agent 坏了"，其实是预算太小（V-2）。
	c := m.cfgFn().Normalized()
	budget := time.Duration(c.SessionSeconds(agentCfg)) * time.Second
	// 扩展根 = 工作区里除主目录以外的目录；只在 agent 声明吃这个字段时下发。
	extra := m.extraDirsFor(proc, th)
	if th.SessionID != "" && proc.loadSession && !agentCfg.DegradeResume {
		hooks.status("正在重接上次会话…")
		lctx, lcancel := context.WithTimeout(context.Background(), budget)
		lres, err := proc.conn.LoadSession(lctx, acp.LoadSessionRequest{
			SessionId: acp.SessionId(th.SessionID), Cwd: proc.cwd, McpServers: []acp.McpServer{},
			AdditionalDirectories: extra,
		})
		lcancel()
		if err == nil {
			lv.setOptions(lres.ConfigOptions)
			lv.setModelState(lres.Models)
			lv.setModeState(lres.Modes)
			lv.sessionID = acp.SessionId(th.SessionID)
			ps.setOwner(lv.sessionID, th.ID)
			// 重接成功也把用户手动选过的能力按回去：agent 未必把这些存进会话。
			// 注意顺序：这里的请求带 sessionId，必须先把 id 落到 lv 上再重放。
			// 工作区默认档位先下发，手动选择随后覆盖它。
			m.applyWorkspacePolicy(lv, th)
			m.reapplyChoices(lv, th)
			log.Printf("[智能体] %s thread %s 已重接会话 %s", th.Agent, th.ID, th.SessionID)
			return th.SessionID, false, nil
		}
		log.Printf("[智能体] %s thread %s 重接会话失败（%v），新开会话", th.Agent, th.ID, err)
	} else if th.SessionID != "" && agentCfg.DegradeResume {
		log.Printf("[智能体] %s 标记为 resume 不可靠，跳过重接", th.Agent)
	}
	nctx, ncancel := context.WithTimeout(context.Background(), budget)
	res, err := proc.conn.NewSession(nctx, acp.NewSessionRequest{
		Cwd: proc.cwd, McpServers: []acp.McpServer{},
		AdditionalDirectories: extra,
	})
	// SDK 把超时包成不带 Unwrap 链的 *RequestError，errors.Is 认不出来，
	// 只能趁 cancel 之前看 ctx 自己的状态。
	timedOut := errors.Is(nctx.Err(), context.DeadlineExceeded)
	ncancel()
	if err != nil {
		if timedOut {
			return "", false, fmt.Errorf(
				"%s 没在 %d 秒内建好会话（它首启要列 skill 表/初始化，慢是正常的）；"+
					"可在设置 → 智能体把它的「建会话超时」调大，stderr: %s",
				th.Agent, budget/time.Second, firstLine(proc.stderrTail(6)))
		}
		return "", false, fmt.Errorf("建立会话失败: %w（常见原因：未登录；stderr: %s）",
			err, firstLine(proc.stderrTail(6)))
	}
	lv.setOptions(res.ConfigOptions)
	lv.setModelState(res.Models)
	lv.setModeState(res.Modes)
	lv.sessionID = res.SessionId // 先落 id，重放请求要带它
	ps.setOwner(res.SessionId, th.ID)
	m.applyWorkspacePolicy(lv, th) // 工作区默认档位先下发
	m.reapplyChoices(lv, th)       // 新开会话（含降级）同样按上次的选择重放
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

// workspaceBrief 生成随首轮消息附带的工作区说明（纯文本块，不进投影时间线），
// 返回文本与"目录清单指纹"（用于判断是否需要重新交代）。
// 说明只陈述事实：工作区名、主目录、扩展根；agent 不支持 additionalDirectories
// 时明确写"其余文件夹没下发"，避免它以为能访问却在工具里报错。
func (m *Manager) workspaceBrief(th *Thread, proc *agentProc) (string, string) {
	dirs := m.workspaceDirs(th.WorkspaceID)
	if len(dirs) == 0 {
		return "", ""
	}
	name := th.WorkspaceID
	m.mu.Lock()
	if ws := m.workspaces[th.WorkspaceID]; ws != nil && ws.Name != "" {
		name = ws.Name
	}
	store := m.memory
	m.mu.Unlock()
	fp := strings.Join(dirs, "\n")

	supported := proc != nil && proc.extraRoots
	var b strings.Builder
	b.WriteString("[Friday 工作区] 本会话属于工作区「" + name + "」，它的源文件夹是：\n")
	for _, d := range dirs {
		switch {
		case d == th.Cwd:
			b.WriteString("- " + d + "（主目录，本会话的 cwd）\n")
		case supported:
			b.WriteString("- " + d + "（扩展目录，已下发）\n")
		default:
			// 不吹牛：列出来是为了让 agent 知道"它属于本工作区但现在够不到"
			b.WriteString("- " + d + "（属于本工作区，但当前 agent 不支持多目录，未下发——不要尝试访问）\n")
		}
	}
	b.WriteString("用户问「当前工作区有哪些项目/文件夹」时以上面这份列表为准，不要去上级目录扫描推断；列表之外的不属于本会话。")
	if store != nil {
		if digest := store.DigestScoped(th.WorkspaceID, 4, 1200); digest != "" {
			b.WriteString("\n\n[Friday 项目记忆] 本工作区已记下的背景（来自用户或之前的会话）：\n")
			b.WriteString(digest)
			b.WriteString("\n这些是背景事实，不必逐条复述；需要细节时按用户要求确认。")
			fp += "\n--memory--\n" + digest
		}
	}
	return b.String(), fp
}

// SetMemory 注入长期记忆仓库（启动时调用一次）：工作区首轮说明会附带该
// 工作区的项目记忆速览，所有 agent 共用同一份，避免每个 agent 各记一套。
func (m *Manager) SetMemory(store *memory.WikiStore) {
	m.mu.Lock()
	m.memory = store
	m.mu.Unlock()
}

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
	// 上一轮超时了、agent 当时可能还在跑：宽度过了也没停的话，这条连接是脏的，
	// 不能直接在上面再发一轮（同一 session 两轮并发违背 C3）——先回收再重接。
	if lv.dirty.Load() {
		m.killLive(id, "上一轮超时后仍未收尾，重接前回收")
		lv, degraded, err = m.ensureLive(ctx, th, agentCfg, hooks)
		if err != nil {
			return TurnResult{}, err
		}
	}
	if !lv.proc.alive() {
		return TurnResult{}, errors.New("agent 连接已断开，请重试")
	}

	// 轮前快照：git 仓库用无侵入基线，非 git 目录退化为副本。快照失败不阻塞
	// 本轮，只把“为什么不能还原”写进改动摘要（UI 明说，不假装成功）。
	turnID := fmt.Sprintf("t%d", time.Now().UnixNano())
	snap, snapErr := beginTurnSnapshot(th.Cwd, m.snapshotBase(), id, turnID)
	if snapErr != nil {
		log.Printf("[智能体] thread %s 轮前快照失败: %v", id, snapErr)
	} else if m.workspaceBusy(th.Cwd) {
		snap.meta.Note = joinNote(snap.meta.Note, "同一目录还有别的会话在跑，本次改动的归属可能混在一起")
	}

	turnTimeout := time.Duration(c.TurnSeconds(agentCfg)) * time.Second
	turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	// turn 结束信号：pump 收尾与权限审批撤卡共用（权限钩子在 SDK 的请求
	// goroutine 上读到它，感知 turn 已结束即自动撤卡）。
	turnDone := make(chan struct{})
	m.mu.Lock()
	lv.turnDone = turnDone
	lv.stopping.Store(false)
	if lv.stopTimer != nil {
		lv.stopTimer.Stop()
		lv.stopTimer = nil
	}
	th.State = StateRunning
	th.TurnStartedAt = time.Now()
	if th.Title == "" {
		// 首轮定标题：取第一条消息的第一个非空行，去掉列表/标题符号并折叠空白，
		// 截到列表行放得下的长度。比整段前 40 字更像"标题"，也不需要额外模型调用。
		th.Title = titleFromMessage(text)
	}
	m.mu.Unlock()

	// prompt 块：图片在前、文本在后（与主聊天"图片先注入"的惯例一致）。
	blocks := make([]acp.ContentBlock, 0, len(imgs)+1)
	for _, img := range imgs {
		blocks = append(blocks, acp.ImageBlock(base64.StdEncoding.EncodeToString(img.Data), img.MIME))
	}
	blocks = append(blocks, acp.TextBlock(text))

	// 工作区说明：agent 看不到"工作区"这个概念，只会自己扫目录。每个会话（以及
	// 目录清单变化后）第一轮带一段说明告诉它本会话到底包含哪些文件夹。
	brief, briefFp := m.workspaceBrief(th, lv.proc)
	m.mu.Lock()
	injectBrief := brief != "" && (th.BriefedSession != th.SessionID || th.BriefedDirs != briefFp)
	handoff := th.Handoff
	m.mu.Unlock()
	if injectBrief {
		blocks = append([]acp.ContentBlock{acp.TextBlock(brief)}, blocks...)
	}
	if handoff != "" {
		blocks = append([]acp.ContentBlock{acp.TextBlock(handoff)}, blocks...)
	}
	briefSession := th.SessionID

	// 事件由进程级常驻泵按 sessionId 分发（M-6）：这里只登记本轮接收器，
	// Prompt 返回后立刻摘掉——之后迟到的通知没有接收方，泵会丢弃。
	sink := newTurnSink(hooks)
	lv.setSink(sink)

	// 快到单轮上限时先给 UI 一条状态帧：让用户知道不是卡死，而是马上会被中止
	warnAfter := turnTimeout - 5*time.Second
	if warnAfter < time.Second {
		warnAfter = time.Second
	}
	warnTimer := time.AfterFunc(warnAfter, func() {
		hooks.status(fmt.Sprintf("已接近单轮上限（%d 秒），本轮即将被中止；中止后可以发「继续」接着做。",
			int(turnTimeout/time.Second)))
	})
	// 静默看门狗：agent 收下这一轮却迟迟不吐任何 update（opencode 实测会卡在
	// 自己的模型调用上，最后既不报错也不出字）时，先给一条"没回音"的状态帧——
	// 用户面对的不再是一片安静，而是"它在等什么、可以做什么"（V-2）。
	silenceTimer := time.AfterFunc(turnSilenceNotice, func() {
		if !sink.sawNothing() {
			return
		}
		hooks.status(fmt.Sprintf("%s 收到这一轮后 %d 秒没有任何回音：可能在等模型响应，也可能卡在登录/额度。"+
			"可以先停止这一轮，或到终端跑一次 %s 看它是否可用。",
			th.Agent, int(turnSilenceNotice/time.Second), th.Agent))
	})
	started := time.Now()
	res, promptErr := lv.proc.conn.Prompt(turnCtx, acp.PromptRequest{
		SessionId: lv.sessionID, Prompt: blocks,
	})
	warnTimer.Stop()
	silenceTimer.Stop()
	m.settleTurn(lv) // 先把已入队的事件排空，再摘接收器（否则结尾文本会丢）
	close(turnDone)
	sink.finish()
	elapsed := time.Since(started)

	// 轮末算改动：即使本轮失败/中止也算（agent 可能已经改了盘）。
	var turnChanges *TurnChanges
	if snap != nil {
		turnChanges = snap.Finish()
		m.pruneSnapshots(id, 30)
	} else {
		note := "本轮没有快照"
		if snapErr != nil {
			note += "：" + snapErr.Error()
		}
		turnChanges = &TurnChanges{
			TurnID: turnID, At: time.Now(), Snapshot: false, Note: note,
		}
	}

	// 说明送达成功才记账：失败的那轮下次还会带
	if injectBrief && promptErr == nil {
		m.mu.Lock()
		if current, ok := m.threads[id]; ok {
			current.BriefedSession = briefSession
			current.BriefedDirs = briefFp
			if serr := m.saveLocked(); serr != nil {
				log.Printf("[智能体] 工作区说明记账失败: %v", serr)
			}
		}
		m.mu.Unlock()
	}
	// 交接摘要同样只在送达成功后清空；失败保留，下一轮还能带上。
	if handoff != "" && promptErr == nil {
		m.mu.Lock()
		if current, ok := m.threads[id]; ok && current.Handoff == handoff {
			current.Handoff = ""
			if serr := m.saveLocked(); serr != nil {
				log.Printf("[智能体] 交接摘要清理失败: %v", serr)
			}
		}
		m.mu.Unlock()
	}

	// 中止原因分开记：DeadlineExceeded 才是"超时"，Canceled 是用户停止或客户端断开。
	// 取消只是通知，agent 不一定执行（实测有 agent 超时后继续跑工具）——
	// 超时的处理是"先 cancel + 标记脏 + 宽限后（且同组无其它轮次）才回收进程"，
	// 不再为了一轮立刻杀掉整条连接（M-6 后连接是共享的）。
	ctxErr := turnCtx.Err()
	aborted := ctxErr != nil
	stopped := lv.stopping.Load()
	cause := ""
	if aborted {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			cause = "timeout"
		} else {
			cause = "cancelled"
		}
	}
	if aborted && promptErr != nil {
		_ = lv.proc.conn.Cancel(context.Background(), acp.CancelNotification{SessionId: lv.sessionID})
		if cause == "timeout" {
			lv.dirty.Store(true)
			ps := lv.ps
			lv.graceTimer = time.AfterFunc(timeoutKillGrace, func() {
				if !lv.dirty.Load() || ps == nil {
					return
				}
				// 同一条连接上还有别的轮次在跑就先不动，等空闲回收/下次使用再收
				if m.procHasActiveTurn(ps) {
					return
				}
				m.killProc(ps.key, "超时后仍未收尾，回收连接")
			})
		} else {
			m.killLive(id, "本轮被中止，回收进程") // 客户端已放弃这一轮，没人接收结果
		}
	}
	interrupted := aborted || (stopped && promptErr != nil)
	abortNote := ""
	abortNoteTail := ""
	// 一整轮什么都没吐就被中止：把 agent 自己的 stderr 尾巴带出来，否则用户只能
	// 看到"连接已断开"，猜不到是 agent 内部报错（opencode 的模型调用失败就是这样
	// 悄悄挂掉的）。
	if interrupted && sink.allText() == "" {
		if tail := firstLine(lv.proc.stderrTail(4)); tail != "" {
			abortNoteTail = "；agent 侧 stderr 尾部：" + tail
		}
	}
	switch {
	case !interrupted:
	case cause == "timeout":
		abortNote = "超时（已中止本轮 agent 执行；上下文仍在，可以发「继续」接着做）"
	case stopped:
		abortNote = "被用户停止"
	default:
		abortNote = "连接已断开（页面关闭或网络中断），本轮结果不会回到本对话"
	}
	abortNote += abortNoteTail

	m.mu.Lock()
	if lv.stopTimer != nil {
		lv.stopTimer.Stop()
		lv.stopTimer = nil
	}
	lv.turnDone = nil
	current, keep := m.threads[id] // turn 进行中被删除则不再写投影
	if keep {
		current.State = StateIdle
		current.TurnStartedAt = time.Time{}
		current.UpdatedAt = time.Now()
	}
	m.mu.Unlock()

	result := TurnResult{
		Ms:      elapsed.Milliseconds(),
		Changes: turnChanges.Summary(),
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
		result.StopReason = cause // timeout | cancelled（客户端断开/用户停止）
	case stopped:
		// 停止指令未生效、兜底强杀：按用户取消收尾，不算崩溃。
		result.Answer = sink.allText()
		result.StopReason = "cancelled"
	default:
		runErr = fmt.Errorf("agent 连接中断: %v（stderr: %s）", promptErr, firstLine(lv.proc.stderrTail(4)))
		result.StopReason = "error"
		errText = runErr.Error()
	}

	if keep {
		m.mu.Lock()
		var files []string
		for _, img := range imgs {
			if img.ID != "" {
				files = append(files, img.ID)
			}
		}
		m.appendTurnLocked(current, text, files, result, degraded, abortNote, promptErr != nil && !interrupted, errText)
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

	log.Printf("[智能体] 轮次结束 thread=%s agent=%s 用时=%s 原因=%s tokens=%d/%d",
		id, th.Agent, elapsed.Round(time.Second), result.StopReason, result.In, result.Out)
	m.armIdle(procKey(th.Agent, th.Cwd))
	return result, runErr
}

// appendTurnLocked 把本轮问答追加进投影（调用方须持 m.mu）。
// degraded 为真说明本轮跑在新会话里（旧上下文丢失），插入显式标注。
func (m *Manager) appendTurnLocked(th *Thread, question string, files []string, r TurnResult, degraded bool, abortNote string, crashed bool, errText string) {
	if degraded && len(th.Messages) > 0 {
		th.Messages = append(th.Messages, Entry{Role: "note", At: time.Now(),
			Text: "agent 进程已重建，旧会话无法恢复，上下文已重置（历史记录仍可见）"})
	}
	th.Messages = append(th.Messages, Entry{
		Role: "user", Text: question, At: time.Now(), Images: len(files), Files: files,
	})
	entry := Entry{Role: "assistant", Text: r.Answer, At: time.Now(),
		In: r.In, Out: r.Out, Ms: r.Ms, Stopped: r.StopReason, Changes: r.Changes}
	if entry.Text == "" {
		entry.Text = "（本轮没有产出文本）"
	}
	th.Messages = append(th.Messages, entry)
	if abortNote != "" {
		th.Messages = append(th.Messages, Entry{Role: "note", At: time.Now(),
			Text: "本轮对话在完成前中断：" + abortNote})
	}
	if crashed {
		text := "本轮失败：" + truncateRunes(errText, 160)
		text += "。下次发消息将重新拉起进程并尝试重接会话。"
		th.Messages = append(th.Messages, Entry{Role: "note", At: time.Now(), Text: text})
	}
}

// Stop 取消该 thread 正在进行的一轮：先显式 session/cancel（C8），Prompt 端
// 会拿到 stopReason=cancelled 正常收尾；若 agent 不理会取消，宽限 stopKillGrace
// 后强杀进程组，保证"停止"真的停。没有 PausedTurn 机制——agent 侧会话上下文
// 还在，用户直接发下一句即可续聊（强杀后下轮按 session/load 重接）。
func (m *Manager) Stop(id string) bool {
	m.mu.Lock()
	lv, ok := m.live[id]
	sid := ""
	if th, tok := m.threads[id]; tok {
		sid = th.SessionID
	}
	if ok && sid != "" && lv.turnDone != nil {
		lv.stopping.Store(true)
		if lv.stopTimer == nil {
			turnDone := lv.turnDone
			lv.stopTimer = time.AfterFunc(stopKillGrace, func() {
				select {
				case <-turnDone:
					return // 这一轮已自行收尾
				default:
				}
				m.killLive(id, "停止指令未生效，强杀进程")
			})
		}
	}
	m.mu.Unlock()
	if !ok || sid == "" {
		// 还没建会话的两种情况：排队等槽位（撤掉这次等待就是正确的“停止”），
		// 或者本来就没在跑（返回 false，前端不给按钮）。
		m.mu.Lock()
		cancel := m.queueCancels[id]
		m.mu.Unlock()
		if cancel != nil {
			cancel()
			return true
		}
		return false
	}
	return lv.proc.conn.Cancel(context.Background(), acp.CancelNotification{SessionId: acp.SessionId(sid)}) == nil
}

// ensureProc 保证 thread 有存活会话并返回它（Options/SetOption 共用）。
// 全程持 thread 串行锁：与 Send 互斥，避免和 turn 里的 ensureLive 竞争；
// 返回的 unlock 在用完后调用（顺带停掉这条连接的空闲回收计时）。
func (m *Manager) ensureProc(id string) (*live, *Thread, func(), error) {
	mu := m.threadLock(id)
	mu.Lock()
	c := m.cfgFn().Normalized()
	if !c.Enabled {
		mu.Unlock()
		return nil, nil, nil, errors.New("智能体功能未开启（设置 → 智能体）")
	}
	m.mu.Lock()
	th, ok := m.threads[id]
	m.mu.Unlock()
	if !ok {
		mu.Unlock()
		return nil, nil, nil, fmt.Errorf("未找到 thread %s", id)
	}
	agentCfg, avail := c.Agent(th.Agent)
	if !avail {
		mu.Unlock()
		return nil, nil, nil, fmt.Errorf("thread 绑定的智能体 %q 已删除或停用", th.Agent)
	}
	// 槽位占满/冷启动都会在这里等待（ensureLive 随 ctx 等待），必须带超时兜底
	// ——否则 chip 触发的 Options 请求在并发已满时无限期挂住，前端按钮永远置灰。
	// 预算 = 握手（含冷启动）+ 建会话 + 一点排队余量：只按握手的 10s 算，
	// 首启慢的 agent（opencode 列 skill 表就要 9s）会被这条误判成"能力拿不到"。
	ctx, cancel := context.WithTimeout(context.Background(), m.dialBudget(agentCfg))
	defer cancel()
	lv, _, err := m.ensureLive(ctx, th, agentCfg, TurnHooks{})
	if err != nil {
		mu.Unlock()
		return nil, nil, nil, err
	}
	return lv, th, func() { m.touchProc(procKey(th.Agent, th.Cwd)); mu.Unlock() }, nil
}

// Capabilities 返回该 thread 当前会话的统一能力清单（归一化，见 options.go）。
// 协议里没有「未建会话先查列表」的方法，会话不在时这里会拉起 agent 建会话
// ——列表是静态配置（agent 装好后不变），进程随后照常空闲回收。
func (m *Manager) Capabilities(id string) ([]Capability, error) {
	lv, _, unlock, err := m.ensureProc(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return normalizeCapabilities(lv.getOptions(), lv.getModelState(), lv.getModeState()), nil
}

// SetCapability 切换某项能力的取值（如 id=model, value=gpt-6.1-sol）：
// 按归一化时的 source 路由到 set_config_option / set_model / set_mode。
// 切换即时生效于会话；config-option 响应回带全量配置项，session-model /
// session-mode 响应为空、本地兜底改当前值（真值靠 agent 推送刷新）。
func (m *Manager) SetCapability(id, capID, value string) ([]Capability, error) {
	lv, th, unlock, err := m.ensureProc(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := m.applyCapability(lv, th, capID, value); err != nil {
		return nil, err
	}
	// 记住这次选择：agent 不保证把选项存进会话，重开时靠这份记录重放
	m.recordChoice(th, capID, value)
	return normalizeCapabilities(lv.getOptions(), lv.getModelState(), lv.getModeState()), nil
}

// resolveCapSource 从会话缓存重建 capID → (source, 原始 configID) 的映射
// （与 normalizeCapabilities 同规则）。
func resolveCapSource(lv *live, capID string) (source, configID string) {
	for _, o := range lv.getOptions() {
		origID, cap, ok := convertConfigOption(o.Select)
		if ok && cap.ID == capID {
			return cap.Source, origID
		}
	}
	switch capID {
	case CapKindModel:
		if lv.getModelState() != nil {
			return CapSourceSessionModel, ""
		}
	case CapKindApprovalMode:
		if lv.getModeState() != nil {
			return CapSourceSessionMode, ""
		}
	}
	return "", ""
}

// applyCapability 把某项能力切到 value，按 source 路由到对应协议方法。
// 调用方须已持该 thread 的串行锁（SetCapability 走 ensureProc，重放走 ensureSession）。
func (m *Manager) applyCapability(lv *live, th *Thread, capID, value string) error {
	source, configID := resolveCapSource(lv, capID)
	if source == "" {
		return fmt.Errorf("能力项 %q 不存在或该 agent 不支持", capID)
	}
	c := m.cfgFn().Normalized()
	handshake := time.Duration(c.HandshakeTimeoutSeconds) * time.Second
	if agentCfg, ok := c.Agent(th.Agent); ok {
		handshake = time.Duration(c.HandshakeSeconds(agentCfg)) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), handshake)
	defer cancel()
	switch source {
	case CapSourceConfigOption:
		res, err := lv.proc.conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
			ValueId: &acp.SetSessionConfigOptionValueId{
				ConfigId:  acp.SessionConfigId(configID),
				SessionId: lv.sessionID,
				Value:     acp.SessionConfigValueId(value),
			},
		})
		if err != nil {
			return fmt.Errorf("切换失败: %w", err)
		}
		if len(res.ConfigOptions) > 0 {
			lv.setOptions(res.ConfigOptions)
		} else {
			lv.setCurrent(configID, value)
		}
	case CapSourceSessionModel:
		if _, err := lv.proc.conn.SetSessionModel(ctx, acp.SetSessionModelRequest{
			ModelId: acp.SessionModelId(value), SessionId: lv.sessionID,
		}); err != nil {
			return fmt.Errorf("切换失败: %w", err)
		}
		lv.setCurrentModel(value)
	case CapSourceSessionMode:
		if _, err := lv.proc.conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
			ModeId: acp.SessionModeId(value), SessionId: lv.sessionID,
		}); err != nil {
			return fmt.Errorf("切换失败: %w", err)
		}
		lv.setCurrentMode(value)
	}
	return nil
}

// recordChoice 记下用户这次的选择并落盘（只记手动选过的，不从 agent 推送反推）。
func (m *Manager) recordChoice(th *Thread, capID, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if th.CapChoices == nil {
		th.CapChoices = map[string]string{}
	}
	if th.CapChoices[capID] == value {
		return
	}
	th.CapChoices[capID] = value
	if err := m.saveLocked(); err != nil {
		log.Printf("[智能体] 能力选择落盘失败: %v", err)
	}
}

// reapplyChoices 会话（重）建立后把用户上次手动选的值按回去：很多 agent 不把
// 这些选项存进 session，重开后回默认——Friday 侧的记录就是"别给我忘掉"的那份意图。
// 逐项尽力而为：agent 这版不认某个能力（清单里没有）就跳过，不因此让会话建不起来。
func (m *Manager) reapplyChoices(lv *live, th *Thread) {
	m.mu.Lock()
	choices := make(map[string]string, len(th.CapChoices))
	for k, v := range th.CapChoices {
		choices[k] = v
	}
	m.mu.Unlock()
	if len(choices) == 0 {
		return
	}
	for _, cap := range normalizeCapabilities(lv.getOptions(), lv.getModelState(), lv.getModeState()) {
		want, ok := choices[cap.ID]
		if !ok || want == "" || cap.CurrentValue == want {
			continue
		}
		if err := m.applyCapability(lv, th, cap.ID, want); err != nil {
			log.Printf("[智能体] %s 重放能力选择 %s=%s 失败: %v", th.Agent, cap.ID, want, err)
			continue
		}
		log.Printf("[智能体] %s 已按上次的选择重放 %s=%s", th.Agent, cap.ID, want)
	}
}

// applyWorkspacePolicy 把工作区默认审批档位下发给会话。它只做“能映射就映射”：
// agent 没报 approval_mode（或取值对不上）时明确记录原因，绝不假装已经只读；
// 手动在 chips 里改过的选择由 reapplyChoices 随后覆盖，工作区档位只是默认值。
//
// 映射不到精确等价值时只朝“更保守”的方向退让一档（见 approvalTiers），并把实际落点
// 记进 PolicyValue/PolicyNote：codex 只有 read-only / workspace-write / agent /
// agent-full-access，「每次都问」只能落到 workspace-write——不写清落点的话，用户会
// 以为逐条审批在跑，而它其实只是“越界才问”（V-1/V-4）。
func (m *Manager) applyWorkspacePolicy(lv *live, th *Thread) {
	m.mu.Lock()
	policy := WorkspacePolicyAsk
	if ws := m.workspaces[th.WorkspaceID]; ws != nil {
		policy = NormalizeWorkspacePolicy(ws.Policy)
	}
	m.mu.Unlock()

	var approval *Capability
	for _, cap := range normalizeCapabilities(lv.getOptions(), lv.getModelState(), lv.getModeState()) {
		if cap.Kind == CapKindApprovalMode {
			c := cap
			approval = &c
			break
		}
	}
	if approval == nil {
		m.setPolicyState(th, policyState{
			note: "该 agent 没有上报审批模式，工作区默认档位无法下发；改动仍会记录并可还原",
		})
		return
	}
	pick, ok := pickApproval(policy, approval.Values)
	if !ok {
		m.setPolicyState(th, policyState{note: fmt.Sprintf(
			"该 agent 的审批模式里没有能对应「%s」的取值（可选：%s）；没改它的设置，"+
				"它自己的默认档可能比「%s」更宽松，需要时请在档位下拉里手动选一个。改动仍会记录并可还原",
			policyLabel(policy), joinSelectValues(approval.Values, 6), policyLabel(policy))})
		return
	}
	note := policyFallbackNote(policy, pick)
	if approval.CurrentValue == pick.Value {
		m.setPolicyState(th, policyState{applied: policy, value: pick.Value, name: pick.Name,
			fallback: !pick.Exact, note: note})
		return
	}
	if err := m.applyCapability(lv, th, approval.ID, pick.Value); err != nil {
		log.Printf("[智能体] %s 应用工作区默认档位 %s 失败: %v", th.Agent, policy, err)
		m.setPolicyState(th, policyState{note: "工作区默认档位下发失败：" + err.Error() + "；改动仍会记录并可还原"})
		return
	}
	m.setPolicyState(th, policyState{applied: policy, value: pick.Value, name: pick.Name,
		fallback: !pick.Exact, note: note})
}

// policyState 是一次档位下发的结果投影。
type policyState struct {
	applied  string // 生效的工作区档位（ask/readonly/auto）；空=没下发
	value    string // 实际下发给 agent 的审批模式取值
	name     string // 该取值的显示名（agent 给的）
	fallback bool   // 是否“退让命中”（agent 没有语义等价档）
	note     string // 退让原因 / 未下发原因
}

func (m *Manager) setPolicyState(th *Thread, st policyState) {
	m.mu.Lock()
	th.PolicyApplied, th.PolicyNote = st.applied, st.note
	th.PolicyValue, th.PolicyValueName, th.PolicyFallback = st.value, st.name, st.fallback
	m.mu.Unlock()
}

func policyLabel(policy string) string {
	switch policy {
	case WorkspacePolicyReadOnly:
		return "只读"
	case WorkspacePolicyAuto:
		return "自动"
	default:
		return "每次都问"
	}
}

// approvalPick 是在 agent 审批模式取值里挑中的那一项。Kind 说明它是哪一层命中：
// exact=语义等价；limited=可写但受限；readonly=只能给到只读；auto-limited=自动但不免审批。
type approvalPick struct {
	SelectValue
	Exact bool
	Kind  string
}

// approvalTier 是一层候选词。
type approvalTier struct {
	keywords []string
	kind     string
	exact    bool
}

// approvalTiers 给出每个工作区档位的候选层，按“先精确、再只朝更保守方向退让”排序。
// 各 agent 词汇不同（ask/default/manual、read-only/plan、full-access/yolo），只能按语义
// 关键词匹配。两条红线：ask 绝不命中自动档；退让绝不跨过“还能不能干活”这条线——
// 只读是另一个档位，用户想只读会显式选，不能由 ask 悄悄退过去（那会让 agent 改不了文件）。
// 所以 ask 只允许退到“可写但受限”，再挑不到就明确不下发。
func approvalTiers(policy string) []approvalTier {
	switch policy {
	case WorkspacePolicyReadOnly:
		return []approvalTier{{
			keywords: []string{"read-only", "readonly", "read only", "read_only", "只读", "plan", "计划"},
			kind:     "readonly", exact: true,
		}}
	case WorkspacePolicyAuto:
		return []approvalTier{
			{keywords: []string{"full-access", "full access", "full_access", "no-approval",
				"auto-approve", "yolo", "bypass", "免审批", "全自动"}, kind: "exact", exact: true},
			{keywords: []string{"auto", "自动", "accept", "agent"}, kind: "auto-limited"},
		}
	default: // ask
		return []approvalTier{
			{keywords: []string{"ask", "询问", "每次", "确认", "prompt", "manual",
				"default", "suggest", "建议"}, kind: "exact", exact: true},
			{keywords: []string{"workspace-write", "workspace write", "workspace_write",
				"sandbox", "工作区", "受限"}, kind: "limited"},
		}
	}
}

// pickApproval 按层顺序取第一个命中的取值。
func pickApproval(policy string, vals []SelectValue) (approvalPick, bool) {
	for _, tier := range approvalTiers(policy) {
		for _, v := range vals {
			if containsAny(strings.ToLower(v.Value+" "+v.Name), tier.keywords...) {
				return approvalPick{SelectValue: v, Exact: tier.exact, Kind: tier.kind}, true
			}
		}
	}
	return approvalPick{}, false
}

// policyFallbackNote 把“退让到了哪一档、差别是什么”讲清楚；精确命中时返回空串。
func policyFallbackNote(policy string, pick approvalPick) string {
	if pick.Exact {
		return ""
	}
	shown := pick.Value
	if pick.Name != "" && !strings.EqualFold(pick.Name, pick.Value) {
		shown = pick.Name + "（" + pick.Value + "）"
	}
	switch pick.Kind {
	case "limited":
		return "该 agent 没有「每次都问」的等价取值，已退到最接近的保守档 " + shown +
			"：它不是逐条审批，越界动作才会问你；实际行为由该 agent 决定。要更严选「只读」，要免批选「自动」。"
	case "auto-limited":
		return "该 agent 没有「全自动」的等价取值，已退到 " + shown +
			"：它仍可能向你请求审批，不是完全放行。"
	default:
		return "工作区档位「" + policyLabel(policy) + "」已下发为 " + shown + "。"
	}
}

// joinSelectValues 把可选取值拼成一句人话（截断，用于“没有对应取值”的提示）。
func joinSelectValues(vals []SelectValue, max int) string {
	parts := make([]string, 0, len(vals))
	for i, v := range vals {
		if i >= max {
			parts = append(parts, fmt.Sprintf("…共 %d 项", len(vals)))
			break
		}
		if v.Name != "" && !strings.EqualFold(v.Name, v.Value) {
			parts = append(parts, v.Name+"("+v.Value+")")
			continue
		}
		parts = append(parts, v.Value)
	}
	return strings.Join(parts, " / ")
}

func containsAny(s string, want ...string) bool {
	for _, w := range want {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// Test 对指定 agent 做一次握手诊断// Test 对指定 agent 做一次握手诊断（设置页「测试」按钮）：拉起进程、initialize、
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
	proc, err := m.spawn(agentCfg, ws, m.handshakeBudget(agentCfg), nil, nil)
	if err != nil {
		return err
	}
	m.markWarm(agentCfg.Name) // 测通了说明包已经拉下来了，下次按常规握手预算即可
	proc.kill()
	return nil
}

// Reload 配置热更新：杀掉配置变化/被删除/被停用 agent 的存活进程（上下文由
// session/load 在下次打开时尝试恢复），并发槽位按新值重建。
func (m *Manager) Reload(cfg config.AcpConfig) {
	var toKill []*procState
	m.mu.Lock()
	prev := m.lastCfg
	m.lastCfg = cfg
	// 连接按 agent+目录 共享：配置变了的 agent 名下的连接整条回收（会话下次
	// 使用时按 session/load 重接），没变的连接不动——重载配置不该打断别人的对话。
	n := cfg.Normalized().MaxConcurrent
	keep := make([]*procState, 0, len(m.procs))
	for _, ps := range m.procs {
		agentName := strings.SplitN(ps.key, "\x00", 2)[0]
		ac, ok := cfg.Agent(agentName)
		old, hadOld := prev.Agent(agentName)
		if !ok || !ac.IsEnabled() || !hadOld || agentChanged(old, ac) {
			toKill = append(toKill, ps)
			continue
		}
		keep = append(keep, ps)
	}
	// 上限调小：多出来的活连接直接回收，新槽位表才装得下。
	for len(keep) > n {
		extra := keep[len(keep)-1]
		keep = keep[:len(keep)-1]
		toKill = append(toKill, extra)
	}
	// 槽位表重建后，活下来的连接必须在新表里重新记账并把 slot 指过去：否则它们
	// 回收时抽的是旧 channel 的令牌，新上限被悄悄放大，甚至把空 channel 抽干后
	// 让下一次失败回收死在阻塞归还上。
	m.sem = make(chan struct{}, n)
	for _, ps := range keep {
		m.sem <- struct{}{}
		ps.slot = m.sem
	}
	m.mu.Unlock()
	for _, ps := range toKill {
		m.releaseProc(ps, "配置变更")
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
	all := make([]*procState, 0, len(m.procs))
	for _, ps := range m.procs {
		all = append(all, ps)
	}
	m.mu.Unlock()
	for _, ps := range all {
		m.releaseProc(ps, "服务关闭")
	}
}

// ---------- 持久化 ----------

type threadFile struct {
	Workspaces []*Workspace `json:"workspaces,omitempty"`
	Threads    []*Thread    `json:"threads"`
}

func loadThreads(path string) (*threadFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &threadFile{}, nil
		}
		return nil, fmt.Errorf("读取 thread 文件 %s 失败: %w", path, err)
	}
	var f threadFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("thread 文件 %s 损坏: %w", path, err)
	}
	return &f, nil
}

// saveLocked 落盘（调用方须持 m.mu）。写失败不吞：工作区与会话投影是用户数据。
func (m *Manager) saveLocked() error {
	f := threadFile{
		Workspaces: make([]*Workspace, 0, len(m.wsOrder)),
		Threads:    make([]*Thread, 0, len(m.order)),
	}
	for _, id := range m.wsOrder {
		if ws := m.workspaces[id]; ws != nil {
			f.Workspaces = append(f.Workspaces, ws)
		}
	}
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

// titleFromMessage 从首条消息派生会话标题：第一个非空行、去掉常见的 markdown
// 前缀（# > - * ` 与序号）与行首装饰，折叠连续空白，截 24 个 rune（列表行宽度）。
// 纯符号/纯空白行跳过；整条消息没有可读文本（例如只发了图片）时返回空，
// 留给后面某一轮再定标题。
func titleFromMessage(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "#>-*`~+ \t")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		return truncateRunes(line, 24)
	}
	return ""
}

// truncateRunes 截断到 n 个 rune。
func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
