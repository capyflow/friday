//go:build sherpa

package asr

import (
	"context"
	"fmt"
	"log"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// SherpaRecognizer 通过 sherpa-onnx OfflineRecognizer 实现离线语音识别。
type SherpaRecognizer struct {
	recognizer *sherpa.OfflineRecognizer
}

// NewSherpaRecognizer 创建离线语音识别器。
// modelDir 包含 ASR 模型文件（paraformer/zipformer 的 encoder/decoder/tokens 等）。
func NewSherpaRecognizer(modelDir string) (*SherpaRecognizer, error) {
	log.Printf("[asr] 加载模型中: %s", modelDir)
	config := sherpa.OfflineRecognizerConfig{}
	config.FeatConfig.SampleRate = 16000
	config.FeatConfig.FeatureDim = 80

	// 使用 Paraformer 离线中文模型
	config.ModelConfig.Paraformer.Model = modelDir + "/model.int8.onnx"
	config.ModelConfig.Tokens = modelDir + "/tokens.txt"
	config.ModelConfig.NumThreads = 2
	config.ModelConfig.Debug = 0
	config.ModelConfig.Provider = "cpu"
	config.DecodingMethod = "greedy_search"

	recognizer := sherpa.NewOfflineRecognizer(&config)
	if recognizer == nil {
		return nil, fmt.Errorf("创建 OfflineRecognizer 失败，请检查模型目录 %q", modelDir)
	}
	log.Printf("[asr] 模型加载完成")

	return &SherpaRecognizer{recognizer: recognizer}, nil
}

// Recognize 将 16kHz 单声道 int16 PCM 识别为文字。
func (r *SherpaRecognizer) Recognize(ctx context.Context, samples []int16) (string, error) {
	if len(samples) == 0 {
		return "", nil
	}

	// int16 PCM → float32 [-1, 1]
	floats := make([]float32, len(samples))
	for i, s := range samples {
		floats[i] = float32(s) / 32768.0
	}

	stream := sherpa.NewOfflineStream(r.recognizer)
	defer sherpa.DeleteOfflineStream(stream)

	stream.AcceptWaveform(16000, floats)
	r.recognizer.Decode(stream)

	result := stream.GetResult()
	if result == nil {
		return "", fmt.Errorf("语音识别结果为空")
	}

	return result.Text, nil
}

// Close 释放 OfflineRecognizer 资源。
func (r *SherpaRecognizer) Close() error {
	if r.recognizer != nil {
		sherpa.DeleteOfflineRecognizer(r.recognizer)
		r.recognizer = nil
	}
	return nil
}
