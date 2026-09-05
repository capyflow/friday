package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/capyflow/friday/internal/audio"
)

// PlayAudioTool 通过喇叭播放一个本地 wav 音频文件。
type PlayAudioTool struct {
	Player audio.Player
}

func (t *PlayAudioTool) Name() string { return "play_audio" }

func (t *PlayAudioTool) Description() string {
	return "通过喇叭播放一个本地 wav 音频文件。用于用户要求播放声音/音乐/音频文件的场景。" +
		"仅支持 16-bit PCM wav（立体声自动转单声道），mp3 等其他格式不支持。"
}

func (t *PlayAudioTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "wav 文件路径"},
		},
		"required": []string{"path"},
	}
}

func (t *PlayAudioTool) Call(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", fmt.Errorf("缺少参数 path")
	}
	if t.Player == nil {
		return "", fmt.Errorf("当前没有可用的音频播放设备")
	}
	samples, rate, err := audio.DecodeWAV(path)
	if err != nil {
		return "", err
	}
	if err := t.Player.Play(ctx, samples, rate); err != nil {
		return "", err
	}
	return marshalJSON(map[string]any{
		"ok":      "true",
		"path":    path,
		"seconds": fmt.Sprintf("%.1f", float64(len(samples))/float64(rate)),
	}), nil
}

// PlayEffectTool 播放内置提示音效（运行期合成，无需音频文件）。
type PlayEffectTool struct {
	Player audio.Player
}

func (t *PlayEffectTool) Name() string { return "play_effect" }

func (t *PlayEffectTool) Description() string {
	return "播放内置提示音效（无需音频文件）。用于引起注意、确认操作、提示任务到点等场景。可选音效：" +
		strings.Join(audio.EffectNames(), "/") + "。"
}

func (t *PlayEffectTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "enum": audio.EffectNames(), "description": "音效名称"},
		},
		"required": []string{"name"},
	}
}

func (t *PlayEffectTool) Call(ctx context.Context, args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "", fmt.Errorf("缺少参数 name")
	}
	if t.Player == nil {
		return "", fmt.Errorf("当前没有可用的音频播放设备")
	}
	samples, rate, err := audio.Effect(name)
	if err != nil {
		return "", err
	}
	if err := t.Player.Play(ctx, samples, rate); err != nil {
		return "", err
	}
	return marshalJSON(map[string]string{"ok": "true", "effect": name}), nil
}
