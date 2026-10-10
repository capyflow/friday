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
	ID          string    `json:"id"`
	TaskID      string    `json:"taskId"`
	Text        string    `json:"text"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"lastError,omitempty"`
	DeliveredTo []string  `json:"deliveredTo"`
	Task        task.Task `json:"task"`
	Prepared    bool      `json:"prepared"`
	Push        bool      `json:"push"`
	NextAttempt time.Time `json:"nextAttempt,omitempty"`
}

type Center struct {
	mu         sync.Mutex
	processMu  sync.Mutex
	path       string
	notices    map[string]Notice
	resolve    Resolver
	targets    []Target
	start      sync.Once
	wake       chan struct{}
	done       chan struct{}
	started    bool
	webEnabled bool
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
	n := Notice{ID: id, TaskID: t.ID, Task: t, Text: t.Text, Status: "pending", CreatedAt: now, UpdatedAt: now, DeliveredTo: []string{}}
	if err := c.putLocked(n); err != nil {
		return err
	}
	c.signal()
	return nil
}

func (c *Center) List() []Notice {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.valuesLocked()
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

// SetWebEnabled 只有管理后台实际启用时，通知中心才是可见的投递位置。
func (c *Center) SetWebEnabled(enabled bool) { c.mu.Lock(); c.webEnabled = enabled; c.mu.Unlock() }

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
	n.Attempts++
	if err := c.putLocked(n); err != nil {
		c.mu.Unlock()
		return false, err
	}
	c.mu.Unlock()
	workCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
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

func (c *Center) put(n Notice) error { c.mu.Lock(); defer c.mu.Unlock(); return c.putLocked(n) }

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
	return nil
}

func (c *Center) valuesLocked() []Notice {
	out := make([]Notice, 0, len(c.notices))
	for _, n := range c.notices {
		n.DeliveredTo = slices.Clone(n.DeliveredTo)
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
