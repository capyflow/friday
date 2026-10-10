package notification

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/task"
)

type recordingTarget struct {
	name  string
	mu    sync.Mutex
	err   error
	texts []string
}

func (r *recordingTarget) Name() string { return r.name }
func (r *recordingTarget) Notify(_ context.Context, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.texts = append(r.texts, text)
	return r.err
}
func (r *recordingTarget) setError(err error) { r.mu.Lock(); defer r.mu.Unlock(); r.err = err }
func (r *recordingTarget) calls() int         { r.mu.Lock(); defer r.mu.Unlock(); return len(r.texts) }

func testTask() task.Task {
	when := time.Date(2026, 10, 10, 8, 0, 0, 123, time.FixedZone("CST", 8*3600))
	return task.Task{ID: "reminder", Kind: task.KindOnce, Text: "该喝水了", When: when, Next: when}
}

func newCenter(t *testing.T, path string, resolver Resolver, targets ...Target) *Center {
	t.Helper()
	c, err := New(path, resolver, targets)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func onlyNotice(t *testing.T, c *Center) Notice {
	t.Helper()
	list := c.List()
	if len(list) != 1 {
		t.Fatalf("通知数量 = %d，want 1: %+v", len(list), list)
	}
	return list[0]
}

func processNotice(t *testing.T, c *Center) {
	t.Helper()
	did, err := c.ProcessOne(context.Background())
	if err != nil || !did {
		t.Fatalf("ProcessOne = %v, %v，want true, nil", did, err)
	}
}

func retryDueNow(t *testing.T, c *Center) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.notices {
		n.NextAttempt = time.Unix(0, 0)
		if err := c.putLocked(n); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnqueueStableKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	c := newCenter(t, path, nil)
	original := testTask()
	if err := c.Enqueue(original); err != nil {
		t.Fatal(err)
	}
	id := onlyNotice(t, c).ID
	// 时区表达不同但计划时刻相同，以及重启后的同次触发，均须去重。
	copy := original
	copy.Next = original.Next.UTC()
	copy.Text = "重复接收时内容变化也不能另建通知"
	if err := c.Enqueue(copy); err != nil {
		t.Fatal(err)
	}
	restored := newCenter(t, path, nil)
	if err := restored.Enqueue(original); err != nil {
		t.Fatal(err)
	}
	if got := onlyNotice(t, restored); got.ID != id || got.Text != original.Text {
		t.Fatalf("稳定键或原通知被改变: %+v", got)
	}
	copy.Next = original.Next.Add(time.Hour)
	if err := restored.Enqueue(copy); err != nil {
		t.Fatal(err)
	}
	if len(restored.List()) != 2 {
		t.Fatal("下一计划时刻应该创建另一条通知")
	}
}

func TestEnqueueSaveFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "notifications.json")
	c := newCenter(t, path, nil)
	if err := c.Enqueue(testTask()); err == nil {
		t.Fatal("父目录缺失应返回保存错误")
	}
	if len(c.List()) != 0 {
		t.Fatal("入队失败不得保留内存通知")
	}
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	onlyNotice(t, newCenter(t, path, nil))
}

func TestLocalNoticeDeliveredAndRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	c := newCenter(t, path, nil)
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	processNotice(t, c)
	want := onlyNotice(t, c)
	if want.Status != "delivered" || want.Text != testTask().Text || !slices.Equal(want.DeliveredTo, []string{"web"}) {
		t.Fatalf("无远程通道也必须有实际可见的本地通知: %+v", want)
	}
	restored := newCenter(t, path, nil)
	got := onlyNotice(t, restored)
	if got.ID != want.ID || got.Status != want.Status || got.Text != want.Text || got.Attempts != 1 ||
		!got.CreatedAt.Equal(want.CreatedAt) || !slices.Equal(got.DeliveredTo, want.DeliveredTo) {
		t.Fatalf("重启后处理结果丢失: %+v，want %+v", got, want)
	}
	if did, err := restored.ProcessOne(context.Background()); did || err != nil {
		t.Fatalf("已送达通知不能重跑: %v, %v", did, err)
	}
}

