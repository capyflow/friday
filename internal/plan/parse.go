// parse.go 把模型在规划轮的产出解析成可审阅的步骤清单。
package plan

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

type rawStep struct {
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	DoneWhen string   `json:"doneWhen"`
	Tools    []string `json:"tools"`
}

type rawPlan struct {
	Title   string    `json:"title"`
	Summary string    `json:"summary"`
	Steps   []rawStep `json:"steps"`
}

// ExtractJSON 从回答里抠出第一段 JSON 对象文本。
//
// 模型常把 JSON 包在 ```json 围栏里或前后加一句说明；这里剥围栏、取第一个
// '{' 到与之配对的 '}'（按引号/转义状态计数，不在字符串里误判配对）。
func ExtractJSON(answer string) (string, bool) {
	s := strings.TrimSpace(answer)
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "{"); i >= 0 {
			s = s[i:]
		}
		if j := strings.LastIndex(s, "}"); j >= 0 {
			return s[:j+1], true
		}
		return "", false
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case !inStr && c == '{':
			depth++
		case !inStr && c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// Parse 解析规划产出。步骤为空、标题缺失或数量超限都算无效（由调用方决定
// 重试还是降级），错误信息会作为纠正提示的一部分回给模型。
func Parse(goal, answer string) (*Plan, error) {
	text, ok := ExtractJSON(answer)
	if !ok {
		return nil, fmt.Errorf("回答里找不到 JSON 对象（请只输出约定的 JSON 结构，不要加解释文字）")
	}
	var raw rawPlan
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	if len(raw.Steps) == 0 {
		return nil, fmt.Errorf("steps 为空——至少要给出一个可执行的步骤")
	}
	if len(raw.Steps) > maxSteps {
		return nil, fmt.Errorf("steps 有 %d 条，超过上限 %d 条——请合并细碎步骤", len(raw.Steps), maxSteps)
	}
	p := &Plan{
		Title:   clampRunes(strings.TrimSpace(raw.Title), maxTitleLen),
		Summary: strings.TrimSpace(raw.Summary),
		Goal:    strings.TrimSpace(goal),
		Steps:   make([]Step, 0, len(raw.Steps)),
	}
	if p.Title == "" {
		p.Title = clampRunes(p.Goal, 40)
	}
	for i, rs := range raw.Steps {
		title := clampRunes(strings.TrimSpace(rs.Title), maxTitleLen)
		if title == "" {
			return nil, fmt.Errorf("第 %d 步缺少 title", i+1)
		}
		p.Steps = append(p.Steps, Step{
			N: i + 1, Title: title,
			Detail:   strings.TrimSpace(rs.Detail),
			DoneWhen: strings.TrimSpace(rs.DoneWhen),
			Tools:    cleanTools(rs.Tools),
			State:    StepPending,
		})
	}
	return p, nil
}

// MergeInto 把解析出的步骤写进已存在的草稿计划。
func MergeInto(dst *Plan, parsed *Plan) {
	dst.Title, dst.Summary = parsed.Title, parsed.Summary
	dst.Steps = parsed.Steps
	dst.State = StateAwaiting
}

// CorrectionHint 是解析失败后追加给模型的纠正说明。
func CorrectionHint(err error) string {
	return "\n\n（上一次输出无法解析为计划 JSON：" + err.Error() +
		"。请重新输出，只输出一个 JSON 对象，不要任何解释文字、不要代码围栏。）"
}

func clampRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:max])) + "…"
}

func cleanTools(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
