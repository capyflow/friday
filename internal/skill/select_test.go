package skill

import (
	"testing"
)

// entry 从 SKILL.md 文本造条目（与后台保存走同一条解析路径）。
func entry(t *testing.T, id, raw string) *Entry {
	t.Helper()
	e, err := parseEntry(id, "/tmp/"+id+"/SKILL.md", ScopeProject, raw)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", id, err)
	}
	return e
}

func mustSkill(t *testing.T, id, name, desc string, auto bool, triggers, keywords []string, priority int) *Entry {
	t.Helper()
	raw := "---\nname: " + name + "\ndescription: " + desc + "\n"
	if auto {
		raw += "auto: true\n"
	}
	if len(triggers) > 0 {
		raw += "triggers: [" + quoteJoin(triggers) + "]\n"
	}
	if len(keywords) > 0 {
		raw += "keywords: [" + quoteJoin(keywords) + "]\n"
	}
	if priority != 0 {
		raw += "priority: 10\n"
	}
	raw += "---\n技能正文\n"
	return entry(t, id, raw)
}

func quoteJoin(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += `"` + x + `"`
	}
	return out
}

var defaultCfg = MatchConfig{Enabled: true}

// TestMatchTriggerExactAndPrefix 触发词相等或"词 + 空格"开头即命中。
func TestMatchTriggerExactAndPrefix(t *testing.T) {
	morning := mustSkill(t, "morning", "晨会准备", "汇总今日待办与天气", true, []string{"晨会"}, nil, 10)
	entries := []*Entry{morning}

	for _, text := range []string{"晨会", "晨会 只看天气", "  晨会　全部说一下  "} {
		got, why, ok := Match(text, entries, defaultCfg)
		if !ok || got != morning {
			t.Errorf("%q 应命中晨会准备，实际 ok=%v got=%v", text, ok, got)
		}
		if len(why) != 1 || why[0] != "trigger:晨会" {
			t.Errorf("%q 命中依据应为触发词，实际 %+v", text, why)
		}
	}
	// "晨会" 出现在句中不算触发词命中（除非另有加权分）
	if _, _, ok := Match("今天不开晨会了", entries, defaultCfg); ok {
		t.Error("触发词必须是开头整词，句中不应命中")
	}
}

// TestMatchRequiresOptIn 未写 auto: true 的技能永不参与自动匹配。
func TestMatchRequiresOptIn(t *testing.T) {
	manual := mustSkill(t, "manual", "天气播报", "查天气并给出穿衣建议", false, []string{"天气"}, []string{"天气"}, 0)
	if _, _, ok := Match("天气怎么样", []*Entry{manual}, defaultCfg); ok {
		t.Error("非 opt-in 技能不应被自动命中")
	}
	// 但显式调用仍然可用
	if got, found := func() (*Entry, bool) {
		for _, e := range []*Entry{manual} {
			if e.Usable() && e.Name() == "天气播报" {
				return e, true
			}
		}
		return nil, false
	}(); !found || !got.Usable() {
		t.Error("Usable 只受 enabled 影响，与 auto 无关")
	}
}

// TestMatchDisabledOrBrokenExcluded 关闭或解析失败的条目不参与。
func TestMatchDisabledOrBrokenExcluded(t *testing.T) {
	off := entry(t, "off", "---\nname: 关闭的\ndescription: 测试\nauto: true\nenabled: false\n---\n正文\n")
	if _, _, ok := Match("关闭的 内容", []*Entry{off}, defaultCfg); ok {
		t.Error("enabled: false 的技能不应命中")
	}
	broken := &Entry{ID: "broken", ParseErr: "frontmatter 缺少 name"}
	if _, _, ok := Match("随便什么", []*Entry{broken}, defaultCfg); ok {
		t.Error("解析失败的条目必须被排除")
	}
}

// TestMatchScoreThresholds 绝对分不足或领先差距不足都不启用。
func TestMatchScoreThresholds(t *testing.T) {
	strong := mustSkill(t, "strong", "发票整理", "识别并归档发票", true, nil, []string{"发票"}, 0)
	weak := mustSkill(t, "weak", "日常问答", "回答各种日常问题", true, nil, []string{"整理"}, 0)

	// "发票"关键词命中：3 / (1+6/40) ≈ 2.59 ≥ 2.0，且明显领先
	got, why, ok := Match("帮我看下发票", []*Entry{strong, weak}, defaultCfg)
	if !ok || got != strong {
		t.Fatalf("应命中强相关技能，实际 ok=%v got=%v", ok, nameOf(got))
	}
	if len(why) == 0 {
		t.Error("应给出命中依据")
	}

	// 只有一个弱命中（desc 分词 1 分）→ 不足绝对分 → 不启用
	descOnly := mustSkill(t, "desc", "旅行规划", "帮我规划行程与酒店预订", true, nil, nil, 0)
	if e, _, ok := Match("去旅行", []*Entry{descOnly}, defaultCfg); ok {
		t.Errorf("低分命中不应启用，实际 %+v", nameOf(e))
	}

	// 两个技能得分完全相同 → 差距不足 → 不启用（不确定就不套提示词）
	a := mustSkill(t, "a", "周报甲", "汇总本周工作", true, nil, []string{"周报"}, 0)
	b := mustSkill(t, "b", "周报乙", "汇总本周工作", true, nil, []string{"周报"}, 0)
	if e, _, ok := Match("写个周报", []*Entry{a, b}, defaultCfg); ok {
		t.Errorf("同分不应启用，实际 %+v", nameOf(e))
	}
}

