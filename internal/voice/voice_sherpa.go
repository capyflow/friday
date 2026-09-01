//go:build sherpa

package voice

import (
	"fmt"
	"os"

	"github.com/capyflow/friday/internal/asr"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/tts"
	"github.com/capyflow/friday/internal/wake"
)

// newComponentsImpl 在 sherpa 编译标签启用时构建真实语音组件。
func newComponentsImpl(cfg *config.Config) (*Components, error) {
	// 校验模型目录
	if err := validateModelDir(cfg.ASR.ModelDir, "asr"); err != nil {
		return nil, err
	}
	if err := validateModelDir(cfg.Wake.ModelDir, "wake"); err != nil {
		return nil, err
	}
	if err := validateModelDir(cfg.TTS.ModelDir, "tts"); err != nil {
		return nil, err
	}

	recorder := audio.NewPortaudioRecorder(cfg.Audio.InputDevice)
	player := audio.NewPortaudioPlayer(cfg.Audio.OutputDevice)

	var detector wake.Detector
	if cfg.Wake.Engine == "sherpa-onnx" {
		d, err := wake.NewSherpaDetector(cfg.Wake.ModelDir, cfg.Wake.Keywords, cfg.Audio.InputDevice)
		if err != nil {
			return nil, fmt.Errorf("初始化唤醒词检测器: %w", err)
		}
		detector = d
	} else {
		return nil, fmt.Errorf("唤醒词引擎 %q 不受 sherpa 实现支持", cfg.Wake.Engine)
	}

	var recognizer asr.Recognizer
	if cfg.ASR.Engine == "sherpa-onnx" {
		r, err := asr.NewSherpaRecognizer(cfg.ASR.ModelDir)
		if err != nil {
			return nil, fmt.Errorf("初始化语音识别器: %w", err)
		}
		recognizer = r
	} else {
		return nil, fmt.Errorf("ASR 引擎 %q 不受 sherpa 实现支持", cfg.ASR.Engine)
	}

	var synthesizer tts.Synthesizer
	if cfg.TTS.Engine == "sherpa-onnx" {
		s, err := tts.NewSherpaSynthesizer(cfg.TTS.ModelDir, cfg.TTS.Voice)
		if err != nil {
			return nil, fmt.Errorf("初始化语音合成器: %w", err)
		}
		synthesizer = s
	} else {
		return nil, fmt.Errorf("TTS 引擎 %q 不受 sherpa 实现支持", cfg.TTS.Engine)
	}

	return &Components{
		Wake:     detector,
		Recorder: recorder,
		Player:   player,
		ASR:      recognizer,
		TTS:      synthesizer,
	}, nil
}

// validateModelDir 检查模型目录是否存在且可读。
func validateModelDir(dir, label string) error {
	if dir == "" {
		return fmt.Errorf("%s 模型目录不能为空（config.%s.modelDir）", label, label)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%s 模型目录 %q 不存在: %w", label, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 模型路径 %q 不是目录", label, dir)
	}
	return nil
}
