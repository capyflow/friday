// brain.go 定义机器人的"对话大脑"：所有入口（网页/终端/消息通道/定时任务）
// 共用同一个 Agent，按会话 key（SessionPool）持有各自独立的对话历史。
// 同一会话内的请求串行执行（上下文连续、流式不交叉），不同会话可并行对话；
// 流式增量经 Agent.WithDelta 按请求路由到各自的订阅者，互不串扰。
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/skill"
	"github.com/capyflow/friday/internal/trace"
)

// Brain 持有 Agent 与会话池，按会话 key 串行化提问。
// rw 锁保护 Agent 的原子替换：Ask 持读锁（不同会话并行），SetAgent 持写锁
// （等待全部进行中的请求结束后生效）。
type Brain struct {
	rw    sync.RWMutex
	agent *vxagent.Agent
	pool  *SessionPool
	ctx   *ContextManager // 上下文引擎管理器（Reset 清归档、热更新重建用；可为 nil）

	customMu sync.RWMutex           // 保护 custom（自定义指令表，SetCustom 可运行期替换）
	custom   []config.CustomCommand // 自定义指令：触发词 → 提示词展开

	skillMu   sync.RWMutex      // 保护技能仓库指针与匹配开关（SetSkills 可运行期替换）
	skills    *skill.Store      // 技能仓库（可为 nil：不启用技能）
	skillAuto skill.MatchConfig // 自动匹配开关与阈值

	memMu  sync.RWMutex      // 保护长期记忆仓库指针
	memory *memory.WikiStore // 长期记忆（可为 nil：不注入速览）

	projMu     sync.RWMutex // 保护工作目录（切工作模式时会变）
	projectDir string       // 当前工作目录：AGENTS.md / README.md 从这里找（空=不注入）
}

// NewBrain 创建对话大脑。pool 为会话池（含持久化存储），ctx 为上下文引擎
// 管理器（均可为 nil：pool 无持久化则重启失忆，ctx 为 nil 则不治理上下文），
// 由调用方构建。
func NewBrain(ag *vxagent.Agent, pool *SessionPool, ctx *ContextManager) *Brain {
	return &Brain{agent: ag, pool: pool, ctx: ctx}
}

// CtxMgr 返回上下文引擎管理器（热更新路径重建引擎用，可为 nil）。
func (b *Brain) CtxMgr() *ContextManager { return b.ctx }

// SetMemory 注入长期记忆仓库（启动时调用一次）：每个会话开始时注入一份
// 「记忆速览」，让 agent 一开口就知道用户是谁、有哪些已知偏好——清掉上下文之后
// 也不必让用户再提醒"去看看记忆"。
func (b *Brain) SetMemory(store *memory.WikiStore) {
	b.memMu.Lock()
	b.memory = store
	b.memMu.Unlock()
}

// memoryWarmup 生成会话开始时注入的记忆速览（没有记忆页面时返回空串，不注入）。
func (b *Brain) memoryWarmup() string {
	b.memMu.RLock()
	store := b.memory
	b.memMu.RUnlock()
	if store == nil {
		return ""
	}
	digest := store.Digest(0, 0)
	if digest == "" {
		return ""
	}
	return "【长期记忆速览｜会话开始时自动注入，不是用户说的话】\n" +
		"以下是长期记忆里已经记下的要点，直接当作已知背景（比如用户希望的称谓、偏好、重要的人和事）：\n" +
		digest + "\n" +
		"需要细节或原文时调用 recall 查整页；不要在回复里复述这一段，也不必声明「我看了记忆」。"
}

// SetProjectDir 注入当前工作目录（启动时一次；未来的"工作模式"切目录时再调一次）。
// 传空串等于关掉项目上下文注入。
func (b *Brain) SetProjectDir(dir string) {
	b.projMu.Lock()
	b.projectDir = strings.TrimSpace(dir)
	b.projMu.Unlock()
}

