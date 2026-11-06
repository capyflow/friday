// interactions.go 承接两类「提问不走 request_permission」的 agent：
//
//   - CodeBuddy 的 AskUserQuestion 走私有扩展方法 `_codebuddy.ai/question`，
//     并且只在客户端声明 clientCapabilities._meta["codebuddy.ai"].question
//     后才把工具暴露给模型；
//   - MiniMax 的 ask_user 走 elicitation/create（客户端声明
//     clientCapabilities.elicitation.form 时），否则退化成一条窄权限请求。
//
// 两条通道都要求阻塞等用户作答，SDK 已把入站请求放在独立 goroutine，直接复用
// 审批队列的问答卡即可。这里只负责「协议字段 → 问题 → 卡片 → 答复」的翻译。
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// codeBuddyQuestionMethod 是 CodeBuddy 在 ACP 模式下的提问扩展方法。
const codeBuddyQuestionMethod = "_codebuddy.ai/question"

// clientCapabilitiesFor 按 agent 白名单声明交互能力：只对自己实测过的
// 通道开口，别把 elicitation 之类的实验能力广播给所有 agent（会改变它们
// 的工具暴露面）。
func clientCapabilitiesFor(cfg config.AcpAgentConfig) acp.ClientCapabilities {
	caps := acp.ClientCapabilities{
		Fs:       acp.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
		Terminal: false,
	}
	switch {
	case agentTextContains(cfg, "codebuddy"):
		caps.Meta = map[string]any{"codebuddy.ai": map[string]any{"question": true}}
	case agentTextContains(cfg, "minimax"):
		caps.Elicitation = &acp.ElicitationCapabilities{
			Form: &acp.ElicitationFormCapabilities{},
		}
	}
	return caps
}

// agentTextContains 在配置名、命令与参数里找特征串：用户可以把 agent 改名，
// 但 npx 包名（@tencent-ai/codebuddy-code、@minimax-ai/code）改不掉。
func agentTextContains(cfg config.AcpAgentConfig, needle string) bool {
	parts := append([]string{cfg.Name, cfg.Command}, cfg.Args...)
	for _, part := range parts {
		if strings.Contains(strings.ToLower(part), needle) {
			return true
		}
	}
	return false
}

// setInteractionHooks 在进程拉起后补挂提问钩子。spawn 的签名只带权限钩子，
// 这里在 initialize 返回、任何 session 建立之前装配，不存在并发读写。
func (p *agentProc) setInteractionHooks(onElicitation elicitationHook, onExtension extensionHook) {
	if p == nil || p.bridge == nil {
		return
	}
	p.bridge.onElicitation = onElicitation
	p.bridge.onExtension = onExtension
}

// elicitationHookFor 把 elicitation/create 按 sessionId 路由回所属 thread。
func (m *Manager) elicitationHookFor(key, agentName string) elicitationHook {
	return func(ctx context.Context, form acp.UnstableCreateElicitationForm) (acp.UnstableCreateElicitationResponse, error) {
		thID := m.threadForSession(key, form.SessionId)
		if thID == "" {
			log.Printf("[智能体] %s 的 elicitation 请求找不到所属会话，按未回答处理", agentName)
			return cancelledElicitation(), nil
		}
		return m.handleElicitation(ctx, thID, agentName, form)
	}
}

// extensionHookFor 把 CodeBuddy 的提问扩展方法路由回所属 thread。未知扩展
// 方法回 method not found，保持协议行为。
func (m *Manager) extensionHookFor(key, agentName string) extensionHook {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method != codeBuddyQuestionMethod {
			return nil, acp.NewMethodNotFound(method)
		}
		var head struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &head); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": "提问参数不是合法 JSON"})
		}
		thID := m.threadForSession(key, head.SessionID)
		if thID == "" {
			log.Printf("[智能体] %s 的提问找不到所属会话，按未回答处理", agentName)
			return codeBuddyCancelled("找不到所属会话"), nil
		}
		return m.handleCodeBuddyQuestion(ctx, thID, agentName, params)
	}
}

// threadForSession 先按 sessionId 精确匹配；没有 sessionId 时，若这条连接
// 上只有一个会话，就归给它（兼容不带 sessionId 的实现）。
func (m *Manager) threadForSession(key, sessionID string) string {
	ps := m.procOf(key)
	if ps == nil {
		return ""
	}
	if sessionID != "" {
		return ps.owner(acp.SessionId(sessionID))
	}
	return ps.onlyOwner()
}

