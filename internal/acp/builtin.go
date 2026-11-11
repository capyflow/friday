// builtin.go 把内置 Friday 自己接成一台「智能体」：不 exec 子进程，而是用两条内存
// 管道把 agent 侧适配器与既有的 client 侧连接对接起来——子进程的 stdin/stdout 换成
// io.Pipe，JSON-RPC 照走，协议层、Manager 的会话编排、轮前快照、改动还原、投影时间线、
// SSE 帧、停止与重接全部原样复用（所以这里只新增「怎么把一轮交给内置 Brain」）。
//
// 与外部 agent 的差别都写在这里，不假装等价：
//   - 模型与权限是全局的：内置对话用「设置 → 模型服务 / 权限」那一份配置，进程级共享。
//     所以 initialize 不回 configOptions 也不回 modes——两条工作区会话各选一个模型会
//     互相打架。工作区默认审批档位因此不下发，原因写进 PolicyNote（见 applyWorkspacePolicy）。
//   - 不声明 additionalDirectories：内置文件工具的读取范围是 config.yaml 的 file.allowDirs
//     加上会话主目录（workdir），扩展根没有下发通道，工作区说明据实描述。
//   - session/load 是真的重接：会话历史存在 SessionPool（按 key 落盘，重启仍在），
//     不需要像外部 agent 那样重放对话；Friday 侧的时间线本来就落在 acp-threads.json 里。
//   - 工具审批不经过 request_permission：内置 exec/写文件的授权卡由框架的审批器
//     直接投进同一个 approval.Queue（与普通对话同一入口），不重复翻译成协议请求。
package acp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/llm"
	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/workdir"
)

// BuiltinRunner 是内置会话跑一轮所需的最小接口（*agent.Brain 满足它）。
// 接口放在消费方这一侧，测试才能拿一个假大脑验协议映射——真 Brain 要配 provider
// 与会话池，那部分已经在 internal/agent 自己覆盖过了，这里不该重复搭一遍。
type BuiltinRunner interface {
	AskWith(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta),
		opts ...agent.CallOpt) (string, error)
	// Reset 清掉某条会话的上下文与持久层历史（删除工作区会话时一并清理）。
	Reset(key string) error
}

// builtinKeyPrefix 是内置会话在 SessionPool 里的 key 前缀。带前缀是为了不跟聊天页的
// 会话（work-*）或主会话抢同一份历史：工作区里那条会话是它自己的上下文，
// 「在终端里跟小五聊过的」不该被当成这里聊过的。
const builtinKeyPrefix = "acp-"

// builtinSessionKey 把 ACP 会话 id 翻成 SessionPool 的会话 key。纯函数，所以重启后
// 拿同一个 id 仍落到同一份历史上——session/load 的凭据就是这么成立的。
func builtinSessionKey(sid string) string {
	return agent.CurrentKey(builtinKeyPrefix + sid)
}

// builtinSession 是这条连接上的一个内置会话（id → 池内 key + 目录）。
type builtinSession struct {
	id  string
	key string
	cwd string
}

// builtinAgent 是内置 Friday 的 agent 侧实现，一条连接一个实例（与外部 agent 的
// 「一 agent + 主目录 一进程」同构）。对话历史不在这里：它归 SessionPool。
type builtinAgent struct {
	brain BuiltinRunner

	mu   sync.Mutex
	cwd  string // 这条连接的默认目录（NewSession 没给 cwd 时兜底）
	sess map[string]*builtinSession

	conn *acp.AgentSideConnection // 反向通知通道（session/update）
}

func newBuiltinAgent(brain BuiltinRunner, cwd string) *builtinAgent {
	return &builtinAgent{brain: brain, cwd: cwd, sess: map[string]*builtinSession{}}
}

// register 记下会话（NewSession / LoadSession 共用），返回会话记录。
// 同一个 id 重复注册沿用第一次的 key：id 是历史归属的唯一凭据，换个 key 等于换一份上下文。
func (a *builtinAgent) register(sid, cwd string) *builtinSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.sess[sid]; ok {
		if cwd != "" {
			s.cwd = cwd
		}
		return s
	}
	s := &builtinSession{id: sid, key: builtinSessionKey(sid), cwd: cwd}
	a.sess[sid] = s
	return s
}

func (a *builtinAgent) session(sid string) (*builtinSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sess[sid]
	return s, ok
}

