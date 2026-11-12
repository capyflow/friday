package agent

import (
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/config"
)

// TestLanguageRule 验证回复语言规则段本身的形状：空语言回落中文，
// 语言名去掉首尾空格后写进规则。
func TestLanguageRule(t *testing.T) {
	r := LanguageRule("")
	if !strings.Contains(r, "【回复语言】") || !strings.Contains(r, "「中文」") {
		t.Fatalf("空语言应回落中文: %q", r)
	}
	r = LanguageRule("  English  ")
	if !strings.Contains(r, "「English」") {
		t.Fatalf("语言名应去空格后写入: %q", r)
	}
	// 规则要说明代码/命令不翻译：否则 agent 顺手把命令也译了。
	if !strings.Contains(r, "代码") {
		t.Fatalf("规则应交代代码保持原样: %q", r)
	}
}

// TestSystemPromptCarriesLanguage 验证系统提示词带上回复语言规则：默认中文、
// 改了跟着变；自定义系统提示词也照样带（否则设置里的语言会静默失效）。
func TestSystemPromptCarriesLanguage(t *testing.T) {
	cfg := &config.Config{}
	p := systemPrompt(cfg)
	if !strings.Contains(p, "【回复语言】") || !strings.Contains(p, "「中文」") {
		t.Fatal("系统提示词应含默认的回复语言规则")
	}

	cfg.Language = "English"
	p = systemPrompt(cfg)
	if !strings.Contains(p, "「English」") {
		t.Fatal("配置的语言应进系统提示词")
	}
	if strings.Contains(p, "「中文」") {
		t.Fatal("换了语言就不该还写着中文")
	}

	cfg.SystemPrompt = "完全自定义的系统提示词"
	p = systemPrompt(cfg)
	if !strings.HasPrefix(p, "完全自定义的系统提示词") {
		t.Fatalf("自定义提示词应整体优先: %q", p[:40])
	}
	if !strings.Contains(p, "「English」") {
		t.Fatal("自定义系统提示词也要带上回复语言规则")
	}
}

// TestEffectiveLanguage 验证配置的取值与兜底。
func TestEffectiveLanguage(t *testing.T) {
	if got := (&config.Config{}).EffectiveLanguage(); got != config.DefaultLanguage {
		t.Fatalf("空配置应回落默认语言: %q", got)
	}
	if got := (&config.Config{Language: "  日本語  "}).EffectiveLanguage(); got != "日本語" {
		t.Fatalf("语言名应去空格: %q", got)
	}
}
