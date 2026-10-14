package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/skill"
)

// recordingProvider 记录最近一次 ChatRequest，用于断言技能是否真的生效
// （指令注入 / 模型覆盖 / 工具面白名单）。
type recordingProvider struct {
	mu       sync.Mutex
	lastReq  *llm.ChatRequest
	calls    int
	response string
}

func (p *recordingProvider) Name() string       { return "rec" }
func (p *recordingProvider) ContextWindow() int { return 128000 }

func (p *recordingProvider) Chat(_ context.Context, req *llm.ChatRequest, _ func(llm.Delta) error) (*llm.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	// 复制一份指针即可：框架每轮新建 req。
	p.lastReq = req
	text := p.response
	if text == "" {
		text = "回答"
	}
	return &llm.ChatResponse{Message: llm.NewTextMessage(llm.RoleAssistant, text)}, nil
}

func (p *recordingProvider) snapshot() (*llm.ChatRequest, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastReq, p.calls
}

// stubTool 是注册表里的占位工具。
type stubTool struct{ name string }

func (t *stubTool) Name() string        { return t.name }
func (t *stubTool) Description() string { return "测试工具 " + t.name }
func (t *stubTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *stubTool) Call(context.Context, map[string]any) (string, error) { return "ok", nil }

// skillBrain 组一个带技能仓库的 Brain。
func skillBrain(t *testing.T, skillFiles map[string]string, autoTrigger bool) (*Brain, *recordingProvider, *skill.Store) {
	t.Helper()
	dir := t.TempDir()
	for id, raw := range skillFiles {
		path := filepath.Join(dir, id, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 用户目录隔离到另一个临时 HOME，避免读到本机真实技能。
	t.Setenv("HOME", t.TempDir())
	store := skill.NewStore(dir)

	reg := vxagent.NewRegistry()
	for _, name := range []string{"alpha", "beta"} {
		if err := reg.Add(&stubTool{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	provider := &recordingProvider{}
	ag := vxagent.New(vxagent.Options{
		Provider:     provider,
		Registry:     reg,
		Model:        "base-model",
		SystemPrompt: "你是小五。",
	})
	pool := NewSessionPool(nil)
	brain := NewBrain(ag, pool, nil)
	brain.SetSkills(store, autoTrigger)
	return brain, provider, store
}

const skillMorning = `---
name: 晨会准备
description: 汇总今日待办与天气，产出晨会简报
allowed-tools: [alpha]
auto: true
triggers: ["晨会"]
---
先报日期，再报天气，最后汇报未完成待办。
`

const skillStrict = `---
name: 严格模型
description: 换个模型试试
model: other-model
temperature: 0.1
auto: true
keywords: ["换个模型"]
---
按更稳定的方式回答。
`

func firstSystemText(req *llm.ChatRequest) string {
	if req == nil {
		return ""
	}
	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem {
			var b strings.Builder
			for _, c := range m.Content {
				b.WriteString(c.Text)
			}
			return b.String()
		}
	}
	return ""
}

func lastUserText(req *llm.ChatRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleUser {
			return req.Messages[i].Content[0].Text
		}
	}
	return ""
}

func toolNames(req *llm.ChatRequest) []string {
	if req == nil {
		return nil
	}
	out := make([]string, 0, len(req.Tools))
	for _, td := range req.Tools {
		out = append(out, td.Name)
	}
	return out
}

// TestAskWithExplicitSkill 显式点名：剥掉调用前缀，注入技能指令并套上工具白名单。
func TestAskWithExplicitSkill(t *testing.T) {
	brain, provider, _ := skillBrain(t, map[string]string{"morning": skillMorning}, true)

	if _, err := brain.AskWith(context.Background(), "/skill 晨会准备 只看天气", nil, nil); err != nil {
		t.Fatalf("AskWith: %v", err)
	}
	req, calls := provider.snapshot()
	if calls != 1 {
		t.Fatalf("应调用模型 1 次，实际 %d", calls)
	}
	if !strings.Contains(firstSystemText(req), "<skill-instruction>") {
		t.Errorf("系统提示词应注入技能指令，实际: %s", firstSystemText(req))
	}
	if !strings.Contains(firstSystemText(req), "先报日期") {
		t.Error("技能正文未注入")
	}
	if got := lastUserText(req); got != "只看天气" {
		t.Errorf("传给模型的提问应剥掉 /skill 前缀，实际 %q", got)
	}
	names := strings.Join(toolNames(req), ",")
	if !strings.Contains(names, "alpha") || strings.Contains(names, "beta") {
		t.Errorf("allowed-tools 应只放行 alpha，实际 %q", names)
	}
}

// TestAskWithUnknownSkillDoesNotCallModel 点名不存在的技能要明确回绝，
// 既不扣 token，也不悄悄降级去猜一个技能。
func TestAskWithUnknownSkillDoesNotCallModel(t *testing.T) {
	brain, provider, _ := skillBrain(t, map[string]string{"morning": skillMorning}, true)

	answer, err := brain.AskWith(context.Background(), "/skill 没有这个技能 正文", nil, nil)
	if err != nil {
		t.Fatalf("应正常返回而非报错: %v", err)
	}
	if !strings.Contains(answer, "没有叫") {
		t.Errorf("回复应说明技能不存在，实际 %q", answer)
	}
	if _, calls := provider.snapshot(); calls != 0 {
		t.Errorf("不该调用模型，实际 %d 次", calls)
	}
}

// TestAskWithAutoMatch 自动匹配只对 auto: true 的技能生效，且靠触发词命中。
func TestAskWithAutoMatch(t *testing.T) {
	brain, provider, _ := skillBrain(t, map[string]string{"morning": skillMorning}, true)

	if _, err := brain.AskWith(context.Background(), "晨会", nil, nil); err != nil {
		t.Fatalf("AskWith: %v", err)
	}
	req, _ := provider.snapshot()
	if !strings.Contains(firstSystemText(req), "<skill-instruction>") {
		t.Error("触发词命中应注入技能")
	}

	// 关键词打分命中另一个技能（换个模型 是它的 keyword）
	brain2, provider2, _ := skillBrain(t, map[string]string{"strict": skillStrict}, true)
	if _, err := brain2.AskWith(context.Background(), "帮我换个模型说说看", nil, nil); err != nil {
		t.Fatalf("AskWith: %v", err)
	}
	req2, _ := provider2.snapshot()
	if req2.Model != "other-model" {
		t.Errorf("技能的 model 应覆盖本轮模型，实际 %q", req2.Model)
	}
	if req2.Temperature == nil || *req2.Temperature != 0.1 {
		t.Errorf("技能的 temperature 应生效，实际 %v", req2.Temperature)
	}
}

// TestAskWithAutoTriggerSwitchOff 全局自动匹配关闭后，只有显式点名还能用。
func TestAskWithAutoTriggerSwitchOff(t *testing.T) {
	brain, provider, _ := skillBrain(t, map[string]string{"morning": skillMorning}, false)

	if _, err := brain.AskWith(context.Background(), "晨会", nil, nil); err != nil {
		t.Fatalf("AskWith: %v", err)
	}
	req, _ := provider.snapshot()
	if strings.Contains(firstSystemText(req), "<skill-instruction>") {
		t.Error("autoTrigger=false 时不应自动注入技能")
	}

	if _, err := brain.AskWith(context.Background(), "晨会", nil, nil, WithSkillName("晨会准备")); err != nil {
		t.Fatalf("AskWith 显式指定: %v", err)
	}
	req2, _ := provider.snapshot()
	if !strings.Contains(firstSystemText(req2), "<skill-instruction>") {
		t.Error("显式 WithSkillName 应无视自动匹配开关")
	}
}

// TestAskWithWithoutAutoSkill 机器生成的提示词（心跳等）不得被自动匹配。
func TestAskWithWithoutAutoSkill(t *testing.T) {
	brain, provider, _ := skillBrain(t, map[string]string{"morning": skillMorning}, true)

	prompt := "你是用户的私人生活管家小五，现在执行一条到点的定时智能任务…晨会 检查一下"
	if _, err := brain.AskWith(context.Background(), prompt, nil, nil, WithoutAutoSkill()); err != nil {
		t.Fatalf("AskWith: %v", err)
	}
	req, _ := provider.snapshot()
	if strings.Contains(firstSystemText(req), "<skill-instruction>") {
		t.Error("WithoutAutoSkill 应跳过自动匹配")
	}
}

// TestAskWithCustomCommandBeatsAutoMatch 自定义指令命中时不做技能自动匹配：
// 触发词是用户显式声明的，优先级高于启发式。
func TestAskWithCustomCommandBeatsAutoMatch(t *testing.T) {
	brain, provider, _ := skillBrain(t, map[string]string{"morning": skillMorning}, true)
	brain.SetCustom([]config.CustomCommand{{Name: "晨会", Prompt: "展开后的自定义指令正文"}})

	if _, err := brain.AskWith(context.Background(), "晨会", nil, nil); err != nil {
		t.Fatalf("AskWith: %v", err)
	}
	req, _ := provider.snapshot()
	if got := lastUserText(req); !strings.Contains(got, "展开后的自定义指令正文") {
		t.Errorf("自定义指令应已展开，实际 %q", got)
	}
	if strings.Contains(firstSystemText(req), "先报日期") {
		t.Error("命中自定义指令时不应再自动套技能")
	}
}

// TestResumeReusesPausedSkill 继续被暂停的回答时必须复用原技能，
// 否则工具面/模型在"继续"时静默漂移。
func TestResumeReusesPausedSkill(t *testing.T) {
	brain, provider, _ := skillBrain(t, map[string]string{"morning": skillMorning}, true)

	if _, err := brain.Resume(context.Background(), SessionWeb, "原问题", "已生成的一半", nil,
		WithSkillName("晨会准备")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	req, calls := provider.snapshot()
	if calls != 1 {
		t.Fatalf("应调用模型 1 次，实际 %d", calls)
	}
	if !strings.Contains(firstSystemText(req), "<skill-instruction>") {
		t.Error("Resume 应沿用暂停前的技能")
	}
	// 继续指令是机器文本：即使里面含触发词也不该再猜技能
	brain2, provider2, _ := skillBrain(t, map[string]string{"morning": skillMorning}, true)
	if _, err := brain2.Resume(context.Background(), SessionWeb, "晨会 原问题", "", nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	req2, _ := provider2.snapshot()
	if strings.Contains(firstSystemText(req2), "<skill-instruction>") {
		t.Error("Resume 未带技能时不应做自动匹配")
	}
}

// TestAskKeepsLegacySignature Ask（不带选项）与图片路径行为不变。
func TestAskKeepsLegacySignature(t *testing.T) {
	brain, provider, _ := skillBrain(t, map[string]string{}, true)

	if _, err := brain.Ask(context.Background(), "随便聊聊", nil, nil); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, calls := provider.snapshot(); calls != 1 {
		t.Errorf("普通对话应调用模型 1 次，实际 %d", calls)
	}

	imgs := []llm.Content{{Type: llm.ContentImage, Data: []byte("fake"), MIME: "image/png"}}
	if _, err := brain.Ask(context.Background(), "看图", imgs, nil); err != nil {
		t.Fatalf("Ask 带图片: %v", err)
	}
	req, calls := provider.snapshot()
	if calls != 2 {
		t.Fatalf("带图片应调用模型 2 次（累计），实际 %d", calls)
	}
	if len(req.Messages) < 3 {
		t.Errorf("图片消息应已在会话里，实际 %d 条", len(req.Messages))
	}
}

// TestSetSkillsNilStoreIsSafe 未启用技能层（store 为 nil）时提问不受影响。
func TestSetSkillsNilStoreIsSafe(t *testing.T) {
	brain, provider, _ := skillBrain(t, map[string]string{}, true)
	brain.SetSkills(nil, true)

	if _, err := brain.AskWith(context.Background(), "晨会", nil, nil); err != nil {
		t.Fatalf("AskWith: %v", err)
	}
	if _, calls := provider.snapshot(); calls != 1 {
		t.Errorf("应照常对话，实际调用 %d 次", calls)
	}
}
