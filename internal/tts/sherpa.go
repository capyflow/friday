//go:build sherpa

package tts

import (
	"context"
	"fmt"
	"strconv"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// SherpaSynthesizer 通过 sherpa-onnx OfflineTts 实现离线语音合成。
type SherpaSynthesizer struct {
	tts     *sherpa.OfflineTts
	voiceID int
}

// NewSherpaSynthesizer 创建离线语音合成器。
// modelDir 包含 TTS 模型文件（VITS 的 model.onnx/tokens.txt/lexicon.txt 等）。
// voice 为说话人 ID 字符串（留空或非法时使用默认说话人 0）。
func NewSherpaSynthesizer(modelDir, voice string) (*SherpaSynthesizer, error) {
	config := sherpa.OfflineTtsConfig{}
	config.Model.Vits.Model = modelDir + "/model.onnx"
	config.Model.Vits.Tokens = modelDir + "/tokens.txt"
	config.Model.Vits.Lexicon = modelDir + "/lexicon.txt"
	config.Model.Vits.DataDir = modelDir + "/espeak-ng-data"
	config.Model.Vits.NoiseScale = 0.667
	config.Model.Vits.NoiseScaleW = 0.8
	config.Model.Vits.LengthScale = 1.0
	config.Model.NumThreads = 2
	config.Model.Debug = 0
	config.Model.Provider = "cpu"
	config.MaxNumSentences = 1

	tts := sherpa.NewOfflineTts(&config)
	if tts == nil {
		return nil, fmt.Errorf("创建 OfflineTts 失败，请检查模型目录 %q", modelDir)
	}

	voiceID := parseVoiceID(voice)
	return &SherpaSynthesizer{tts: tts, voiceID: voiceID}, nil
}

// parseVoiceID 解析说话人 ID；空串或非法值回退到 0。
func parseVoiceID(voice string) int {
	if voice == "" {
		return 0
	}
	id, err := strconv.Atoi(voice)
	if err != nil {
		return 0
	}
	return id
}

// Synthesize 将文字合成为 int16 PCM 音频，返回采样率。
func (s *SherpaSynthesizer) Synthesize(ctx context.Context, text string) ([]int16, int, error) {
	if text == "" {
		return nil, 0, fmt.Errorf("合成文本不能为空")
	}

	audio := s.tts.Generate(text, s.voiceID, 1.0)
	if audio == nil {
		return nil, 0, fmt.Errorf("语音合成失败")
	}

	sampleRate := audio.SampleRate
	if sampleRate == 0 {
		sampleRate = 22050
	}

	// float32 [-1, 1] → int16 PCM
	pcm := make([]int16, len(audio.Samples))
	for i, s := range audio.Samples {
		v := s * 32767
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		pcm[i] = int16(v)
	}

	return pcm, sampleRate, nil
}

// Close 释放 OfflineTts 资源。
func (s *SherpaSynthesizer) Close() error {
	if s.tts != nil {
		sherpa.DeleteOfflineTts(s.tts)
		s.tts = nil
	}
	return nil
}
