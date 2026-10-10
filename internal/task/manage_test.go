package task

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPausedTaskSurvivesRestartAndDoesNotAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	accepted := make(chan Task, 4)
	accept := func(_ context.Context, tk Task) error { accepted <- tk; return nil }
	e := NewReliableEngineFile(accept, path)
	tk, err := e.Schedule(Task{ID: "paused", Text: "暂停", Kind: KindOnce, When: time.Now().Add(-time.Hour), Status: StatusPaused, TodoID: "todo-1"})
	if err != nil {
		t.Fatal(err)
	}
	e.reliable.tick(time.Now())
	e.Close()
	restored := NewReliableEngineFile(accept, path)
	defer restored.Close()
	got, ok := restored.Get(tk.ID)
	if !ok || got.Status != StatusPaused || got.TodoID != "todo-1" || !got.Next.Equal(tk.Next) {
		t.Fatalf("暂停及关联恢复错误: %+v, %v", got, ok)
	}
	restored.reliable.tick(time.Now())
	select {
	case got := <-accepted:
		t.Fatalf("暂停任务不应入队: %+v", got)
	default:
	}
	if _, err := restored.Resume(tk.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-accepted:
		if got.ID != tk.ID || !got.Next.Equal(tk.Next) {
			t.Fatalf("过去单次须补原计划: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("恢复的过去单次未立即入队")
	}
	if got := restored.List(); len(got) != 0 {
		t.Fatalf("单次恢复后应只接受一次: %+v", got)
	}
}

func TestResumeRulesKeepIntervalAnchorAndSkipPausedSlots(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		tk   Task
		want time.Time
	}{
		{"once", Task{Kind: KindOnce, When: now.Add(-time.Hour), Next: now.Add(-time.Hour)}, now.Add(-time.Hour)},
		{"repeat", Task{Kind: KindRepeat, Interval: time.Hour, Times: 5, Remaining: 3, Next: now.Add(-150 * time.Minute)}, now.Add(30 * time.Minute)},
		{"future", Task{Kind: KindRepeat, Interval: time.Hour, Times: 5, Remaining: 3, Next: now.Add(10 * time.Minute)}, now.Add(10 * time.Minute)},
		{"cron", Task{Kind: KindCron, Cron: "0 * * * *", Next: now.Add(-time.Hour)}, now.Truncate(time.Hour).Add(time.Hour)},
		{"agent", Task{Kind: KindAgent, Interval: time.Hour, Times: 5, Remaining: 3, Next: now.Add(-150 * time.Minute)}, now.Add(30 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resumeRun(tc.tk, now)
			if err != nil || !got.Equal(tc.want) {
				t.Fatalf("恢复时刻 got=%v err=%v want=%v", got, err, tc.want)
			}
		})
	}
}

func TestResumeFineIntervalCatchesUpWithoutIteratingMissedSlots(t *testing.T) {
	now := time.Now()
	old := Task{Kind: KindRepeat, Interval: time.Nanosecond, Times: 3, Remaining: 3, Next: now.Add(-48 * time.Hour)}
	started := time.Now()
	got, err := resumeRun(old, now)
	if err != nil || !got.Equal(now.Add(time.Nanosecond)) {
		t.Fatalf("应直接定位下一锚点: got=%v err=%v", got, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("恢复不能逐个遍历暂停期间错过的槽位: %v", elapsed)
	}
}

func TestTaskDefinitionUpdatePreservesIdentityAndOnlyResetsChangedSchedule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := NewReliableEngineFile(nil, path)
	defer e.Close()
	old, err := e.Schedule(Task{ID: "same", Text: "原文", Kind: KindRepeat, Interval: time.Hour, Times: 5, Status: StatusPaused, Heartbeat: true, NotificationParentID: "notice-parent"})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟已经完成两次的暂停周期记录，不依赖真实计时等待。
	e.reliable.mu.Lock()
	old.Remaining = 3
	next := map[string]Task{old.ID: old}
	err = e.reliable.commit(next)
	e.reliable.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := e.Update(old.ID, Task{ID: "forged", Text: "新文", Kind: KindRepeat, Interval: time.Hour, Times: 5, TodoID: "forged", Channel: "web", Status: StatusActive, NotificationParentID: "forged"})
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID != old.ID || changed.TodoID != old.TodoID || changed.NotificationParentID != old.NotificationParentID || changed.Status != StatusPaused || !changed.Heartbeat ||
		!changed.CreatedAt.Equal(old.CreatedAt) || !changed.Next.Equal(old.Next) || changed.Remaining != 3 || changed.Text != "新文" || changed.Channel != "web" {
		t.Fatalf("正文编辑不得篡改身份或重置周期: %+v", changed)
	}
	newPlan, err := e.Update(old.ID, Task{Text: "新计划", Kind: KindRepeat, Interval: 2 * time.Hour, Times: 4})
	if err != nil {
		t.Fatal(err)
	}
	if newPlan.Remaining != 4 || !newPlan.Next.After(old.Next) || newPlan.Status != StatusPaused {
		t.Fatalf("新定时规则应重算计划但保持暂停: %+v", newPlan)
	}
	e.Close()
	r := NewReliableEngineFile(nil, path)
	defer r.Close()
	got, ok := r.Get(old.ID)
	if !ok || !equalTaskViews(got, newPlan) {
		t.Fatalf("编辑定义未持久化: got=%+v want=%+v", got, newPlan)
	}
}

