package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roots 造一组"后台可写 + 两个约定地址"的发现根，顺序即优先级。
func roots(managed, shared, home string) []Root {
	return []Root{
		{Dir: managed, Scope: ScopeProject, Source: "Friday 技能目录", Writable: true},
		{Dir: shared, Scope: ScopeProject, Source: "项目 .agents/skills"},
		{Dir: home, Scope: ScopeUser, Source: "公共 ~/.agents/skills"},
	}
}

func find(list []Summary, id string) *Summary {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

// TestRootsPriorityAndOverride 三个根都参与发现，同名时优先级高的赢，
// 被覆盖的那条要在后台说明"被谁覆盖"，不能静默消失。
func TestRootsPriorityAndOverride(t *testing.T) {
	base := t.TempDir()
	managed := filepath.Join(base, "skills")
	shared := filepath.Join(base, "work", ".agents", "skills")
	home := filepath.Join(base, "home", ".agents", "skills")
	writeSkill(t, managed, "morning", morningSkill)
	writeSkill(t, shared, "morning-shared", morningSkill) // 与后台那条同名
	writeSkill(t, home, "weather-home", weatherSkill)

	s := NewStoreRoots(roots(managed, shared, home)...)
	if err := s.Err(); err != nil {
		t.Fatalf("Store.Err: %v", err)
	}
	// Candidates 是"可生效"的条目（不看 auto）：同名的两条只剩后台可写那条，
	// 另一根里的「天气播报」是不同技能，照常可用。
	cands := s.Candidates()
	if len(cands) != 2 || cands[0].Name() != "晨会准备" || cands[0].Source != "Friday 技能目录" {
		t.Fatalf("同名技能只该后台那条生效，另一条不同名的照常可用: %+v", cands)
	}
	for _, e := range cands {
		if e.ID == "morning-shared" {
			t.Error("被覆盖的那条不该参与选择")
		}
	}
	list := s.List()
	shadowed := find(list, "morning-shared")
	if shadowed == nil {
		t.Fatalf("被覆盖的技能仍要出现在列表里: %+v", list)
	}
	if shadowed.Note == "" || !strings.Contains(shadowed.Note, "Friday 技能目录") {
		t.Errorf("覆盖说明要点名是谁覆盖了谁: %+v", shadowed.Note)
	}
	if !shadowed.ReadOnly {
		t.Error("约定地址的技能应只读")
	}
	if got := find(list, "weather-home"); got == nil || got.Source != "公共 ~/.agents/skills" {
		t.Errorf("个人全局根的技能要标对来源: %+v", got)
	}
	if got := len(s.Roots()); got != 3 {
		t.Errorf("Roots 应返回 3 个启用根，得到 %d", got)
	}
}

// TestReadOnlyRootsAreTolerant 约定地址是别人维护的：目录名奇怪也照收，
// 后台不写它、更不替用户创建它。
func TestReadOnlyRootsAreTolerant(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, ".agents", "skills")
	writeSkill(t, shared, "My Skill", morningSkill) // 带空格：可写根会判非法
	missing := filepath.Join(base, "nope", "skills")

	s := NewStoreRoots([]Root{
		{Dir: filepath.Join(base, "skills"), Scope: ScopeProject, Source: "Friday 技能目录", Writable: true},
		{Dir: shared, Scope: ScopeProject, Source: "项目 .agents/skills"},
		{Dir: missing, Scope: ScopeUser, Source: "还没建过的地址"},
	}...)
	got := find(s.List(), "My Skill")
	if got == nil {
		t.Fatalf("共享根里目录名带空格的技能应被收下: %+v", s.List())
	}
	if got.ParseErr != "" {
		t.Errorf("不该替别人判目录名非法: %s", got.ParseErr)
	}
	if _, err := s.SetEnabled("My Skill", false); err == nil || !strings.Contains(err.Error(), "只读") {
		t.Errorf("只读根的技能不该能被后台改写: %v", err)
	}
	if err := s.Delete("My Skill"); err == nil || !strings.Contains(err.Error(), "不删") {
		t.Errorf("只读根的技能不该能被后台删除: %v", err)
	}
	// 不存在的只读根：既不报错，也不被创建出来
	if err := s.Err(); err != nil {
		t.Errorf("约定地址没建过不该算故障: %v", err)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("只读根不该被自动创建")
	}
	if _, err := os.Stat(filepath.Join(base, "skills")); err != nil {
		t.Error("后台可写根该被创建出来")
	}
}

