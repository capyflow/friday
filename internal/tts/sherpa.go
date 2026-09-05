//go:build sherpa

package tts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// SherpaSynthesizer 通过 sherpa-onnx OfflineTts 实现离线语音合成。
type SherpaSynthesizer struct {
	tts     *sherpa.OfflineTts
	voiceID int
	speed   float64
}

// NewSherpaSynthesizer 创建离线语音合成器。
// modelDir 包含 TTS 模型文件，按目录布局自动识别模型类型：
//   - 存在 hifigan*.onnx → Matcha 声学模型 + vocoder（如 matcha-icefall-zh-baker）
//   - 否则按 VITS 处理（如 vits-zh-ll、vits-zh-aishell3）
//
// 中文文本前端两种布局同样自动适配：
//   - dict/（jieba 词典，如 vits-zh-ll）→ 设为 DictDir
//   - espeak-ng-data/（多语种模型）→ 设为 DataDir
//
// 目录下的 *.fst（数字/日期/电话/多音字规则）自动接入 rule_fsts。
// voice 为说话人 ID 字符串（留空或非法时使用默认说话人 0）。
// speed 为语速倍率（1.0 正常，越大越快；<=0 视为 1.0）。
func NewSherpaSynthesizer(modelDir, voice string, speed float64) (*SherpaSynthesizer, error) {
	config := sherpa.OfflineTtsConfig{}
	config.RuleFsts = ruleFsts(modelDir)
	config.Model.NumThreads = 2
	config.Model.Debug = 0
	config.Model.Provider = "cpu"
	config.MaxNumSentences = 1

	lexicon := modelDir + "/lexicon.txt"
	if !fileExists(lexicon) {
		lexicon = ""
	}
	dictDir, dataDir := "", ""
	switch {
	case dirExists(modelDir + "/dict"):
		dictDir = modelDir + "/dict"
	case dirExists(modelDir + "/espeak-ng-data"):
		dataDir = modelDir + "/espeak-ng-data"
	default:
		return nil, fmt.Errorf("TTS 模型目录 %q 缺少文本前端（dict/ 或 espeak-ng-data/），请确认模型完整", modelDir)
	}

	steps := globOne(modelDir, "model-steps-*.onnx")
	hifigan := globOne(modelDir, "hifigan*.onnx")
	if hifigan == "" {
		hifigan = globOne(modelDir, "vocos*.onnx")
	}
	if steps != "" || hifigan != "" {
		// Matcha：新格式声学模型内嵌解码器（model-steps-*.onnx，vocoder 留空），
		// 或经典布局 model.onnx + hifigan vocoder。
		acoustic := steps
		if acoustic == "" {
			acoustic = modelDir + "/model.onnx"
		}
		config.Model.Matcha.AcousticModel = acoustic
		config.Model.Matcha.Vocoder = hifigan
		config.Model.Matcha.Tokens = modelDir + "/tokens.txt"
		config.Model.Matcha.Lexicon = lexicon
		config.Model.Matcha.DictDir = dictDir
		config.Model.Matcha.DataDir = dataDir
		config.Model.Matcha.NoiseScale = 0.667
		config.Model.Matcha.LengthScale = 1.0
	} else {
		config.Model.Vits.Model = modelDir + "/model.onnx"
		config.Model.Vits.Tokens = modelDir + "/tokens.txt"
		config.Model.Vits.Lexicon = lexicon
		config.Model.Vits.DictDir = dictDir
		config.Model.Vits.DataDir = dataDir
		config.Model.Vits.NoiseScale = 0.667
		config.Model.Vits.NoiseScaleW = 0.8
		config.Model.Vits.LengthScale = 1.0
	}

	tts := sherpa.NewOfflineTts(&config)
	if tts == nil {
		return nil, fmt.Errorf("创建 OfflineTts 失败，请检查模型目录 %q", modelDir)
	}

	voiceID := parseVoiceID(voice)
	if speed <= 0 {
		speed = 1.0
	}
	return &SherpaSynthesizer{tts: tts, voiceID: voiceID, speed: speed}, nil
}

// ruleFsts 返回 modelDir 下全部 *.fst 规则文件（逗号分隔），无则返回空串。
func ruleFsts(modelDir string) string {
	matches, err := filepath.Glob(modelDir + "/*.fst")
	if err != nil || len(matches) == 0 {
		return ""
	}
	return strings.Join(matches, ",")
}

// globOne 返回 modelDir 下匹配 pattern 的第一个文件路径，无则返回空串。
func globOne(modelDir, pattern string) string {
	matches, err := filepath.Glob(filepath.Join(modelDir, pattern))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
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

// fileExists 报告 path 是否存在且为普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// dirExists 报告 path 是否存在且为目录。
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Synthesize 将文字合成为 int16 PCM 音频，返回采样率。
func (s *SherpaSynthesizer) Synthesize(ctx context.Context, text string) ([]int16, int, error) {
	if text == "" {
		return nil, 0, fmt.Errorf("合成文本不能为空")
	}

	audio := s.tts.Generate(text, s.voiceID, float32(s.speed))
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
