package task

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const legacyRepeatFile = `[{"id":"legacy","text":"旧周期提醒","kind":"repeat","interval":"1h","times":3,"remaining":3}]`

func TestLegacyNextMigrationPrecedesAcceptAndSurvivesAdvanceFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(legacyRepeatFile), 0o600); err != nil {
		t.Fatal(err)
	}
	var accepted []Task
	backup := path + ".saved"
	first := NewReliableEngineFile(func(_ context.Context, tk Task) error {
		accepted = append(accepted, tk)
		// 可靠接收完成之后，只阻止随后任务次数推进写盘。
		if err := os.Rename(path, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return nil
	}, path)
	defer first.Close()
	if err := first.Err(); err != nil {
		t.Fatal(err)
	}
	migrated, err := (&fileStore{path: path}).load()
	if err != nil || len(migrated) != 1 || migrated[0].Next.IsZero() {
		t.Fatalf("启动调度前须先持久化迁移 Next: tasks=%+v err=%v", migrated, err)
	}
	when := migrated[0].Next
	first.reliable.tick(when)
	if got := first.List(); len(got) != 1 || !got[0].Next.Equal(when) || got[0].Remaining != 3 {
		t.Fatalf("入队后推进失败必须保留同次触发: %+v", got)
	}
	first.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	second := NewReliableEngineFile(func(_ context.Context, tk Task) error {
		accepted = append(accepted, tk)
		return nil
	}, path)
	defer second.Close()
	if err := second.Err(); err != nil {
		t.Fatal(err)
	}
	if got := second.List(); len(got) != 1 || !got[0].Next.Equal(when) || got[0].Remaining != 3 {
		t.Fatalf("重启不得给待重试的同次触发生成新 Next: %+v", got)
	}
	second.reliable.tick(when)
	if len(accepted) != 2 || !accepted[0].Next.Equal(accepted[1].Next) || accepted[0].Remaining != accepted[1].Remaining {
		t.Fatalf("同次重试必须复用稳定去重键: %+v", accepted)
	}
	if got := second.List(); len(got) != 1 || got[0].Remaining != 2 || !got[0].Next.Equal(when.Add(time.Hour)) {
		t.Fatalf("可靠重试成功后只推进一次: %+v", got)
	}
}

func TestLegacyNextMigrationWriteFailurePreventsAcceptAndPreservesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(path, []byte(legacyRepeatFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if probe, err := os.CreateTemp(dir, "permission-probe-*"); err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("当前用户可绕过目录写入权限")
	}
	accepted := make(chan Task, 1)
	engine := NewReliableEngineFile(func(_ context.Context, tk Task) error { accepted <- tk; return nil }, path)
	defer engine.Close()
	if engine.Err() == nil {
		t.Fatal("首次 Next 迁移写失败必须阻止构造启动")
	}
	engine.Close()
	select {
	case tk := <-accepted:
		t.Fatalf("迁移失败不得接受任何任务: %+v", tk)
	default:
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != legacyRepeatFile {
		t.Fatalf("迁移失败必须保留原文件: bytes=%q err=%v", raw, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 修复权限后重启才恢复迁移与调度。
	restored := NewReliableEngineFile(nil, path)
	defer restored.Close()
	if restored.Err() != nil {
		t.Fatal(restored.Err())
	}
	if stored, err := (&fileStore{path: path}).load(); err != nil || len(stored) != 1 || stored[0].Next.IsZero() {
		t.Fatalf("写入恢复后须完成迁移: tasks=%+v err=%v", stored, err)
	}
}
