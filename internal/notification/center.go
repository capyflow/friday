// Package notification 保存任务结果与投递状态，支持重启恢复、有限重试和通道去重。
package notification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/friday/internal/task"
)

type Target interface {
	Name() string
	Notify(context.Context, string) error
}

// Resolver 只生成内容及是否推送；投递重试不会再次调用已成功的模型/工具。
type Resolver func(context.Context, task.Task) (string, bool, error)

type Notice struct {
	ID              string     `json:"id"`
	TaskID          string     `json:"taskId"`
	Text            string     `json:"text"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	Attempts        int        `json:"attempts"`
	LastError       string     `json:"lastError,omitempty"`
	DeliveredTo     []string   `json:"deliveredTo"`
	Task            task.Task  `json:"task"`
	Prepared        bool       `json:"prepared"`
	Push            bool       `json:"push"`
	NextAttempt     time.Time  `json:"nextAttempt,omitempty"`
	ReadAt          *time.Time `json:"readAt,omitempty"`
	ParentID        string     `json:"parentId,omitempty"`
	SnoozeTaskID    string     `json:"snoozeTaskId,omitempty"`
	SnoozeAt        time.Time  `json:"snoozeAt,omitempty"`
	SnoozeCommitted bool       `json:"snoozeCommitted,omitempty"`
}

type Center struct {
	mu              sync.Mutex
	processMu       sync.Mutex
	path            string
	notices         map[string]Notice
	resolve         Resolver
	targets         []Target
	start           sync.Once
	wake            chan struct{}
	done            chan struct{}
	started         bool
	webEnabled      bool
	desktopDispatch func(Notice) error
	deliveryGuard   func(task.Task) (bool, error)
	noticeHook      func(Notice) // 新通知落库/状态变化时回调（移动端长链接推送用）
}

func New(path string, resolve Resolver, targets []Target) (*Center, error) {
	c := &Center{path: path, notices: map[string]Notice{}, resolve: resolve, targets: slices.Clone(targets), wake: make(chan struct{}, 1), done: make(chan struct{}), webEnabled: true}
	if path == "" {
		return c, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("通知记录无法读取，原路径已保留: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("读取通知记录失败: %w", err)
	}
	var values []Notice
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("通知记录损坏，原文件已保留: %w", err)
	}
	if values == nil {
		return nil, fmt.Errorf("通知记录必须是数组，原文件已保留")
	}
	for _, n := range values {
		if n.ID == "" {
			return nil, fmt.Errorf("通知记录缺少 ID，原文件已保留")
		}
		if _, exists := c.notices[n.ID]; exists {
			return nil, fmt.Errorf("通知记录 ID 重复，原文件已保留")
		}
		switch n.Status {
		case "processing":
			n.Status = "pending"
		case "pending", "delivered", "failed", "silent":
		default:
			return nil, fmt.Errorf("通知 %s 状态无效，原文件已保留", n.ID)
		}
		c.notices[n.ID] = n
	}
	return c, nil
}

// Enqueue 使用计划触发时刻作为去重键；入队成功后调度器才能推进任务次数。
func (c *Center) Enqueue(t task.Task) error {
	when := t.Next
	if when.IsZero() {
		when = t.When
	}
	if t.ID == "" || when.IsZero() {
		return fmt.Errorf("任务入队缺少 ID 或计划触发时间")
	}
	sum := sha256.Sum256([]byte(t.ID + ":" + when.UTC().Format(time.RFC3339Nano)))
	id := hex.EncodeToString(sum[:16])
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.notices[id]; exists {
		return nil
	}
	now := time.Now()
	n := Notice{ID: id, TaskID: t.ID, Task: t, Text: t.Text, Status: "pending", CreatedAt: now, UpdatedAt: now, DeliveredTo: []string{}, ParentID: t.NotificationParentID}
	if err := c.putLocked(n); err != nil {
		return err
	}
	c.signal()
	return nil
}

func (c *Center) List() []Notice {
	return c.ListForTask("")
}

func (c *Center) ListForTask(taskID string) []Notice {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.valuesLocked()
	if taskID != "" {
		filtered := make([]Notice, 0, len(out))
		for _, n := range out {
			if n.TaskID == taskID {
				filtered = append(filtered, n)
			}
		}
		out = filtered
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

func (c *Center) Unread() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, notice := range c.notices {
		if notice.ReadAt == nil && notice.Prepared && notice.Push {
			n++
		}
	}
	return n
}

func (c *Center) MarkRead(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.notices[id]
	if !ok {
		return fmt.Errorf("通知不存在")
	}
	if n.ReadAt != nil {
		return nil
	}
	now := time.Now()
	n.ReadAt = &now
	return c.putLocked(n)
}

// MarkAllRead 把当前所有"可见未读"通知（已准备结果且需要推送的）标为已读，
// 返回本次改动的条数。一次落盘、失败回滚：与 MarkRead 一样，保存失败不能把
// 内存状态改成"已读"而磁盘还是未读。
func (c *Center) MarkAllRead() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	type prior struct {
		id string
		n  Notice
	}
	changed := make([]prior, 0)
	for id, n := range c.notices {
		if n.ReadAt != nil || !n.Prepared || !n.Push {
			continue
		}
		changed = append(changed, prior{id: id, n: n})
		at := now
		n.ReadAt = &at
		n.UpdatedAt = now
		c.notices[id] = n
	}
	if len(changed) == 0 {
		return 0, nil
	}
	if err := c.saveLocked(); err != nil {
		for _, p := range changed {
			c.notices[p.id] = p.n
		}
		return 0, err
	}
	return len(changed), nil
}

// Snooze 先持久化固定任务 ID/时间，再通过幂等 Schedule 接收；任务文件失败时
// 保留待恢复计划，RetrySnoozes 在重启前恢复，避免创建多个延后提醒。
func (c *Center) Snooze(id string, delay time.Duration, eng *task.Engine) (task.Task, error) {
	if delay < time.Minute || delay > 7*24*time.Hour {
		return task.Task{}, fmt.Errorf("稍后提醒须在1分钟至7天之间")
	}
	c.processMu.Lock()
	defer c.processMu.Unlock()
	c.mu.Lock()
	n, ok := c.notices[id]
	if !ok || !n.Prepared || !n.Push {
		c.mu.Unlock()
		return task.Task{}, fmt.Errorf("只有可见提醒可以延后")
	}
	if n.Task.TodoID != "" {
		c.mu.Unlock()
		return task.Task{}, fmt.Errorf("关联待办提醒请在待办页调整提醒时间，完成待办后会取消未来提醒")
	}
	if n.SnoozeTaskID == "" {
		n.SnoozeTaskID = "snooze-" + n.ID
		n.SnoozeAt = time.Now().Add(delay)
		if err := c.putLocked(n); err != nil {
			c.mu.Unlock()
			return task.Task{}, err
		}
	}
	c.mu.Unlock()
	return c.scheduleSnooze(n, eng)
}

func (c *Center) scheduleSnooze(n Notice, eng *task.Engine) (task.Task, error) {
	t := task.Task{ID: n.SnoozeTaskID, Text: n.Text, Kind: task.KindOnce, When: n.SnoozeAt, Next: n.SnoozeAt, Channel: n.Task.Channel, NotificationParentID: n.ID}
	// 已触发/取消的延后任务不能由开机恢复再次创建。
	if n.SnoozeCommitted {
		if existing, ok := eng.Get(t.ID); ok {
			return existing, nil
		}
		return t, nil
	}
	existing, exists := eng.Get(t.ID)
	if exists {
		t = existing
	} else {
		var err error
		t, err = eng.Schedule(t)
		if err != nil {
			return task.Task{}, fmt.Errorf("延后计划已保存，但任务同步失败；修复存储后重试: %w", err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.notices[n.ID]
	current.SnoozeCommitted = true
	now := time.Now()
	current.ReadAt = &now
	if err := c.putLocked(current); err != nil {
		return task.Task{}, fmt.Errorf("延后任务已创建，但确认保存失败；重试会复用同一任务: %w", err)
	}
	return t, nil
}

func (c *Center) RestoreSnoozes(eng *task.Engine) error {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	c.mu.Lock()
	values := c.valuesLocked()
	c.mu.Unlock()
	for _, n := range values {
		if n.SnoozeTaskID != "" && !n.SnoozeCommitted {
			if _, err := c.scheduleSnooze(n, eng); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetWebEnabled 只有管理后台实际启用时，通知中心才是可见的投递位置。
func (c *Center) SetWebEnabled(enabled bool) { c.mu.Lock(); c.webEnabled = enabled; c.mu.Unlock() }

// PostManual 记录一条不由任务触发的手动通知（如成本超预算告警）。
//
// 它不复用 Enqueue 的"任务 ID + 触发时刻"去重键，也不进投递队列——工作循环
// 只取 pending/processing，这里直接落终态，因此不会被重试逻辑反复搬运。
// delivered 是已经推过的通道名；为空时状态记为 silent（只留档、未打扰）。
// 与 PostOnce 的分工：手动告警每次都是新事件（键里带时刻，永不重号），且沿历史
// 行为只进通知中心、不走桌面提示桥；需要"人不在页面上也得弹一下"的走 PostOnce。
func (c *Center) PostManual(text string, delivered []string) (Notice, error) {
	return c.postManual("manual:"+text+":"+time.Now().Format(time.RFC3339Nano),
		text, delivered, false)
}

// PostOnce 记录一条有身份的手动通知：key 就是它的身份（如
// acp:<会话>:<轮>:done），同一个 key 只落一条、只提示一次——重复触发不该再
// 打扰用户一遍，也不该在通知中心堆出两条一样的。
//
// 它同时走桌面提示桥（装配了才发）：调用方是智能体这类"用户在别的窗口干活，
// 这一下得把他叫回来"的场景。发完不标失败重试：桥失败只说明系统通知没开或
// 外壳拒绝，内容仍在通知中心。
func (c *Center) PostOnce(key, text string, delivered []string) (Notice, error) {
	if strings.TrimSpace(key) == "" {
		return Notice{}, fmt.Errorf("通知标识为空")
	}
	return c.postManual("once:"+key, text, delivered, true)
}

// postManual 是两条手动通知的共同落档路径。pushDesktop 决定是否走桌面提示桥；
// 已存在同键通知就直接返回既有记录，不再提示第二次。
func (c *Center) postManual(key, text string, delivered []string, pushDesktop bool) (Notice, error) {
	if strings.TrimSpace(text) == "" {
		return Notice{}, fmt.Errorf("通知正文为空")
	}
	now := time.Now()
	sum := sha256.Sum256([]byte(key))
	status := "silent"
	if len(delivered) > 0 {
		status = "delivered"
	}
	n := Notice{
		ID:          hex.EncodeToString(sum[:16]),
		Text:        strings.TrimSpace(text),
		Status:      status,
		CreatedAt:   now,
		UpdatedAt:   now,
		DeliveredTo: slices.Clone(delivered),
		Prepared:    true,
		Push:        true,
	}
	c.mu.Lock()
	if pushDesktop {
		if existing, ok := c.notices[n.ID]; ok {
			c.mu.Unlock()
			return existing, nil
		}
	}
	if err := c.putLocked(n); err != nil {
		c.mu.Unlock()
		return Notice{}, err
	}
	desktopDispatch := c.desktopDispatch
	c.mu.Unlock()
	if pushDesktop && desktopDispatch != nil {
		if err := desktopDispatch(n); err != nil {
			log.Printf("[通知] 桌面提示转交失败，内容保留在通知中心: %v", err)
		}
	}
	return n, nil
}

// SetDesktopDispatch 接入桌面系统提示桥；真实内容始终保存在通知中心。
func (c *Center) SetDesktopDispatch(dispatch func(Notice) error) {
	c.mu.Lock()
	c.desktopDispatch = dispatch
	c.mu.Unlock()
}

// SetDeliveryGuard 每次尝试投递前检查关联是否仍然有效，包括已经准备结果的重试。
func (c *Center) SetDeliveryGuard(guard func(task.Task) (bool, error)) {
	c.mu.Lock()
	c.deliveryGuard = guard
	c.mu.Unlock()
}

// SetNoticeHook 挂载通知变化回调（移动端长链接推送用）：新通知落库或投递
// 状态变化时以最新快照回调一次。回调在 Center 内部锁内被同步调用——必须
// 非阻塞，且不得再调用 Center 的任何方法，否则死锁。
func (c *Center) SetNoticeHook(hook func(Notice)) {
	c.mu.Lock()
	c.noticeHook = hook
	c.mu.Unlock()
}

func (c *Center) Retry(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, exists := c.notices[id]
	if !exists {
		return fmt.Errorf("通知不存在")
	}
	if n.Status != "failed" {
		return fmt.Errorf("只有失败通知可以重试")
	}
	n.Status, n.LastError, n.Attempts = "pending", "", 0
	n.NextAttempt = time.Time{}
	n.UpdatedAt = time.Now()
	if err := c.putLocked(n); err != nil {
		return err
	}
	c.signal()
	return nil
}

func (c *Center) Start(ctx context.Context) {
	c.start.Do(func() {
		c.mu.Lock()
		c.started = true
		c.mu.Unlock()
		go func() {
			defer close(c.done)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				for ctx.Err() == nil {
					did, err := c.ProcessOne(ctx)
					if err != nil {
						log.Printf("[通知] 处理失败: %v", err)
					}
					if !did || err != nil {
						break
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				case <-c.wake:
				}
			}
		}()
	})
}

// Wait 在调用方取消 Start 的 context 后等待当前工作退出。
func (c *Center) Wait() {
	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	if started {
		<-c.done
	}
}

func (c *Center) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// ProcessOne 处理一条到期通知；通道成功状态落盘后才继续下一通道。
func (c *Center) ProcessOne(ctx context.Context) (bool, error) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	c.mu.Lock()
	var n Notice
	for _, candidate := range c.valuesLocked() {
		if (candidate.Status == "pending" || candidate.Status == "processing") && !candidate.NextAttempt.After(time.Now()) {
			n = candidate
			break
		}
	}
	if n.ID == "" {
		c.mu.Unlock()
		return false, nil
	}
	n.Status, n.UpdatedAt = "processing", time.Now()
	webEnabled := c.webEnabled
	desktopDispatch := c.desktopDispatch
	deliveryGuard := c.deliveryGuard
	n.Attempts++
	if err := c.putLocked(n); err != nil {
		c.mu.Unlock()
		return false, err
	}
	c.mu.Unlock()
	workCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if deliveryGuard != nil {
		allowed, err := deliveryGuard(n.Task)
		if err != nil {
			return true, c.fail(n, err)
		}
		if !allowed {
			n.Status, n.Prepared, n.Push, n.LastError = "silent", true, false, "关联事项已完成、删除或更新，剩余投递已停止"
			return true, c.put(n)
		}
	}
	if !n.Prepared {
		text, push, err := n.Task.Text, true, error(nil)
		if c.resolve != nil {
			text, push, err = c.resolve(workCtx, n.Task)
		}
		if err != nil {
			return true, c.fail(n, err)
		}
		n.Text, n.Push, n.Prepared = text, push, true
		if !push {
			n.Status = "silent"
		}
		if err := c.put(n); err != nil {
			return true, err
		}
		if !push {
			return true, nil
		}
	}
	if webEnabled && !slices.Contains(n.DeliveredTo, "web") {
		n.DeliveredTo = append(n.DeliveredTo, "web")
		if err := c.put(n); err != nil {
			return true, err
		}
	}
	matched := webEnabled && (n.Task.Channel == "" || n.Task.Channel == "web")
	if desktopDispatch != nil {
		if err := desktopDispatch(n); err != nil {
			log.Printf("[通知] 桌面提示转交失败，内容保留在通知中心: %v", err)
		}
	}
	var deliveryErr error
	for _, target := range c.targets {
		if n.Task.Channel != "" && n.Task.Channel != target.Name() {
			continue
		}
		matched = true
		if slices.Contains(n.DeliveredTo, target.Name()) {
			continue
		}
		if err := target.Notify(workCtx, n.Text); err != nil {
			deliveryErr = errors.Join(deliveryErr, fmt.Errorf("%s: %w", target.Name(), err))
			continue
		}
		n.DeliveredTo = append(n.DeliveredTo, target.Name())
		if err := c.put(n); err != nil {
			return true, err
		}
	}
	if !matched {
		if n.Task.Channel == "" {
			deliveryErr = fmt.Errorf("没有可用的通知通道，请启用网页通知中心或配置推送通道")
		} else {
			deliveryErr = fmt.Errorf("未配置目标通知通道 %s", n.Task.Channel)
		}
	}
	if deliveryErr != nil {
		return true, c.fail(n, deliveryErr)
	}
	n.Status, n.LastError, n.NextAttempt = "delivered", "", time.Time{}
	return true, c.put(n)
}

func (c *Center) fail(n Notice, reason error) error {
	n.LastError = reason.Error()
	if n.Attempts >= 5 {
		n.Status = "failed"
	} else {
		n.Status = "pending"
		n.NextAttempt = time.Now().Add(time.Second * time.Duration(1<<min(n.Attempts, 6)))
	}
	return c.put(n)
}

func (c *Center) put(n Notice) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 工作者在锁外执行期间，用户可能已读/延后；不得用旧快照覆盖这些独立字段。
	if current, ok := c.notices[n.ID]; ok {
		n.ReadAt = current.ReadAt
		n.SnoozeTaskID, n.SnoozeAt, n.SnoozeCommitted = current.SnoozeTaskID, current.SnoozeAt, current.SnoozeCommitted
	}
	return c.putLocked(n)
}

func (c *Center) putLocked(n Notice) error {
	n.UpdatedAt = time.Now()
	before, exists := c.notices[n.ID]
	c.notices[n.ID] = n
	if err := c.saveLocked(); err != nil {
		if exists {
			c.notices[n.ID] = before
		} else {
			delete(c.notices, n.ID)
		}
		return err
	}
	// 只在"新通知"或"状态变化"时叫钩子：已读/延后这类局部字段更新不推，
	// 免得长链接被自触发的回显刷屏（自己点已读又收到一遍同一条）。
	if hook := c.noticeHook; hook != nil && (!exists || before.Status != n.Status) {
		hook(n)
	}
	return nil
}

func (c *Center) valuesLocked() []Notice {
	out := make([]Notice, 0, len(c.notices))
	for _, n := range c.notices {
		n.DeliveredTo = slices.Clone(n.DeliveredTo)
		if n.ReadAt != nil {
			at := *n.ReadAt
			n.ReadAt = &at
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (c *Center) saveLocked() error {
	if c.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(c.valuesLocked(), "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), ".notifications-*.tmp")
	if err != nil {
		return fmt.Errorf("保存通知失败: %w", err)
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, c.path)
}