// projectWarmup 生成会话开始时注入的项目上下文（工作目录下的 AGENTS.md 全文 +
// README.md 摘要）。没有这两个文件时返回空串，不注入。
func (b *Brain) projectWarmup() string {
	b.projMu.RLock()
	dir := b.projectDir
	b.projMu.RUnlock()
	return projectContext(dir)
}

// SetSkills 注入技能仓库与自动匹配配置（启动时调用一次）。技能是按次生效的，
// 后台增删改只需 Store.Reload，不需要重建 Agent。
func (b *Brain) SetSkills(store *skill.Store, autoTrigger bool) {
	b.skillMu.Lock()
	defer b.skillMu.Unlock()
	b.skills = store
	b.skillAuto = skill.MatchConfig{Enabled: autoTrigger}
}

// callCfg 汇总本次提问的可选项。
type callCfg struct {
	skillName string // 显式点名的技能
	inline    *vxagent.Skill
	noAuto    bool // 关闭自动匹配（心跳/智能任务等机器生成的提示词）
	jsonMode  bool // 要求模型只输出 JSON（计划模式规划轮用）
	maxIter   int  // 按次覆盖工具轮数上限（计划/目标用）
}

// CallOpt 修改本次提问的行为。
type CallOpt func(*callCfg)

// WithSkillName 指定本轮使用的技能（名字不存在时明确回复，不降级为自动匹配）。
func WithSkillName(name string) CallOpt {
	return func(c *callCfg) { c.skillName = strings.TrimSpace(name) }
}

// WithInlineSkill 直接传入一份内存技能（不进技能仓库）。
//
// 用途：计划模式的规划轮需要一份限定工具面的内置指令包——它是实现细节，
// 不该出现在用户可增删改的技能列表里。
func WithInlineSkill(sk *vxagent.Skill) CallOpt {
	return func(c *callCfg) { c.inline = sk }
}

// WithoutAutoSkill 关闭本轮的技能自动匹配。
func WithoutAutoSkill() CallOpt {
	return func(c *callCfg) { c.noAuto = true }
}

// WithJSONMode 要求本轮回答只输出合法 JSON（计划模式产步骤清单用）。
func WithJSONMode() CallOpt {
	return func(c *callCfg) { c.jsonMode = true }
}

// WithMaxIterations 覆盖本轮的工具调用轮数上限（n<=0 时忽略）。
func WithMaxIterations(n int) CallOpt {
	return func(c *callCfg) { c.maxIter = n }
}

// SkillSink 收集本轮实际启用的技能名。暂停-继续必须复用同一技能（技能限定了
// 工具面/模型），而自动匹配的结果只有 Brain 知道，因此用出参回传给调用方。
type SkillSink struct {
	mu   sync.Mutex
	name string
}

// Name 返回本轮启用的技能名（未启用为空）。
func (s *SkillSink) Name() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

func (s *SkillSink) set(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
}

type skillSinkKey struct{}

// WithSkillSink 把出参挂到 ctx 上，Ask/AskWith 会在选中技能时写入。
func WithSkillSink(ctx context.Context, sink *SkillSink) context.Context {
	return context.WithValue(ctx, skillSinkKey{}, sink)
}

func skillSinkFrom(ctx context.Context) *SkillSink {
	s, _ := ctx.Value(skillSinkKey{}).(*SkillSink)
	return s
}

// Ask 提问并等待完整回答（等价于 AskWith 不带选项）。
func (b *Brain) Ask(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta)) (string, error) {
	return b.AskWith(ctx, text, imgs, onDelta)
}

