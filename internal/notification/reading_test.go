package notification

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/task"
)

func blockNoticePath(t *testing.T, path string) func() {
	t.Helper()
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, path); err != nil {
			t.Fatal(err)
		}
	}
}

func visibleNotice(t *testing.T, c *Center) Notice {
	t.Helper()
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	processNotice(t, c)
	return onlyNotice(t, c)
}

func TestMarkReadPersistsAcrossRestartAndSaveFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	c := newCenter(t, path, nil)
	n := visibleNotice(t, c)
	if c.Unread() != 1 {
		t.Fatalf("可见通知应未读: %d", c.Unread())
	}
	restore := blockNoticePath(t, path)
	if err := c.MarkRead(n.ID); err == nil {
		t.Fatal("已读保存失败应返回错误")
	}
	if got := onlyNotice(t, c); got.ReadAt != nil || c.Unread() != 1 {
		t.Fatalf("失败已读污染内存: %+v", got)
	}
	restore()
	if err := c.MarkRead(n.ID); err != nil {
		t.Fatal(err)
	}
	want := onlyNotice(t, c)
	if want.ReadAt == nil || c.Unread() != 0 {
		t.Fatalf("成功已读状态错误: %+v", want)
	}
	r := newCenter(t, path, nil)
	got := onlyNotice(t, r)
	if got.ReadAt == nil || !got.ReadAt.Equal(*want.ReadAt) || r.Unread() != 0 {
		t.Fatalf("重启丢失已读: %+v", got)
	}
	if err := r.MarkRead(n.ID); err != nil {
		t.Fatal(err)
	}
	if got := onlyNotice(t, r); !got.ReadAt.Equal(*want.ReadAt) {
		t.Fatal("重复已读应幂等保留原时刻")
	}
}

type blockingReadTarget struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingReadTarget) Name() string { return "remote" }
func (b *blockingReadTarget) Notify(ctx context.Context, _ string) error {
	close(b.started)
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestMarkReadDuringDeliveryIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	target := &blockingReadTarget{started: make(chan struct{}), release: make(chan struct{})}
	c := newCenter(t, path, nil, target)
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	n := onlyNotice(t, c)
	processed := make(chan error, 1)
	go func() { _, err := c.ProcessOne(context.Background()); processed <- err }()
	select {
	case <-target.started:
	case <-time.After(time.Second):
		t.Fatal("投递未开始")
	}
	if err := c.MarkRead(n.ID); err != nil {
		t.Fatal(err)
	}
	close(target.release)
	if err := <-processed; err != nil {
		t.Fatal(err)
	}
	got := onlyNotice(t, c)
	if got.ReadAt == nil || c.Unread() != 0 {
		t.Fatalf("投递回写覆盖了成功保存的已读状态: %+v", got)
	}
	if got := onlyNotice(t, newCenter(t, path, nil)); got.ReadAt == nil {
		t.Fatal("持久化已读被投递回写覆盖")
	}
}

func TestSnoozePlanningSaveFailureAndRepeatedRestartKeepOneTask(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifications.json")
	tasksPath := filepath.Join(dir, "tasks.json")
	c := newCenter(t, path, nil)
	n := visibleNotice(t, c)
	e := task.NewReliableEngineFile(nil, tasksPath)
	defer e.Close()
	restore := blockNoticePath(t, path)
	if _, err := c.Snooze(n.ID, time.Minute, e); err == nil {
		t.Fatal("延后计划写失败不得返回成功")
	}
	if e.Len() != 0 || onlyNotice(t, c).SnoozeTaskID != "" {
		t.Fatal("延后计划写失败不能新建任务或污染内存")
	}
	restore()
	first, err := c.Snooze(n.ID, time.Hour, e)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Snooze(n.ID, 2*time.Hour, e)
	if err != nil || second.ID != first.ID || !second.When.Equal(first.When) || e.Len() != 1 {
		t.Fatalf("重复延后应复用原计划: first=%+v second=%+v err=%v", first, second, err)
	}
	e.Close()
	r := newCenter(t, path, nil)
	restarted := task.NewReliableEngineFile(nil, tasksPath)
	defer restarted.Close()
	if err := r.RestoreSnoozes(restarted); err != nil {
		t.Fatal(err)
	}
	third, err := r.Snooze(n.ID, time.Minute, restarted)
	if err != nil || third.ID != first.ID || restarted.Len() != 1 || third.NotificationParentID != n.ID {
		t.Fatalf("重启应保持Parent与唯一任务: %+v %v", third, err)
	}
	if err := r.Enqueue(third); err != nil {
		t.Fatal(err)
	}
	var child Notice
	for _, notice := range r.List() {
		if notice.TaskID == third.ID {
			child = notice
		}
	}
	if child.ParentID != n.ID {
		t.Fatalf("延后子通知ParentID丢失: %+v", child)
	}
}