// questionChoice 是一个可回传的答案：Label/Description 用于按钮，Value 是
// 回给 agent 的原始值（CodeBuddy 用文案，MiniMax 用 option id）。
type questionChoice struct {
	Label       string
	Description string
	Value       string
}

// questionPrompt 是一道题在内部的中性形状。AllowText 表示除了选项还接受
// 自由文本；OtherKey 是 MiniMax schema 里配套的 `<stepId>__other` 字段名。
type questionPrompt struct {
	ID              string
	Header          string
	Question        string
	MultiSelect     bool
	Choices         []questionChoice
	AllowText       bool
	OtherKey        string
	TextPlaceholder string
}

// questionAnswer 是一次作答：要么选中某个选项（多选时是 Choices 全部勾选项），
// 要么用户自己输入了文本。
type questionAnswer struct {
	Choice  questionChoice
	Choices []questionChoice
	Text    string
	Custom  bool
}

// answerValues 把一次选项作答翻成回传给 agent 的值：多选是选中值的数组
// （只勾一项也是数组，schema 里 array 字段认的就是这个形状），单选是单个值。
// 自由文本不在其列——CodeBuddy 与 MiniMax 的文本落点不同，由各 handler 自己落。
func (p questionPrompt) answerValues(a questionAnswer) any {
	if p.MultiSelect {
		choices := a.Choices
		if len(choices) == 0 && a.Choice.Value != "" {
			choices = []questionChoice{a.Choice}
		}
		vals := make([]string, 0, len(choices))
		for _, c := range choices {
			vals = append(vals, c.Value)
		}
		return vals
	}
	return a.Choice.Value
}

func (p questionPrompt) group() approval.QuestionGroup {
	g := approval.QuestionGroup{Header: p.Header, Question: p.Question, MultiSelect: p.MultiSelect}
	for _, c := range p.Choices {
		g.Options = append(g.Options, approval.QuestionOption{Label: c.Label, Description: c.Description})
	}
	return g
}

// codeBuddyCancelled 是 CodeBuddy 约定的"用户没答"形状。
func codeBuddyCancelled(reason string) map[string]any {
	return map[string]any{"outcome": "cancelled", "reason": reason}
}

// handleCodeBuddyQuestion 解析 `_codebuddy.ai/question` 的 schema.questions，
// 一题一张卡顺序问完，按 q_0/q_1 的键回 answers。
func (m *Manager) handleCodeBuddyQuestion(ctx context.Context, threadID, agentName string, params json.RawMessage) (any, error) {
	prompts, ids, err := parseCodeBuddyQuestions(params)
	if err != nil {
		log.Printf("[智能体] %s 的提问无法解析（按未回答处理）: %v", agentName, err)
		return codeBuddyCancelled("问题格式无法解析"), nil
	}
	answers := make(map[string]any, len(prompts))
	for i, p := range prompts {
		answer, ok := m.askPrompt(ctx, threadID, agentName, p)
		if !ok {
			return codeBuddyCancelled("用户未回答"), nil
		}
		if answer.Custom {
			// CodeBuddy 的自由文本就是答案本身。
			answers[ids[i]] = answer.Text
			continue
		}
		answers[ids[i]] = p.answerValues(answer)
	}
	return map[string]any{"outcome": "submitted", "answers": answers}, nil
}