// AskWith 提问并等待完整回答，可带本轮行为选项。会话由 ctx 中的会话 key 决定
// （WithSessionKey，未设置时用网页会话）；onDelta 为本次请求的流式增量回调
// （可为 nil）；imgs 为随消息发送的图片内容（可为 nil），会作为一条独立的用户
// 消息紧邻问题文本之前注入会话。
func (b *Brain) AskWith(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta), opts ...CallOpt) (answer string, err error) {
	key := CurrentKey(SessionKeyFrom(ctx))
	keyMu := b.pool.Lock(key)
	keyMu.Lock()
	defer keyMu.Unlock()

	cfg := callCfg{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	// 技能选择：自定义指令命中优先于自动匹配；显式点名不成立时直接回绝。
	question, askOpts, shortCircuit, err := b.resolveSkill(ctx, text, cfg)
	if err != nil {
		return "", err
	}
	if shortCircuit != "" {
		return shortCircuit, nil
	}

	b.rw.RLock()
	defer b.rw.RUnlock()
	ag := b.agent
	if onDelta != nil {
		ag = ag.WithDelta(onDelta) // 流式增量只发给本次请求的订阅者
	}

	sess := b.pool.Get(ctx, key)
	// 新会话或刚清过上下文（历史为空）：先把长期记忆的速览垫进去。写在会话里而
	// 不是每轮拼进提问，避免每轮重复计费，也不会污染用户看到的问题文本。
	if len(sess.Messages()) == 0 {
		// 项目约定先垫（它框定"在哪个目录、按什么规矩干活"），再垫个人记忆速览。
		if pc := b.projectWarmup(); pc != "" {
			sess.Add(llm.NewTextMessage(llm.RoleSystem, pc))
		}
		if warm := b.memoryWarmup(); warm != "" {
			sess.Add(llm.NewTextMessage(llm.RoleSystem, warm))
		}
	}
	defer func() {
		if saveErr := b.pool.Save(context.WithoutCancel(ctx), sess); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("保存对话失败: %w", saveErr))
		}
	}()

	if len(imgs) == 0 {
		return ag.Ask(ctx, sess, question, askOpts...)
	}
	// 多模态：先注入仅含图片的用户消息，再走文本 Ask。
	// 框架的 Ask 只收字符串，且它失败时会回滚到自己的起点（不含图片消息），
	// 因此这里自行管理回滚；成功后把图片数据替换为占位文本，
	// 避免大体积 base64 常驻会话（每轮重复上传、内存膨胀）。
	preLen := len(sess.Messages())
	sess.Add(llm.Message{Role: llm.RoleUser, Content: imgs})
	answer, err = ag.Ask(ctx, sess, question, askOpts...)
	if err != nil {
		sess.Rollback(preLen)
		return answer, err
	}
	b.sanitizeImages(sess, preLen, len(imgs))
	return answer, nil
}

