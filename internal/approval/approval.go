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

// Item 是一条待审批操作（管理后台展示用）。
type Item struct {
	ID        int64     `json:"id"`
	Tool      string    `json:"tool"`    // 发起审批的工具名：exec / write_file / edit_file
	Summary   string    `json:"summary"` // 一句话摘要（如"执行命令 uname -a"）
	Detail    string    `json:"detail"`  // 详情（命令全文 / 文件内容预览）
	CreatedAt time.Time `json:"createdAt"`
}

// decision 是审批结果：是否批准，以及是否"批准并记住"（本进程内相同工具
// 与参数的后续调用不再询问，由权限层的 Remember 机制实现）。
type decision struct {
	allow    bool
	remember bool
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

// Timeout 返回审批等待时长。
func (q *Queue) Timeout() time.Duration { return q.timeout }

// Request 发起一次审批：confirm 模式入队并阻塞等待（批准返回 nil，
// 拒绝/超时返回 ErrDenied）；auto 模式直接放行（exec 的白名单匹配由权限层
// 完成，不经此入口）。off 模式返回 ErrDisabled。
func (q *Queue) Request(tool, summary, detail string) error {
	_, err := q.RequestRemember(tool, summary, detail)
	return err
}

// RequestRemember 发起一次审批并回传"是否记住"：confirm 模式入队阻塞等待，
// 批准返回 remember（用户选择了"批准并记住"），拒绝/超时返回 ErrDenied；
// auto 模式直接放行；off 模式返回 ErrDisabled。
func (q *Queue) RequestRemember(tool, summary, detail string) (remember bool, err error) {
	q.mu.Lock()
	if q.mode != ModeConfirm {
		q.mu.Unlock()
		if q.mode == ModeOff {
			return false, ErrDisabled
		}
		return false, nil // auto：直接放行
	}
	q.seq++
	req := &request{
		item: Item{ID: q.seq, Tool: tool, Summary: summary, Detail: detail, CreatedAt: time.Now()},
		ch:   make(chan decision, 1),
	}
	q.pending[req.item.ID] = req
	q.mu.Unlock()

	select {
	case d := <-req.ch:
		q.remove(req.item.ID)
		if !d.allow {
			return false, ErrDenied
		}
		return d.remember, nil
	case <-time.After(q.timeout):
		q.remove(req.item.ID)
		return false, ErrDenied
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
