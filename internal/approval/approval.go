// Package approval 实现危险操作的审批队列（管理后台网页批准）：
//   - exec 的策略判定（白名单/黑名单/模式）已迁移到 vortexagent 全局权限层，
//     由 internal/agent/permission.go 翻译成 Checker 规则；本队列退化为纯审批
//     通道：权限层未放行的调用经 ApprovalUI 桥接在此入队，批准后执行。
//   - confirm：操作进入待审批队列，管理后台弹审批卡片，超时（默认 5 分钟）
//     未处理自动拒绝，调用方收到"用户未批准"；批准时可附带"记住"——本进程内
//     相同工具与参数的后续调用不再询问（权限层 Remember 语义）。
//   - auto：Request 直接放行（exec 的白名单匹配已由权限层完成）；
//   - off：返回 ErrDisabled（exec_command 在 off 模式不注册；write_file/edit_file
//     不受 off 影响，仍按 confirm/auto 语义执行）。
package approval

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Mode 是审批模式。
type Mode string

const (
	ModeAuto    Mode = "auto"    // 白名单内直接执行，其余拒绝
	ModeConfirm Mode = "confirm" // 危险操作进审批队列，网页批准后执行
	ModeOff     Mode = "off"     // 禁用 exec
)

// DefaultTimeout 是审批等待的默认时长。
const DefaultTimeout = 5 * time.Minute

// ErrDisabled 表示 exec 已被禁用。
var ErrDisabled = errors.New("exec 工具已被禁用（tools.exec.mode=off）")

// KindQuestion 是问答卡的 Kind 值：agent 在问用户（选项由 agent 自己给），
// 不是请求授权。空 Kind 按授权卡处理，老调用方不受影响。
const KindQuestion = "question"

// 审批卡的来源入口：前端据此决定卡片就地展开还是收进右上角的「其他会话待处理」。
// SourceChat 是内置主对话（含心跳/任务等无会话归属的调用）；SourceACP 是工作区
// 里某条会话（外部 agent 或内置 Friday 都算）。
const (
	SourceChat = "chat"
	SourceACP  = "acp"
)

// Origin 描述一次审批属于哪个对话入口。Thread* 只在 SourceACP 里有值：
// 前端靠 threadId 判断"这是不是我正在看的会话"，并据此跳转。
type Origin struct {
	Source      string `json:"source,omitempty"`
	ThreadID    string `json:"threadId,omitempty"`
	ThreadTitle string `json:"threadTitle,omitempty"`
	Agent       string `json:"agent,omitempty"`
}

// OriginResolver 把一次审批问询的 ctx 还原成来源。内置 Friday 的工作区会话只有
// ctx 里带着会话 key（approval 包不认识 ACP），所以由装配方注入反查函数。
type OriginResolver func(context.Context) Origin

// ErrDenied 表示操作被用户拒绝或审批超时。
var ErrDenied = errors.New("用户未批准")

// denyPatterns 是毁灭性命令黑名单：翻译成权限层的 exec_command deny 规则后，
// 无论白名单命中还是用户批准都拦截（见 DenyPatterns）。
var denyPatterns = []string{
	"rm -rf", "rm -fr", "rm -r /", "rm -r ~",
	"mkfs", "dd if=", "> /dev/sd", "> /dev/nvme", "> /dev/mmc",
	":(){", "fork()", // fork 炸弹
	"shutdown", "reboot", "halt", "init 0", "init 6", "poweroff",
	"chmod -r 777 /", "chown -r ", "mv /* ", " history -c", "history -c",
}

// DenyPatterns 返回毁灭性命令黑名单的副本。权限层把它逐条映射为
// exec_command:<pattern> deny 规则（CommandDenyMatcher 词边界扫描，连藏在
// 命令替换里的片段也拦得住），任何模式下命中即拒。
func DenyPatterns() []string {
	out := make([]string, len(denyPatterns))
	copy(out, denyPatterns)
	return out
}

