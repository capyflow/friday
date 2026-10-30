package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustSave(t *testing.T, s *WikiStore, title, body, source string) Page {
	t.Helper()
	p, created, err := s.Save("", title, body, source)
	if err != nil {
		t.Fatalf("Save(%q): %v", title, err)
	}
	if !created {
		t.Fatalf("Save(%q) 应为新建", title)
	}
	if !strings.HasPrefix(p.ID, "m") {
		t.Fatalf("ID 应以 m 开头，got %q", p.ID)
	}
	return p
}

// TestSaveReadSearch 新建/读取/搜索：标题与正文都参与匹配，命中带片段。
func TestSaveReadSearch(t *testing.T) {
	s := NewWikiStore("", "")
	p1 := mustSave(t, s, "用户偏好", "- 喜欢在晚饭后散步\n- 对花粉过敏", "user")
	time.Sleep(2 * time.Millisecond)
	mustSave(t, s, "家庭与重要的人", "- 女儿叫小红", "agent")

	// 读取整页。
	got, ok := s.Read(p1.ID)
	if !ok || got.Title != "用户偏好" || !strings.Contains(got.Body, "花粉过敏") {
		t.Fatalf("Read 结果不符，got %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("时间戳不应为零值")
	}

	// 标题命中 / 正文命中（带片段）/ 未命中。
	hits := s.Search("偏好")
	if len(hits) != 1 || hits[0].ID != p1.ID {
		t.Fatalf("标题搜索应命中 1 页，got %+v", hits)
	}
	if hits[0].Snippet != "" {
		t.Fatalf("标题命中无需片段，got %q", hits[0].Snippet)
	}
	hits = s.Search("花粉")
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "花粉过敏") {
		t.Fatalf("正文搜索应命中并带片段，got %+v", hits)
	}
	if len(s.Search("不存在的关键词")) != 0 {
		t.Fatal("无关关键词应命中 0 页")
	}
	if s.Count() != 2 {
		t.Fatalf("Count 应为 2，got %d", s.Count())
	}

	// 列表按更新时间倒序。
	all := s.Pages()
	if len(all) != 2 || all[0].Title != "家庭与重要的人" {
		t.Fatalf("Pages 应按更新时间倒序，got %+v", all)
	}
}

// TestSaveUpsertByTitle 相同标题保存 = 更新该页；id 更新可改标题。
func TestSaveUpsertByTitle(t *testing.T) {
	s := NewWikiStore("", "")
	p1 := mustSave(t, s, "用户偏好", "喜欢散步", "user")
	time.Sleep(2 * time.Millisecond)

	p2, created, err := s.Save("", "用户偏好", "喜欢散步\n- 对花粉过敏", "agent")
	if err != nil || created {
		t.Fatalf("同标题应更新而非新建，created=%v err=%v", created, err)
	}
	if p2.ID != p1.ID {
		t.Fatalf("更新应保持同一页面，got %s want %s", p2.ID, p1.ID)
	}
	if s.Count() != 1 {
		t.Fatalf("同标题保存后应仍为 1 页，got %d", s.Count())
	}
	got, _ := s.Read(p1.ID)
	if got.Source != "agent" || !strings.Contains(got.Body, "花粉过敏") {
		t.Fatalf("更新后内容不符，got %+v", got)
	}
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatal("更新后 UpdatedAt 应晚于 CreatedAt")
	}

	// 按 id 更新并改标题。
	if _, _, err := s.Save(p1.ID, "偏好与习惯", "新正文", "admin"); err != nil {
		t.Fatalf("按 id 更新: %v", err)
	}
	got, _ = s.Read(p1.ID)
	if got.Title != "偏好与习惯" || got.Body != "新正文" {
		t.Fatalf("按 id 更新结果不符，got %+v", got)
	}
}

// TestSaveValidation 空标题/正文、不存在的 id、非法 ID 都应报错。
func TestSaveValidation(t *testing.T) {
	s := NewWikiStore("", "")
	if _, _, err := s.Save("", "  ", "正文", "user"); err == nil {
		t.Fatal("空标题应报错")
	}
	if _, _, err := s.Save("", "标题", "   ", "user"); err == nil {
		t.Fatal("空正文应报错")
	}
	if _, _, err := s.Save("m404", "标题", "正文", "user"); err == nil {
		t.Fatal("更新不存在的 id 应报错")
	}
	if _, _, err := s.Save("../evil", "标题", "正文", "user"); err == nil {
		t.Fatal("路径穿越 ID 应报错")
	}
	if s.Count() != 0 {
		t.Fatalf("失败保存不应入库，实际 %d", s.Count())
	}
	// Delete/Read 对非法 ID 同样安全。
	if s.Delete("../../etc/passwd") || s.Count() != 0 {
		t.Fatal("非法 ID 删除应返回 false")
	}
	if _, ok := s.Read("../../etc/passwd"); ok {
		t.Fatal("非法 ID 读取不应命中")
	}
}