// TestMatchTriggerTieBreaksOnOrder 同一触发词被多个技能声明时取排序在前的
// （Store 已按"项目优先 → priority 降序 → ID"排好，Match 不自行重排）。
func TestMatchTriggerTieBreaksOnOrder(t *testing.T) {
	first := mustSkill(t, "first", "晨会准备", "汇总今日待办与天气", true, []string{"晨会"}, nil, 10)
	second := mustSkill(t, "second", "晨会改名", "汇总今日待办与天气", true, []string{"晨会"}, nil, 5)

	got, why, ok := Match("晨会", []*Entry{first, second}, defaultCfg)
	if !ok || got != first {
		t.Fatalf("同触发词应取传入顺序在前者，实际 ok=%v got=%v", ok, nameOf(got))
	}
	if len(why) == 0 || why[0] != "trigger:晨会" {
		t.Errorf("命中依据应为触发词，实际 %+v", why)
	}

	// 顺序反过来，仍取新的首位——排序责任在 Store，Match 保持稳定。
	got2, _, ok2 := Match("晨会", []*Entry{second, first}, defaultCfg)
	if !ok2 || got2 != second {
		t.Errorf("同分应取排序在前者，实际 %v", nameOf(got2))
	}
}

// TestMatchRespectsDisabledSwitch 总开关关闭时一律不自动匹配。
func TestMatchRespectsDisabledSwitch(t *testing.T) {
	morning := mustSkill(t, "morning", "晨会准备", "汇总今日待办与天气", true, []string{"晨会"}, nil, 0)
	if _, _, ok := Match("晨会", []*Entry{morning}, MatchConfig{Enabled: false}); ok {
		t.Error("skills.autoTrigger=false 时不应自动命中")
	}
}

// TestParseInvocation 显式调用语法。
func TestParseInvocation(t *testing.T) {
	cases := []struct {
		in        string
		wantName  string
		wantRest  string
		wantFound bool
		verb      string
	}{
		{"/skill 晨会准备 只看天气", "晨会准备", "只看天气", true, "斜杠调用"},
		{"/skill 晨会准备", "晨会准备", "", true, "斜杠调用无正文"},
		{"$晨会准备 只看天气", "晨会准备", "只看天气", true, "美元符号调用"},
		{"$Morning-Brief 今日", "Morning-Brief", "今日", true, "ASCII 名调用"},
		{"/skill", "", "", false, "光有命令"},
		{"$", "", "", false, "光有符号"},
		{"晨会准备", "", "", false, "普通消息"},
		{"/skill　晨会准备　补充", "晨会准备", "补充", true, "全角空格调用"},
	}
	for _, tc := range cases {
		name, rest, ok := ParseInvocation(tc.in)
		if ok != tc.wantFound || name != tc.wantName || rest != tc.wantRest {
			t.Errorf("%s(%q) = (%q,%q,%v)，期望 (%q,%q,%v)",
				tc.verb, tc.in, name, rest, ok, tc.wantName, tc.wantRest, tc.wantFound)
		}
		if tc.wantFound != IsInvocation(tc.in) {
			t.Errorf("IsInvocation(%q) = %v，期望 %v", tc.in, IsInvocation(tc.in), tc.wantFound)
		}
	}
	// "/skills"（列表命令）不该被当成显式调用一个名为 "s…" 的技能
	if name, _, ok := ParseInvocation("/skills"); ok {
		t.Errorf("/skills 不应解析成调用，实际 name=%q", name)
	}
}

// TestTokens 切词形状：ASCII 整词 + CJK 二元组，且去重。
func TestTokens(t *testing.T) {
	got := tokens("晨会简报 morning-brief")
	set := map[string]bool{}
	for _, tok := range got {
		set[tok] = true
	}
	for _, want := range []string{"晨会", "会简", "简报", "morning", "brief"} {
		if !set[want] {
			t.Errorf("tokens 应包含 %q，实际 %+v", want, got)
		}
	}
	if len(got) != len(set) {
		t.Errorf("tokens 有重复: %+v", got)
	}
}

func nameOf(e *Entry) string {
	if e == nil {
		return ""
	}
	return e.Name()
}
