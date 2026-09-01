package tools

import (
	"context"
	"fmt"

	"github.com/capyflow/friday/internal/output"
)

// SpeakNowTool 立即用语音播报一段文字。
type SpeakNowTool struct {
	Speaker output.Speaker
}

func (t *SpeakNowTool) Name() string { return "speak_now" }

func (t *SpeakNowTool) Description() string {
	return "立即用语音播报一段文字给用户。用于需要口头告知用户的场景（如提示、答复）。"
}

func (t *SpeakNowTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "要播报的文字"},
		},
		"required": []string{"text"},
	}
}

func (t *SpeakNowTool) Call(ctx context.Context, args map[string]any) (string, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return "", fmt.Errorf("缺少参数 text")
	}
	if err := t.Speaker.Speak(ctx, text); err != nil {
		return "", err
	}
	return marshalJSON(map[string]string{"ok": "true", "spoken": text}), nil
}

// DisplayNowTool 立即在屏幕显示一段文字。
type DisplayNowTool struct {
	Displayer output.Displayer
}

func (t *DisplayNowTool) Name() string { return "display_now" }

func (t *DisplayNowTool) Description() string {
	return "立即在屏幕显示一段文字。用于需要文字展示的场景（如提示、状态）。"
}

func (t *DisplayNowTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "要显示的文字"},
		},
		"required": []string{"text"},
	}
}

func (t *DisplayNowTool) Call(ctx context.Context, args map[string]any) (string, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return "", fmt.Errorf("缺少参数 text")
	}
	if err := t.Displayer.Display(ctx, text); err != nil {
		return "", err
	}
	return marshalJSON(map[string]string{"ok": "true", "displayed": text}), nil
}