// TestLongBodyAcceptedInEveryRoot 正文长度不再影响可用性：两个根都照收，
// 后台列表不带任何"太长"的标记（超预算的走概览 + read_skill，是渲染层的事）。
func TestLongBodyAcceptedInEveryRoot(t *testing.T) {
	long := func(name string) string {
		return "---\nname: " + name + "\ndescription: 正文超过注入预算\nauto: true\n---\n" +
			strings.Repeat("甲", InlineBudget+10)
	}
	base := t.TempDir()
	shared := filepath.Join(base, ".agents", "skills")
	writeSkill(t, shared, "long-shared", long("共享长文"))
	writeSkill(t, filepath.Join(base, "skills"), "long-managed", long("自有长文"))

	s := NewStoreRoots([]Root{
		{Dir: filepath.Join(base, "skills"), Scope: ScopeProject, Source: "Friday 技能目录", Writable: true},
		{Dir: shared, Scope: ScopeProject, Source: "项目 .agents/skills"},
	}...)
	for _, id := range []string{"long-shared", "long-managed"} {
		e := find(s.List(), id)
		if e == nil {
			t.Fatalf("%s 应出现在列表里", id)
		}
		if e.ParseErr != "" || e.Note != "" {
			t.Errorf("%s 不该因为正文长被排除: err=%q note=%q", id, e.ParseErr, e.Note)
		}
	}
	if got := len(s.Candidates()); got != 2 {
		t.Errorf("两个长文技能都该可被点名/自动匹配，候选数 %d", got)
	}
}

// TestSameDirNameAcrossRoots 两个根里目录名撞车时，ID 归优先级高的那条（后台按 ID
// 编辑只能是它），但低优先级那条照常参与选择——它的技能名可能完全不同。
func TestSameDirNameAcrossRoots(t *testing.T) {
	base := t.TempDir()
	managed := filepath.Join(base, "skills")
	shared := filepath.Join(base, ".agents", "skills")
	writeSkill(t, managed, "report", morningSkill) // 技能名「晨会准备」，auto: true
	writeSkill(t, shared, "report", weatherSkill)  // 技能名「天气播报」，auto: false

	s := NewStoreRoots([]Root{
		{Dir: managed, Scope: ScopeProject, Source: "Friday 技能目录", Writable: true},
		{Dir: shared, Scope: ScopeProject, Source: "项目 .agents/skills"},
	}...)
	// 低优先级那条只是 ID 被占，不该因此被踢出选择：两条不同名的技能都该可生效
	if got := len(s.Candidates()); got != 2 {
		t.Fatalf("ID 撞车不该把另一条技能顺带废掉，候选数 %d: %+v", got, s.List())
	}
	for _, e := range s.List() {
		if e.Note != "" {
			t.Errorf("两条技能名不同，谁都不该被排除: %+v", e)
		}
	}
	// 后台按 ID 取原文时拿到的是优先级高的那条
	raw, err := s.Raw("report")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "晨会准备") {
		t.Error("同 ID 撞车时应解析到后台可写那条")
	}
	// 两条都要在列表里，且各自带来源与路径
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("两条都该在列表里: %+v", list)
	}
	for _, e := range list {
		if e.Source == "" || e.Path == "" {
			t.Errorf("每条都要能看出来自哪个根、哪个文件: %+v", e)
		}
	}
}
