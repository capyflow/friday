package todo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
	got, ok := s.Complete("#"+td.ID, true)
	if !ok || !got.Done || got.DoneAt == nil {
		t.Fatalf("Complete 失败: %+v ok=%v", got, ok)
	}
	// 重新加载验证落盘。
	s2 := NewStore(path)
	list := s2.List()
	if len(list) != 1 || !list[0].Done {
		t.Fatalf("重启后应保留已完成状态: %+v", list)
	}
	// 取消完成。
	if got, ok := s2.Complete(td.ID, false); !ok || got.Done || got.DoneAt != nil {
		t.Fatalf("取消完成失败: %+v ok=%v", got, ok)
	}
}

func TestStoreDelete(t *testing.T) {
	s := NewStore("")
	td, _ := s.Add("取快递")
	if !s.Delete(td.ID) {
		t.Fatal("Delete 应成功")
	}
	if s.Delete(td.ID) {
		t.Fatal("重复删除应返回 false")
	}
	if len(s.List()) != 0 {
		t.Fatal("删除后列表应为空")
	}
}

func TestStoreCorruptedFileIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	os.WriteFile(path, []byte("{broken"), 0o644)
	s := NewStore(path)
	if len(s.List()) != 0 {
		t.Fatal("损坏文件应空启动")
	}
	if _, err := s.Add("新待办"); err != nil {
		t.Fatalf("空启动后应能继续写入: %v", err)
	}
	b, _ := os.ReadFile(path)
	var ts []Todo
	if err := json.Unmarshal(b, &ts); err != nil || len(ts) != 1 {
		t.Fatalf("损坏后应能恢复写回: %v %s", err, b)
	}
}