// resolveSkill 决定本轮用哪个技能，返回改写后的提问、传给框架的 AskOption，
// 以及（点名了一个不存在的技能时）直接回给用户的话。
//
// 优先级：显式点名 > 自定义指令（命中则跳过自动匹配）> 自动匹配（opt-in 技能）。
// 每次决策都写轨迹与"思考过程"事件，让"为什么套了这段提示词"可查。
func (b *Brain) resolveSkill(ctx context.Context, text string, cfg callCfg) (question string, askOpts []vxagent.AskOption, shortCircuit string, err error) {
	question = text
	if cfg.jsonMode {
		askOpts = append(askOpts, vxagent.WithJSONMode())
	}
	if cfg.maxIter > 0 {
		askOpts = append(askOpts, vxagent.WithMaxIterations(cfg.maxIter))
	}
	if cfg.inline != nil {
		// 内置技能（规划轮等）：直接生效，不参与仓库查找，也不写进 SkillSink
		// ——它不是用户可选的技能，暂停现场没必要复用一份内部提示词。
		askOpts = append(askOpts, vxagent.WithSkill(cfg.inline))
		trace.Record(ctx, "skill", "启用内置技能 %s（系统内部）", cfg.inline.Metadata.Name)
		return question, askOpts, "", nil
	}

	b.skillMu.RLock()
	store, autoCfg := b.skills, b.skillAuto
	b.skillMu.RUnlock()

	// 1) 显式点名：/skill 名 正文 或 $名 正文；Brain 级 WithSkillName 优先于此语法。
	name := cfg.skillName
	invoked := false
	if name == "" {
		if parsed, rest, ok := skill.ParseInvocation(text); ok {
			name, question, invoked = parsed, rest, true
			if question == "" {
				question = text // 只打了 "/skill 名"：把原句交给技能，让它按清单办事
			}
		}
	}
	if name != "" && (invoked || cfg.skillName != "") {
		e := b.lookupSkill(store, name)
		if e == nil {
			note := "没有叫「" + name + "」的可用技能（名字可在「设置 → 技能」里核对）。"
			if invoked {
				question = text
			}
			trace.Record(ctx, "skill", "显式点名的技能 %q 不存在或已禁用，未调用模型", name)
			PublishStep(ctx, Step{Kind: "skill", Name: name, Status: "fail", Detail: "技能不存在"})
			return question, askOpts, note, nil
		}
		askOpts = append(askOpts, vxagent.WithSkill(skillForTurn(e)))
		skillSinkFrom(ctx).set(e.Name())
		why := append([]string{"explicit:" + name}, e.MatchedBy...)
		trace.Record(ctx, "skill", "启用技能 %s（依据：%s）", e.Name(), strings.Join(why, "、"))
		PublishStep(ctx, Step{Kind: "skill", Name: e.Name(), Status: "ok", Detail: strings.Join(why, "、")})
		return question, askOpts, "", nil
	}

	// 2) 自定义指令：命中即展开，并且不再做技能自动匹配。
	expanded, matchedCustom := b.expandCustom(question)
	question = expanded
	if matchedCustom {
		trace.Record(ctx, "skill", "命中自定义指令，跳过技能自动匹配")
		return question, askOpts, "", nil
	}

	// 3) 自动匹配（心跳等机器提示词走 WithoutAutoSkill 跳过）
	if cfg.noAuto || store == nil || !autoCfg.Enabled {
		return question, askOpts, "", nil
	}
	if e, why, ok := skill.Match(question, store.Candidates(), autoCfg); ok {
		askOpts = append(askOpts, vxagent.WithSkill(skillForTurn(e)))
		skillSinkFrom(ctx).set(e.Name())
		trace.Record(ctx, "skill", "自动启用技能 %s（依据：%s）", e.Name(), strings.Join(why, "、"))
		PublishStep(ctx, Step{Kind: "skill", Name: e.Name(), Status: "ok", Detail: strings.Join(why, "、")})
		return question, askOpts, "", nil
	}
	trace.Record(ctx, "skill", "未匹配到技能（可用技能 %d 个，自动匹配需 auto: true）", len(store.Candidates()))
	return question, askOpts, "", nil
}

// lookupSkill 按名字取可用技能（禁用/解析失败视为不存在）。
func (b *Brain) lookupSkill(store *skill.Store, name string) *skill.Entry {
	if store == nil {
		return nil
	}
	e, ok := store.FindName(name)
	if !ok || !e.Usable() {
		return nil
	}
	return e
}

