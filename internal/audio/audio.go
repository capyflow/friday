// Package audio 定义音频采集/播放抽象，与具体后端（portaudio/ALSA）解耦。
// Recorder 固定输出 16kHz 单声道 PCM（sherpa-onnx 等 ASR 的标准输入格式）。
package audio

import (
	"context"
	"time"
)

// Recorder 从麦克风采集 PCM 音频。
type Recorder interface {
	// Record 录制 duration 时长的音频，返回 16kHz 单声道 int16 PCM。
	Record(ctx context.Context, duration time.Duration) ([]int16, error)
	Close() error
}

// Player 播放 PCM 音频到喇叭。
type Player interface {
	// Play 播放 int16 PCM 音频，sampleRate 为采样率（如 22050、16000）。
	Play(ctx context.Context, samples []int16, sampleRate int) error
	Close() error
}

// Silence 生成 n 个采样点的静音 PCM（用于占位/测试）。
func Silence(n int) []int16 {
	return make([]int16, n)
}