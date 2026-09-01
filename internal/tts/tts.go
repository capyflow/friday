// Package tts 定义语音合成抽象：把文字转成可播放的 PCM 音频。
package tts

import "context"

// Synthesizer 把文字合成为 PCM 音频。
type Synthesizer interface {
	// Synthesize 返回 int16 PCM 数据与其采样率。
	Synthesize(ctx context.Context, text string) ([]int16, int, error)
	Close() error
}