// Resume 继续被暂停的对话。ctx 取消时框架会回滚本轮（用户消息与已生成的
// 部分回答都不在会话里），这里把它们重新注入会话历史，再以继续指令发问，
// 让模型接着上次中断的地方完成任务——继续产生的内容与原问题在同一会话上下文中。
// partial 为空表示被暂停时还没有任何输出（仅注入原问题）。
//
// 必须复用暂停前那一轮的技能：技能限定了工具面/模型/温度，继续时换一套
// 语义会静默漂移。opts 里的 WithSkillName 由调用方从暂停现场带上；
// 自动匹配在这里一律关闭（继续指令是机器文本，拿它猜技能结果不可预测）。
func (b *Brain) Resume(ctx context.Context, key, question, partial string, onDelta func(llm.Delta), opts ...CallOpt) (answer string, err error) {
	fullKey := CurrentKey(key)
	keyMu := b.pool.Lock(fullKey)
	keyMu.Lock()
	defer keyMu.Unlock()

	cfg := callCfg{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	// 只认显式指定的技能：继续指令是机器文本，既不展开自定义指令也不做自动匹配。
	var askOpts []vxagent.AskOption
	if cfg.skillName != "" {
		b.skillMu.RLock()
		store := b.skills
		b.skillMu.RUnlock()
		if e := b.lookupSkill(store, cfg.skillName); e != nil {
			askOpts = append(askOpts, vxagent.WithSkill(skillForTurn(e)))
			trace.Record(ctx, "skill", "继续被暂停的回答，沿用技能 %s", e.Name())
		} else if cfg.skillName != "" {
			trace.Record(ctx, "skill", "暂停现场记录的技能 %q 已不可用，继续时不再启用", cfg.skillName)
		}
	}
	if cfg.maxIter > 0 {
		askOpts = append(askOpts, vxagent.WithMaxIterations(cfg.maxIter))
	}

	b.rw.RLock()
	defer b.rw.RUnlock()
	ag := b.agent
	if onDelta != nil {
		ag = ag.WithDelta(onDelta)
	}

	sess := b.pool.Get(ctx, fullKey)
	defer func() {
		if saveErr := b.pool.Save(context.WithoutCancel(ctx), sess); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("保存对话失败: %w", saveErr))
		}
	}()

	preLen := len(sess.Messages())
	sess.Add(llm.NewTextMessage(llm.RoleUser, question))
	if partial != "" {
		sess.Add(llm.NewTextMessage(llm.RoleAssistant,
			partial+"\n\n（这个回答被用户暂停，尚未完成）"))
	}
	answer, err = ag.Ask(ctx, sess, "请接着上面被暂停的回答继续完成任务，把没做完的部分做完；已经生成过的内容不要重复输出。", askOpts...)
	if err != nil {
		sess.Rollback(preLen)
		return answer, err
	}
	return answer, nil
}

// sanitizeImages 把历史中某条消息的图片内容替换为占位文本。
func (b *Brain) sanitizeImages(sess *sessionstore.Session, idx, n int) {
	msgs := sess.Messages()
	if idx >= len(msgs) {
		return
	}
	var keep []llm.Content
	for _, c := range msgs[idx].Content {
		if c.Type != llm.ContentImage {
			keep = append(keep, c)
		}
	}
	keep = append(keep, llm.Content{Type: llm.ContentText,
		Text: fmt.Sprintf("[用户发送了 %d 张图片，内容已于本轮处理]", n)})
	msgs[idx].Content = keep
	sess.ResetTo(msgs)
}

// Reset 清除指定会话的上下文：丢弃内存与持久层历史，下次对话新开会话；
// 已归档的早期对话与引擎运行态一并清除——会话 ID 固定为 prefix+key，
// 归档不清的话新会话会带出旧摘要区，等于没清干净。
func (b *Brain) Reset(key string) error {
	fullKey := CurrentKey(key)
	mu := b.pool.Lock(fullKey)
	mu.Lock()
	defer mu.Unlock()
	b.rw.RLock()
	defer b.rw.RUnlock()
	if err := b.pool.Reset(context.Background(), fullKey); err != nil {
		return err
	}
	if b.ctx != nil {
		return b.ctx.ResetSession(context.Background(), sessionIDPrefix+fullKey)
	}
	return nil
}

// Reconfigure 等待在途请求落盘后构建并切换 Agent；失败保留原实例。
func (b *Brain) Reconfigure(build func() (*vxagent.Agent, error)) error {
	b.rw.Lock()
	defer b.rw.Unlock()
	ag, err := build()
	if err != nil {
		return err
	}
	b.agent = ag
	return nil
}

// SetAgent 原子替换内部 Agent（热更新模型/供应商时使用）。
// 等待所有会话中进行中的请求完成后生效。
func (b *Brain) SetAgent(ag *vxagent.Agent) {
	b.rw.Lock()
	defer b.rw.Unlock()
	b.agent = ag
}

// Pool 返回会话池（管理后台会话列表/清除、热更新重建 Agent 时使用）。
func (b *Brain) Pool() *SessionPool { return b.pool }