// Option 是问答卡上的一个可选项，来自 agent 自己给的 permission options。
// ID 是回传给 agent 的 optionId——协议只认它给过的那些，不能自己编。
type Option struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// QuestionOption / QuestionGroup 是问题正文的结构化形态：前端按它排版，
// 不再把 rawInput 的 JSON 糊在卡上。
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type QuestionGroup struct {
	Header      string           `json:"header,omitempty"`
	Question    string           `json:"question,omitempty"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
}

// MaxAnswerText 是问答卡自由文本答案的上限（按 rune 计）。它是用户输入，
// 会原样交给 agent，限长既防误粘贴整篇文档，也避免卡死前端。
const MaxAnswerText = 4000

// Item 是一条待审批操作（管理后台展示用）。
type Item struct {
	ID        int64     `json:"id"`
	Tool      string    `json:"tool"`    // 发起审批的工具名：exec / write_file / edit_file
	Summary   string    `json:"summary"` // 一句话摘要（如"执行命令 uname -a"）
	Detail    string    `json:"detail"`  // 详情（命令全文 / 文件内容预览）
	CreatedAt time.Time `json:"createdAt"`
	// Kind 区分两种卡：空值按授权（批准/拒绝），"question" 是 agent 在问用户，
	// 按钮来自 Options，点哪条就把那条的 optionId 回给 agent。
	Kind string `json:"kind,omitempty"`
	// Options 与 Questions 只在问答卡上有值：前者是可回传的选项，后者是排版
	// 用的问题正文（可能比选项更细，例如带每项说明）。
	Options   []Option        `json:"options,omitempty"`
	Questions []QuestionGroup `json:"questions,omitempty"`
	// AllowText 表示这张问答卡除选项外还接受自由文本（CodeBuddy 的
	// AskUserQuestion 与 MiniMax 的 Other 字段都允许用户自己写）。
	AllowText       bool   `json:"allowText,omitempty"`
	TextPlaceholder string `json:"textPlaceholder,omitempty"`
	// MultiSelect 表示这张卡的选项可以勾选多条、一次提交。只有交互通道
	// （elicitation / 扩展方法）能真正收多条答案；权限通道协议一次只认一个
	// optionId（zcode 把多选题拆成逐条 yes/no），所以那边永远单选。
	MultiSelect bool `json:"multiSelect,omitempty"`
	// Origin 标出这张卡属于哪个对话入口（聊天 / 某条工作区会话）。前端据此把
	// 卡片放回原来的页面：不是当前会话的卡收进右上角提示，点击跳过去再处理。
	Origin
}

// decision 是审批结果：是否批准，以及是否"批准并记住"（本进程内相同工具
// 与参数的后续调用不再询问，由权限层的 Remember 机制实现）。
type decision struct {
	allow     bool
	remember  bool
	optionID  string   // 问答卡：用户选中的 optionId（授权卡恒为空）
	optionIDs []string // 多选问答卡：用户勾选的全部 optionId（与 optionID 二选一）
	text      string   // 问答卡：用户自己输入的答案（与 optionID 二选一）
}

// request 是待审批请求的内部形态，携带结果回传通道。
type request struct {
	item Item
	ch   chan decision
}

// Queue 是审批队列：确认模式下工具调用方在此阻塞等待网页批准。
type Queue struct {
	mu        sync.Mutex
	mode      Mode
	allowlist []string
	timeout   time.Duration
	seq       int64
	pending   map[int64]*request
	// editsFree 编辑类内部审批（write_file/edit_file/mcp_edit 变更）免审批开关：
	// permissions.mode 为 acceptEdits/fullAccess 时开启（运行时经设置页热切换）。
	editsFree bool
	// origin 把审批问询的 ctx 还原成来源（工作区会话 / 主对话）。为 nil 时一律
	// 按主对话处理——只装配了队列、没接工作区反查的运行形态（测试）仍然可用。
	origin OriginResolver
}

// New 创建审批队列。mode 非法时回退为 confirm（最保守）；allowlist 为 exec
// 白名单（auto/confirm 模式下的免审批命令）；timeout<=0 用默认 5 分钟。
func New(mode string, allowlist []string, timeout time.Duration) *Queue {
	m := Mode(strings.TrimSpace(mode))
	switch m {
	case ModeAuto, ModeConfirm, ModeOff:
	default:
		m = ModeConfirm
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Queue{
		mode:      m,
		allowlist: allowlist,
		timeout:   timeout,
		pending:   make(map[int64]*request),
	}
}

// Mode 返回当前审批模式。
func (q *Queue) Mode() Mode { return q.mode }

// SetOriginResolver 注入来源反查（启动时调用一次）。入队的每张卡都会带上
// 解析出的来源，前端据此决定卡片就地展开还是挂到右上角提示。
func (q *Queue) SetOriginResolver(r OriginResolver) {
	q.mu.Lock()
	q.origin = r
	q.mu.Unlock()
}

// originFrom 解析 ctx 的来源；没装配解析器时按主对话处理。
func (q *Queue) originFrom(ctx context.Context) Origin {
	q.mu.Lock()
	r := q.origin
	q.mu.Unlock()
	if r == nil {
		return Origin{Source: SourceChat}
	}
	o := r(ctx)
	if o.Source == "" {
		o.Source = SourceChat
	}
	return o
}

// SetEditsFree 设置编辑类内部审批免审批开关（permissions.mode 切换时由热更新
// 路径调用；只影响 write_file/edit_file/mcp_edit 的内部审批，exec 与权限层无关）。
func (q *Queue) SetEditsFree(free bool) {
	q.mu.Lock()
	q.editsFree = free
	q.mu.Unlock()
}

// EditsFree 返回编辑类内部审批是否免审批。
func (q *Queue) EditsFree() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.editsFree
}

// InternalApprovalRequired 返回编辑类工具（write_file/edit_file/mcp_edit 变更）
// 当前是否需要走内部网页审批：仅 confirm 模式且未开启编辑免审批时需要。
// auto/off 模式沿用历史行为（直接放行）。
func (q *Queue) InternalApprovalRequired() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.mode == ModeConfirm && !q.editsFree
}

// Timeout 返回审批等待时长。
func (q *Queue) Timeout() time.Duration { return q.timeout }

// Request 发起一次审批（无 ctx 版本，来源按主对话处理）。需要把卡片归还到
// 对应工作区会话的调用方用 RequestCtx。confirm 模式入队并阻塞等待（批准返回
// nil，拒绝/超时返回 ErrDenied）；auto 模式直接放行；off 模式返回 ErrDisabled。
func (q *Queue) Request(tool, summary, detail string) error {
	_, err := q.RequestRemember(tool, summary, detail)
	return err
}

// RequestCtx 同 Request，但带调用方 ctx：来源解析器据此判断这张卡属于哪个
// 对话入口（工作区会话 / 主对话）。
func (q *Queue) RequestCtx(ctx context.Context, tool, summary, detail string) error {
	_, err := q.RequestRememberCtx(ctx, tool, summary, detail)
	return err
}

// RequestRemember 发起一次审批并回传"是否记住"：confirm 模式入队阻塞等待，
// 批准返回 remember（用户选择了"批准并记住"），拒绝/超时返回 ErrDenied；
// auto 模式直接放行；off 模式返回 ErrDisabled。
func (q *Queue) RequestRemember(tool, summary, detail string) (remember bool, err error) {
	return q.RequestRememberCtx(context.Background(), tool, summary, detail)
}

// RequestRememberCtx 是 RequestRemember 的带 ctx 版本。
func (q *Queue) RequestRememberCtx(ctx context.Context, tool, summary, detail string) (remember bool, err error) {
	_, remember, err = q.request(ctx, tool, summary, detail, 0, true)
	return remember, err
}

// RequestAlways 无视队列模式强制入队一次审批，使用自定义超时（<=0 用默认）。
// 智能体权限请求专用：无论全局权限模式如何，子 agent 的敏感操作一律人工把关
// ——这是对话空间的独立闸门，不随 exec 审批模式联动；超时也远长于 exec
// 审批（人可能不在屏幕前）。批准返回 true；拒绝/超时返回 ErrDenied。
func (q *Queue) RequestAlways(tool, summary, detail string, timeout time.Duration) (bool, error) {
	allow, _, err := q.request(context.Background(), tool, summary, detail, timeout, false)
	return allow, err
}

// Pending 是一次进行中的审批句柄：入队立即返回，不阻塞调用方。Wait 阻塞等
// 结果；Cancel 供业务侧放弃等待（典型场景：智能体 turn 已结束/超时/被停止，
// 卡面向的会话已不存在，继续等用户点卡没有意义）——Cancel 把卡从待审批列表
// 移除并按拒绝结算，迟到的用户决定落空。Wait 只可调用一次。
type Pending struct {
	q       *Queue
	req     *request
	timeout time.Duration
	cancel  chan struct{}
	once    sync.Once
	done    chan struct{}
	doneOne sync.Once
}

// Card 是一次入队的完整形态。授权卡只填 Tool/Summary/Detail；问答卡额外给
// Kind="question" + Options（按钮，回传用它们的 ID）+ Questions（正文排版）。
type Card struct {
	Tool      string
	Summary   string
	Detail    string
	Kind      string
	Options   []Option
	Questions []QuestionGroup
	// AllowText/TextPlaceholder 用于问答卡的自由文本输入。
	AllowText       bool
	TextPlaceholder string
	// MultiSelect 允许一次勾选多条选项提交（见 Item.MultiSelect）。
	MultiSelect bool
	// Origin 是这张卡的来源；空 Source 按主对话处理。
	Origin Origin
}

// RequestCancelable 入队一次审批并立即返回句柄（不阻塞）。timeout<=0 用队列
// 默认。与 RequestAlways 同一语义（无视模式强制入队），只是把「等结果」拆成
// 了独立步骤，让调用方能在等待期间按业务事件撤卡。
func (q *Queue) RequestCancelable(tool, summary, detail string, timeout time.Duration) *Pending {
	return q.RequestCard(Card{Tool: tool, Summary: summary, Detail: detail}, timeout)
}

// RequestCard 入队一张授权卡或问答卡，语义同 RequestCancelable（无视队列模式
// 强制入队——智能体的权限闸门是对话空间的独立闸门）。card.Origin 会原样带到
// 卡片上，前端据此把卡片放回对应会话。
func (q *Queue) RequestCard(card Card, timeout time.Duration) *Pending {
	origin := card.Origin
	if origin.Source == "" {
		origin.Source = SourceChat
	}
	q.mu.Lock()
	if timeout <= 0 {
		timeout = q.timeout
	}
	q.seq++
	req := &request{
		item: Item{
			ID: q.seq, Tool: card.Tool, Summary: card.Summary, Detail: card.Detail,
			CreatedAt: time.Now(), Kind: card.Kind,
			Options: card.Options, Questions: card.Questions,
			AllowText: card.AllowText, TextPlaceholder: card.TextPlaceholder,
			MultiSelect: card.MultiSelect, Origin: origin,
		},
		ch: make(chan decision, 1),
	}
	q.pending[req.item.ID] = req
	q.mu.Unlock()
	return &Pending{
		q:       q,
		req:     req,
		timeout: timeout,
		cancel:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Done 返回结算信号：Wait 返回前关闭（无论批准、拒绝、超时还是 Cancel）。
func (p *Pending) Done() <-chan struct{} { return p.done }

// ID 是这张卡的审批编号（队列内单调递增）。业务侧拿它给系统通知做幂等键：
// 同一张卡只该提示一次，而同一轮里的两张卡是两个编号、各提示一次。
func (p *Pending) ID() int64 { return p.req.item.ID }

// Outcome 是一次审批的完整结算：批准与否、是否"批准并记住"、问答卡选中的
// optionId（多选卡是 OptionIDs），以及用户自己输入的文本答案（与选项二选一）。
type Outcome struct {
	Allow     bool
	Remember  bool
	OptionID  string
	OptionIDs []string
	Text      string
}

// Wait 阻塞等待审批结果：批准返回 (true, nil)；拒绝、超时或被 Cancel 返回
// (false, ErrDenied)。问答卡要拿选中项请用 WaitDecision。
func (p *Pending) Wait() (bool, error) {
	out, err := p.WaitDecision()
	return out.Allow, err
}

// WaitDecision 阻塞等完整结算。拒绝/超时/撤卡返回 ErrDenied（Outcome 里的
// OptionID 仍带回用户选的那条，供调用方区分"选了拒绝选项"与"没答"）。
func (p *Pending) WaitDecision() (Outcome, error) {
	defer p.doneOne.Do(func() { close(p.done) })
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case d := <-p.req.ch:
		p.q.remove(p.req.item.ID)
		if !d.allow {
			return Outcome{OptionID: d.optionID, OptionIDs: d.optionIDs, Text: d.text}, ErrDenied
		}
		return Outcome{Allow: true, Remember: d.remember,
			OptionID: d.optionID, OptionIDs: d.optionIDs, Text: d.text}, nil
	case <-p.cancel:
		p.q.remove(p.req.item.ID)
		return Outcome{}, ErrDenied
	case <-timer.C:
		p.q.remove(p.req.item.ID)
		return Outcome{}, ErrDenied
	}
}

// Cancel 业务侧放弃等待：幂等；已结算（含已超时）的审批无操作。
func (p *Pending) Cancel() { p.once.Do(func() { close(p.cancel) }) }

// request 是审批入队的内部实现：respectMode=false 时无视模式强制入队。
// 返回 (是否批准, 是否"批准并记住", 错误)。ctx 只用于解析卡片来源。
func (q *Queue) request(ctx context.Context, tool, summary, detail string, timeout time.Duration, respectMode bool) (allow bool, remember bool, err error) {
	origin := q.originFrom(ctx)
	q.mu.Lock()
	if respectMode && q.mode != ModeConfirm {
		q.mu.Unlock()
		if q.mode == ModeOff {
			return false, false, ErrDisabled
		}
		return true, false, nil // auto：直接放行
	}
	if timeout <= 0 {
		timeout = q.timeout
	}
	q.seq++
	req := &request{
		item: Item{ID: q.seq, Tool: tool, Summary: summary, Detail: detail,
			CreatedAt: time.Now(), Origin: origin},
		ch: make(chan decision, 1),
	}
	q.pending[req.item.ID] = req
	q.mu.Unlock()

	select {
	case d := <-req.ch:
		q.remove(req.item.ID)
		if !d.allow {
			return false, false, ErrDenied
		}
		return true, d.remember, nil
	case <-time.After(timeout):
		q.remove(req.item.ID)
		return false, false, ErrDenied
	}
}

// remove 从待审批表中移除请求（结果已送达或已超时）。
func (q *Queue) remove(id int64) {
	q.mu.Lock()
	delete(q.pending, id)
	q.mu.Unlock()
}

// Pending 返回当前待审批列表（按提交顺序）。
func (q *Queue) Pending() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Item, 0, len(q.pending))
	for _, r := range q.pending {
		out = append(out, r.item)
	}
	for i := 1; i < len(out); i++ { // seq 递增，按 ID 排序即提交顺序
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Decide 审批一条待处理操作：approve=true 批准放行（不记住）。
// 返回是否确实存在该请求。
func (q *Queue) Decide(id int64, approve bool) bool {
	return q.DecideWith(id, approve, false)
}

// DecideWith 审批一条待处理操作：approve=true 批准放行；remember=true 表示
// "批准并记住"——本进程内相同工具与参数的后续调用免审批（权限层 Remember）。
// 返回是否确实存在该请求。
func (q *Queue) DecideWith(id int64, approve, remember bool) bool {
	q.mu.Lock()
	req, ok := q.pending[id]
	q.mu.Unlock()
	if !ok {
		return false
	}
	req.ch <- decision{allow: approve, remember: remember}
	return true
}

// DecideOption 回答一张问答卡：optionID 必须是这张卡给过的选项，否则返回 false
// ——把乱填的 id 回传给 agent 只会换来一句"选项不存在"。授权卡（Kind 非
// question）同样返回 false：它要的是批准/拒绝，不是选项。
func (q *Queue) DecideOption(id int64, optionID string) bool {
	q.mu.Lock()
	req, ok := q.pending[id]
	valid := false
	if ok && req.item.Kind == KindQuestion {
		for _, o := range req.item.Options {
			if o.ID == optionID {
				valid = true
				break
			}
		}
	}
	q.mu.Unlock()
	if !valid {
		return false
	}
	req.ch <- decision{allow: true, optionID: optionID}
	return true
}

// DecideText 用自由文本回答一张问答卡：只有 Kind=question 且 AllowText=true
// 的卡接受，空文本与超长文本一律拒绝。文本原样交给 agent，不做任何解释。
func (q *Queue) DecideText(id int64, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > MaxAnswerText {
		return false
	}
	q.mu.Lock()
	req, ok := q.pending[id]
	valid := ok && req.item.Kind == KindQuestion && req.item.AllowText
	q.mu.Unlock()
	if !valid {
		return false
	}
	req.ch <- decision{allow: true, text: text}
	return true
}

// DecideOptions 用一组勾选的选项回答多选问答卡：只有 Kind=question 且
// MultiSelect=true 的卡接受；id 必须都是这张卡给过的，重复项按顺序去重，
// 全是空白视为没选。返回是否入账。
func (q *Queue) DecideOptions(id int64, optionIDs []string) bool {
	seen := make(map[string]bool, len(optionIDs))
	picked := make([]string, 0, len(optionIDs))
	for _, raw := range optionIDs {
		oid := strings.TrimSpace(raw)
		if oid == "" || seen[oid] {
			continue
		}
		seen[oid] = true
		picked = append(picked, oid)
	}
	if len(picked) == 0 {
		return false
	}
	q.mu.Lock()
	req, ok := q.pending[id]
	valid := false
	if ok && req.item.Kind == KindQuestion && req.item.MultiSelect {
		valid = true
		for _, oid := range picked {
			found := false
			for _, o := range req.item.Options {
				if o.ID == oid {
					found = true
					break
				}
			}
			if !found {
				valid = false
				break
			}
		}
	}
	q.mu.Unlock()
	if !valid {
		return false
	}
	req.ch <- decision{allow: true, optionIDs: picked}
	return true
}
