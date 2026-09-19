package voice

import (
	"fmt"
	"time"

	"github.com/capyflow/friday/archive/voice-mode/asr"
	"github.com/capyflow/friday/archive/voice-mode/tts"
	"github.com/capyflow/friday/archive/voice-mode/vad"
	"github.com/capyflow/friday/archive/voice-mode/wake"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
)

// Components 是语音链路全部组件；NewComponents 按配置构建。
type Components struct {
	Wake     wake.Detector
	Recorder audio.Recorder
	Player   audio.Player
	ASR      asr.Recognizer
	TTS      tts.Synthesizer
	// Keywords 是实际生效的唤醒词（来自模型目录 keywords.txt，可能与配置不同）。
	Keywords []string
	// VAD 为 nil 时录音退回固定 MaxRecord 时长，否则说完自动断句。
	VAD *vad.Detector
}

// NewComponents 依据配置构建语音组件。
// 注意：sherpa-onnx 引擎需要以 -tags sherpa 编译（见 voice_sherpa.go）。
func NewComponents(cfg *config.Config) (*Components, error) {
	if cfg.Wake.Engine == "none" && cfg.ASR.Engine == "none" && cfg.TTS.Engine == "none" {
		return nil, fmt.Errorf("语音引擎未配置（asr/wake/tts 均为 none），使用文本模式")
	}
	return newComponentsImpl(cfg)
}

// VoiceLoop 构建语音主循环；handle 处理识别出的文字。
func VoiceLoop(c *Components, maxRecord time.Duration) *Loop {
	return &Loop{
		Wake:      c.Wake,
		Recorder:  c.Recorder,
		ASR:       c.ASR,
		VAD:       c.VAD,
		MaxRecord: maxRecord,
	}
}