func TestUpdateLinkedTodoRequiresTodoManagerButPauseResumeRemainAvailable(t *testing.T) {
	e := NewReliableEngineFile(nil, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	old, err := e.Schedule(Task{ID: "linked", TodoID: "todo", Text: "源文", Kind: KindOnce, When: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Update(old.ID, Task{Text: "篡改", Kind: KindOnce, When: time.Now().Add(2 * time.Hour)}); err == nil {
		t.Fatal("关联任务必须拒绝直接修改")
	}
	got, _ := e.Get(old.ID)
	if !equalTaskViews(got, old) {
		t.Fatalf("被拒绝的修改不能污染关联任务: %+v", got)
	}
	if _, err := e.Pause(old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(old.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPausedGoalStoreAndLegacyScheduleRetainPause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	g, err := buildGoal(Task{ID: "paused", Text: "暂停", Kind: KindRepeat, Interval: time.Hour, Times: 3, Remaining: 3,
		Status: StatusPaused, TodoID: "todo", NotificationParentID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	g.SetNextRunAt(time.Now().Add(time.Hour))
	s := newGoalStore(&fileStore{path: path})
	if err := s.Save(g); err != nil {
		t.Fatal(err)
	}
	loaded, err := newGoalStore(&fileStore{path: path}).LoadAll()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("暂停目标必须保留在文件: %v %v", loaded, err)
	}
	got := taskView(loaded[0].Snapshot())
	if got.Status != StatusPaused || got.TodoID != "todo" || got.NotificationParentID != "parent" {
		t.Fatalf("暂停元数据丢失: %+v", got)
	}
	legacy := NewEngineFile(nil, path)
	defer legacy.Close()
	if got, ok := legacy.Get("paused"); !ok || got.Status != StatusPaused {
		t.Fatalf("旧兼容入口未保留暂停目标: %+v %v", got, ok)
	}
}

func TestTaskManagementWriteFailureRollsBackAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	e := NewReliableEngineFile(nil, path)
	defer e.Close()
	old, err := e.Schedule(Task{ID: "keep", Text: "保留", Kind: KindRepeat, Interval: time.Hour, Times: 3})
	if err != nil {
		t.Fatal(err)
	}
	backup := path + ".saved"
	block := func() {
		t.Helper()
		if err := os.Rename(path, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	restore := func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, path); err != nil {
			t.Fatal(err)
		}
	}
	block()
	if _, err := e.Update(old.ID, Task{Text: "不保留", Kind: KindCron, Cron: "0 8 * * *"}); err == nil {
		t.Fatal("编辑落盘失败应报错")
	}
	if _, err := e.Pause(old.ID); err == nil {
		t.Fatal("暂停落盘失败应报错")
	}
	got, _ := e.Get(old.ID)
	if !equalTaskViews(got, old) {
		t.Fatalf("失败编辑污染内存: %+v", got)
	}
	restore()
	paused, err := e.Pause(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	block()
	if _, err := e.Resume(old.ID); err == nil {
		t.Fatal("恢复落盘失败应报错")
	}
	got, _ = e.Get(old.ID)
	if !equalTaskViews(got, paused) {
		t.Fatalf("失败恢复污染内存: %+v", got)
	}
	restore()
	e.Close()
	r := NewReliableEngineFile(nil, path)
	defer r.Close()
	got, _ = r.Get(old.ID)
	if !equalTaskViews(got, paused) {
		t.Fatalf("重启未保留失败前状态: %+v", got)
	}
	if _, err := r.Resume(old.ID); err != nil {
		t.Fatal(err)
	}
}

func TestEditThenPauseCancelNeverAcceptsObsoletePlan(t *testing.T) {
	accepted := make(chan Task, 4)
	e := NewReliableEngineFile(func(_ context.Context, tk Task) error { accepted <- tk; return nil }, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	when := time.Now().Add(time.Hour)
	old, err := e.Schedule(Task{ID: "edit", Kind: KindOnce, When: when, Text: "旧"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := e.Update(old.ID, Task{Kind: KindOnce, When: when.Add(time.Hour), Text: "新"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Next.Equal(old.Next) {
		t.Fatal("改时间必须建立新的下一计划键")
	}
	e.reliable.tick(old.Next)
	select {
	case got := <-accepted:
		t.Fatalf("旧计划不应触发: %+v", got)
	default:
	}
	if _, err := e.Pause(old.ID); err != nil {
		t.Fatal(err)
	}
	e.reliable.tick(updated.Next)
	select {
	case got := <-accepted:
		t.Fatalf("暂停不应触发: %+v", got)
	default:
	}
	if ok, err := e.CancelErr(old.ID); !ok || err != nil {
		t.Fatalf("取消失败: %v %v", ok, err)
	}
	e.reliable.tick(updated.Next.Add(time.Hour))
	select {
	case got := <-accepted:
		t.Fatalf("取消后不应触发: %+v", got)
	default:
	}
}

func TestTaskStatusCompatibilityAndClosedMutationErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(`[{"id":"legacy","kind":"cron","cron":"0 8 * * *"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewReliableEngineFile(nil, path)
	got, ok := e.Get("legacy")
	if !ok || got.Status != StatusActive {
		t.Fatalf("旧空状态应启用: %+v", got)
	}
	if _, err := e.Schedule(Task{Kind: KindOnce, When: time.Now().Add(time.Hour), Status: "bad"}); err == nil {
		t.Fatal("非法状态应拒绝")
	}
	e.Close()
	for _, fn := range []func() (Task, error){func() (Task, error) { return e.Pause(got.ID) }, func() (Task, error) { return e.Resume(got.ID) }, func() (Task, error) { return e.Update(got.ID, got) }} {
		if _, err := fn(); err != ErrClosed {
			t.Fatalf("关闭后应 ErrClosed: %v", err)
		}
	}
}
