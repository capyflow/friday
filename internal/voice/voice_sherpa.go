//go:build sherpa

package voice

import (
	"fmt"
	"log"
	"os"
	"slices"

	"github.com/capyflow/friday/internal/asr"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/tts"
	"github.com/capyflow/friday/internal/vad"
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

	// 防呆：配置的唤醒词若未注册进 keywords.txt（拼音格式），KWS 不会响应。
	keywords, err := wake.KeywordsFromFile(cfg.Wake.ModelDir)
	if err != nil {
		return nil, fmt.Errorf("读取唤醒词表: %w", err)
	}
	for _, kw := range cfg.Wake.Keywords {
		if !slices.Contains(keywords, kw) {
			log.Printf("[wake] 注意: 配置的唤醒词 %q 未在 %s/keywords.txt 注册，实际生效: %v"+
				"（新增格式：声母 带调韵母 @中文词，如 x iǎo zh ù sh ǒu @小助手）", kw, cfg.Wake.ModelDir, keywords)
		}
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
		s, err := tts.NewSherpaSynthesizer(cfg.TTS.ModelDir, cfg.TTS.Voice, cfg.TTS.Speed)
		if err != nil {
			return nil, fmt.Errorf("初始化语音合成器: %w", err)
		}
		synthesizer = s
	} else {
		return nil, fmt.Errorf("TTS 引擎 %q 不受 sherpa 实现支持", cfg.TTS.Engine)
	}

	// VAD 断句可选：模型缺失时回退固定时长录音，不阻塞语音链路。
	var detectorVAD *vad.Detector
	if cfg.VAD.ModelPath != "" {
		if fileExists(cfg.VAD.ModelPath) {
			v, err := vad.New(cfg.VAD.ModelPath, cfg.VAD.Threshold, cfg.VAD.MinSilence, cfg.VAD.MinSpeech)
			if err != nil {
				return nil, err
			}
			detectorVAD = v
		} else {
			log.Printf("[vad] 模型 %q 不存在，回退为固定时长录音（scripts/download-models.sh 可下载）", cfg.VAD.ModelPath)
		}
	}

	return &Components{
		Wake:     detector,
		Recorder: recorder,
		Player:   player,
		ASR:      recognizer,
		TTS:      synthesizer,
		Keywords: keywords,
		VAD:      detectorVAD,
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

// fileExists 报告 path 是否存在且为普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
