package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const morningSkill = `---
name: 晨会准备
description: 汇总今日待办与天气，产出晨会简报
allowed-tools: [list_todos, get_weather]
enabled: true
auto: true
triggers: ["晨会"]
keywords: ["简报", "站会"]
priority: 10
---
先说日期，再查天气，最后汇报未完成待办。
`

const weatherSkill = `---
name: 天气播报
description: 查天气并给出穿衣带伞建议
auto: false
---
按城市查天气。
`

// writeSkill 在 dir/<id>/SKILL.md 落一份技能文件。
func writeSkill(t *testing.T, dir, id, content string) string {
	t.Helper()
	path := filepath.Join(dir, id, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newStoreAt(t *testing.T, dir string) *Store {
	t.Helper()
	s := NewStore(dir, "")
	if err := s.Err(); err != nil {
		t.Fatalf("Store.Err: %v", err)
	}
	return s
}

// TestStoreScansSortsAndSummarizes 扫描、排序（项目优先 → priority 降序 → ID）与摘要。
func TestStoreScansSortsAndSummarizes(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "low-priority", strings.Replace(morningSkill, "priority: 10", "priority: 1", 1))
	writeSkill(t, dir, "weather", weatherSkill)

	s := newStoreAt(t, dir)
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("应扫到 2 个技能，实际 %d: %+v", len(list), list)
	}
	// 两个都是项目作用域，priority 10 的前置
	if list[0].ID != "low-priority" {
		t.Errorf("priority 更高的应排在前面，实际首位 %s", list[0].ID)
	}
	var morning *Summary
	for i := range list {
		if list[i].Name == "晨会准备" {
			morning = &list[i]
		}
	}
	if morning == nil {
		t.Fatalf("未找到晨会准备: %+v", list)
	}
	if !morning.Enabled || !morning.Auto || morning.Priority != 1 {
		t.Errorf("扩展键解析不符: %+v", morning)
	}
	if len(morning.AllowedTools) != 2 || morning.AllowedTools[0] != "list_todos" {
		t.Errorf("框架字段 allowed-tools 未读通: %+v", morning.AllowedTools)
	}
	if morning.ReadOnly {
		t.Error("项目技能应可写")
	}
}

// TestStoreBrokenFileKeptWithError 坏文件不静默丢弃：给原因、保留原文件、不参与选择。
func TestStoreBrokenFileKeptWithError(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "broken", "正文没有 frontmatter\n")
	writeSkill(t, dir, "ok", morningSkill)

	s := newStoreAt(t, dir)
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("坏文件也应出现在列表里: %+v", list)
	}
	var broken *Summary
	for i := range list {
		if list[i].ID == "broken" {
			broken = &list[i]
		}
	}
	if broken == nil || broken.ParseErr == "" {
		t.Fatalf("坏文件应有 ParseErr: %+v", broken)
	}
	if len(s.Candidates()) != 1 {
		t.Errorf("坏文件不应参与选择，实际候选 %d", len(s.Candidates()))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "正文没有 frontmatter\n" {
		t.Errorf("原文件应被保留，实际 %q", data)
	}
}

// TestStoreDuplicateNameNoted 重名不静默丢弃，后来者带排除原因。
func TestStoreDuplicateNameNoted(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "a", morningSkill)
	writeSkill(t, dir, "b", strings.Replace(morningSkill, "priority: 10", "priority: 5", 1))

	s := newStoreAt(t, dir)
	var dup *Summary
	for i := range s.List() {
		if s.List()[i].ID == "b" {
			dup = &s.List()[i]
		}
	}
	if dup == nil || dup.Note == "" {
		t.Fatalf("重名技能应带 Note: %+v", dup)
	}
	if !strings.Contains(dup.Note, "重复") {
		t.Errorf("Note 应说明重复，实际 %q", dup.Note)
	}
	if len(s.Candidates()) != 1 {
		t.Errorf("重复的那条应被排除，候选数 %d", len(s.Candidates()))
	}
}