func TestDeliveryRetryDoesNotRerunResolverOrSuccessfulTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	var resolved int
	resolver := func(context.Context, task.Task) (string, bool, error) { resolved++; return "模型结果", true, nil }
	good := &recordingTarget{name: "feishu"}
	bad := &recordingTarget{name: "dingtalk", err: errors.New("断网")}
	c := newCenter(t, path, resolver, good, bad)
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	processNotice(t, c)
	n := onlyNotice(t, c)
	if !n.Prepared || n.Status != "pending" || !strings.Contains(n.LastError, "dingtalk") ||
		!slices.Equal(n.DeliveredTo, []string{"web", "feishu"}) {
		t.Fatalf("部分失败状态不符: %+v", n)
	}
	if did, err := c.ProcessOne(context.Background()); did || err != nil {
		t.Fatalf("退避期间不应立即重试: %v, %v", did, err)
	}
	c = newCenter(t, path, resolver, good, bad)
	bad.setError(nil)
	retryDueNow(t, c)
	processNotice(t, c)
	n = onlyNotice(t, c)
	if resolved != 1 || good.calls() != 1 || bad.calls() != 2 {
		t.Fatalf("重试重复执行: resolver=%d good=%d bad=%d", resolved, good.calls(), bad.calls())
	}
	if n.Text != "模型结果" || n.Status != "delivered" || n.Attempts != 2 || n.LastError != "" ||
		!slices.Equal(n.DeliveredTo, []string{"web", "feishu", "dingtalk"}) {
		t.Fatalf("恢复后重试结果不符: %+v", n)
	}
}

func TestFiveFailuresThenManualRetry(t *testing.T) {
	var resolved int
	resolver := func(context.Context, task.Task) (string, bool, error) { resolved++; return "提醒", true, nil }
	bad := &recordingTarget{name: "dingtalk", err: errors.New("服务不可达")}
	c := newCenter(t, "", resolver, bad)
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 5; attempt++ {
		retryDueNow(t, c)
		processNotice(t, c)
		if got := onlyNotice(t, c); got.Attempts != attempt {
			t.Fatalf("尝试次数 = %d，want %d", got.Attempts, attempt)
		}
	}
	n := onlyNotice(t, c)
	if n.Status != "failed" || n.LastError == "" || bad.calls() != 5 || resolved != 1 {
		t.Fatalf("失败阈值状态不符: %+v", n)
	}
	if did, err := c.ProcessOne(context.Background()); did || err != nil {
		t.Fatalf("达到阈值后不得自动重试: %v, %v", did, err)
	}
	bad.setError(nil)
	if err := c.Retry(n.ID); err != nil {
		t.Fatal(err)
	}
	if got := onlyNotice(t, c); got.Status != "pending" || got.Attempts != 0 || !got.Prepared {
		t.Fatalf("人工重试状态不符: %+v", got)
	}
	processNotice(t, c)
	if got := onlyNotice(t, c); got.Status != "delivered" || got.Attempts != 1 {
		t.Fatalf("人工重试未送达: %+v", got)
	}
	if resolved != 1 || bad.calls() != 6 {
		t.Fatal("人工重试不得重新生成已经成功的结果")
	}
	if err := c.Retry(n.ID); err == nil {
		t.Fatal("已送达通知不应允许重试")
	}
}

func TestSilentResolverDoesNotDeliver(t *testing.T) {
	target := &recordingTarget{name: "dingtalk"}
	c := newCenter(t, "", func(context.Context, task.Task) (string, bool, error) { return "无需通知", false, nil }, target)
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	processNotice(t, c)
	if n := onlyNotice(t, c); n.Status != "silent" || len(n.DeliveredTo) != 0 || target.calls() != 0 {
		t.Fatalf("静默任务发生投递: %+v", n)
	}
}

func TestUnconfiguredTargetRetainsWebNoticeAndFailure(t *testing.T) {
	c := newCenter(t, "", nil)
	task := testTask()
	task.Channel = "dingtalk"
	if err := c.Enqueue(task); err != nil {
		t.Fatal(err)
	}
	processNotice(t, c)
	n := onlyNotice(t, c)
	if n.Status != "pending" || !strings.Contains(n.LastError, "dingtalk") || !slices.Equal(n.DeliveredTo, []string{"web"}) {
		t.Fatalf("指定通道未配置不能假报远程送达: %+v", n)
	}
}

func TestProcessingNoticeRestoresWithoutRepeatingPreparedWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	c := newCenter(t, path, nil)
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	n := onlyNotice(t, c)
	n.Status, n.Prepared, n.Push, n.Text, n.Attempts = "processing", true, true, "已生成结果", 1
	n.DeliveredTo = []string{"web", "feishu"}
	if err := c.put(n); err != nil {
		t.Fatal(err)
	}
	var resolved int
	good := &recordingTarget{name: "feishu"}
	remaining := &recordingTarget{name: "dingtalk"}
	restored := newCenter(t, path, func(context.Context, task.Task) (string, bool, error) { resolved++; return "不该生成", true, nil }, good, remaining)
	if got := onlyNotice(t, restored); got.Status != "pending" {
		t.Fatalf("在途状态未恢复: %+v", got)
	}
	processNotice(t, restored)
	if got := onlyNotice(t, restored); got.Status != "delivered" || got.Text != "已生成结果" || resolved != 0 || good.calls() != 0 || remaining.calls() != 1 {
		t.Fatalf("重启重复执行或丢失结果: %+v", got)
	}
}

