package agent

import (
	"os"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/memory"
	"github.com/capyflow/friday/internal/output"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/tools"
)

// TestBuild verifies 用合法配置能成功组装 Agent（NewProvider 不联网，只校验配置）。
func TestBuild(t *testing.T) {
	os.Setenv("TEST_API_KEY", "sk-test")
	defer os.Unsetenv("TEST_API_KEY")

	cfg := &config.Config{}
	cfg.Provider = config.ProviderConfig{
		Name:        "openai",
		BaseURL:     "https://api.deepseek.com/v1",
		Model:       "deepseek-chat",
		APIKeyEnv:   "TEST_API_KEY",
		MaxTokens:   1024,
		Temperature: 0.7,
	}

	registry := vxagent.NewRegistry()
	eng := task.NewEngine(nil)
	defer eng.Close()
	mem := memory.NewStore("")
	if err := RegisterTools(registry, eng, mem, output.NewConsoleSpeaker(os.Stdout), output.NewConsoleDisplayer(os.Stdout), audio.NewConsolePlayer(os.Stdout), &tools.ChatController{}); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	ag, err := Build(cfg, registry, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if ag == nil {
		t.Fatal("Agent 为空")
	}
}
