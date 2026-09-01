// Package voice 编排语音交互链路：唤醒词 → 录音 → 语音识别 → 交给对话处理。
// 还提供 TTS+喇叭 组装成 output.Speaker 的适配器。
package voice

import (
	"context"
	"strings"
	"time"

	"github.com/capyflow/friday/internal/asr"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/tts"
	"github.com/capyflow/friday/internal/wake"
)

// Loop 是语音交互主循环。
type Loop struct {
	Wake     wake.Detector
	Recorder audio.Recorder
	ASR      asr.Recognizer
	// MaxRecord 单次录音时长上限。
	MaxRecord time.Duration
}

// Run 持续执行"等待唤醒 → 录音 → 识别 → handle"，直到 ctx 取消。
// handle 收到识别出的文字；识别为空时跳过。
func (l *Loop) Run(ctx context.Context, handle func(ctx context.Context, text string) error) error {
	for {
		if err := l.Wake.Wait(ctx); err != nil {
			return err
		}
		samples, err := l.Recorder.Record(ctx, l.MaxRecord)
		if err != nil {
			return err
		}
		text, err := l.ASR.Recognize(ctx, samples)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if err := handle(ctx, text); err != nil {
			return err
		}
	}
}

// SpeakerAdapter 把 Synthesizer + Player 组装成 output.Speaker。
type SpeakerAdapter struct {
	Synth  tts.Synthesizer
	Player audio.Player
}

func (s *SpeakerAdapter) Speak(ctx context.Context, text string) error {
	samples, rate, err := s.Synth.Synthesize(ctx, text)
	if err != nil {
		return err
	}
	return s.Player.Play(ctx, samples, rate)
}
