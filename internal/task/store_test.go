package task

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/autonomous"
)

func equalTaskViews(a, b Task) bool {
	if !a.When.Equal(b.When) || !a.Next.Equal(b.Next) || !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	a.When, a.Next, a.CreatedAt = b.When, b.Next, b.CreatedAt
	return reflect.DeepEqual(a, b)
}

func savedTestGoal(t *testing.T, id string) *autonomous.Goal {
	t.Helper()
	g, err := buildGoal(Task{ID: id, Text: "保留", Kind: KindRepeat, Interval: time.Hour, Times: 3, Remaining: 3, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	g.SetNextRunAt(time.Now().Add(time.Hour))
	return g
}

func TestGoalStoreSaveMissingDirectoryRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "tasks.json")
	s := newGoalStore(&fileStore{path: path})
	g := savedTestGoal(t, "new")
	if err := s.Save(g); err == nil {
		t.Fatal("父目录缺失必须返回写入错误")
	}
	if _, err := s.Get(g.ID); err == nil {
		t.Fatal("失败注册不得出现在缓存中")
	}
	if goals, err := s.LoadAll(); err != nil || len(goals) != 0 {
		t.Fatalf("失败注册不得出现在列表: goals=%v err=%v", goals, err)
	}
	if err := os.Mkdir(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(g); err != nil {
		t.Fatalf("修复目录后重试应成功: %v", err)
	}
	goals, err := newGoalStore(&fileStore{path: path}).LoadAll()
	if err != nil || len(goals) != 1 || goals[0].ID != g.ID {
		t.Fatalf("重启应恢复重试成功的目标: goals=%v err=%v", goals, err)
	}
}

func TestEngineSchedulePersistenceFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "tasks.json")
	eng := NewEngineFile(nil, path)
	defer eng.Close()
	request := Task{ID: "retry", Text: "可靠注册", Kind: KindOnce, When: time.Now().Add(time.Hour)}
	if _, err := eng.Schedule(request); err == nil {
		t.Fatal("Schedule 必须返回真实落盘错误")
	}
	if got := eng.List(); len(got) != 0 {
		t.Fatalf("失败任务不能进入调度器: %+v", got)
	}
	if err := os.Mkdir(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Schedule(request); err != nil {
		t.Fatalf("失败注册不得残留 ID，修复目录后同 ID 应能重试: %v", err)
	}
	eng.Close()
	restarted := NewEngineFile(nil, path)
	defer restarted.Close()
	if got := restarted.List(); len(got) != 1 || got[0].ID != request.ID {
		t.Fatalf("重启应仅恢复成功注册: %+v", got)
	}
}

func TestEngineCancelPersistenceFailurePreservesTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	eng := NewEngineFile(nil, path)
	defer eng.Close()
	request := Task{ID: "keep", Text: "取消失败保留", Kind: KindOnce, When: time.Now().Add(time.Hour)}
	if _, err := eng.Schedule(request); err != nil {
		t.Fatal(err)
	}
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if eng.Cancel(request.ID) {
		t.Fatal("取消落盘失败不能返回成功")
	}
	if got := eng.List(); len(got) != 1 || got[0].ID != request.ID {
		t.Fatalf("取消失败必须保留调度中的任务: %+v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	eng.Close()
	restarted := NewEngineFile(nil, path)
	defer restarted.Close()
	if got := restarted.List(); len(got) != 1 || got[0].ID != request.ID {
		t.Fatalf("取消失败后重启必须恢复原任务: %+v", got)
	}
	if !restarted.Cancel(request.ID) {
		t.Fatal("写入恢复后应能取消")
	}
	if tasks, err := (&fileStore{path: path}).load(); err != nil || len(tasks) != 0 {
		t.Fatalf("成功取消必须落盘: tasks=%v err=%v", tasks, err)
	}
}

func TestGoalStoreWritePermissionFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	s := newGoalStore(&fileStore{path: path})
	if _, err := s.LoadAll(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	probe, err := os.CreateTemp(dir, "permission-probe-*")
	if err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("当前用户可绕过目录写入权限")
	}
	if err := s.Save(savedTestGoal(t, "new")); err == nil {
		t.Fatal("目录不可写必须返回持久化错误")
	}
	if goals, err := s.LoadAll(); err != nil || len(goals) != 0 {
		t.Fatalf("目录不可写时不能缓存未保存目标: goals=%v err=%v", goals, err)
	}
}

