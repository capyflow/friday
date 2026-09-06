package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustAdd(t *testing.T, s *Store, content, source string) Memory {
	t.Helper()
	m, err := s.Add(content, source)
	if err != nil {
		t.Fatalf("Add(%q): %v", content, err)
	}
	return m
}

// TestAddListSearchOrder 新增/列表/搜索，列表按创建时间倒序。
func TestAddListSearchOrder(t *testing.T) {
	s := NewStore("")
	mustAdd(t, s, "用户喜欢散步", "user")
	time.Sleep(2 * time.Millisecond)
	mustAdd(t, s, "用户对花粉过敏", "agent")
	time.Sleep(2 * time.Millisecond)
	mustAdd(t, s, "女儿叫小红", "user")

	all := s.List()
	if len(all) != 3 || all[0].Content != "女儿叫小红" {
		t.Fatalf("List 应按时间倒序，got %+v", all)
	}
	if all[0].ID == "" || all[0].ID[0] != 'm' {
		t.Fatalf("ID 应以 m 开头，got %q", all[0].ID)
	}

	got := s.Search("喜欢")
	if len(got) != 1 || got[0].Content != "用户喜欢散步" {
		t.Fatalf("Search 应命中 1 条，got %+v", got)
	}
	if n := len(s.Search("不存在的关键词")); n != 0 {
		t.Fatalf("无关关键词应命中 0 条，got %d", n)
	}
	if n := s.Count(); n != 3 {
		t.Fatalf("Count 应为 3，got %d", n)
	}
}

// TestAddEmpty 空内容应报错。
func TestAddEmpty(t *testing.T) {
	s := NewStore("")
	if _, err := s.Add("   ", "user"); err == nil {
		t.Fatal("空内容应报错")
	}
	if s.Count() != 0 {
		t.Fatalf("空内容不应入库，got %d", s.Count())
	}
}

// TestDelete 删除一条记忆。
func TestDelete(t *testing.T) {
	s := NewStore("")
	m := mustAdd(t, s, "用户怕打雷", "user")
	if !s.Delete(m.ID) {
		t.Fatal("Delete 应返回 true")
	}
	if s.Delete(m.ID) {
		t.Fatal("重复 Delete 应返回 false")
	}
	if s.Count() != 0 {
		t.Fatalf("删除后应为 0，got %d", s.Count())
	}
}

// TestPersistRoundTrip 落盘 → 重启恢复；删除也同步落盘。
func TestPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	s1 := NewStore(path)
	m1 := mustAdd(t, s1, "用户喜欢番茄", "user")
	m2 := mustAdd(t, s1, "每周三要开会", "agent")
	if !s1.Delete(m2.ID) {
		t.Fatal("Delete 应返回 true")
	}

	s2 := NewStore(path)
	all := s2.List()
	if len(all) != 1 {
		t.Fatalf("重启后应恢复 1 条，实际 %d", len(all))
	}
	got := all[0]
	if got.ID != m1.ID || got.Content != m1.Content || got.Source != m1.Source || !got.CreatedAt.Equal(m1.CreatedAt) {
		t.Fatalf("恢复的记忆不符，got %+v want %+v", got, m1)
	}
}

// TestCorruptFile 损坏文件应空启动不崩溃。
func TestCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	if err := os.WriteFile(path, []byte("{{{不是 JSON"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	s := NewStore(path)
	if s.Count() != 0 {
		t.Fatalf("损坏文件应空启动，实际 %d 条", s.Count())
	}
	mustAdd(t, s, "恢复后可正常写入", "user")
	if !strings.Contains(readFile(t, path), "恢复后可正常写入") {
		t.Fatal("新增后文件应包含新记忆")
	}
}

// TestRecent 最近 N 条。
func TestRecent(t *testing.T) {
	s := NewStore("")
	for i := 0; i < 5; i++ {
		mustAdd(t, s, "记忆"+string(rune('A'+i)), "user")
		time.Sleep(time.Millisecond)
	}
	got := s.Recent(3)
	if len(got) != 3 || got[0].Content != "记忆E" {
		t.Fatalf("Recent 应返回最新 3 条，got %+v", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	return string(b)
}