// send 投一条 session/update。用 context.Background()：这一轮的 ctx 可能刚被取消
// （用户点停止），而已经生成的那半句文本仍要送到 client 侧的接收器里，否则时间线上
// 会留下「停止前写的字凭空消失」。管道对端已经断开时 sendMessage 自己报错，丢弃即可。
func (a *builtinAgent) send(sid string, u acp.SessionUpdate) {
	if a.conn == nil {
		return
	}
	if err := a.conn.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId(sid), Update: u,
	}); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		log.Printf("[智能体] 内置会话 %s 推送过程事件失败（忽略）: %v", sid, sanitizeErrLog(err))
	}
}

// sanitizeErrLog 沿用日志脱敏（错误里可能带绝对路径）。
func sanitizeErrLog(err error) string { return sanitizeLog(err.Error()) }

// ---------- acp.Agent 实现 ----------

func (a *builtinAgent) Initialize(_ context.Context, p acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: p.ProtocolVersion,
		AgentInfo:       &acp.Implementation{Name: "friday", Version: "builtin"},
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: true, // 上下文按会话 key 落盘，重开真的接得回来
			PromptCapabilities: acp.PromptCapabilities{
				Image: true, // 随消息的图片解码后交给内置模型（不支持视觉的模型会照直报错）
			},
			// sessionCapabilities 全空：不声明 close/list/resume/additionalDirectories，
			// client 侧就不会调过来（Manager 按能力位决定是否下发）。
		},
	}, nil
}

func (a *builtinAgent) NewSession(_ context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	cwd := a.cwd
	if strings.TrimSpace(p.Cwd) != "" {
		// 与内置会话目录同一套校验：必须已存在且是目录。会话跑在一个不存在的目录里
		// 只会在第一次 read_file 时炸，这里提前失败更好读。
		abs, err := workdir.Normalize(p.Cwd)
		if err != nil {
			return acp.NewSessionResponse{}, err
		}
		cwd = abs
	}
	if len(p.AdditionalDirectories) > 0 {
		log.Printf("[智能体] 内置 Friday 收到 %d 个扩展目录，按设计忽略（读取范围=会话主目录 + config 的 file.allowDirs）",
			len(p.AdditionalDirectories))
	}
	id := randHex(8)
	a.register(id, cwd)
	return acp.NewSessionResponse{SessionId: acp.SessionId(id)}, nil
}

// LoadSession 重新挂上既有会话：历史在 SessionPool 里，不需要重放给 client
// （Friday 侧的时间线另有投影，重放只会把同一段对话写两遍）。
func (a *builtinAgent) LoadSession(_ context.Context, p acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	if strings.TrimSpace(string(p.SessionId)) == "" {
		return acp.LoadSessionResponse{}, errors.New("内置 Friday: 缺少 sessionId")
	}
	cwd := a.cwd
	if strings.TrimSpace(p.Cwd) != "" {
		abs, err := workdir.Normalize(p.Cwd)
		if err != nil {
			return acp.LoadSessionResponse{}, err
		}
		cwd = abs
	}
	a.register(string(p.SessionId), cwd)
	return acp.LoadSessionResponse{}, nil
}

