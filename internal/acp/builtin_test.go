// builtin_test.go 用一台假大脑验内置条目的整条协议链路：initialize 能力、会话建立、
// 一轮对话的事件流（文本/思考/工具）、停止、重接、能力清单为空、删除时清历史。
// 走的是真实的 JSON-RPC（两条内存管道 + Manager 既有的编排），所以这里同时在验
// "进程内对接"这件事本身能不能跑通。
package acp

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/workdir"
)

// stubAsk 是一次被适配器交给大脑的轮次（ctx 上的归属都记下来，供断言）。
type stubAsk struct {
	Text     string
	Imgs     int
	Key      string
	Dir      string
	Origin   string
	Source   string // trace 的来源标记
	ReqID    string
	ToolCall bool
}

// stubRunner 实现 BuiltinRunner：handler 决定这一轮怎么演（发什么事件、返回什么）。
type stubRunner struct {
	mu      sync.Mutex
	asks    []stubAsk
	handler func(ctx context.Context, in stubAsk) (string, error)
	reset   []string
}

func (r *stubRunner) AskWith(ctx context.Context, text string, imgs []llm.Content,
	onDelta func(llm.Delta), opts ...agent.CallOpt) (string, error) {
	in := stubAsk{
		Text: text, Imgs: len(imgs),
		Key:    agent.SessionKeyFrom(ctx),
		Dir:    workdir.From(ctx),
		Origin: agent.OriginFrom(ctx),
		ReqID:  logx.IDFrom(ctx),
	}
	if src, ok := trace.Get(in.ReqID); ok {
		in.Source = src.Source
	}
	r.mu.Lock()
	r.asks = append(r.asks, in)
	h := r.handler
	r.mu.Unlock()
	if h == nil {
		return "", errors.New("stubRunner 没配 handler")
	}
	return h(ctx, in)
}

func (r *stubRunner) Reset(key string) error {
	r.mu.Lock()
	r.reset = append(r.reset, key)
	r.mu.Unlock()
	return nil
}

func (r *stubRunner) askCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asks)
}

func (r *stubRunner) lastAsk(t *testing.T) stubAsk {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.asks) == 0 {
		t.Fatal("大脑没被调用过")
	}
	return r.asks[len(r.asks)-1]
}

func (r *stubRunner) resetKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.reset...)
}

// streamRound 演一轮"有思考、有工具、有正文、有用量"的正常回答。
func streamRound(ctx context.Context, in stubAsk) (string, error) {
	agent.PublishStep(ctx, agent.Step{Kind: "round", Detail: "耗时 0.1s"}) // 不该进时间线
	agent.PublishStep(ctx, agent.Step{Kind: "tool", Name: "exec", Status: "run", Detail: "ls"})
	agent.PublishStep(ctx, agent.Step{Kind: "tool", Name: "exec", Status: "ok", Detail: "note.txt"})
	trace.RecordLLMCall(ctx, 1, "{}", 100, 20, 50)
	return "从 note.txt 读到了", nil
}

// newBuiltinManager 建一台只依赖假大脑的 Manager（清单里的 friday 由 Normalized 注入）。
func newBuiltinManager(t *testing.T, runner BuiltinRunner, policy string) (*Manager, Workspace) {
	t.Helper()
	cfg := config.AcpConfig{Enabled: true, HandshakeTimeoutSeconds: 5, SessionTimeoutSeconds: 10}
	dir := t.TempDir()
	m := NewManager(func() config.AcpConfig { return cfg }, nil, filepath.Join(dir, "threads.json"))
	m.SetBuiltin(runner)
	t.Cleanup(m.Close)
	ws, err := m.CreateWorkspace("proj", []string{dir})
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}
	if policy != "" {
		// 直接改内存里的工作区档位：只读/自动的下发判定靠的就是这份字段
		m.mu.Lock()
		m.workspaces[ws.ID].Policy = policy
		m.mu.Unlock()
	}
	return m, ws
}

func mustBuiltinThread(t *testing.T, m *Manager, ws Workspace) Thread {
	t.Helper()
	th, err := m.CreateSession(config.BuiltinAgentName, ws.ID, "", "")
	if err != nil {
		t.Fatalf("创建内置会话失败: %v", err)
	}
	return th
}