func TestCorruptNoticeFilePreserved(t *testing.T) {
	for _, raw := range []string{"{broken", "null", `[{"id":"one","status":"unknown"}]`, `[{"status":"pending"}]`,
		`[{"id":"one","status":"pending"},{"id":"one","status":"pending"}]`} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "notifications.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(path, nil, nil); err == nil {
				t.Fatal("损坏通知文件应阻止空启动")
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != raw {
				t.Fatalf("原文件必须保留: %q, %v", got, err)
			}
		})
	}
}

func TestDanglingNoticeSymlinkPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifications.json")
	target := filepath.Join(dir, "missing.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path, nil, nil); err == nil {
		t.Fatal("损坏软链接不能当作新通知文件")
	}
	if got, err := os.Readlink(path); err != nil || got != target {
		t.Fatalf("原软链接没有保留: %q, %v", got, err)
	}
}

func TestCenterWorkerCancellation(t *testing.T) {
	started := make(chan struct{})
	c := newCenter(t, "", func(ctx context.Context, _ task.Task) (string, bool, error) {
		close(started)
		<-ctx.Done()
		return "", false, ctx.Err()
	})
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker 未接收通知")
	}
	cancel()
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("取消后 worker 未退出")
	}
	if n := onlyNotice(t, c); n.Status != "pending" || n.Prepared || !strings.Contains(n.LastError, "canceled") {
		t.Fatalf("取消后状态不符: %+v", n)
	}
}

func TestReliableSchedulerRetryAfterAdvanceSaveFailureDeduplicatesNotice(t *testing.T) {
	dir := t.TempDir()
	tasksPath := filepath.Join(dir, "tasks.json")
	backup := tasksPath + ".saved"
	noticesPath := filepath.Join(dir, "notifications.json")
	c := newCenter(t, noticesPath, nil)
	accepted := make(chan error, 16)
	var calls atomic.Int32
	engine := task.NewReliableEngineFile(func(_ context.Context, tk task.Task) error {
		if err := c.Enqueue(tk); err != nil {
			accepted <- err
			return err
		}
		if calls.Add(1) == 1 {
			// 入队已经提交，用目录阻止随后任务快照的 rename，而非破坏通知文件。
			if err := os.Rename(tasksPath, backup); err != nil {
				accepted <- err
				return err
			}
			if err := os.Mkdir(tasksPath, 0o700); err != nil {
				accepted <- err
				return err
			}
		}
		accepted <- nil
		return nil
	}, tasksPath)
	defer engine.Close()
	if engine.Err() != nil {
		t.Fatal(engine.Err())
	}
	if _, err := engine.Schedule(task.Task{ID: "once", Kind: task.KindOnce, Text: "提醒", When: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("任务未入队")
	}
	// List 等待调度器释放锁，因此这里已经完成那次失败的推进写盘。
	if list := engine.List(); len(list) != 1 || list[0].ID != "once" {
		t.Fatalf("写盘失败必须保留原任务: %+v", list)
	}
	if err := os.Remove(tasksPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, tasksPath); err != nil {
		t.Fatal(err)
	}
	// 注册未来任务触发唤醒，立即重试到期任务，避免等待退避计时器。
	if _, err := engine.Schedule(task.Task{ID: "wake", Kind: task.KindOnce, When: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// 初始 Schedule 的 wake 可能触发一次额外失败重试；等待可靠推进完成而非假设回调总数。
	deadline := time.After(time.Second)
	for {
		select {
		case err := <-accepted:
			if err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("修复文件后未重新接收并推进")
		}
		if list := engine.List(); len(list) == 1 && list[0].ID == "wake" {
			break
		}
	}
	if calls.Load() < 2 {
		t.Fatalf("accept 次数 = %d，want 至少 2", calls.Load())
	}
	n := onlyNotice(t, newCenter(t, noticesPath, nil))
	if n.TaskID != "once" {
		t.Fatalf("重复接收破坏稳定键: %+v", n)
	}
	processNotice(t, c)
	if got := onlyNotice(t, c); got.Status != "delivered" {
		t.Fatalf("唯一通知未送达: %+v", got)
	}
	raw, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored []map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil || len(stored) != 1 || stored[0]["id"] != "wake" {
		t.Fatalf("持久化推进不符: %s, %v", raw, err)
	}
}
