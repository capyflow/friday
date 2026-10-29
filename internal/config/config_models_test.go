package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestUpdateProviderModels 验证模型库的热写回：注释保留、active 与 models 落盘、
// 激活条目投影到扁平字段（含 contextWindow）、apiKey 空串不动已有密钥。
func TestUpdateProviderModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := `# 顶部注释
provider:
  name: openai
  baseURL: https://api.example.com/v1
  model: demo
  apiKey: sk-old # 行内注释
memory:
  file: memories.json
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	p := ProviderConfig{
		Name: "openai", BaseURL: "https://api.stepfun.com/step_plan/v1", Model: "step-5-preview",
		APIKey: "sk-old", ContextWindow: 128000, Active: "step-5",
		Models: []ModelProfile{
			{Name: "step-5", BaseURL: "https://api.stepfun.com/step_plan/v1", Model: "step-5-preview",
				APIKey: "sk-step", ContextWindow: 128000, Inputs: []string{InputText, InputImage}},
			{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat",
				APIKeyEnv: "DEEPSEEK_API_KEY", ContextWindow: 65536},
		},
	}
	if err := UpdateProviderModels(path, p); err != nil {
		t.Fatalf("UpdateProviderModels: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# 顶部注释", "# 行内注释", "sk-old", "active: step-5", "contextWindow: 128000", "deepseek-chat"} {
		if !strings.Contains(text, want) {
			t.Fatalf("保存后应保留/包含 %q，实际:\n%s", want, text)
		}
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	// 扁平投影 + 库条目完整回读。
	if cfg.Provider.BaseURL != p.BaseURL || cfg.Provider.Model != p.Model || cfg.Provider.ContextWindow != 128000 {
		t.Fatalf("扁平字段投影不符: %+v", cfg.Provider)
	}
	if cfg.Provider.Active != "step-5" || len(cfg.Provider.Models) != 2 {
		t.Fatalf("模型库不符: active=%q models=%+v", cfg.Provider.Active, cfg.Provider.Models)
	}
	if cfg.Provider.Models[0].Inputs[0] != InputText || len(cfg.Provider.Models[1].Inputs) != 0 {
		t.Fatalf("inputs 回读不符: %+v", cfg.Provider.Models)
	}

	// 再存一次：扁平 contextWindow 变 0 时也应显式覆盖（不能残留旧值）。
	p.ContextWindow = 0
	p.Active = "deepseek"
	p.BaseURL, p.Model = "https://api.deepseek.com/v1", "deepseek-chat"
	if err := UpdateProviderModels(path, p); err != nil {
		t.Fatalf("UpdateProviderModels(second): %v", err)
	}
	data, _ = os.ReadFile(path)
	var cfg2 Config
	if err := yaml.Unmarshal(data, &cfg2); err != nil {
		t.Fatal(err)
	}
	if cfg2.Provider.ContextWindow != 0 || cfg2.Provider.Active != "deepseek" {
		t.Fatalf("二次保存应覆盖 contextWindow/active: %+v", cfg2.Provider)
	}
}

// TestPlaceholderModelPair 首次启动种子里的示例值必须能被识别为「未配置」，
// 且不能误伤用户自己的真实模型（两项必须成对匹配）。
func TestPlaceholderModelPair(t *testing.T) {
	if !(ModelProfile{BaseURL: PlaceholderBaseURL, Model: PlaceholderModel}).IsPlaceholder() {
		t.Fatal("初始占位应识别为未配置")
	}
	if !(ModelProfile{BaseURL: " " + PlaceholderBaseURL + "/ ", Model: " 请填模型名 "}).IsPlaceholder() {
		t.Fatal("占位识别应忽略空白与末尾斜杠")
	}
	for _, m := range []ModelProfile{
		{BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
		{BaseURL: "https://api.deepseek.com/v1", Model: PlaceholderModel},
		{BaseURL: PlaceholderBaseURL, Model: "deepseek-chat"},
	} {
		if m.IsPlaceholder() {
			t.Fatalf("真实配置被误判为占位: %+v", m)
		}
	}
}

// TestValidateModels 校验模型库规则：name 唯一非空、baseURL/model 非空、inputs
// 枚举合法、active 必须命中；空库不受约束。
func TestValidateModels(t *testing.T) {
	base := func() *Config {
		cfg := &Config{}
		cfg.applyDefaults()
		cfg.Provider.BaseURL = "https://api.example.com/v1"
		cfg.Provider.Model = "demo"
		return cfg
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("无库配置应通过: %v", err)
	}

	ok := base()
	ok.Provider.Active = "a"
	ok.Provider.Models = []ModelProfile{
		{Name: "a", BaseURL: "https://a.example.com/v1", Model: "m-a", Inputs: []string{InputImage}},
		{Name: "b", BaseURL: "https://b.example.com/v1", Model: "m-b", ContextWindow: 4096},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法库应通过: %v", err)
	}

	cases := []struct {
		name        string
		mutate      func(p *ProviderConfig)
		wantContain string
	}{
		{"缺 name", func(p *ProviderConfig) { p.Models[0].Name = "" }, "缺 name"},
		{"name 重复", func(p *ProviderConfig) { p.Models[1].Name = "a" }, "重复"},
		{"缺 baseURL", func(p *ProviderConfig) { p.Models[0].BaseURL = "" }, "baseURL"},
		{"缺 model", func(p *ProviderConfig) { p.Models[1].Model = "" }, "model"},
		{"未知输入格式", func(p *ProviderConfig) { p.Models[0].Inputs = []string{"audio"} }, "未知格式"},
		{"负窗口", func(p *ProviderConfig) { p.Models[0].ContextWindow = -1 }, "不能为负"},
		{"active 未命中", func(p *ProviderConfig) { p.Active = "nope" }, "未命中"},
	}
	for _, tc := range cases {
		cfg := base()
		cfg.Provider.Active = "a"
		cfg.Provider.Models = []ModelProfile{
			{Name: "a", BaseURL: "https://a.example.com/v1", Model: "m-a"},
			{Name: "b", BaseURL: "https://b.example.com/v1", Model: "m-b"},
		}
		tc.mutate(&cfg.Provider)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.wantContain) {
			t.Fatalf("%s: 应报含 %q 的错误，实际 %v", tc.name, tc.wantContain, err)
		}
	}
}

// TestModelProfileInputs 验证输入格式语义：空声明 = 全支持（向后兼容），
// text 恒隐含，SupportsInput 按归一化结果判断。
func TestModelProfileInputs(t *testing.T) {
	empty := ModelProfile{}
	if got := empty.EffectiveInputs(); len(got) != 2 || got[0] != InputText || got[1] != InputImage {
		t.Fatalf("空声明应为全支持: %v", got)
	}
	if !empty.SupportsInput(InputImage) {
		t.Fatal("空声明应支持图片")
	}
	textOnly := ModelProfile{Inputs: []string{InputText}}
	if textOnly.SupportsInput(InputImage) {
		t.Fatal("只声明 text 不应支持图片")
	}
	imageOnly := ModelProfile{Inputs: []string{InputImage}}
	got := imageOnly.EffectiveInputs()
	if len(got) != 2 || got[0] != InputText || got[1] != InputImage {
		t.Fatalf("text 恒隐含: %v", got)
	}
}

// TestSnapshotClonesModels 快照对模型库深拷贝：改快照的条目与 inputs 不影响原配置。
func TestSnapshotClonesModels(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	cfg.Provider.Models = []ModelProfile{
		{Name: "a", Inputs: []string{InputText, InputImage}},
	}
	snap := cfg.Snapshot()
	snap.Provider.Models[0].Name = "changed"
	snap.Provider.Models[0].Inputs[0] = "changed"
	if cfg.Provider.Models[0].Name != "a" || cfg.Provider.Models[0].Inputs[0] != InputText {
		t.Fatalf("快照修改不应影响原配置: %+v", cfg.Provider.Models)
	}
}

// TestModelProfileMaxTokens 输出预算语义：条目未填按 DefaultMaxTokens，填了用填的；
// 全局缺省同样是 DefaultMaxTokens（曾经的 1024 会让推理模型只出思考、没有正文）。
func TestModelProfileMaxTokens(t *testing.T) {
	if got := (ModelProfile{}).EffectiveMaxTokens(); got != DefaultMaxTokens {
		t.Fatalf("未填应取默认: got %d want %d", got, DefaultMaxTokens)
	}
	if got := (ModelProfile{MaxTokens: -1}).EffectiveMaxTokens(); got != DefaultMaxTokens {
		t.Fatalf("负值应回落默认: got %d", got)
	}
	if got := (ModelProfile{MaxTokens: 8192}).EffectiveMaxTokens(); got != 8192 {
		t.Fatalf("填了就用填的: got %d", got)
	}
	cfg := &Config{}
	cfg.applyDefaults()
	if cfg.Provider.MaxTokens != DefaultMaxTokens {
		t.Fatalf("provider.maxTokens 缺省应为 %d，实际 %d", DefaultMaxTokens, cfg.Provider.MaxTokens)
	}
	bad := &Config{}
	bad.applyDefaults()
	bad.Provider.BaseURL, bad.Provider.Model = "https://a.example.com/v1", "m"
	bad.Provider.Active = "a"
	bad.Provider.Models = []ModelProfile{{Name: "a", BaseURL: "https://a.example.com/v1",
		Model: "m", MaxTokens: -5}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "maxTokens 不能为负") {
		t.Fatalf("负 maxTokens 应被拒: %v", err)
	}
}

// TestUpdateProviderModelsWritesMaxTokens 条目里的 maxTokens 要能落盘并回读；
// 未填的条目不写这个键（保持 YAML 干净，运行时按默认处理）。
func TestUpdateProviderModelsWritesMaxTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("provider:\n  name: openai\n  baseURL: https://a.example.com/v1\n  model: m-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := ProviderConfig{
		Name: "openai", BaseURL: "https://a.example.com/v1", Model: "m-a", MaxTokens: DefaultMaxTokens,
		Active: "a",
		Models: []ModelProfile{
			{Name: "a", BaseURL: "https://a.example.com/v1", Model: "m-a", MaxTokens: 8192},
			{Name: "b", BaseURL: "https://b.example.com/v1", Model: "m-b"},
		},
	}
	if err := UpdateProviderModels(path, p); err != nil {
		t.Fatalf("UpdateProviderModels: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := yaml.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Provider.Models[0].MaxTokens != 8192 {
		t.Fatalf("条目 maxTokens 应落盘回读: %s", data)
	}
	if back.Provider.Models[1].MaxTokens != 0 {
		t.Fatalf("未填的条目不该冒出 maxTokens: %+v", back.Provider.Models[1])
	}
	if !strings.Contains(string(data), "maxTokens: 8192") {
		t.Fatalf("YAML 里应看到显式值:\n%s", data)
	}
	// 扁平 provider.maxTokens 同步为投影后的运行值（重启读的是这里）。
	if back.Provider.MaxTokens != DefaultMaxTokens {
		t.Fatalf("扁平 maxTokens 应落运行值 %d，实际 %d:\n%s", DefaultMaxTokens, back.Provider.MaxTokens, data)
	}
}