// 内置条目出现在清单里、能建会话、一轮跑完把文本/思考/工具都送到 hooks。
func TestBuiltinTurnStreamsEvents(t *testing.T) {
	r := &stubRunner{handler: func(ctx context.Context, in stubAsk) (string, error) {
		// 工作区说明是 manager 随首轮附带的文本块，内置会话同样收得到（与外部 agent 一致），
		// 所以这里验的是"用户正文在里面"，不是全文相等。
		if !strings.Contains(in.Text, "看下目录里有什么") {
			t.Errorf("问题文本没送到: %q", in.Text)
		}
		// 只演事件、不发 delta：正文靠 AskWith 的返回值，顺带验"增量为空时补发一次"
		// 那条兜底在内置链路上也成立。
		return streamRound(ctx, in)
	}}
	m, ws := newBuiltinManager(t, r, "")
	th := mustBuiltinThread(t, m, ws)

	var mu sync.Mutex
	var text, thought []string
	var steps []agent.Step
	hooks := TurnHooks{
		OnText:    func(s string) { mu.Lock(); text = append(text, s); mu.Unlock() },
		OnThought: func(s string) { mu.Lock(); thought = append(thought, s); mu.Unlock() },
		OnStep:    func(s agent.Step) { mu.Lock(); steps = append(steps, s); mu.Unlock() },
	}
	res, err := m.Send(context.Background(), th.ID, "看下目录里有什么", nil, hooks)
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if res.Answer != "从 note.txt 读到了" {
		t.Fatalf("回答不符: %q", res.Answer)
	}
	if res.StopReason != string(acp.StopReasonEndTurn) {
		t.Fatalf("结束原因应为 end_turn，实际 %q", res.StopReason)
	}
	if res.In != 100 || res.Out != 20 {
		t.Fatalf("单轮用量应取自 trace（100/20），实际 %d/%d", res.In, res.Out)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(text, "") != "从 note.txt 读到了" {
		t.Fatalf("文本增量没汇成完整回答: %#v", text)
	}
	// 工具：一次调用 = 一行（run → ok 必须归到同一个 toolCallId），round 事件不进时间线
	if len(steps) != 2 {
		t.Fatalf("应只投出两条工具事件（round 不算）: %#v", steps)
	}
	if steps[0].ID == "" || steps[0].ID != steps[1].ID {
		t.Fatalf("同一次工具调用的 run/ok 必须同 id 才能并成一行: %#v", steps)
	}
	if steps[0].Status != "run" || steps[1].Status != "ok" || steps[0].Name != "exec" {
		t.Fatalf("工具状态/名称不符: %#v", steps)
	}
	if steps[0].Detail != "ls" || steps[1].Detail != "note.txt" {
		t.Fatalf("工具详情（参数/结果）应带上: %#v", steps)
	}
	if len(thought) != 0 {
		t.Fatalf("这一轮没发思考，不该有思考帧: %#v", thought)
	}
	got, _ := m.Get(th.ID)
	if len(got.Messages) != 2 || got.Messages[1].Role != "assistant" || got.Messages[1].In != 100 {
		t.Fatalf("投影时间线不符: %+v", got.Messages)
	}
	if got.SessionID == "" {
		t.Fatal("内置会话也要记 sessionId（重接与停止都靠它）")
	}
}

// 会话归属要独立于聊天页：key 带 acp- 前缀、目录=会话主目录、来源标 acp。
func TestBuiltinTurnCarriesSessionContext(t *testing.T) {
	r := &stubRunner{handler: streamRound}
	m, ws := newBuiltinManager(t, r, "")
	th := mustBuiltinThread(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "一句", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	ask := r.lastAsk(t)
	// sessionId 是建会话时由"对面"生成的，Manager 只在投影里回写，所以拿最新的会话记录比
	fresh, _ := m.Get(th.ID)
	if !strings.HasPrefix(ask.Key, "acp-") || ask.Key == agent.SessionWeb || strings.HasPrefix(ask.Key, "work-") {
		t.Fatalf("内置会话的池内 key 该自成一族，实际 %q", ask.Key)
	}
	if ask.Key != builtinSessionKey(fresh.SessionID) {
		t.Fatalf("池内 key 应由会话 id 派生（重接才找得到同一份历史）: %q vs %q",
			ask.Key, builtinSessionKey(fresh.SessionID))
	}
	if ask.Dir != ws.Dirs[0] {
		t.Fatalf("工作目录应为会话主目录: %q vs %q", ask.Dir, ws.Dirs[0])
	}
	if ask.Origin != agent.OriginAcp || ask.Source != "acp" {
		t.Fatalf("来源应标成 acp（成本与轨迹页要能分出这是工作区发起的）: %q / %q", ask.Origin, ask.Source)
	}
	// 第二条会话是另一份上下文，不许和第一条共用池内 key
	th2 := mustBuiltinThread(t, m, ws)
	if _, err := m.Send(context.Background(), th2.ID, "另一句", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if k2 := r.lastAsk(t).Key; k2 == ask.Key {
		t.Fatalf("两条内置会话串在同一份上下文上了: %q", k2)
	}
}

// 没有会话级能力项：清单为空、切档位给出可行动的说明、工作区档位明确记为"没下发"。
func TestBuiltinHasNoSessionCapabilities(t *testing.T) {
	r := &stubRunner{handler: streamRound}
	m, ws := newBuiltinManager(t, r, WorkspacePolicyReadOnly)
	th := mustBuiltinThread(t, m, ws)
	caps, err := m.Capabilities(th.ID)
	if err != nil {
		t.Fatalf("取能力清单失败: %v", err)
	}
	if len(caps) != 0 {
		t.Fatalf("内置条目不该上报会话级能力项: %+v", caps)
	}
	if _, err := m.SetCapability(th.ID, CapKindModel, "gpt-x"); err == nil ||
		!strings.Contains(err.Error(), "全局") {
		t.Fatalf("切模型应说明全局设置在哪，实际: %v", err)
	}
	got, _ := m.Get(th.ID)
	if got.PolicyApplied != "" || !strings.Contains(got.PolicyNote, "内置") ||
		!strings.Contains(got.PolicyNote, "只读") {
		t.Fatalf("只读档位对内置会话要明说没下发（不许假装生效）: %+v", got)
	}
}

// 连接被回收后重开：走 session/load，落到同一个池内 key，且不标"上下文已重置"。
func TestBuiltinReconnectKeepsContext(t *testing.T) {
	r := &stubRunner{handler: streamRound}
	m, ws := newBuiltinManager(t, r, "")
	th := mustBuiltinThread(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "第一轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	first := r.lastAsk(t)
	sidAfterFirst, _ := m.Get(th.ID)
	// 模拟空闲回收（连接没了，但会话 id 已经写进投影）
	m.killProc(procKey(config.BuiltinAgentName, th.Cwd), "测试回收")
	if _, err := m.Send(context.Background(), th.ID, "第二轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("回收后再发失败: %v", err)
	}
	second := r.lastAsk(t)
	if second.Key != first.Key {
		t.Fatalf("重接应回到同一份上下文: %q vs %q", second.Key, first.Key)
	}
	got, _ := m.Get(th.ID)
	if got.Degraded {
		t.Fatalf("内置会话重接成功不该被标成降级: %+v", got)
	}
	if got.SessionID != sidAfterFirst.SessionID {
		t.Fatalf("会话 id 应保持不变: %q vs %q", got.SessionID, sidAfterFirst.SessionID)
	}
}

// 停止：session/cancel → 大脑的 ctx 被取消 → 这一轮按 cancelled 收尾，不算失败。
func TestBuiltinStopCancelsTurn(t *testing.T) {
	running := make(chan struct{})
	r := &stubRunner{handler: func(ctx context.Context, _ stubAsk) (string, error) {
		close(running)
		<-ctx.Done() // 一直挂着，直到取消
		return "", ctx.Err()
	}}
	m, ws := newBuiltinManager(t, r, "")
	th := mustBuiltinThread(t, m, ws)

	done := make(chan TurnResult, 1)
	go func() {
		res, err := m.Send(context.Background(), th.ID, "长任务", nil, TurnHooks{})
		if err != nil {
			t.Errorf("取消的一轮不该算失败: %v", err)
		}
		done <- res
	}()
	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("等大脑开跑超时")
	}
	waitFor(t, 5*time.Second, "会话进入运行态", func() bool {
		got, _ := m.Get(th.ID)
		return got.State == StateRunning && got.SessionID != ""
	})
	if !m.Stop(th.ID) {
		t.Fatal("Stop 应能取消进行中的一轮")
	}
	select {
	case res := <-done:
		if res.StopReason != string(acp.StopReasonCancelled) {
			t.Fatalf("停止后应按 cancelled 收尾，实际 %q", res.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("等这一轮收尾超时")
	}
}

// 大脑报错要走"失败"路径：错误原文上浮，时间线上留一条失败说明（不是静默的空回答）。
func TestBuiltinRunnerErrorSurfaces(t *testing.T) {
	r := &stubRunner{handler: func(ctx context.Context, _ stubAsk) (string, error) {
		return "", errors.New("模型服务拒绝了当前密钥")
	}}
	m, ws := newBuiltinManager(t, r, "")
	th := mustBuiltinThread(t, m, ws)
	_, err := m.Send(context.Background(), th.ID, "一句", nil, TurnHooks{})
	if err == nil || !strings.Contains(err.Error(), "模型服务拒绝了当前密钥") {
		t.Fatalf("大脑的错误应原样上浮，实际: %v", err)
	}
	got, _ := m.Get(th.ID)
	if len(got.Messages) < 2 {
		t.Fatalf("问答都要留下: %+v", got.Messages)
	}
	last := got.Messages[len(got.Messages)-1]
	if last.Role != "note" || !strings.Contains(last.Text, "内置") {
		t.Fatalf("失败说明要指向内置会话，实际: %+v", last)
	}
	asst := got.Messages[len(got.Messages)-2]
	if asst.Stopped != "error" {
		t.Fatalf("失败那一轮要标 error: %+v", asst)
	}
}

// 图片解码交给大脑；内置吃不下的块（音频/资源链接）要在时间线上说一声，不能静默丢。
func TestBuiltinPromptBlocksMapping(t *testing.T) {
	r := &stubRunner{handler: func(ctx context.Context, in stubAsk) (string, error) {
		if in.Imgs != 1 {
			t.Errorf("应带上 1 张图片，实际 %d", in.Imgs)
		}
		return streamRound(ctx, in)
	}}
	m, ws := newBuiltinManager(t, r, "")
	th := mustBuiltinThread(t, m, ws)
	res, err := m.Send(context.Background(), th.ID, "这张图里是什么",
		[]Image{{MIME: "image/png", Data: []byte("fake-bytes")}}, TurnHooks{})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if !strings.Contains(res.Answer, "从 note.txt") {
		t.Fatalf("回答不符: %q", res.Answer)
	}
	ask := r.lastAsk(t)
	if !strings.Contains(ask.Text, "这张图里是什么") {
		t.Fatalf("问题文本没送到: %q", ask.Text)
	}
	// 协议里没声明吃的块：直接经 agent 侧发一条 unsupported 的说明（走 promptBlocks）
	_, _, note := promptBlocks([]acp.ContentBlock{
		acp.TextBlock("正文"),
		{Audio: &acp.ContentBlockAudio{Data: base64.StdEncoding.EncodeToString([]byte("x")), MimeType: "audio/wav"}},
	})
	if note == "" || !strings.Contains(note, "音频") {
		t.Fatalf("音频块应给出被忽略的说明，实际 %q", note)
	}
}

// 没注入大脑（装配缺失）：报错要说清是内置会话跑不起来，而不是含糊的"agent 坏了"。
func TestBuiltinWithoutRunnerFailsClearly(t *testing.T) {
	r := &stubRunner{handler: streamRound}
	m, ws := newBuiltinManager(t, r, "")
	m.SetBuiltin(nil)
	th := mustBuiltinThread(t, m, ws)
	_, err := m.Send(context.Background(), th.ID, "一句", nil, TurnHooks{})
	if err == nil || !strings.Contains(err.Error(), "内置 Friday 未接入对话大脑") {
		t.Fatalf("装配缺失要明说，实际: %v", err)
	}
}

// 删会话要连池内那份历史一起清（否则 acp-<id> 留在数据目录里没人再列得出它）。
func TestBuiltinDeleteClearsPoolSession(t *testing.T) {
	r := &stubRunner{handler: streamRound}
	m, ws := newBuiltinManager(t, r, "")
	th := mustBuiltinThread(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "一句", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	fresh, _ := m.Get(th.ID)
	want := builtinSessionKey(fresh.SessionID)
	if !m.Delete(th.ID) {
		t.Fatal("删除会话失败")
	}
	keys := r.resetKeys()
	if len(keys) != 1 || keys[0] != want {
		t.Fatalf("应清掉这条会话的池内历史（%s），实际 %v", want, keys)
	}
}

// 内置条目也在"测试握手"里走一遍：不 exec 子进程，只验连接能建立。
func TestBuiltinTestHandshake(t *testing.T) {
	r := &stubRunner{handler: streamRound}
	m, _ := newBuiltinManager(t, r, "")
	if err := m.Test(config.BuiltinAgentName); err != nil {
		t.Fatalf("内置条目的握手诊断失败: %v", err)
	}
	if n := r.askCount(); n != 0 {
		t.Fatalf("握手诊断不该跑一轮对话，实际跑了 %d 次", n)
	}
}

// 保存配置（Reload）不该把内置会话的连接当成"agent 已删除"回收掉。
func TestBuiltinConnectionSurvivesReload(t *testing.T) {
	r := &stubRunner{handler: streamRound}
	m, ws := newBuiltinManager(t, r, "")
	th := mustBuiltinThread(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "一句", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if m.procOf(procKey(config.BuiltinAgentName, th.Cwd)) == nil {
		t.Fatal("内置连接应在场（前置条件）")
	}
	m.Reload(config.AcpConfig{Enabled: true, IdleKillSeconds: -1}) // 原始清单：没有 friday
	if m.procOf(procKey(config.BuiltinAgentName, th.Cwd)) == nil {
		t.Fatal("Reload 后内置会话的连接被回收了（归一化没做到位）")
	}
	// 会话不能因为"配置里没有这台 agent"而显示成已失联
	if got, _ := m.Get(th.ID); got.State == StateOrphaned {
		t.Fatalf("内置会话被当成孤儿: %+v", got)
	}
}