func TestSnoozeTaskSaveFailureRestoresPendingPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifications.json")
	tasksPath := filepath.Join(dir, "missing", "tasks.json")
	c := newCenter(t, path, nil)
	n := visibleNotice(t, c)
	e := task.NewReliableEngineFile(nil, tasksPath)
	defer e.Close()
	if _, err := c.Snooze(n.ID, time.Hour, e); err == nil || !strings.Contains(err.Error(), "同步失败") {
		t.Fatalf("任务写失败须明确报错: %v", err)
	}
	planned := onlyNotice(t, c)
	if planned.SnoozeTaskID == "" || planned.SnoozeCommitted || e.Len() != 0 {
		t.Fatalf("应持久保留待恢复计划: %+v", planned)
	}
	e.Close()
	if err := os.Mkdir(filepath.Dir(tasksPath), 0o700); err != nil {
		t.Fatal(err)
	}
	r := newCenter(t, path, nil)
	restarted := task.NewReliableEngineFile(nil, tasksPath)
	defer restarted.Close()
	if err := r.RestoreSnoozes(restarted); err != nil {
		t.Fatal(err)
	}
	if got := restarted.List(); len(got) != 1 || got[0].ID != planned.SnoozeTaskID || !got[0].When.Equal(planned.SnoozeAt) || got[0].NotificationParentID != n.ID {
		t.Fatalf("待恢复延后计划不符: %+v", got)
	}
	if got := onlyNotice(t, r); !got.SnoozeCommitted || got.ReadAt == nil {
		t.Fatalf("恢复后应确认及已读: %+v", got)
	}
}

