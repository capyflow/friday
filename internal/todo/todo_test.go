package todo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func equalTodos(a, b []Todo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].CreatedAt.Equal(b[i].CreatedAt) || (a[i].DoneAt == nil) != (b[i].DoneAt == nil) {
			return false
		}
		if a[i].DoneAt != nil && !a[i].DoneAt.Equal(*b[i].DoneAt) {
			return false
		}
		a[i].CreatedAt = b[i].CreatedAt
		a[i].DoneAt = b[i].DoneAt
	}
	return reflect.DeepEqual(a, b)
}

func TestStoreAddAndList(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "todos.json"))
	if _, err := s.Add("交房租"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.Add(""); err == nil {
		t.Fatal("空内容应当报错")
	}
	if _, err := s.Add("买牛奶"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("期望 2 条待办，实际 %d", len(got))
	}
	if got[0].Text != "交房租" {
		t.Fatalf("未完成待办应按创建时间正序，实际第一条 %q", got[0].Text)
	}
	if s.CountPending() != 2 {
		t.Fatalf("期望 2 条未完成，实际 %d", s.CountPending())
	}
}

func TestStoreCompleteAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	s := NewStore(path)
	td, _ := s.Add("还书")
	// ID 容忍 "#t..." 形式（工具展示带 # 前缀，模型原样传回）。
	got, ok, err := s.Complete("#"+td.ID, true)
	if err != nil || !ok || !got.Done || got.DoneAt == nil {
		t.Fatalf("Complete 失败: %+v ok=%v err=%v", got, ok, err)
	}
	// 重新加载验证落盘。
	s2 := NewStore(path)
	list := s2.List()
	if len(list) != 1 || !list[0].Done {
		t.Fatalf("重启后应保留已完成状态: %+v", list)
	}
	// 取消完成。
	if got, ok, err := s2.Complete(td.ID, false); err != nil || !ok || got.Done || got.DoneAt != nil {
		t.Fatalf("取消完成失败: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestStoreDelete(t *testing.T) {
	s := NewStore("")
	td, _ := s.Add("取快递")
	if deleted, err := s.Delete(td.ID); err != nil || !deleted {
		t.Fatalf("Delete 应成功: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := s.Delete(td.ID); err != nil || deleted {
		t.Fatalf("重复删除应返回 false: deleted=%v err=%v", deleted, err)
	}
	if len(s.List()) != 0 {
		t.Fatal("删除后列表应为空")
	}
}

func TestStoreCorruptedFilePreserved(t *testing.T) {
	for _, raw := range []string{"{broken", "null", `[{"id":"","text":"missing id"}]`, `[{"id":"a","text":"one"},{"id":"a","text":"two"}]`} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "todos.json")
			if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			s := NewStore(path)
			if s.Err() == nil {
				t.Fatal("损坏文件必须暴露恢复错误")
			}
			if _, err := s.Add("新待办"); err == nil {
				t.Fatal("损坏文件必须阻止 Add")
			}
			if _, _, err := s.Complete("a", true); err == nil {
				t.Fatal("损坏文件必须阻止 Complete")
			}
			if _, err := s.Delete("a"); err == nil {
				t.Fatal("损坏文件必须阻止 Delete")
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != raw {
				t.Fatalf("必须保留原始文件: err=%v bytes=%q", err, b)
			}
		})
	}
}

func TestStoreAddMissingDirectoryRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "todos.json")
	s := NewStore(path)
	if _, err := s.Add("买牛奶"); err == nil {
		t.Fatal("父目录缺失必须返回写入错误")
	}
	if len(s.List()) != 0 || NewStore(path).CountPending() != 0 {
		t.Fatal("落盘失败不得保留待办或报告重启可恢复")
	}
	if err := os.Mkdir(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("重试"); err != nil {
		t.Fatalf("修复目录后应能重试: %v", err)
	}
	if got := NewStore(path).List(); len(got) != 1 || got[0].Text != "重试" {
		t.Fatalf("重启应只恢复重试成功的待办: %+v", got)
	}
}

func TestStoreWritePermissionFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "todos.json"))
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
	if _, err := s.Add("不能保存"); err == nil {
		t.Fatal("目录不可写必须返回持久化错误")
	}
	if len(s.List()) != 0 {
		t.Fatal("写入失败必须保持空仓库")
	}
}

func TestStoreMutationsRenameFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	s := NewStore(path)
	td, err := s.Add("保留")
	if err != nil {
		t.Fatal(err)
	}
	before := s.List()
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	// 让写临时文件成功而替换目标失败，验证提交只能发生在 rename 成功后。
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("不应保留"); err == nil {
		t.Fatal("Add 应报告 rename 失败")
	}
	if _, ok, err := s.Complete(td.ID, true); err == nil || ok {
		t.Fatalf("Complete 应报告失败: ok=%v err=%v", ok, err)
	}
	if deleted, err := s.Delete(td.ID); err == nil || deleted {
		t.Fatalf("Delete 应报告失败: deleted=%v err=%v", deleted, err)
	}
	if got := s.List(); !equalTodos(got, before) {
		t.Fatalf("失败后内存必须保持原状态: got=%+v before=%+v", got, before)
	}
	if leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".todos.json.tmp-*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("失败后应清理临时文件: %v %v", leftovers, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(path).List(); !equalTodos(got, before) {
		t.Fatalf("重启必须恢复失败前状态: got=%+v before=%+v", got, before)
	}
	if _, _, err := s.Complete(td.ID, true); err != nil {
		t.Fatalf("写入恢复后应能完成: %v", err)
	}
	if deleted, err := s.Delete(td.ID); err != nil || !deleted {
		t.Fatalf("写入恢复后应能删除: deleted=%v err=%v", deleted, err)
	}
	if got := NewStore(path).List(); len(got) != 0 {
		t.Fatalf("重启后删除应持久生效: %+v", got)
	}
}

func TestStoreUnreadableFilePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if s.Err() == nil {
		t.Fatal("读取目录失败不能误判首次启动")
	}
	if _, err := s.Add("不能覆盖"); err == nil {
		t.Fatal("读取失败必须阻止覆盖")
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("原目录应保留: %v %v", info, err)
	}
}

func TestStoreReadPermissionFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	const original = `[{"id":"saved","text":"保留"}]`
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
	s := NewStore(path)
	if s.Err() == nil {
		t.Fatal("权限错误应保留为读取错误")
	}
	if _, err := s.Add("不能覆盖"); err == nil {
		t.Fatal("权限错误不得覆盖原文件")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != original {
		t.Fatalf("原内容应保留: %q %v", b, err)
	}
}
