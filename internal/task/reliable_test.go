package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReliableAcceptFailurePreservesOnceUntilRecovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	acceptErr := errors.New("通知队列不可写")
	var accepted []Task
	engine := NewReliableEngineFile(func(_ context.Context, tk Task) error { accepted = append(accepted, tk); return acceptErr }, path)
	defer engine.Close()
	when := time.Now().Add(time.Hour)
	original, err := engine.Schedule(Task{ID: "once", Kind: KindOnce, Text: "提醒", When: when})
	if err != nil {
		t.Fatal(err)
	}
	engine.reliable.tick(when)
	if list := engine.List(); len(list) != 1 || !list[0].Next.Equal(original.Next) {
		t.Fatalf("接受失败消费了单次任务: %+v", list)
	}
	stored, err := (&fileStore{path: path}).load()
	if err != nil || len(stored) != 1 || !stored[0].Next.Equal(original.Next) {
		t.Fatalf("失败任务没有留在磁盘: %+v, %v", stored, err)
	}
	acceptErr = nil
	engine.reliable.tick(when)
	if engine.Len() != 0 || len(accepted) != 2 || !accepted[0].Next.Equal(accepted[1].Next) {
		t.Fatalf("恢复后未可靠接收原触发: tasks=%+v accepted=%+v", engine.List(), accepted)
	}
	restored := NewReliableEngineFile(nil, path)
	defer restored.Close()
	if restored.Err() != nil || restored.Len() != 0 {
		t.Fatalf("重启后单次任务仍存在: %+v, %v", restored.List(), restored.Err())
	}
}

func TestReliableRepeatAndCronNextPersistAcrossRestart(t *testing.T) {
	for _, input := range []Task{
		{ID: "repeat", Kind: KindRepeat, Interval: time.Hour, Times: 3, Text: "多次"},
		{ID: "cron", Kind: KindCron, Cron: "0 8 * * *", Text: "循环"},
	} {
		t.Run(string(input.Kind), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tasks.json")
			var accepted []Task
			engine := NewReliableEngineFile(func(_ context.Context, tk Task) error { accepted = append(accepted, tk); return nil }, path)
			original, err := engine.Schedule(input)
			if err != nil {
				engine.Close()
				t.Fatal(err)
			}
			now := original.Next.Add(10 * time.Minute)
			engine.reliable.tick(now)
			list := engine.List()
			if len(list) != 1 || len(accepted) != 1 || !accepted[0].Next.Equal(original.Next) || !list[0].Next.After(now) {
				engine.Close()
				t.Fatalf("周期推进错误: tasks=%+v accepted=%+v", list, accepted)
			}
			want := list[0]
			if input.Kind == KindRepeat && (want.Remaining != 2 || !want.Next.Equal(original.Next.Add(time.Hour))) {
				engine.Close()
				t.Fatalf("repeat 次数或锚点漂移: %+v", want)
			}
			engine.Close()
			restored := NewReliableEngineFile(nil, path)
			defer restored.Close()
			got := restored.List()
			if restored.Err() != nil || len(got) != 1 || got[0].Remaining != want.Remaining || !got[0].Next.Equal(want.Next) {
				t.Fatalf("重启改变了周期计划: %+v, %v，want %+v", got, restored.Err(), want)
			}
		})
	}
}

func TestReliableScheduleAndCancelSaveFailureRollBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	engine := NewReliableEngineFile(nil, path)
	defer engine.Close()
	original, err := engine.Schedule(Task{ID: "keep", Kind: KindOnce, When: time.Now().Add(time.Hour), Text: "保留"})
	if err != nil {
		t.Fatal(err)
	}
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, err := engine.CancelErr(original.ID); err == nil || ok {
		t.Fatalf("取消写盘失败必须报错: %v, %v", ok, err)
	}
	if _, err := engine.Schedule(Task{ID: "new", Kind: KindOnce, When: time.Now().Add(2 * time.Hour)}); err == nil {
		t.Fatal("注册写盘失败必须报错")
	}
	if list := engine.List(); len(list) != 1 || list[0].ID != original.ID {
		t.Fatalf("保存失败改变了内存任务: %+v", list)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	stored, err := (&fileStore{path: path}).load()
	if err != nil || len(stored) != 1 || stored[0].ID != original.ID {
		t.Fatalf("保存失败改变了磁盘任务: %+v, %v", stored, err)
	}
	if ok, err := engine.CancelErr(original.ID); !ok || err != nil {
		t.Fatalf("修复写入后取消失败: %v, %v", ok, err)
	}
	if engine.Len() != 0 {
		t.Fatal("成功取消后仍有任务")
	}
}

func TestReliableCorruptFileRejectsMutationsAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	raw := []byte("{broken")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	engine := NewReliableEngineFile(nil, path)
	if engine.Err() == nil {
		t.Fatal("损坏文件必须返回启动错误")
	}
	if _, err := engine.Schedule(Task{When: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("不能注册后覆盖损坏文件")
	}
	if _, err := engine.CancelErr("missing"); err == nil {
		t.Fatal("损坏文件时不能把取消当作成功")
	}
	engine.Close()
	engine.Close()
	if got, err := os.ReadFile(path); err != nil || string(got) != string(raw) {
		t.Fatalf("损坏原文件被改变: %q, %v", got, err)
	}
}

func TestReliableClosedEngineRejectsScheduleAndCancel(t *testing.T) {
	engine := NewReliableEngineFile(nil, filepath.Join(t.TempDir(), "tasks.json"))
	engine.Close()
	engine.Close()
	if _, err := engine.Schedule(Task{When: time.Now().Add(time.Hour)}); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后注册 = %v，want ErrClosed", err)
	}
	if _, err := engine.CancelErr("missing"); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后取消 = %v，want ErrClosed", err)
	}
}