// TestStoreProjectOverridesUser 同名时项目覆盖用户目录，用户条目带说明。
func TestStoreProjectOverridesUser(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, UserScopeDir)
	writeSkill(t, userDir, "morning-user", morningSkill)

	dir := t.TempDir()
	writeSkill(t, dir, "morning-project", morningSkill)

	s := NewStore(dir, userDir)
	var userEntry *Summary
	for i := range s.List() {
		if s.List()[i].Scope == ScopeUser {
			userEntry = &s.List()[i]
		}
	}
	if userEntry == nil {
		t.Fatalf("用户目录技能应出现在列表里: %+v", s.List())
	}
	if !userEntry.ReadOnly {
		t.Error("用户目录技能应只读")
	}
	if userEntry.Note == "" || !strings.Contains(userEntry.Note, "覆盖") {
		t.Errorf("被项目覆盖的用户技能应说明原因: %+v", userEntry)
	}
	if len(s.Candidates()) != 1 || s.Candidates()[0].Scope != ScopeProject {
		t.Errorf("生效的应是项目版本: %+v", s.Candidates())
	}
}

// TestStoreSaveAssignsIDAndReloads 未指定 id 时按技能名生成，保存即生效。
func TestStoreSaveAssignsIDAndReloads(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)

	summary, err := s.Save("", morningSkill)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if summary.ID == "" || summary.Name != "晨会准备" {
		t.Fatalf("保存结果不符: %+v", summary)
	}
	raw, err := s.Raw(summary.ID)
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	if !strings.Contains(raw, "晨会准备") {
		t.Errorf("落盘内容不符: %q", raw)
	}
	if _, ok := s.FindName("晨会准备"); !ok {
		t.Error("保存后应能按名字找到（无需重启）")
	}

	// 同名再存一次：应作为不同 id 保存但被标记重名，而不是覆盖第一条
	second, err := s.Save("second-copy", morningSkill)
	if err == nil {
		t.Fatalf("重名保存应被拒绝，实际 %+v", second)
	}
	if !strings.Contains(err.Error(), "已被") {
		t.Errorf("重名错误应指明冲突对象，实际 %v", err)
	}
}

// TestStoreSaveDistinctNamesSurviveIDCollision 中文名的回落 id 是同毫秒时间戳，
// 连续新建两条不能因目录名相撞而误判重名或覆盖前一条。
func TestStoreSaveDistinctNamesSurviveIDCollision(t *testing.T) {
	s := newStoreAt(t, t.TempDir())

	first, err := s.Save("", morningSkill)
	if err != nil {
		t.Fatalf("Save 晨会准备: %v", err)
	}
	second, err := s.Save("", weatherSkill)
	if err != nil {
		t.Fatalf("Save 天气播报: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("两条技能共用了目录 id %q，后者覆盖了前者", first.ID)
	}
	if got := len(s.List()); got != 2 {
		t.Errorf("应保留两条技能，实际 %d", got)
	}
	if _, ok := s.FindName("晨会准备"); !ok {
		t.Error("第二条保存后第一条应仍在（按名查找）")
	}
}

// TestStoreSaveRejectsInvalid 非法内容/目录名不落盘。
func TestStoreSaveRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)

	cases := []struct {
		name string
		id   string
		raw  string
		want string
	}{
		{"无 frontmatter", "x", "只有正文\n", "frontmatter"},
		{"缺 name", "y", "---\nauto: true\n---\n正文\n", "name"},
		{"frontmatter 内有分隔线", "z", "---\nnote: ---\nname: n\n---\n正文\n", "分隔线"},
		{"目录名非法", "../escape", morningSkill, "目录名"},
	}
	for _, tc := range cases {
		if _, err := s.Save(tc.id, tc.raw); err == nil {
			t.Errorf("%s: 应被拒绝", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: 错误应包含 %q，实际 %v", tc.name, tc.want, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("非法保存不应留下文件: %+v", entries)
	}
}

// TestStoreSaveAcceptsLongBody 正文超预算不再判无效：照常保存，命中时走"概览 +
// read_skill 按需取"那条通道（渲染在 agent 层，这里只验条目与概览）。
func TestStoreSaveAcceptsLongBody(t *testing.T) {
	s := newStoreAt(t, t.TempDir())
	long := "---\nname: 超长\ndescription: 一份很长的技能\n---\n" + strings.Repeat("字", InlineBudget+500) + "\n"
	sum, err := s.Save("", long)
	if err != nil {
		t.Fatalf("超长正文应能保存: %v", err)
	}
	if sum.ParseErr != "" || sum.Note != "" {
		t.Fatalf("不该因为长就判无效: %+v", sum)
	}
	e, ok := s.Find(sum.ID)
	if !ok {
		t.Fatalf("保存后应能按 id 找到: %s", sum.ID)
	}
	ov := e.Overview()
	for _, want := range []string{"超过一次注入的预算", "read_skill", "开头一段"} {
		if !strings.Contains(ov, want) {
			t.Errorf("概览应包含 %q，实际:\n%s", want, ov)
		}
	}
	if len([]rune(ov)) > InlineBudget {
		t.Errorf("概览本身不该超过预算: %d 字", len([]rune(ov)))
	}
}

// TestStoreSetEnabledKeepsBodyAndComments enabled 开关只改那一个键。
func TestStoreSetEnabledKeepsBodyAndComments(t *testing.T) {
	dir := t.TempDir()
	raw := "---\n# 我的技能，注释要留住\nname: 晨会准备\ndescription: 简报\nauto: true\n---\n正文一段\n"
	writeSkill(t, dir, "morning", raw)
	s := newStoreAt(t, dir)

	summary, err := s.SetEnabled("morning", false)
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if summary.Enabled {
		t.Error("关闭后 Enabled 应为 false")
	}

	updated, err := s.Raw("morning")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"我的技能，注释要留住", "name: 晨会准备", "正文一段", "auto: true"} {
		if !strings.Contains(updated, want) {
			t.Errorf("改写后应保留 %q，实际:\n%s", want, updated)
		}
	}
	if !strings.Contains(updated, "enabled: false") {
		t.Errorf("应写入 enabled: false，实际:\n%s", updated)
	}
	// 关闭后不再参与选择
	if len(s.Candidates()) != 0 {
		t.Error("已禁用的技能不应出现在候选里")
	}

	if _, err = s.SetEnabled("morning", true); err != nil {
		t.Fatalf("重新启用: %v", err)
	}
	if len(s.Candidates()) != 1 {
		t.Error("重新启用后应回到候选")
	}
}