func TestSnoozeScheduleSuccessConfirmationFailureRestoresSameTask(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifications.json")
	tasksPath := filepath.Join(dir, "tasks.json")
	c := newCenter(t, path, nil)
	n := visibleNotice(t, c)
	e := task.NewReliableEngineFile(nil, tasksPath)
	defer e.Close()
	// 准备阶段已持久化计划；接下来只阻止完成任务创建后的确认写盘。
	planned := n
	planned.SnoozeTaskID = "snooze-" + n.ID
	planned.SnoozeAt = time.Now().Add(time.Hour)
	c.mu.Lock()
	err := c.putLocked(planned) // 模拟 Snooze 的计划提交入口；put 现仅用于 worker 状态合并
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restore := blockNoticePath(t, path)
	if _, err := c.scheduleSnooze(planned, e); err == nil || !strings.Contains(err.Error(), "确认保存失败") {
		t.Fatalf("确认失败须明确报告任务已创建: %v", err)
	}
	if got := e.List(); len(got) != 1 || got[0].ID != planned.SnoozeTaskID || got[0].NotificationParentID != n.ID {
		t.Fatalf("确认失败前任务应已成功保存: %+v", got)
	}
	if got := onlyNotice(t, c); got.SnoozeCommitted || got.ReadAt != nil {
		t.Fatalf("失败确认应保持之前状态: %+v", got)
	}
	restore()
	e.Close()
	r := newCenter(t, path, nil)
	restarted := task.NewReliableEngineFile(nil, tasksPath)
	defer restarted.Close()
	if err := r.RestoreSnoozes(restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.Len() != 1 || !onlyNotice(t, r).SnoozeCommitted {
		t.Fatal("确认失败恢复不应多建任务")
	}
	if _, err := r.Snooze(n.ID, time.Minute, restarted); err != nil || restarted.Len() != 1 {
		t.Fatalf("确认恢复后重复操作必须幂等: %v", err)
	}
}

func TestSnoozePreservesChannelAndRejectsLinkedTodo(t *testing.T) {
	e := task.NewReliableEngineFile(nil, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	c := newCenter(t, "", nil)
	n := visibleNotice(t, c)
	c.mu.Lock()
	n.Task.Channel = "feishu"
	err := c.putLocked(n)
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Snooze(n.ID, time.Hour, e)
	if err != nil || got.Channel != "feishu" {
		t.Fatalf("延后必须保留显式通道: %+v %v", got, err)
	}
	c.mu.Lock()
	linked := n
	linked.ID = "linked"
	linked.Task.TodoID = "todo"
	err = c.putLocked(linked)
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snooze(linked.ID, time.Hour, e); err == nil {
		t.Fatal("关联待办不能绕过待办门卫创建独立延后任务")
	}
	if e.Len() != 1 {
		t.Fatal("关联待办延后被拒绝后不能多建任务")
	}
}

func TestListForTaskFiltersBeforeGlobalLimit(t *testing.T) {
	c := newCenter(t, "", nil)
	c.mu.Lock()
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("other-%d", i)
		c.notices[id] = Notice{ID: id, TaskID: "other", CreatedAt: time.Now().Add(time.Duration(i) * time.Hour)}
	}
	c.notices["wanted"] = Notice{ID: "wanted", TaskID: "wanted-task", CreatedAt: time.Now().Add(-time.Hour)}
	c.mu.Unlock()
	got := c.ListForTask("wanted-task")
	if len(got) != 1 || got[0].ID != "wanted" {
		t.Fatalf("本任务历史不能被其它任务最近100条挤出: %+v", got)
	}
	if len(c.List()) != 100 {
		t.Fatal("全局列表仍应有100条限制")
	}
}

func TestPreparedTodoRetryRechecksDeliveryGuardWithoutRepeatingRemote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	target := &recordingTarget{name: "remote", err: errors.New("暂时不可用")}
	c := newCenter(t, path, nil, target)
	allowed := true
	c.SetDeliveryGuard(func(task.Task) (bool, error) { return allowed, nil })
	tk := testTask()
	tk.TodoID = "todo"
	if err := c.Enqueue(tk); err != nil {
		t.Fatal(err)
	}
	processNotice(t, c)
	n := onlyNotice(t, c)
	if !n.Prepared || n.Status != "pending" || target.calls() != 1 {
		t.Fatalf("应已准备并等待远程重试: %+v calls=%d", n, target.calls())
	}
	// 已准备内容跨重启保留；完成待办后守卫拒绝剩余通道，而不是再次调用 resolver。
	r := newCenter(t, path, nil, target)
	allowed = false
	r.SetDeliveryGuard(func(task.Task) (bool, error) { return allowed, nil })
	retryDueNow(t, r)
	processNotice(t, r)
	n = onlyNotice(t, r)
	if n.Status != "silent" || n.Push || target.calls() != 1 {
		t.Fatalf("已完成待办不得再次远程投递: %+v calls=%d", n, target.calls())
	}
	if len(n.DeliveredTo) != 1 || n.DeliveredTo[0] != "web" {
		t.Fatalf("已成功通道必须保留历史: %+v", n)
	}
	if got := onlyNotice(t, newCenter(t, path, nil, target)); got.Status != "silent" || got.Push {
		t.Fatalf("停止投递状态未持久化: %+v", got)
	}
}
