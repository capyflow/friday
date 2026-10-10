package todo

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/task"
)

func reminderStore(t *testing.T) (*Store, *task.Engine, string, string) {
	t.Helper()
	dir := t.TempDir()
	path, tasksPath := filepath.Join(dir, "todos.json"), filepath.Join(dir, "tasks.json")
	engine := task.NewReliableEngineFile(nil, tasksPath)
	t.Cleanup(engine.Close)
	s := NewStore(path)
	if err := s.BindEngine(engine); err != nil {
		t.Fatal(err)
	}
	return s, engine, path, tasksPath
}

func futureTimes() (*time.Time, *time.Time) {
	remind := time.Now().Add(2 * time.Hour)
	due := remind.Add(time.Hour)
	return &due, &remind
}

func blockReminderFile(t *testing.T, path string) func() {
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

func TestDeadlineOnlyDoesNotCreateReminder(t *testing.T) {
	s, engine, _, _ := reminderStore(t)
	due, _ := futureTimes()
	td, err := s.AddWithDeadline("交房租", due, nil)
	if err != nil || td.DueAt == nil || !td.DueAt.Equal(*due) || td.ReminderTaskID != "" || engine.Len() != 0 {
		t.Fatalf("截止时间不应自动创建提醒: %+v %v tasks=%+v", td, err, engine.List())
	}
}

func TestReminderCreateUpdateCompleteReopenDelete(t *testing.T) {
	s, engine, _, _ := reminderStore(t)
	due, remind := futureTimes()
	td, err := s.AddWithDeadline("交房租", due, remind)
	if err != nil {
		t.Fatal(err)
	}
	first, found := engine.Get(td.ReminderTaskID)
	if !found || first.TodoID != td.ID || !first.When.Equal(*remind) {
		t.Fatalf("提醒未关联: %+v", first)
	}
	if ok, err := s.ShouldRemind(first); !ok || err != nil {
		t.Fatalf("有效关联提醒被阻止: %v %v", ok, err)
	}
	newDue, newRemind := due.Add(time.Hour), remind.Add(time.Hour)
	updated, ok, err := s.Update(td.ID, "交下月房租", &newDue, &newRemind)
	if err != nil || !ok || updated.ReminderTaskID == td.ReminderTaskID {
		t.Fatalf("更新没有新代际: %+v %v %v", updated, ok, err)
	}
	if _, found := engine.Get(td.ReminderTaskID); found {
		t.Fatal("旧未来提醒仍在任务列表")
	}
	if accepted, err := s.ShouldRemind(first); accepted || err != nil {
		t.Fatalf("旧代际不可入队: %v %v", accepted, err)
	}
	current, found := engine.Get(updated.ReminderTaskID)
	if !found || !strings.Contains(current.Text, "交下月房租") {
		t.Fatalf("新提醒内容错误: %+v", current)
	}
	completed, ok, err := s.Complete(updated.ID, true)
	if err != nil || !ok || !completed.Done || engine.Len() != 0 {
		t.Fatalf("完成未取消提醒: %+v %v %v tasks=%+v", completed, ok, err, engine.List())
	}
	if accepted, _ := s.ShouldRemind(current); accepted {
		t.Fatal("已完成待办可继续入队")
	}
	reopened, ok, err := s.Complete(updated.ID, false)
	if err != nil || !ok || reopened.Done || reopened.ReminderTaskID == updated.ReminderTaskID || engine.Len() != 1 {
		t.Fatalf("重开未生成新的未来提醒: %+v %v %v", reopened, ok, err)
	}
	active, _ := engine.Get(reopened.ReminderTaskID)
	if deleted, err := s.Delete(reopened.ID); !deleted || err != nil || engine.Len() != 0 {
		t.Fatalf("删除未取消提醒: %v %v", deleted, err)
	}
	if accepted, _ := s.ShouldRemind(active); accepted {
		t.Fatal("已删除待办可继续入队")
	}
}

func TestDeadlineMissingDirectoryDoesNotCreateOrphanTask(t *testing.T) {
	dir := t.TempDir()
	engine := task.NewReliableEngineFile(nil, filepath.Join(dir, "tasks.json"))
	defer engine.Close()
	s := NewStore(filepath.Join(dir, "missing", "todos.json"))
	if err := s.BindEngine(engine); err != nil {
		t.Fatal(err)
	}
	due, remind := futureTimes()
	if _, err := s.AddWithDeadline("不能保存", due, remind); err == nil {
		t.Fatal("待办写入失败必须返回错误")
	}
	if len(s.List()) != 0 || engine.Len() != 0 {
		t.Fatal("源事实写入失败不得留下待办或提醒任务")
	}
}

func TestReminderTodoRenameFailurePreservesOldAssociation(t *testing.T) {
	s, engine, path, _ := reminderStore(t)
	due, remind := futureTimes()
	td, err := s.AddWithDeadline("原待办", due, remind)
	if err != nil {
		t.Fatal(err)
	}
	repair := blockReminderFile(t, path)
	defer repair()
	if _, ok, err := s.Update(td.ID, "新内容", due, remind); err == nil || ok {
		t.Fatalf("更新必须报告原子替换失败: %v %v", ok, err)
	}
	if _, ok, err := s.Complete(td.ID, true); err == nil || ok {
		t.Fatalf("完成必须报告失败: %v %v", ok, err)
	}
	if deleted, err := s.Delete(td.ID); err == nil || deleted {
		t.Fatalf("删除必须报告失败: %v %v", deleted, err)
	}
	got, _ := s.Get(td.ID)
	tk, found := engine.Get(td.ReminderTaskID)
	if got.Text != td.Text || got.Done || !found {
		t.Fatalf("失败改变原关联: %+v %+v", got, tk)
	}
	if accepted, err := s.ShouldRemind(tk); !accepted || err != nil {
		t.Fatalf("未提交更新不应破坏有效提醒: %v %v", accepted, err)
	}
}

func TestReminderScheduleFailureKeepsSourceAndRestoresAcrossRestart(t *testing.T) {
	s, engine, path, tasksPath := reminderStore(t)
	// 先创建独立任务使任务文件存在，然后阻止关联任务快照替换。
	if _, err := engine.Schedule(task.Task{ID: "other", Kind: task.KindOnce, When: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	repair := blockReminderFile(t, tasksPath)
	due, remind := futureTimes()
	td, err := s.AddWithDeadline("已保存待办", due, remind)
	if err == nil || !strings.Contains(err.Error(), "待办已保存") || td.ID == "" {
		t.Fatalf("跨文件失败必须说明提交状态: %+v %v", td, err)
	}
	if got, ok := s.Get(td.ID); !ok || got.ReminderTaskID == "" || s.SyncError() == nil {
		t.Fatalf("源事实或失败状态丢失: %+v", got)
	}
	if accepted, err := s.ShouldRemind(task.Task{ID: td.ReminderTaskID, TodoID: td.ID, When: *remind}); accepted || err == nil {
		t.Fatalf("同步失败应关闭提醒门卫: %v %v", accepted, err)
	}
	repair()
	engine.Close()
	restartedEngine := task.NewReliableEngineFile(nil, tasksPath)
	defer restartedEngine.Close()
	restored := NewStore(path)
	if err := restored.BindEngine(restartedEngine); err != nil {
		t.Fatal(err)
	}
	tk, exists := restartedEngine.Get(td.ReminderTaskID)
	if !exists || restartedEngine.Len() != 2 {
		t.Fatalf("重启未恢复缺失任务或改变独立任务: %+v", restartedEngine.List())
	}
	if accepted, err := restored.ShouldRemind(tk); !accepted || err != nil {
		t.Fatalf("恢复后门卫仍关闭: %v %v", accepted, err)
	}
	if err := restored.Reconcile(); err != nil || restartedEngine.Len() != 2 {
		t.Fatalf("重复恢复不幂等: %v %+v", err, restartedEngine.List())
	}
}

func TestCompletionCancelFailureBlocksFutureReminderUntilRecovery(t *testing.T) {
	s, engine, path, tasksPath := reminderStore(t)
	due, remind := futureTimes()
	td, err := s.AddWithDeadline("完成后不能提醒", due, remind)
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := engine.Get(td.ReminderTaskID)
	repair := blockReminderFile(t, tasksPath)
	completed, ok, err := s.Complete(td.ID, true)
	if err == nil || !ok || !completed.Done || !strings.Contains(err.Error(), "待办已保存") {
		t.Fatalf("完成状态必须提交并标记同步故障: %+v %v %v", completed, ok, err)
	}
	if accepted, _ := s.ShouldRemind(tk); accepted {
		t.Fatal("取消写盘失败也不能继续接受已完成待办提醒")
	}
	if got, _ := NewStore(path).Get(td.ID); !got.Done {
		t.Fatal("重启丢失已提交完成状态")
	}
	repair()
	if err := s.Reconcile(); err != nil || engine.Len() != 0 || s.SyncError() != nil {
		t.Fatalf("恢复取消失败: %v tasks=%+v", err, engine.List())
	}
	if accepted, err := s.ShouldRemind(tk); accepted || err != nil {
		t.Fatalf("恢复后已完成旧提醒仍有效: %v %v", accepted, err)
	}
}

func TestReconcileRemovesOrphanAndStaleLinkedTasks(t *testing.T) {
	s, engine, path, tasksPath := reminderStore(t)
	due, remind := futureTimes()
	td, err := s.AddWithDeadline("当前版本", due, remind)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range []task.Task{
		{ID: "orphan", TodoID: "deleted", Kind: task.KindOnce, When: *remind},
		{ID: "stale", TodoID: td.ID, Kind: task.KindOnce, When: *remind},
	} {
		if _, err := engine.Schedule(tk); err != nil {
			t.Fatal(err)
		}
	}
	engine.Close()
	restarted := task.NewReliableEngineFile(nil, tasksPath)
	defer restarted.Close()
	restored := NewStore(path)
	if err := restored.BindEngine(restarted); err != nil {
		t.Fatal(err)
	}
	if tasks := restarted.List(); len(tasks) != 1 || tasks[0].ID != td.ReminderTaskID {
		t.Fatalf("源事实恢复没有清理失效任务: %+v", tasks)
	}
}

func TestOldTodoJSONCompatibleAndReturnedTimesAreCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	if err := os.WriteFile(path, []byte(`[{"id":"old","text":"历史待办","done":false}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if s.Err() != nil || len(s.List()) != 1 {
		t.Fatalf("旧格式无法恢复: %v %+v", s.Err(), s.List())
	}
	due, _ := futureTimes()
	td, err := s.AddWithDeadline("有截止", due, nil)
	if err != nil {
		t.Fatal(err)
	}
	*td.DueAt = time.Time{}
	*due = time.Time{}
	got, _ := s.Get(td.ID)
	if got.DueAt.IsZero() {
		t.Fatal("输入/返回指针修改了已提交待办")
	}
	listed := s.List()
	for _, td := range listed {
		if td.DueAt != nil {
			*td.DueAt = time.Time{}
		}
	}
	got, _ = s.Get(td.ID)
	if got.DueAt.IsZero() {
		t.Fatal("List 的时间指针修改了仓库")
	}
}

func TestShouldRemindAndCompletionHaveNoLockInversion(t *testing.T) {
	s, engine, _, _ := reminderStore(t)
	due, remind := futureTimes()
	td, err := s.AddWithDeadline("并发完成", due, remind)
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := engine.Get(td.ReminderTaskID)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _ = s.ShouldRemind(tk)
		}
	}()
	go func() { defer wg.Done(); _, _, _ = s.Complete(td.ID, true) }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("门卫/业务修改出现锁反转")
	}
	if accepted, _ := s.ShouldRemind(tk); accepted {
		t.Fatal("完成后旧提醒仍可入队")
	}
	if accepted, err := s.ShouldRemind(task.Task{}); !accepted || err != nil {
		t.Fatalf("非待办任务应正常运行: %v %v", accepted, err)
	}
}