// TestStoreSetEnabledRejectsUserScope 用户目录技能后台只读。
func TestStoreSetEnabledRejectsUserScope(t *testing.T) {
	base := t.TempDir()
	writeSkill(t, filepath.Join(base, UserScopeDir), "u1", morningSkill)

	s := NewStore(t.TempDir(), filepath.Join(base, UserScopeDir))
	if _, err := s.SetEnabled("u1", false); err == nil {
		t.Fatal("用户目录技能应拒绝改写")
	}
	if err := s.Delete("u1"); err == nil {
		t.Fatal("用户目录技能应拒绝删除")
	}
}

// TestStoreDeleteRemovesDirectory 删除后不再出现在列表，且不留空壳目录。
func TestStoreDeleteRemovesDirectory(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "morning", morningSkill)
	s := newStoreAt(t, dir)

	if err := s.Delete("morning"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(s.List()) != 0 {
		t.Errorf("删除后列表应为空: %+v", s.List())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("不应留下残留目录: %+v", entries)
	}
	if err := s.Delete("ghost"); err == nil {
		t.Error("删除不存在的技能应报错")
	}
}

// TestStoreReloadPicksUpManualEdit 手改文件后 Reload 生效（磁盘是唯一真相）。
func TestStoreReloadPicksUpManualEdit(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "morning", morningSkill)
	s := newStoreAt(t, dir)

	edited := strings.Replace(morningSkill, "name: 晨会准备", "name: 晚间复盘", 1)
	if err := os.WriteFile(filepath.Join(dir, "morning", "SKILL.md"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Reload()
	if _, ok := s.FindName("晚间复盘"); !ok {
		t.Error("Reload 后应看到手改的新名字")
	}
	if _, ok := s.FindName("晨会准备"); ok {
		t.Error("旧名字应消失")
	}
}

// TestStoreEmptyDirIsMemoryOnly 未配置目录时可用但不落盘。
func TestStoreEmptyDirIsMemoryOnly(t *testing.T) {
	s := NewStore("", "")
	if len(s.List()) != 0 {
		t.Errorf("空目录仓库应为空: %+v", s.List())
	}
	if _, err := s.Save("", morningSkill); err == nil {
		t.Error("未配置目录时保存应报错")
	}
}