// Prompt 把这一轮交给内置 Brain，并把流式增量与工具事件翻成 ACP 的 session/update。
// ctx 由 SDK 在收到 session/cancel 时取消（AgentSideConnection 自己维护 per-session
// 取消句柄），所以这里只需顺着 ctx 收口。
func (a *builtinAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	sid := string(p.SessionId)
	s, ok := a.session(sid)
	if !ok {
		return acp.PromptResponse{}, fmt.Errorf("内置 Friday: 没有会话 %s（先建新会话再发消息）", sid)
	}
	if a.brain == nil {
		return acp.PromptResponse{}, errors.New("内置 Friday 未接入对话大脑（启动装配缺失）")
	}
	question, imgs, unsupported := promptBlocks(p.Prompt)
	if question == "" && len(imgs) == 0 {
		return acp.PromptResponse{}, errors.New("内置 Friday: 消息为空")
	}

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rctx = logx.WithID(rctx, logx.NextID())
	id := logx.IDFrom(rctx)
	rctx = agent.WithSessionKey(rctx, s.key)
	rctx = agent.WithOrigin(rctx, agent.OriginAcp)
	rctx = workdir.With(rctx, s.cwd)
	trace.Begin(id, traceSourceAcp, s.key, question)
	if unsupported != "" {
		a.note(sid, unsupported)
	}

	// 工具事件泵：内置链路的过程事件走 agent.SubscribeSteps，这里翻成 tool_call /
	// tool_call_update 投出去（与 SSE 的 step 帧同源，所以工作区时间线的形状一致）。
	steps, stopSteps := agent.SubscribeSteps(id)
	defer stopSteps() // 退出时摘订阅（本请求的 id 不会被别处复用，留着只会越积越多）
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		pumpToolSteps(a, sid, steps, rctx.Done())
	}()

	streamed := 0
	answer, err := a.brain.AskWith(rctx, question, imgs, func(d llm.Delta) {
		if d.Thinking != "" {
			a.send(sid, acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
				Content: acp.TextBlock(d.Thinking),
			}})
		}
		if d.Text != "" {
			streamed += len([]rune(d.Text))
			a.send(sid, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.TextBlock(d.Text),
			}})
		}
	})
	if err != nil {
		trace.End(id, err.Error())
	} else {
		trace.End(id, "")
	}
	cancel() // 结束事件泵（它会先把通道里积压的过程事件抽干）
	<-pumpDone

	res := acp.PromptResponse{StopReason: acp.StopReasonEndTurn}
	if u, ok := trace.Get(id); ok {
		if usage := u.Usage(); usage != nil {
			res.Usage = &acp.Usage{
				InputTokens: int(usage.In), OutputTokens: int(usage.Out),
				TotalTokens: int(usage.In + usage.Out),
			}
		}
	}
	if err != nil {
		// 取消（用户停止/页面断开）按协议回 cancelled，而不是把错误抛给 client：
		// 已流出的文本留在接收器里，时间线收的是「被取消的这一轮」，不是一句报错。
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			res.StopReason = acp.StopReasonCancelled
			return res, nil
		}
		return acp.PromptResponse{}, err
	}
	// 增量为空但拿到了完整回答（非流式路径/模型只回终稿）：补发一次，别让时间线空着
	if streamed == 0 && strings.TrimSpace(answer) != "" {
		a.send(sid, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			Content: acp.TextBlock(answer),
		}})
	}
	return res, nil
}

// note 投一条系统备注进时间线（用 user_message_chunk 之外的形态没有意义，这里直接
// 以文本增量入答案流——它说的是「这一轮Friday 少收了什么」，必须让用户看见）。
func (a *builtinAgent) note(sid, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	a.send(sid, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
		Content: acp.TextBlock("[Friday 内置会话] " + text),
	}})
}

// Cancel：SDK 在分发这条通知前已经取消了该会话进行中的 Prompt（见 vendored
// AgentSideConnection 的 sessionCancels），所以这里无需再做别的。
func (a *builtinAgent) Cancel(_ context.Context, p acp.CancelNotification) error { return nil }

// 内置条目不支持的调用：不声明相应能力，client 就不会调过来；真被调到时明说原因，
// 不静默成功（静默成功会让调用方以为动作已经生效）。
func (a *builtinAgent) Authenticate(_ context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, errors.New("内置 Friday 不需要登录：模型密钥在「设置 → 模型服务」里配置")
}

func (a *builtinAgent) Logout(_ context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, errors.New("内置 Friday 没有独立的登录态（用的是「设置 → 模型服务」的密钥）")
}

func (a *builtinAgent) CloseSession(_ context.Context, _ acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, fmt.Errorf("内置 Friday 不支持关闭会话（删掉工作区里那条会话即可，历史会一并清除）")
}

func (a *builtinAgent) ListSessions(_ context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, errors.New("内置 Friday 的会话清单在工作区页面里，不经协议枚举")
}

func (a *builtinAgent) ResumeSession(_ context.Context, _ acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, errors.New("内置 Friday 用 session/load 重接会话（历史按会话 key 落盘，无需另开恢复通道）")
}

func (a *builtinAgent) SetSessionConfigOption(_ context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, errors.New("内置 Friday 的模型与权限是全局设置（设置 → 模型服务 / 权限），没有会话级配置项")
}

func (a *builtinAgent) SetSessionMode(_ context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, errors.New("内置 Friday 没有会话级审批档位：工作区的档位只影响外部 agent，内置会话沿用「设置 → 权限」")
}

// ---------- 事件翻译 ----------

