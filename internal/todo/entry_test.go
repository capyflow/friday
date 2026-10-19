package todo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLegacyTodos(t *testing.T, path string, ts []Todo) {
	t.Helper()
	b, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		t.Fatalf("编码旧版待办失败: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("写入旧版待办失败: %v", err)
	}
}

func TestParseTags(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"无标签内容", nil},
		{"#工作 周报思路", []string{"工作"}},
		{"想法 #a/b 和 #c_1 以及重复 #a/b", []string{"a/b", "c_1"}},
		{"中文#标签 开头无空格", []string{"标签"}},
		{"井号# 后无内容 #", nil},
	}
	for _, c := range cases {
		if got := parseTags(c.text); len(got) != 0 || len(c.want) != 0 {
			if !equalStrings(got, c.want) {
				t.Errorf("parseTags(%q) = %v, 期望 %v", c.text, got, c.want)
			}
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLegacyTodosMigrateToKindTodo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	writeLegacyTodos(t, path, []Todo{{ID: "t01", Text: "买牛奶", CreatedAt: time.Now()}})
	s := NewStore(path)
	got := s.List()
	if len(got) != 1 || got[0].Kind != KindTodo {
		t.Fatalf("旧数据应迁移为 todo: %+v", got)
	}
	// 下次写盘把 kind 落盘。
	if _, err := s.Add("交房租"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取落盘文件: %v", err)
	}
	var ts []Todo
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatalf("解析落盘文件: %v", err)
	}
	for _, td := range ts {
		if td.Kind != KindTodo {
			t.Fatalf("落盘后 kind 应为 todo: %+v", td)
		}
	}
}

func TestAddEntryNoteAndTodo(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "todos.json"))
	note, err := s.AddEntry(KindNote, "#灵感 闪念想法", nil, nil)
	if err != nil {
		t.Fatalf("AddEntry note: %v", err)
	}
	if note.ID[0] != 'n' {
		t.Fatalf("笔记 ID 应为 n 前缀: %q", note.ID)
	}
	if !equalStrings(note.Tags, []string{"灵感"}) {
		t.Fatalf("笔记标签应自动派生: %v", note.Tags)
	}
	// 笔记忽略时间参数。
	when := time.Now().Add(time.Hour)
	note2, err := s.AddEntry(KindNote, "另一条", &when, &when)
	if err != nil {
		t.Fatalf("AddEntry note2: %v", err)
	}
	if note2.DueAt != nil || note2.RemindAt != nil || note2.Done {
		t.Fatalf("笔记不应带时间或完成状态: %+v", note2)
	}
	// 笔记没有完成状态。
	if _, _, err := s.Complete(note.ID, true); err == nil {
		t.Fatal("笔记完成应报错")
	}
	// 待办不受影响，且 List 不含笔记。
	td, err := s.Add("买牛奶")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if td.ID[0] != 't' {
		t.Fatalf("待办 ID 应为 t 前缀: %q", td.ID)
	}
	if got := s.List(); len(got) != 1 || got[0].Text != "买牛奶" {
		t.Fatalf("List 应只含待办: %+v", got)
	}
	if s.CountPending() != 1 {
		t.Fatalf("CountPending 应不含笔记: %d", s.CountPending())
	}
	if _, err := s.AddEntry("book", "未知类型", nil, nil); err == nil {
		t.Fatal("未知 kind 应报错")
	}
}

func TestUpdateEntryNoteIgnoresTimes(t *testing.T) {
	s := NewStore("")
	note, _ := s.AddEntry(KindNote, "原内容", nil, nil)
	when := time.Now().Add(time.Hour)
	got, ok, err := s.UpdateEntry(note.ID, "新内容 #工作", &when, &when)
	if err != nil || !ok || got.Text != "新内容 #工作" {
		t.Fatalf("UpdateEntry note: %+v ok=%v err=%v", got, ok, err)
	}
	if got.DueAt != nil || got.RemindAt != nil {
		t.Fatalf("笔记更新后不应带时间: %+v", got)
	}
	if !equalStrings(got.Tags, []string{"工作"}) {
		t.Fatalf("标签应随文本刷新: %v", got.Tags)
	}
}

func TestSetPinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	s := NewStore(path)
	note, _ := s.AddEntry(KindNote, "笔记", nil, nil)
	td, _ := s.Add("待办")
	if got, ok, err := s.SetPinned(note.ID, true); err != nil || !ok || !got.Pinned {
		t.Fatalf("SetPinned: %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, err := s.SetPinned(td.ID, true); err != nil || !ok {
		t.Fatalf("待办置顶: ok=%v err=%v", ok, err)
	}
	// 重启后保留置顶。
	s2 := NewStore(path)
	got := s2.ListEntries(EntryFilter{})
	if len(got) != 2 || !got[0].Pinned || !got[1].Pinned {
		t.Fatalf("重启后置顶应保留: %+v", got)
	}
	// 置顶之内仍按创建时间倒序：后创建的待办在前。
	if got[0].ID != td.ID || got[1].ID != note.ID {
		t.Fatalf("置顶内创建时间倒序: %+v", got)
	}
	if _, ok, _ := s2.SetPinned("n404", true); ok {
		t.Fatal("未找到的条目应返回 false")
	}
}

func TestListEntriesFilterAndSort(t *testing.T) {
	s := NewStore("")
	n1, _ := s.AddEntry(KindNote, "#工作 会议纪要", nil, nil)
	t1, _ := s.Add("买牛奶 #生活")
	time.Sleep(2 * time.Millisecond)
	n2, _ := s.AddEntry(KindNote, "#灵感 突然的想法", nil, nil)
	t2, _ := s.Add("还书")
	if _, _, err := s.Complete(t2.ID, true); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, _, err := s.SetPinned(n1.ID, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	all := s.ListEntries(EntryFilter{})
	if len(all) != 4 || all[0].ID != n1.ID {
		t.Fatalf("置顶应在前，其余创建时间倒序: %+v", ids(all))
	}
	// 其余按创建时间倒序：t2 最新，然后 n2、t1。
	if all[1].ID != t2.ID || all[2].ID != n2.ID || all[3].ID != t1.ID {
		t.Fatalf("时间线顺序不对: %+v", ids(all))
	}

	notes := s.ListEntries(EntryFilter{Kind: KindNote})
	if len(notes) != 2 || notes[0].ID != n1.ID {
		t.Fatalf("笔记过滤: %+v", ids(notes))
	}
	done := true
	todosOpen := s.ListEntries(EntryFilter{Kind: KindTodo, Done: &done})
	if len(todosOpen) != 1 || todosOpen[0].ID != t2.ID {
		t.Fatalf("已完成待办过滤: %+v", ids(todosOpen))
	}
	life := s.ListEntries(EntryFilter{Tag: "生活"})
	if len(life) != 1 || life[0].ID != t1.ID {
		t.Fatalf("标签过滤: %+v", ids(life))
	}
	milk := s.ListEntries(EntryFilter{Q: "牛奶"})
	if len(milk) != 1 || milk[0].ID != t1.ID {
		t.Fatalf("关键词过滤: %+v", ids(milk))
	}
	if tags := s.Tags(); !equalStrings(tags, []string{"工作", "灵感", "生活"}) {
		t.Fatalf("标签集合: %v", tags)
	}
}

func ids(in []Todo) []string {
	out := make([]string, len(in))
	for i, td := range in {
		out[i] = td.ID
	}
	return out
}