// parseCodeBuddyQuestions 宽松解析 CodeBuddy 的提问参数：扩展方法把它放在
// schema.questions，interruption 的 toolInput 直接就是 questions。字段名按
// 协议固定，但 id/question/options 允许缺失（缺了用序号和 header 兜底）。
func parseCodeBuddyQuestions(params json.RawMessage) ([]questionPrompt, []string, error) {
	var payload struct {
		Questions []codeBuddyQuestion `json:"questions"`
		Schema    struct {
			Questions []codeBuddyQuestion `json:"questions"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return nil, nil, err
	}
	questions := payload.Schema.Questions
	if len(questions) == 0 {
		questions = payload.Questions
	}
	if len(questions) == 0 {
		return nil, nil, errors.New("questions 为空")
	}
	prompts := make([]questionPrompt, 0, len(questions))
	ids := make([]string, 0, len(questions))
	for i, q := range questions {
		choices := make([]questionChoice, 0, len(q.Options))
		for _, o := range q.Options {
			label := strings.TrimSpace(o.Label)
			if label == "" {
				continue
			}
			// CodeBuddy 的 answer 是文案本身，不是 option id。
			choices = append(choices, questionChoice{
				Label: label, Description: strings.TrimSpace(o.Description), Value: label,
			})
		}
		if len(choices) == 0 {
			return nil, nil, fmt.Errorf("第 %d 题没有可选项", i+1)
		}
		id := strings.TrimSpace(q.ID)
		if id == "" {
			id = fmt.Sprintf("q_%d", i)
		}
		question := strings.TrimSpace(q.Question)
		if question == "" {
			question = strings.TrimSpace(q.Header)
		}
		prompts = append(prompts, questionPrompt{
			ID: id, Header: strings.TrimSpace(q.Header), Question: question,
			MultiSelect: q.MultiSelect, Choices: choices,
			AllowText: true, TextPlaceholder: "也可以直接输入你的答案…",
		})
		ids = append(ids, id)
	}
	return prompts, ids, nil
}

// codeBuddyQuestion 是 CodeBuddy 单题的协议形状（扩展方法与 interruption 共用）。
type codeBuddyQuestion struct {
	ID          string `json:"id"`
	Question    string `json:"question"`
	Header      string `json:"header"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

// codeBuddyInterruption 是 CodeBuddy 在 session_info_update 里广播的交互请求。
// AskUserQuestion 的题干在 toolInput.questions 里，答复要通过
// _codebuddy.ai/resolveInterruption 回传。
type codeBuddyInterruption struct {
	InterruptionID string          `json:"interruptionId"`
	ToolCallID     string          `json:"toolCallId"`
	ToolName       string          `json:"toolName"`
	ToolInput      json.RawMessage `json:"toolInput"`
}

// codeBuddyInterruptionFrom 从 session_info_update 的 _meta 里取中断请求。
func codeBuddyInterruptionFrom(meta map[string]any) *codeBuddyInterruption {
	raw, ok := meta["codebuddy.ai/interruptionRequest"]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ir codeBuddyInterruption
	if json.Unmarshal(b, &ir) != nil {
		return nil
	}
	return &ir
}

// handleCodeBuddyInterruption 处理 AskUserQuestion 的 interruption：
// 逐题出卡，答完调 resolveInterruption 回 answers；没答回 deny。
// 必须异步跑（事件泵不能被卡片阻塞，见 bridge.go 的 C6 纪律）。
func (m *Manager) handleCodeBuddyInterruption(ps *procState, sessionID acp.SessionId, threadID string, ir *codeBuddyInterruption) {
	if ir.ToolName != "AskUserQuestion" {
		return
	}
	agentName := ps.proc.agent
	prompts, ids, err := parseCodeBuddyQuestions(ir.ToolInput)
	if err != nil {
		log.Printf("[智能体] %s 的提问无法解析（按跳过处理）: %v", agentName, err)
		m.resolveCodeBuddyInterruption(ps, sessionID, ir.ToolCallID, "deny", nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(),
		config.AcpPermissionTimeoutSeconds*time.Second+time.Minute)
	defer cancel()
	answers := make(map[string]any, len(prompts))
	for i, p := range prompts {
		answer, ok := m.askPrompt(ctx, threadID, agentName, p)
		if !ok {
			m.resolveCodeBuddyInterruption(ps, sessionID, ir.ToolCallID, "deny", nil)
			return
		}
		if answer.Custom {
			answers[ids[i]] = answer.Text
			continue
		}
		answers[ids[i]] = p.answerValues(answer)
	}
	m.resolveCodeBuddyInterruption(ps, sessionID, ir.ToolCallID, "allow", answers)
}

// resolveCodeBuddyInterruption 把答案发回 CodeBuddy。deny 时 answers 可为空，
// CodeBuddy 会把该题标成 skipped，模型据此继续。
func (m *Manager) resolveCodeBuddyInterruption(ps *procState, sessionID acp.SessionId, toolCallID, decision string, answers map[string]any) {
	if ps == nil || ps.proc == nil || ps.proc.conn == nil || toolCallID == "" {
		return
	}
	params := map[string]any{
		"sessionId":  string(sessionID),
		"toolCallId": toolCallID,
		"decision":   decision,
	}
	if len(answers) > 0 {
		params["answers"] = answers
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := ps.proc.conn.CallExtension(ctx, "_codebuddy.ai/resolveInterruption", params); err != nil {
		log.Printf("[智能体] %s 回答 CodeBuddy 提问失败: %v", ps.proc.agent, err)
	}
}

// handleElicitation 把 elicitation 表单逐题变成问答卡；答复按
// requestedSchema 的键回填 content。
func (m *Manager) handleElicitation(ctx context.Context, threadID, agentName string, form acp.UnstableCreateElicitationForm) (acp.UnstableCreateElicitationResponse, error) {
	prompts, err := elicitationPrompts(form)
	if err != nil {
		log.Printf("[智能体] %s 的 elicitation 无法转成问答卡（按未回答处理）: %v", agentName, err)
		return cancelledElicitation(), nil
	}
	content := make(map[string]any, len(prompts))
	for _, p := range prompts {
		answer, ok := m.askPrompt(ctx, threadID, agentName, p)
		if !ok {
			return cancelledElicitation(), nil
		}
		if answer.Custom {
			if p.OtherKey != "" {
				// MiniMax 的 Other 字段是 `<stepId>__other`；Jx 优先读它。
				content[p.OtherKey] = answer.Text
			} else {
				content[p.ID] = answer.Text
			}
			continue
		}
		content[p.ID] = p.answerValues(answer)
	}
	return acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: content},
	}, nil
}

// elicitationPrompts 解析 form 的 JSON Schema：支持 oneOf/anyOf 的单选与
// items.anyOf 的多选。自由文本字段（没有可选值）不渲染——现有的卡没有输入框，
// 必答的自由文本会让整张表单按未回答结算，宁可不给半截答案。
func elicitationPrompts(form acp.UnstableCreateElicitationForm) ([]questionPrompt, error) {
	props := form.RequestedSchema.Properties
	if len(props) == 0 {
		return nil, errors.New("requestedSchema.properties 为空")
	}
	required := make(map[string]bool, len(form.RequestedSchema.Required))
	for _, k := range form.RequestedSchema.Required {
		required[k] = true
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]questionPrompt, 0, len(keys))
	for _, key := range keys {
		// MiniMax 的 "Other" 输入框是主字段的配套字段，单独处理的只是主字段。
		if strings.HasSuffix(key, "__other") {
			continue
		}
		prop, ok := props[key].(map[string]any)
		if !ok {
			continue
		}
		question := firstAnyString(prop, "title", "label", "description")
		if question == "" {
			question = strings.TrimSpace(form.Message)
		}
		typ := strings.ToLower(firstAnyString(prop, "type"))
		choices := elicitationChoices(prop)
		otherKey := ""
		if _, ok := props[key+"__other"].(map[string]any); ok {
			otherKey = key + "__other"
		}
		p := questionPrompt{
			ID: key, Question: question, MultiSelect: typ == "array", Choices: choices,
		}
		switch {
		case len(choices) > 0 && otherKey != "":
			p.AllowText = true
			p.OtherKey = otherKey
			p.TextPlaceholder = "其他（自己输入）…"
		case len(choices) == 0 && typ == "string":
			// 纯自由文本字段。可选字段不渲染，避免卡片只能答不能跳。
			if !required[key] {
				continue
			}
			p.AllowText = true
			p.TextPlaceholder = "请输入你的答案…"
		case len(choices) == 0:
			if required[key] {
				return nil, fmt.Errorf("必答题 %q 既没有可选项也不是文本", key)
			}
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("没有可渲染的选项题")
	}
	return out, nil
}

// elicitationChoices 从 JSON Schema 里取选项：单选的 oneOf/anyOf，多选的
// items.anyOf。const 是回传给 agent 的 option id，title 是按钮文案。
func elicitationChoices(prop map[string]any) []questionChoice {
	raw := prop["oneOf"]
	if raw == nil {
		raw = prop["anyOf"]
	}
	if raw == nil {
		if items, ok := prop["items"].(map[string]any); ok {
			raw = items["oneOf"]
			if raw == nil {
				raw = items["anyOf"]
			}
		}
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]questionChoice, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		label := firstAnyString(m, "title", "label", "const")
		if label == "" {
			continue
		}
		value := firstAnyString(m, "const")
		if value == "" {
			value = label
		}
		out = append(out, questionChoice{
			Label: label, Description: firstAnyString(m, "description", "detail"), Value: value,
		})
	}
	return out
}

// askPrompt 把一道题入队成问答卡并阻塞等作答。返回 false 表示没答（超时、
// 撤卡或用户选了取消项）。
func (m *Manager) askPrompt(ctx context.Context, threadID, agentName string, prompt questionPrompt) (questionAnswer, bool) {
	if m.approver == nil || (len(prompt.Choices) == 0 && !prompt.AllowText) {
		return questionAnswer{}, false
	}
	m.mu.Lock()
	var turnDone chan struct{}
	if lv := m.live[threadID]; lv != nil {
		turnDone = lv.turnDone
	}
	m.mu.Unlock()
	if turnDone == nil {
		log.Printf("[智能体] %s 在无进行中对话时提问，按未回答处理", agentName)
		return questionAnswer{}, false
	}

	group := prompt.group()
	options := make([]approval.Option, 0, len(prompt.Choices))
	for i, c := range prompt.Choices {
		options = append(options, approval.Option{ID: fmt.Sprintf("q:o%d", i), Name: c.Label})
	}
	summary := strings.TrimSpace(prompt.Question)
	if summary == "" {
		summary = strings.TrimSpace(prompt.Header)
	}
	if summary == "" {
		summary = "请选择"
	}
	if prompt.Header != "" && prompt.Question != "" {
		summary = prompt.Header + " · " + prompt.Question
	}
	timeout := config.AcpPermissionTimeoutSeconds * time.Second
	card := m.approver.RequestCard(approval.Card{
		Tool: "智能体提问", Summary: summary,
		Detail:          questionDetail(agentName, []approval.QuestionGroup{group}),
		Kind:            approval.KindQuestion,
		Options:         options,
		Questions:       []approval.QuestionGroup{group},
		AllowText:       prompt.AllowText,
		TextPlaceholder: prompt.TextPlaceholder,
		MultiSelect:     prompt.MultiSelect,
	}, timeout)
	hint := "选一项"
	if prompt.MultiSelect {
		hint = "可多选：勾选若干项后提交"
	}
	if prompt.AllowText {
		hint += "，或直接输入你的答案"
	}
	m.notice(fmt.Sprintf("acp:%s:question:%d", threadID, card.ID()),
		fmt.Sprintf("%s 在等你回答：%s。回到 Friday 的 agent 对话页%s；"+
			"%d 分钟内无人处理按未选择结算。", agentName, summary, hint, timeout/time.Minute))
	go func() {
		select {
		case <-ctx.Done():
			card.Cancel()
		case <-turnDone:
			card.Cancel()
		case <-card.Done():
		}
	}()

	out, err := card.WaitDecision()
	if err != nil && !errors.Is(err, approval.ErrDenied) {
		log.Printf("[智能体] %s 提问结算异常（按未回答处理）: %v", agentName, err)
	}
	if out.Text != "" {
		log.Printf("[智能体] %s 提问已回答（自由输入）: %s → %s", agentName,
			sanitizeLog(summary), sanitizeLog(out.Text))
		return questionAnswer{Text: out.Text, Custom: true}, true
	}
	// 多选卡一次结算带回多条勾选；单选仍是单条 OptionID。
	ids := out.OptionIDs
	if len(ids) == 0 && out.OptionID != "" {
		ids = []string{out.OptionID}
	}
	if len(ids) == 0 {
		log.Printf("[智能体] %s 提问未回答: %s", agentName, sanitizeLog(summary))
		return questionAnswer{}, false
	}
	chosen := make([]questionChoice, 0, len(ids))
	labels := make([]string, 0, len(ids))
	for _, oid := range ids {
		for i, o := range options {
			if o.ID != oid {
				continue
			}
			chosen = append(chosen, prompt.Choices[i])
			labels = append(labels, prompt.Choices[i].Label)
			break
		}
	}
	if len(chosen) == 0 {
		log.Printf("[智能体] %s 提问结算到未知选项（按未回答处理）: %s", agentName, sanitizeLog(summary))
		return questionAnswer{}, false
	}
	log.Printf("[智能体] %s 提问已回答: %s → %s", agentName,
		sanitizeLog(summary), sanitizeLog(strings.Join(labels, "、")))
	if len(chosen) == 1 {
		return questionAnswer{Choice: chosen[0], Choices: chosen}, true
	}
	return questionAnswer{Choices: chosen}, true
}

// firstAnyString 返回 map 中第一个非空字符串字段。
func firstAnyString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
