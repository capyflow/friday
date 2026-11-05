// question.go 识别「agent 借权限通道问用户」这一类请求，并整理成问答卡的数据。
//
// 为什么要专门判：ACP 的 PermissionOption.kind 只允许 allow_once/allow_always/
// reject_once/reject_always，optionId 与 name 却是自由串——所以 agent 的
// AskUserQuestion、ExitPlanMode 这类交互只能把答案编码成选项，走
// session/request_permission 发出来（zcode 就是这样，它自己客户端里有个
// isUserInputBackedPermissionToolName 专门认这两个工具名）。一律压成
// 「批准/拒绝」就把答案丢了：用户看到一坨 rawInput JSON 加三个授权按钮，
// 点下去 agent 也不知道他选了哪条。
//
// 判定规则（按证据强度排，任一命中即问答卡）：
//  1. rawInput 里有 questions 数组——AskUserQuestion 的正文形状，唯一可靠的结构证据；
//  2. toolCall 的标题/类型命中 AskUserQuestion 这类工具名；
//  3. 选项 id 全是「<label>:yes / <label>:no」——zcode 多选题的专用方案；
//  4. 有一条选项既不像允许也不像拒绝——答案就编码在选项里。
//
// 判不准时宁可留在授权卡（原行为），不要把普通授权变成一堆按钮。
//
// 别再按「选项多于两个」判：ACP 的 kind 有 allow_once/allow_always/reject_once/
// reject_always 四种，本机 zcode 的沙箱授权就一次给满 4 条（仅此一次/始终允许/
// 拒绝一次/始终拒绝），按条数判会把每一条命令权限都变成问答卡。
package acp

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/approval"
)

// permissionIDTokens 是授权答复的 optionId 词元。id 按「非字母数字」切段后逐段
// 比对，所以 sandbox_allow_once（含 allow）算授权，q1_opt_keep、__skip__
// （skip 不在表里）算答案；用切段而不是子串，免得 nosql 被 "no" 误伤。
var permissionIDTokens = map[string]bool{
	"allow": true, "approve": true, "permit": true, "proceed": true, "yes": true,
	"reject": true, "deny": true, "no": true, "cancel": true, "cancelled": true, "abort": true,
	"once": true, "always": true,
}

// permissionNameMarkers 做包含匹配：本地化文案常带解释，「同意——退出计划模式」
// 「仅此一次」这种也得认下来。短词（yes/no/是/否）不进这张表，走全等比较。
var permissionNameMarkers = []string{
	"allow", "approve", "permit", "proceed", "reject", "deny", "cancel", "abort",
	"允许", "批准", "同意", "通过", "拒绝", "取消", "一次", "始终", "一直",
}

var permissionNameExact = map[string]bool{
	"yes": true, "no": true, "ok": true, "是": true, "否": true, "好": true, "行": true,
}

// questionToolHints 是"这个工具就是在问用户"的名字片段（小写包含比较）。
// toolCall.title 通常被 agent 拿去放问题正文，工具名未必送过来，所以只作辅助信号。
// ExitPlanMode 不在其列：它问的是"可不可以退出计划模式"，授权卡就是对的形状。
var questionToolHints = []string{"askuserquestion", "ask_user_question", "askuser"}

// isQuestionRequest 判这条权限请求是不是在问用户。
func isQuestionRequest(r acp.RequestPermissionRequest) bool {
	if len(questionGroups(r)) > 0 {
		return true
	}
	if hint := strings.ToLower(toolCallText(r.ToolCall)); hint != "" {
		for _, needle := range questionToolHints {
			if strings.Contains(hint, needle) {
				return true
			}
		}
	}
	if len(r.Options) == 0 {
		return false
	}
	if allAnswerPairs(r.Options) {
		return true
	}
	for _, o := range r.Options {
		if !isPermissionOption(o) {
			return true // 自定义选项 = 答案编码在这里
		}
	}
	return false
}

// allAnswerPairs 认 zcode 多选题的选项方案：每条 id 都是「<label>:yes」或
// 「<label>:no」，授权答复不会长这样。多选题不带 __skip__ 兜底项，而那些
// label 本身（「包含：接口A」）又确实会被词元判成答案，所以这里单独拦一道；
// 正常路径靠规则 1（rawInput 带 questions）先命中，这条只兜住 questions 缺失的退化形状。
func allAnswerPairs(opts []acp.PermissionOption) bool {
	for _, o := range opts {
		id := strings.ToLower(strings.TrimSpace(string(o.OptionId)))
		head, tail, ok := strings.Cut(id, ":")
		if !ok || head == "" || (tail != "yes" && tail != "no") {
			return false
		}
	}
	return true
}

