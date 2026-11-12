package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// TestSystemPromptPersona 验证系统提示词按人设变化：默认人设、切换人设、
// 自定义覆盖、cfg.SystemPrompt 最高优先级。
func TestSystemPromptPersona(t *testing.T) {
	cfg := &config.Config{}

	// 1. 未配置人设：回退 default（贴心管家），共用段完整。
	p := systemPrompt(cfg)
	if !strings.Contains(p, "亲切干练") || !strings.Contains(p, "小五") {
		t.Fatalf("默认人设应为贴心管家且含身份定位:\n%s", p[:200])
	}
	if !strings.Contains(p, "【人文关怀】") || !strings.Contains(p, "【闪念：笔记与待办】") {
		t.Fatal("系统提示词应包含人文关怀与工具规则共用段")
	}

	// 2. 切到活泼调皮：提示词随之变化，且共用规则不丢。
	cfg.Persona.Active = "playful"
	playful := systemPrompt(cfg)
	if !strings.Contains(playful, "元气满满") || strings.Contains(playful, "亲切干练") {
		t.Fatalf("切换人设后性格段未生效:\n%s", playful[:200])
	}
	for _, shared := range []string{"【人文关怀】", "【闪念：笔记与待办】", "【长期记忆】", "schedule_task"} {
		if !strings.Contains(playful, shared) {
			t.Fatalf("切换人设后共用段 %s 不应丢失", shared)
		}
	}

	// 3. 自定义覆盖内置同 ID：文案替换生效。
	cfg.Persona.Presets = []config.Persona{
		{ID: "playful", Name: "活泼调皮", Prompt: "- 完全自定义的调皮文案。"},
	}
	if got := systemPrompt(cfg); !strings.Contains(got, "完全自定义的调皮文案") {
		t.Fatal("自定义预设应覆盖内置文案")
	}

	// 4. 自定义全新人设并激活。
	cfg.Persona.Presets = append(cfg.Persona.Presets,
		config.Persona{ID: "grumpy", Name: "暴躁老哥", Prompt: "- 语气很冲但活儿漂亮。"})
	cfg.Persona.Active = "grumpy"
	if got := systemPrompt(cfg); !strings.Contains(got, "语气很冲但活儿漂亮") {
		t.Fatal("激活全新自定义人设应生效")
	}

	// 5. cfg.SystemPrompt 最高优先级（整体替换默认提示词，不含人设段拼装）；
	//    但回复语言的约定仍然追加——否则设置里的语言在自定义提示词下会静默失效。
	cfg.SystemPrompt = "完全自定义的系统提示词"
	got := systemPrompt(cfg)
	if !strings.HasPrefix(got, "完全自定义的系统提示词") {
		t.Fatalf("cfg.SystemPrompt 应整体优先: %q", got[:50])
	}
	if !strings.Contains(got, "【回复语言】") {
		t.Fatal("自定义系统提示词也要带上回复语言规则")
	}
}

// TestResolvePersonaFallback 验证人设解析的兜底：active 未命中/正文为空回退 default。
func TestResolvePersonaFallback(t *testing.T) {
	cfg := &config.Config{Persona: config.PersonaConfig{Active: "not-exist"}}
	if got := resolvePersona(cfg); got.ID != "default" {
		t.Fatalf("active 未命中应回退 default，实际 %s", got.ID)
	}
	// 自定义人设正文为空：视为无效覆盖，回退 default。
	cfg.Persona.Active = "empty-one"
	cfg.Persona.Presets = []config.Persona{{ID: "empty-one", Name: "空文案"}}
	if got := resolvePersona(cfg); got.ID != "default" {
		t.Fatalf("空正文自定义应回退 default，实际 %s", got.ID)
	}
}

// TestReloadPersona 验证人设热切换：校验拦截、切换生效、写回 config.yaml。
func TestReloadPersona(t *testing.T) {
	os.Setenv("TEST_API_KEY", "sk-test")
	defer os.Unsetenv("TEST_API_KEY")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgText := `provider:
  name: openai
  baseURL: https://api.example.com/v1
  model: demo
  apiKeyEnv: TEST_KEY
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Provider = config.ProviderConfig{Name: "openai", BaseURL: "https://api.example.com/v1",
		Model: "demo", APIKeyEnv: "TEST_API_KEY"}

	registry := vxagent.NewRegistry()
	pool := NewSessionPool(nil)
	approver := approval.New(string(approval.ModeOff), nil, 0)
	brain := NewBrain(nil, pool, nil) // Agent 由 Reload 构建后注入（未接上下文引擎）

	// 1. 切到不存在的人设：报错且配置不动。
	if err := ReloadPersona(cfg, cfgPath, registry, brain, approver, "no-such", nil); err == nil {
		t.Fatal("切换到不存在的人设应报错")
	}
	if cfg.Persona.Active != "" {
		t.Fatalf("校验失败时 active 不应被修改，实际 %q", cfg.Persona.Active)
	}

	// 2. 正常切换 playful：热生效 + 写回配置。
	if err := ReloadPersona(cfg, cfgPath, registry, brain, approver, "playful", nil); err != nil {
		t.Fatalf("ReloadPersona: %v", err)
	}
	if cfg.Persona.Active != "playful" {
		t.Fatalf("切换后 active 应为 playful，实际 %q", cfg.Persona.Active)
	}
	if brain.agent == nil {
		t.Fatal("切换后 Brain 应持有重建的 Agent")
	}
	if got := systemPrompt(cfg); !strings.Contains(got, "元气满满") {
		t.Fatalf("重建后的系统提示词应为活泼调皮人设，实际:\n%.100s", got)
	}
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "active: playful") {
		t.Fatalf("config.yaml 应写回 active: playful，实际:\n%s", saved)
	}

	// 3. 保存自定义预设并切换：自定义文案生效。
	customs := []config.Persona{{ID: "grumpy", Name: "暴躁老哥", Prompt: "- 语气很冲但活儿漂亮。"}}
	if err := ReloadPersona(cfg, cfgPath, registry, brain, approver, "grumpy", customs); err != nil {
		t.Fatalf("ReloadPersona(custom): %v", err)
	}
	if got := systemPrompt(cfg); !strings.Contains(got, "语气很冲但活儿漂亮") {
		t.Fatal("自定义人设应在重建后的系统提示词中生效")
	}

	// 4. 目标人设的正文为空：拒绝。
	bad := []config.Persona{{ID: "empty", Name: "空文案", Prompt: ""}}
	if err := ReloadPersona(cfg, cfgPath, registry, brain, approver, "empty", bad); err == nil {
		t.Fatal("目标人设正文为空应报错")
	}
}
