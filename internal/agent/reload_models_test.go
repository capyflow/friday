package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/task"
)

// reloadModelsHarness 搭 ReloadProviderModels 的最小运行环境：真实 registry
// （NewProvider 不联网）、真实 config.yaml、带引擎的 Brain（可观察重建与否）。
func reloadModelsHarness(t *testing.T) (*config.Config, string, *vxagent.Registry, *Brain, *approval.Queue) {
	t.Helper()
	os.Setenv("TEST_API_KEY", "sk-test")
	t.Cleanup(func() { os.Unsetenv("TEST_API_KEY") })

	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := `provider:
  name: openai
  baseURL: https://api.a.example/v1
  model: m-a
  apiKeyEnv: TEST_API_KEY
  contextWindow: 4096
  active: a
  models:
    - name: a
      baseURL: https://api.a.example/v1
      model: m-a
      contextWindow: 4096
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	registry := vxagent.NewRegistry()
	eng := task.NewEngine(nil)
	t.Cleanup(func() { eng.Close() })
	mem := memory.NewWikiStore("", "")
	if err := RegisterTools(registry, Deps{
		Engine:    eng,
		Memory:    mem,
		Speaker:   output.NewConsoleSpeaker(os.Stdout),
		Displayer: output.NewConsoleDisplayer(os.Stdout),
		Player:    audio.NewConsolePlayer(os.Stdout),
		Config:    cfg,
	}); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	brain := NewBrain(nil, NewSessionPool(nil), NewContextManager(nil))
	approver := approval.New(string(approval.ModeConfirm), nil, 0)
	return cfg, path, registry, brain, approver
}

// TestReloadProviderModelsActivate 切换激活条目：扁平字段投影 + 引擎与 Agent 重建
// + config.yaml 落盘（models/active/contextWindow 一并写入）。
func TestReloadProviderModelsActivate(t *testing.T) {
	cfg, path, registry, brain, approver := reloadModelsHarness(t)
	before := brain.CtxMgr().Current()

	models := append([]config.ModelProfile{}, cfg.Snapshot().Provider.Models...)
	models = append(models, config.ModelProfile{
		Name: "b", BaseURL: "https://api.b.example/v1", Model: "m-b",
		APIKey: "sk-b", ContextWindow: 8192, Inputs: []string{config.InputImage},
	})
	if err := ReloadProviderModels(cfg, path, registry, brain, approver, "b", models); err != nil {
		t.Fatalf("ReloadProviderModels: %v", err)
	}

	next := cfg.Snapshot()
	if next.Provider.Active != "b" || next.Provider.Model != "m-b" ||
		next.Provider.BaseURL != "https://api.b.example/v1" || next.Provider.ContextWindow != 8192 {
		t.Fatalf("激活条目应投影到扁平字段: %+v", next.Provider)
	}
	if next.Provider.APIKey != "sk-b" {
		t.Fatalf("条目密钥应套用: %q", next.Provider.APIKey)
	}
	if brain.CtxMgr().Current() == nil || brain.CtxMgr().Current() == before {
		t.Fatal("切换激活条目应重建上下文引擎")
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"active: b", "m-b", "contextWindow: 8192", "name: a"} {
		if !strings.Contains(text, want) {
			t.Fatalf("落盘应包含 %q:\n%s", want, text)
		}
	}
}

// TestReloadProviderModelsKeyInherit 保存时密钥留空 = 沿用该条目已存密钥；
// 非激活条目的编辑不触发引擎重建，只落盘。
func TestReloadProviderModelsKeyInherit(t *testing.T) {
	cfg, path, registry, brain, approver := reloadModelsHarness(t)

	// 先给条目 a 配直填密钥（a 是当前激活条目，这次保存会触发重建）。
	models := []config.ModelProfile{
		{Name: "a", BaseURL: "https://api.a.example/v1", Model: "m-a", APIKey: "sk-a", ContextWindow: 4096},
	}
	if err := ReloadProviderModels(cfg, path, registry, brain, approver, "a", models); err != nil {
		t.Fatalf("首次保存: %v", err)
	}

	engineBefore := brain.CtxMgr().Current()
	// 再保存：a 的 apiKey 留空（应继承 sk-a），只加一条非激活条目 b。
	models = append(models, config.ModelProfile{
		Name: "b", BaseURL: "https://api.b.example/v1", Model: "m-b",
	})
	if err := ReloadProviderModels(cfg, path, registry, brain, approver, "a", models); err != nil {
		t.Fatalf("二次保存: %v", err)
	}

	next := cfg.Snapshot()
	if next.Provider.APIKey != "sk-a" {
		t.Fatalf("留空应继承已存密钥: %q", next.Provider.APIKey)
	}
	if len(next.Provider.Models) != 2 {
		t.Fatalf("新条目应入库: %+v", next.Provider.Models)
	}
	if brain.CtxMgr().Current() != engineBefore {
		t.Fatal("只编辑非激活条目不应重建引擎")
	}
}

// TestReloadProviderModelsWindowOnly 只改激活条目的窗口也应重建引擎（窗口固化在
// engine Config），且新窗口对快照可见。
func TestReloadProviderModelsWindowOnly(t *testing.T) {
	cfg, path, registry, brain, approver := reloadModelsHarness(t)
	engineBefore := brain.CtxMgr().Current()

	models := []config.ModelProfile{
		{Name: "a", BaseURL: "https://api.a.example/v1", Model: "m-a", ContextWindow: 999},
	}
	if err := ReloadProviderModels(cfg, path, registry, brain, approver, "a", models); err != nil {
		t.Fatalf("ReloadProviderModels: %v", err)
	}
	if brain.CtxMgr().Current() == engineBefore {
		t.Fatal("窗口变化应重建引擎")
	}
	if got := cfg.Snapshot().Provider.ContextWindow; got != 999 {
		t.Fatalf("扁平 contextWindow 应更新: %d", got)
	}
}

// TestReloadProviderModelsRejects 非法输入直接拒绝且不动配置：重复名、active 未命中、
// 非法地址、未知输入格式。
func TestReloadProviderModelsRejects(t *testing.T) {
	cfg, path, registry, brain, approver := reloadModelsHarness(t)
	before := cfg.Snapshot().Provider

	cases := []struct {
		name   string
		active string
		models []config.ModelProfile
	}{
		{"名称重复", "a", []config.ModelProfile{
			{Name: "a", BaseURL: "https://x/v1", Model: "m"}, {Name: "a", BaseURL: "https://y/v1", Model: "m"},
		}},
		{"active 未命中", "nope", []config.ModelProfile{
			{Name: "a", BaseURL: "https://x/v1", Model: "m"},
		}},
		{"地址非法", "a", []config.ModelProfile{
			{Name: "a", BaseURL: "ftp://x", Model: "m"},
		}},
		{"未知输入格式", "a", []config.ModelProfile{
			{Name: "a", BaseURL: "https://x/v1", Model: "m", Inputs: []string{"video"}},
		}},
		{"缺模型名", "a", []config.ModelProfile{
			{Name: "a", BaseURL: "https://x/v1"},
		}},
	}
	for _, tc := range cases {
		if err := ReloadProviderModels(cfg, path, registry, brain, approver, tc.active, tc.models); err == nil {
			t.Fatalf("%s: 应报错", tc.name)
		}
	}
	after := cfg.Snapshot().Provider
	if after.Active != before.Active || after.Model != before.Model || len(after.Models) != len(before.Models) {
		t.Fatalf("拒绝后配置不应变化: %+v → %+v", before, after)
	}
}