func TestGoalStoreRenameFailurePreservesCommittedSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	s := newGoalStore(&fileStore{path: path})
	g := savedTestGoal(t, "saved")
	if err := s.Save(g); err != nil {
		t.Fatal(err)
	}
	before := g.Snapshot()
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// 框架先就地 RecordRun，后 Save；共享指针会使保存失败前缓存已经被修改。
	g.RecordRun("未保存结果", nil)
	g.SetNextRunAt(time.Now().Add(2 * time.Hour))
	if err := s.Save(g); err == nil {
		t.Fatal("更新应报告 rename 失败")
	}
	got, err := s.Get(g.ID)
	if err != nil || !equalTaskViews(taskView(got.Snapshot()), taskView(before)) {
		t.Fatalf("更新失败后缓存应保持已提交状态: got=%+v err=%v", got, err)
	}
	g.SetStatus(autonomous.GoalStatusDone)
	if err := s.Save(g); err == nil {
		t.Fatal("终态移除必须报告 rename 失败")
	}
	if err := s.Delete(g.ID); err == nil {
		t.Fatal("删除必须报告 rename 失败")
	}
	if got, err := s.Get(g.ID); err != nil || got.GetStatus() != autonomous.GoalStatusActive {
		t.Fatalf("删除失败必须保留 active 目标: got=%+v err=%v", got, err)
	}
	if leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".tasks.json.tmp-*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("失败后应清理临时文件: %v %v", leftovers, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	goals, err := newGoalStore(&fileStore{path: path}).LoadAll()
	if err != nil || len(goals) != 1 || !equalTaskViews(taskView(goals[0].Snapshot()), taskView(before)) {
		t.Fatalf("重启应恢复失败前状态: goals=%v err=%v", goals, err)
	}
	if err := s.Delete(g.ID); err != nil {
		t.Fatalf("恢复写入后应能删除: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// 删除是幂等的：框架第二次 Delete 不应再次尝试落盘。
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(g.ID); err != nil {
		t.Fatalf("重复删除必须幂等: %v", err)
	}
}

func TestGoalStoreGetAndLoadAllReturnIndependentSnapshots(t *testing.T) {
	s := newGoalStore(nil)
	g := savedTestGoal(t, "saved")
	if err := s.Save(g); err != nil {
		t.Fatal(err)
	}
	g.RecordRun("scheduler advance", nil)
	fromGet, err := s.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fromGet.Snapshot().RunCount != 0 {
		t.Fatal("缓存不得共享框架正在修改的目标")
	}
	fromGet.SetStatus(autonomous.GoalStatusDone)
	fromGet.SetMeta("test", "changed")
	fromLoad, err := s.LoadAll()
	if err != nil || len(fromLoad) != 1 || fromLoad[0].GetStatus() != autonomous.GoalStatusActive || fromLoad[0].GetMeta("test") != "" {
		t.Fatalf("Get 返回值不得污染缓存: goals=%v err=%v", fromLoad, err)
	}
	fromLoad[0].RecordRun("copy advance", nil)
	again, err := s.Get(g.ID)
	if err != nil || again.Snapshot().RunCount != 0 {
		t.Fatalf("LoadAll 返回值不得污染缓存: got=%+v err=%v", again, err)
	}
}

func TestGoalStoreCorruptFilesPreserved(t *testing.T) {
	for _, raw := range []string{
		"broken", "null", `[{"id":"missing-when","kind":"once"}]`,
		`[{"id":"bad-cron","kind":"cron","cron":"not a cron"}]`,
		`[{"id":"duplicate","kind":"repeat","interval":"1h","times":3,"remaining":3},{"id":"duplicate","kind":"repeat","interval":"1h","times":3,"remaining":3}]`,
	} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tasks.json")
			if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			s := newGoalStore(&fileStore{path: path})
			if _, err := s.LoadAll(); err == nil || s.Err() == nil {
				t.Fatal("损坏文件必须返回明确错误")
			}
			if err := s.Save(savedTestGoal(t, "new")); err == nil {
				t.Fatal("损坏文件必须阻止保存")
			}
			if err := s.Delete("duplicate"); err == nil {
				t.Fatal("损坏文件必须阻止删除")
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != raw {
				t.Fatalf("原文件必须保留: bytes=%q err=%v", b, err)
			}
		})
	}
}

func TestGoalStoreUnreadableFilePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	s := newGoalStore(&fileStore{path: path})
	if _, err := s.LoadAll(); err == nil {
		t.Fatal("读取目录失败不得忽略")
	}
	if err := s.Save(savedTestGoal(t, "new")); err == nil {
		t.Fatal("读取失败必须阻止覆盖")
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("原目录必须保留: info=%v err=%v", info, err)
	}
}

func TestGoalStoreReadPermissionFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	const original = `[{"id":"saved","kind":"repeat","interval":"1h","times":3,"remaining":3}]`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("当前用户可绕过文件权限")
	}
	s := newGoalStore(&fileStore{path: path})
	if _, err := s.LoadAll(); err == nil {
		t.Fatal("读取权限错误不得忽略")
	}
	if err := s.Save(savedTestGoal(t, "new")); err == nil {
		t.Fatal("读取权限错误必须阻止覆盖")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != original {
		t.Fatalf("原内容应保留: %q %v", b, err)
	}
}

func TestGoalStoreRestoredOnceSurvivesAnotherSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	when := time.Now().Add(time.Hour)
	f := &fileStore{path: path}
	// 旧文件无 Next；缓存中的首次执行时间须回退到持久化的 When。
	if err := f.save([]Task{{ID: "once", Text: "旧单次", Kind: KindOnce, When: when}}); err != nil {
		t.Fatal(err)
	}
	s := newGoalStore(f)
	if _, err := s.LoadAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(savedTestGoal(t, "other")); err != nil {
		t.Fatal(err)
	}
	tasks, err := f.load()
	if err != nil || len(tasks) != 2 {
		t.Fatalf("保存其它目标不能损坏恢复的单次目标: tasks=%v err=%v", tasks, err)
	}
	for _, tk := range tasks {
		if tk.ID == "once" && !tk.When.Equal(when) {
			t.Fatalf("旧单次计划时刻丢失: got=%v want=%v", tk.When, when)
		}
	}
}
