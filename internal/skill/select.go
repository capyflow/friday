// select.go 决定"这句话该用哪个技能"。框架只提供按次的 WithSkill，选择逻辑在这里：
// 规则确定、每次命中都带回可读依据（进轨迹与"思考过程"），且自动匹配是 opt-in——
// 技能必须显式写 auto: true 才会被自动选中。
package skill

import (
	"strings"
	"unicode"
)

// 自动匹配阈值：绝对分不够或领先差距不够都不启用（宁可不启用，也不乱套提示词）。
const (
	DefaultMinScore  = 2.0
	DefaultMinMargin = 0.8
)

// MatchConfig 是自动匹配参数（零值使用默认阈值）。
type MatchConfig struct {
	Enabled   bool
	MinScore  float64
	MinMargin float64
}

func (c MatchConfig) withDefaults() MatchConfig {
	if c.MinScore <= 0 {
		c.MinScore = DefaultMinScore
	}
	if c.MinMargin <= 0 {
		c.MinMargin = DefaultMinMargin
	}
	return c
}

// Match 返回本轮应启用的技能与命中依据。entries 应来自 Store.Candidates()
// （已按"项目优先 → priority 降序 → ID"稳定排序，平票即取先者）。
func Match(text string, entries []*Entry, cfg MatchConfig) (*Entry, []string, bool) {
	if !cfg.Enabled || len(entries) == 0 {
		return nil, nil, false
	}
	q := normalize(text)
	if q == "" {
		return nil, nil, false
	}

	// 只有显式 opt-in（auto: true）的技能参与自动匹配。
	cands := make([]*Entry, 0, len(entries))
	for _, e := range entries {
		if e.Candidate() {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		return nil, nil, false
	}

	// Tier A：触发词命中即采用（与自定义指令同一套判定：相等，或"词 + 空格"开头）。
	for _, e := range cands {
		for _, raw := range e.Extra.Triggers {
			t := normalize(raw)
			if t == "" {
				continue
			}
			if q == t || strings.HasPrefix(q, t+" ") {
				return e, []string{"trigger:" + strings.TrimSpace(raw)}, true
			}
		}
	}

	// Tier B：加权打分；要求绝对分够、且明显领先次优。
	c := cfg.withDefaults()
	var best *Entry
	var bestScore, second float64
	var bestWhy []string
	for _, e := range cands {
		s, why := score(q, e)
		switch {
		case s > bestScore:
			second = bestScore
			best, bestScore, bestWhy = e, s, why
		case s > second:
			second = s
		}
	}
	if best == nil || bestScore < c.MinScore {
		return nil, nil, false
	}
	if second > 0 && bestScore-second < c.MinMargin {
		return nil, nil, false // 差距不足：不明确就不启用
	}
	return best, bestWhy, true
}

// score 算一条消息对某技能的匹配分。
//
// 权重：keywords 命中 3、技能名分词命中 2、描述分词命中 1；
// 除以 (1 + 描述字数/40) 惩罚"描述写得长所以什么都能蹭上"的技能。
func score(q string, e *Entry) (float64, []string) {
	if e == nil || e.Skill == nil {
		return 0, nil
	}
	weight := 0.0
	var why []string
	for _, raw := range e.Extra.Keywords {
		kw := normalize(raw)
		if kw != "" && strings.Contains(q, kw) {
			weight += 3
			why = append(why, "keyword:"+kw)
		}
	}
	for _, tok := range tokens(e.Name()) {
		if strings.Contains(q, tok) {
			weight += 2
			why = append(why, "name:"+tok)
		}
	}
	for _, tok := range tokens(e.Skill.Metadata.Description) {
		if strings.Contains(q, tok) {
			weight++
			why = append(why, "desc:"+tok)
		}
	}
	if weight == 0 {
		return 0, nil
	}
	descRunes := len([]rune(e.Skill.Metadata.Description))
	return weight / (1 + float64(descRunes)/40), why
}

// ParseInvocation 解析显式调用语法 "/skill 名 [正文]" 与 "$名 [正文]"。
// 返回 (技能名, 去掉调用前缀的正文, 是否使用了调用语法)——名字是否存在由调用方判。
func ParseInvocation(text string) (name, rest string, ok bool) {
	trimmed := wideSpaceToPlain(text)
	var remainder string
	switch {
	case strings.HasPrefix(trimmed, "/skill ") || trimmed == "/skill":
		remainder = strings.TrimSpace(trimmed[len("/skill"):])
	case strings.HasPrefix(trimmed, "$") && trimmed != "$":
		remainder = strings.TrimSpace(trimmed[1:])
	default:
		return "", "", false
	}
	name, rest, _ = strings.Cut(remainder, " ")
	name = strings.TrimSpace(name)
	return name, strings.TrimSpace(rest), name != ""
}

// IsInvocation 判断这条消息是否使用了显式调用语法（用于"名字不存在"时明确拒绝，
// 而不是降级去猜技能）。与 ParseInvocation 同一口径：光有 /skill 或 $ 不算调用。
func IsInvocation(text string) bool {
	_, _, ok := ParseInvocation(text)
	return ok
}

// normalize 归一化：小写、去首尾空白、全角空格转半角、连续空白压成一个空格。
func normalize(s string) string {
	s = strings.ToLower(wideSpaceToPlain(s))
	return strings.Join(strings.Fields(s), " ")
}

func wideSpaceToPlain(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	replacer := strings.NewReplacer("　", " ", "\t", " ", "\n", " ", "\r", " ")
	return replacer.Replace(s)
}

// tokens 把文本切成可重叠匹配的片段：ASCII 词整体保留，CJK 取 2-gram
// （单字太泛，整句太窄；2-gram 兼顾"简报/晨会简报"这类包含关系）。
func tokens(s string) []string {
	s = normalize(s)
	if s == "" {
		return nil
	}
	var out []string
	var ascii strings.Builder
	var cjk []rune
	flushASCII := func() {
		if ascii.Len() > 0 {
			out = append(out, ascii.String())
			ascii.Reset()
		}
	}
	flushCJK := func() {
		if len(cjk) == 1 {
			out = append(out, string(cjk))
		}
		for i := 0; i+1 < len(cjk); i++ {
			out = append(out, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			flushASCII()
			cjk = append(cjk, r)
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			flushCJK()
			ascii.WriteRune(r)
		default:
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()

	seen := map[string]bool{}
	uniq := make([]string, 0, len(out))
	for _, t := range out {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		uniq = append(uniq, t)
	}
	return uniq
}