// TestDelete 删除一页；# 前缀 ID 容忍（recall 展示带 #，模型会原样传回）。
func TestDelete(t *testing.T) {
	s := NewWikiStore("", "")
	p := mustSave(t, s, "怕打雷", "用户怕打雷", "user")
	if !s.Delete("#" + p.ID) {
		t.Fatal("Delete 应容忍 # 前缀")
	}
	if s.Delete(p.ID) {
		t.Fatal("重复 Delete 应返回 false")
	}
	if s.Count() != 0 {
		t.Fatalf("删除后应为 0，got %d", s.Count())
	}
}

// TestPersistRoundTrip 落盘 → 重启恢复；删除也同步落盘。
func TestPersistRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memories")
	s1 := NewWikiStore(dir, "")
	p1 := mustSave(t, s1, "用户偏好", "- 喜欢番茄", "user")
	p2 := mustSave(t, s1, "例行安排", "每周三要开会", "agent")
	if !s1.Delete(p2.ID) {
		t.Fatal("Delete 应返回 true")
	}

	s2 := NewWikiStore(dir, "")
	all := s2.Pages()
	if len(all) != 1 {
		t.Fatalf("重启后应恢复 1 页，实际 %d", len(all))
	}
	got, ok := s2.Read(p1.ID)
	if !ok {
		t.Fatal("重启后应能读回原页面")
	}
	if got.Title != p1.Title || got.Body != p1.Body || got.Source != p1.Source ||
		!got.CreatedAt.Equal(p1.CreatedAt) || !got.UpdatedAt.Equal(p1.UpdatedAt) {
		t.Fatalf("恢复的页面不符，got %+v want %+v", got, p1)
	}
	// 页面文件是可读的 markdown + frontmatter。
	raw, err := os.ReadFile(filepath.Join(dir, p1.ID+".md"))
	if err != nil {
		t.Fatalf("读页面文件: %v", err)
	}
	for _, want := range []string{"---", "title: 用户偏好", "喜欢番茄"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("页面文件应包含 %q，got:\n%s", want, raw)
		}
	}
}

// TestCorruptPageFile 损坏页面应跳过不崩溃，后续写入正常。
func TestCorruptPageFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mbroken.md"), []byte("不是 frontmatter"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	s := NewWikiStore(dir, "")
	if s.Count() != 0 {
		t.Fatalf("损坏页面应跳过，实际 %d 页", s.Count())
	}
	p := mustSave(t, s, "恢复后可正常写入", "正文", "user")
	if _, ok := s.Read(p.ID); !ok {
		t.Fatal("新增后应可读回")
	}
}

// TestLegacyMigration 旧版扁平 memories.json 自动迁入《用户档案》页，且只迁一次。
func TestLegacyMigration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memories")
	legacy := filepath.Join(t.TempDir(), "memories.json")
	content := `[
		{"id":"m1","content":"用户喜欢散步","source":"user","createdAt":"2026-09-01T10:00:00+08:00"},
		{"id":"m2","content":"用户对花粉过敏","source":"agent","createdAt":"2026-09-02T10:00:00+08:00"}
	]`
	if err := os.WriteFile(legacy, []byte(content), 0o644); err != nil {
		t.Fatalf("写旧文件: %v", err)
	}

	s := NewWikiStore(dir, legacy)
	all := s.Pages()
	if len(all) != 1 || all[0].Title != "用户档案" {
		t.Fatalf("应迁移出一页《用户档案》，got %+v", all)
	}
	if all[0].Source != "migrate" {
		t.Fatalf("迁移页来源应为 migrate，got %q", all[0].Source)
	}
	got, _ := s.Read(all[0].ID)
	if !strings.Contains(got.Body, "用户喜欢散步") || !strings.Contains(got.Body, "用户对花粉过敏") {
		t.Fatalf("迁移页应包含全部旧条目，got:\n%s", got.Body)
	}
	// 旧文件保留。
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("旧文件应保留不删")
	}
	// 再次启动（目录非空）不重复迁移。
	s2 := NewWikiStore(dir, legacy)
	if s2.Count() != 1 {
		t.Fatalf("不应重复迁移，实际 %d 页", s2.Count())
	}
}