// isPermissionOption 判一条选项属不属于「允许/拒绝」这一类授权答复：
// id 命中授权词元，或文案命中授权措辞，都算。两边都判——有的 agent 用规范
// id + 本地化文案，有的用自定义 id + 规范文案。
func isPermissionOption(o acp.PermissionOption) bool {
	for _, tok := range strings.FieldsFunc(string(o.OptionId), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if permissionIDTokens[strings.ToLower(tok)] {
			return true
		}
	}
	name := strings.ToLower(strings.TrimSpace(o.Name))
	if permissionNameExact[name] {
		return true
	}
	for _, marker := range permissionNameMarkers {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// toApprovalOptions 把 agent 给的权限选项原样搬进卡片：按钮文案用 name（空则回
// id），回传用 optionId——协议只认 agent 自己给过的那些 id，不能自己编。
func toApprovalOptions(opts []acp.PermissionOption) []approval.Option {
	out := make([]approval.Option, 0, len(opts))
	for _, o := range opts {
		name := strings.TrimSpace(o.Name)
		if name == "" {
			name = string(o.OptionId)
		}
		out = append(out, approval.Option{
			ID: string(o.OptionId), Name: name, Kind: string(o.Kind),
		})
	}
	return out
}

// questionGroups 从 rawInput 还原问题正文。字段名一律宽松兼容（label/title/name
// 都当选项文案，question/text/title 都当题干），单题形态 {question, options} 也认。
func questionGroups(r acp.RequestPermissionRequest) []approval.QuestionGroup {
	raw, err := json.Marshal(r.ToolCall.RawInput)
	if err != nil {
		return nil
	}
	var payload struct {
		Question    string           `json:"question"`
		Text        string           `json:"text"`
		Header      string           `json:"header"`
		MultiSelect bool             `json:"multiSelect"`
		Options     []map[string]any `json:"options"`
		Questions   []map[string]any `json:"questions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	items := payload.Questions
	if len(items) == 0 {
		if strings.TrimSpace(payload.Question+payload.Text) == "" && len(payload.Options) == 0 {
			return nil
		}
		items = []map[string]any{{
			"question": payload.Question, "text": payload.Text, "header": payload.Header,
			"multiSelect": payload.MultiSelect, "options": payload.Options,
		}}
	}
	out := make([]approval.QuestionGroup, 0, len(items))
	for _, item := range items {
		g := approval.QuestionGroup{
			Header:      firstString(item, "header"),
			Question:    firstString(item, "question", "text", "title", "message"),
			MultiSelect: firstBool(item, "multiSelect", "multi_select"),
		}
		for _, opt := range firstOptions(item) {
			g.Options = append(g.Options, approval.QuestionOption{
				Label:       firstString(opt, "label", "title", "name", "value"),
				Description: firstString(opt, "description", "detail", "hint"),
			})
		}
		if g.Question == "" && len(g.Options) == 0 {
			continue
		}
		out = append(out, g)
	}
	return out
}

// questionDetail 给卡片正文一个可读兜底：前端按 Questions 结构化排版，但通知中心、
// 日志和旧前端只会看这一行文本，不能是 JSON。
func questionDetail(agent string, groups []approval.QuestionGroup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "智能体 %s 在等你回答（%d 个问题）\n", agent, len(groups))
	for i, g := range groups {
		head := g.Question
		if g.Header != "" {
			head = g.Header + " · " + head
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, head)
		for j, o := range g.Options {
			if o.Description != "" {
				fmt.Fprintf(&b, "   %c) %s —— %s\n", rune('a'+j), o.Label, clipLine(o.Description, 120))
				continue
			}
			fmt.Fprintf(&b, "   %c) %s\n", rune('a'+j), o.Label)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// toolCallText 取 toolCall 上可用来认工具名的文本（title 优先，kind 次之）。
func toolCallText(tc acp.ToolCallUpdate) string {
	if tc.Title != nil && strings.TrimSpace(*tc.Title) != "" {
		return strings.TrimSpace(*tc.Title)
	}
	if tc.Kind != nil {
		return string(*tc.Kind)
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v
		}
	}
	return false
}

func firstOptions(m map[string]any) []map[string]any {
	switch raw := m["options"].(type) {
	case []map[string]any: // 单题形态是自己拼出来的（已定型）
		return append([]map[string]any{}, raw...)
	case []any:
		out := make([]map[string]any, 0, len(raw))
		for _, v := range raw {
			if opt, ok := v.(map[string]any); ok {
				out = append(out, opt)
				continue
			}
			// 有的 agent 只给字符串选项：文案即选项。
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, map[string]any{"label": strings.TrimSpace(s)})
			}
		}
		return out
	}
	return nil
}