// traceSourceAcp 是内置会话跑在智能体空间里时的轨迹来源标记（与外部 agent 的
// 轨迹区分开，成本页/轨迹页能看出这条问答是从工作区发起的）。
const traceSourceAcp = "acp"

// promptBlocks 把 ACP 的内容块拆成内置对话需要的「问题文本 + 图片」。
// 文本块按原顺序拼（工作区说明与交接摘要也是文本块，排在用户正文前面，
// 与发给外部 agent 的内容一致）；音频/资源链接这类内置链路吃不下的块，
// 回一句说明让用户知道这轮少收了什么，而不是悄悄丢掉。
func promptBlocks(blocks []acp.ContentBlock) (question string, imgs []llm.Content, note string) {
	var texts []string
	var skipped []string
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			if t := strings.TrimSpace(b.Text.Text); t != "" {
				texts = append(texts, t)
			}
		case b.Image != nil:
			data, err := base64.StdEncoding.DecodeString(b.Image.Data)
			if err != nil {
				skipped = append(skipped, "一张图片解码失败")
				continue
			}
			mime := b.Image.MimeType
			if mime == "" {
				mime = "image/png"
			}
			imgs = append(imgs, llm.Content{Type: llm.ContentImage, Data: data, MIME: mime})
		case b.Audio != nil:
			skipped = append(skipped, "一段音频")
		case b.ResourceLink != nil:
			skipped = append(skipped, "一个资源链接")
		case b.Resource != nil:
			skipped = append(skipped, "一段嵌入资源")
		}
	}
	if len(skipped) > 0 {
		note = "这条消息里的 " + strings.Join(uniqueStrs(skipped), "、") +
			"内置会话暂不吃（只收文本与图片），已忽略。"
	}
	return strings.Join(texts, "\n\n"), imgs, note
}

func uniqueStrs(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// pumpToolSteps 把内置对话的过程事件（工具调用）投成 ACP 的 tool_call/tool_call_update。
//
// 内置链路的 Step 没有 toolCallId（框架的 OnBefore/OnAfterToolCall 只给工具名），
// 而 ACP 时间线按 toolCallId 归并——同一次调用的「运行中」与「完成」必须落在同一行，
// 否则每个工具都会变成「一个转圈的 + 一个已完成的」。这里按工具名配对：run 时发一个
// id 并入队，ok/fail 时取回该工具名最早未收尾的那个 id。并发同名调用会串到同一批 id
// 上（详情文本仍各归各），这比每条事件新起一行更接近真实时间线。
//
// 退出纪律照 SSE 那条（chat.go）：ctx 结束时先把通道里积压的事件抽干再退，
// 否则轮末的 tool ok/fail 会留在通道里，前端那一行永远转圈。
func pumpToolSteps(a *builtinAgent, sid string, steps <-chan agent.Step, done <-chan struct{}) {
	open := map[string][]acp.ToolCallId{}
	seq := 0
	emit := func(s agent.Step) {
		switch s.Status {
		case "ok", "fail":
			id := takeID(open, s.Name)
			if id == "" {
				// 没见过的收尾（前置 run 丢了）：补一行，状态直接给终态
				seq++
				id = acp.ToolCallId(fmt.Sprintf("%s-%d", sid, seq))
			}
			status := acp.ToolCallStatusCompleted
			if s.Status == "fail" {
				status = acp.ToolCallStatusFailed
			}
			out := s.Detail
			a.send(sid, acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
				ToolCallId: id, Status: &status, RawOutput: out,
			}})
			return
		default: // run（以及缺状态的兜底）
			seq++
			id := acp.ToolCallId(fmt.Sprintf("%s-%d", sid, seq))
			open[s.Name] = append(open[s.Name], id)
			status := acp.ToolCallStatusInProgress
			a.send(sid, acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: id,
				Title:      toolStepTitle(s),
				Kind:       builtinToolKind(s.Name),
				Status:     status,
				RawInput:   s.Detail,
			}})
		}
	}
	for {
		select {
		case <-done:
			for {
				select {
				case s, ok := <-steps:
					if !ok {
						return
					}
					if s.Kind == "tool" {
						emit(s)
					}
				default:
					return
				}
			}
		case s, ok := <-steps:
			if !ok {
				return
			}
			if s.Kind == "tool" {
				emit(s)
			}
		}
	}
}