// TestLegacyMissingOrCorrupt 旧文件不存在或损坏时静默跳过。
func TestLegacyMissingOrCorrupt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memories")
	legacy := filepath.Join(t.TempDir(), "memories.json")
	if err := os.WriteFile(legacy, []byte("{{{不是 JSON"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	if s := NewWikiStore(dir, legacy); s.Count() != 0 {
		t.Fatalf("损坏旧文件应跳过迁移，实际 %d 页", s.Count())
	}
	if s := NewWikiStore(filepath.Join(t.TempDir(), "memories2"), filepath.Join(t.TempDir(), "不存在.json")); s.Count() != 0 {
		t.Fatal("旧文件不存在应空启动")
	}
}

// 记忆速览：按更新时间倒序、正文压成一行并截断、总长受 maxChars 约束；
// 空仓库返回空串（调用方据此不注入）。
func TestWikiStoreDigest(t *testing.T) {
	s := NewWikiStore(t.TempDir(), "")
	if got := s.Digest(0, 0); got != "" {
		t.Fatalf("空仓库不该产出速览，实际 %q", got)
	}
	if _, _, err := s.Save("", "旧页", "旧内容", "user"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // 更新时间倒序依赖 Save 的时间戳
	if _, _, err := s.Save("", "用户偏好", strings.Repeat("偏", 300), "user"); err != nil {
		t.Fatal(err)
	}
	got := s.Digest(0, 0)
	if !strings.HasPrefix(got, "- 用户偏好：") {
		t.Fatalf("应按更新时间倒序且首行为最新页，实际 %q", got)
	}
	if !strings.Contains(got, "…") || !strings.Contains(got, "旧页：旧内容") {
		t.Fatalf("超长正文应截断、其余页仍要列出，实际 %q", got)
	}
	if strings.Contains(got, "\n\n") {
		t.Fatalf("每页应压成单行，实际 %q", got)
	}
	if short := s.Digest(1, 40); len([]rune(short)) > 40 {
		t.Fatalf("maxChars 应生效，实际 %q", short)
	}
}

// 关键词按空格拆开、任意命中即算：模型爱把查询写成「用户 称呼 名字」，
// 整串匹配会什么也找不到（实测就是"记忆里有、recall 说没有"）。
func TestSearchSplitsKeywords(t *testing.T) {
	s := NewWikiStore(t.TempDir(), "")
	if _, _, err := s.Save("", "用户偏好", "称呼：用户希望被称作「陛下」；沟通风格简洁。", "user"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, _, err := s.Save("", "家庭与重要的人", "用户有一个妹妹。", "user"); err != nil {
		t.Fatal(err)
	}

	got := s.Search("用户 称呼 名字") // 「名字」并不存在，其余两个词应命中
	if len(got) != 2 {
		t.Fatalf("任意词命中就应返回两页，实际 %+v", got)
	}
	if got[0].Title != "用户偏好" {
		t.Fatalf("命中词多的页应排前，实际 %q", got[0].Title)
	}
	if !strings.Contains(got[0].Snippet, "称呼") {
		t.Fatalf("片段应取正文里最早的命中（称呼），实际 %q", got[0].Snippet)
	}
	if hit := s.Search("称呼"); len(hit) != 1 || hit[0].Title != "用户偏好" {
		t.Fatalf("单词查询行为不变，实际 %+v", hit)
	}
	if all := s.Search("   "); len(all) != 2 {
		t.Fatalf("空关键词应列出全部页面，实际 %d 页", len(all))
	}
}

func TestScopedMemoryIsolatedAndPersisted(t *testing.T) {
	dir := t.TempDir()
	s := NewWikiStore(dir, "")
	global := mustSave(t, s, "用户偏好", "- 喜欢黑咖啡", "user")
	w1, _, err := s.SaveScoped("ws-a", "", "项目约定", "这个仓库用 pnpm，不要用 npm", "admin")
	if err != nil {
		t.Fatal(err)
	}
	w2, _, err := s.SaveScoped("ws-b", "", "项目约定", "这个仓库用 yarn", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if w1.ID == w2.ID {
		t.Fatal("不同工作区的同名页面不应合并")
	}
	if len(s.Pages()) != 1 || s.Pages()[0].ID != global.ID {
		t.Fatalf("全局 Pages 只应看到全局页: %+v", s.Pages())
	}
	if got := s.PagesScoped("ws-a"); len(got) != 1 || !strings.Contains(got[0].Title, "项目约定") {
		t.Fatalf("工作区 A 页面不符: %+v", got)
	}
	if len(s.SearchScoped("ws-a", "yarn")) != 0 {
		t.Fatal("不应搜到其他工作区的页面")
	}
	if _, ok := s.ReadScoped("ws-a", w2.ID); ok {
		t.Fatal("跨作用域读取应失败")
	}
	if s.DeleteScoped("ws-b", w1.ID) {
		t.Fatal("跨作用域删除应失败")
	}
	if d := s.DigestScoped("ws-a", 4, 500); !strings.Contains(d, "pnpm") || strings.Contains(d, "yarn") {
		t.Fatalf("项目速览内容不符: %q", d)
	}

	// 重开仓库：scope 必须从 frontmatter 恢复，不能退化成全局页。
	reopened := NewWikiStore(dir, "")
	if got := reopened.PagesScoped("ws-a"); len(got) != 1 {
		t.Fatalf("重开后项目页丢失: %+v", got)
	}
	if page, ok := reopened.ReadScoped("ws-a", w1.ID); !ok || !strings.Contains(page.Body, "pnpm") {
		t.Fatalf("重开后项目页正文丢失: %+v", page)
	}
	if len(reopened.Pages()) != 1 {
		t.Fatalf("重开后全局 Pages 应仍只有 1 页，实际 %+v", reopened.Pages())
	}
	if _, ok := reopened.Read(w1.ID); ok {
		t.Fatal("工作区页不应当成全局页读出")
	}
}