func takeID(open map[string][]acp.ToolCallId, name string) acp.ToolCallId {
	q := open[name]
	if len(q) == 0 {
		return ""
	}
	id := q[0]
	if len(q) == 1 {
		delete(open, name)
	} else {
		open[name] = q[1:]
	}
	return id
}

// toolStepTitle 给一次工具调用起一行标题。内置工具名是英文标识符，
// 工作区时间线对外部 agent 也是照直显示工具名（+扳手图标），这里保持一致，
// 不做第二套中文映射（映射只覆盖自家工具，混在一张表里反而更难认）。
func toolStepTitle(s agent.Step) string {
	if s.Name == "" {
		return "工具调用"
	}
	return s.Name
}

// builtinToolKind 把内置工具名映射到 ACP 的工具类别（只影响协议里的 kind）。
func builtinToolKind(name string) acp.ToolKind {
	switch name {
	case "read_file", "list_files":
		return acp.ToolKindRead
	case "write_file", "edit_file", "edit_files":
		return acp.ToolKindEdit
	case "exec", "run_command":
		return acp.ToolKindExecute
	case "grep", "glob", "search", "web_search", "search_code", "find_files",
		"git_status", "git_diff", "git_log":
		return acp.ToolKindSearch
	case "web_fetch":
		return acp.ToolKindFetch
	case "delete_file":
		return acp.ToolKindDelete
	default:
		return acp.ToolKindOther
	}
}

// ---------- 进程内连接 ----------

// startBuiltin 建立一条进程内的 ACP 连接：bridge + client 侧连接的构造与外部 agent
// 完全一致，只是 stdio 换成两条 io.Pipe，对面接 builtinAgent 而不是子进程。
// Manager 上的可替换字段叫 spawnBuiltin（测试注入用），实现在这里。
//
// 没有 stderr 落盘文件（没有子进程），因此 agentProc.stderrFile 留空——报错文案里
// 的「stderr: 」对内置会话恒为空串，那些路径本身也基本走不到（不 exec 就不会
// 握手失败/未登录）。
func (m *Manager) startBuiltin(cfg config.AcpAgentConfig, cwd string, handshake time.Duration,
	onPermission permissionHook, onExit func()) (*agentProc, error) {
	if m.builtinBrain == nil {
		return nil, errors.New("内置 Friday 未接入对话大脑：工作区里跑不了内置会话（内部装配缺失，需重启 Friday）")
	}
	// client → agent、agent → client 各一条管道。
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()

	b := newBridge(onPermission)
	p := &agentProc{
		agent:   cfg.Name,
		cwd:     cwd,
		events:  b.events,
		bridge:  b,
		builtin: true,
	}
	p.conn = acp.NewClientSideConnection(b, c2aW, a2cR)
	p.done = p.conn.Done()

	ba := newBuiltinAgent(m.builtinBrain, cwd)
	asc := acp.NewAgentSideConnection(ba, a2cW, c2aR)
	ba.conn = asc

	var killOnce sync.Once
	p.kill = func() {
		killOnce.Do(func() {
			// 先关写端让对端读到 EOF（client 的 Done 由此关闭 → onExit 收尾），
			// 再关读端解除可能阻塞在管道上的写入。
			_ = c2aW.Close()
			_ = a2cW.Close()
			_ = c2aR.CloseWithError(io.ErrClosedPipe)
			_ = a2cR.CloseWithError(io.ErrClosedPipe)
		})
	}
	go func() {
		<-p.done
		p.kill()
		if onExit != nil {
			onExit()
		}
	}()

	ictx, icancel := context.WithTimeout(context.Background(), handshake)
	defer icancel()
	initRes, err := p.conn.Initialize(ictx, acp.InitializeRequest{
		ProtocolVersion:    acp.ProtocolVersionNumber,
		ClientInfo:         &acp.Implementation{Name: "friday", Version: "1.0"},
		ClientCapabilities: clientCapabilitiesFor(cfg),
	})
	if err != nil {
		p.kill()
		return nil, fmt.Errorf("内置 Friday 连接失败：%v", err)
	}
	p.loadSession = initRes.AgentCapabilities.LoadSession
	p.extraRoots = initRes.AgentCapabilities.SessionCapabilities.AdditionalDirectories != nil
	log.Printf("[智能体] 内置 %s 已就绪（cwd=%s，loadSession=%v，additionalDirectories=%v）",
		cfg.Name, cwd, p.loadSession, p.extraRoots)
	return p, nil
}
