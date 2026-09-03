//go:build sherpa

// Package vad 封装 sherpa-onnx 的 silero VAD，为语音链路提供"说完断句"能力：
// 录音时逐块送入 EndOfSpeech，出现一段已结束的完整语音（静音超过 minSilence）
// 即返回 true，录音随之提前结束；纯静音或说话中途不会触发。
package vad

import (
	"fmt"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const (
	sampleRate    = 16000
	windowSize    = 512 // silero v4 固定窗口（16kHz 下 32ms）
	bufferSeconds = 20  // 内部环形缓冲时长，须覆盖单句最长语音
)

// Detector 是流式语音段检测器。非并发安全：由录音 goroutine 顺序调用。
type Detector struct {
	vad     *sherpa.VoiceActivityDetector
	pending []float32 // 不足一个 window 的残留样本
	speech  bool      // 本句是否已出现过语音（防止一开口前的噪声/静音误断）
}

// New 创建检测器。modelPath 为 silero_vad.onnx；threshold 为语音概率阈值
// （0~1，越大越不敏感）；minSilence/minSpeech 为判定说完/说话的最短秒数。
func New(modelPath string, threshold, minSilence, minSpeech float32) (*Detector, error) {
	cfg := sherpa.VadModelConfig{}
	cfg.SileroVad.Model = modelPath
	cfg.SileroVad.Threshold = threshold
	cfg.SileroVad.MinSilenceDuration = minSilence
	cfg.SileroVad.MinSpeechDuration = minSpeech
	cfg.SileroVad.WindowSize = windowSize
	cfg.SileroVad.MaxSpeechDuration = bufferSeconds
	cfg.SampleRate = sampleRate
	cfg.NumThreads = 1
	cfg.Provider = "cpu"

	v := sherpa.NewVoiceActivityDetector(&cfg, bufferSeconds)
	if v == nil {
		return nil, fmt.Errorf("创建 VAD 失败，请检查模型文件 %q", modelPath)
	}
	return &Detector{vad: v}, nil
}

// EndOfSpeech 送入一块录音样本（16kHz int16，长度任意），
// 当产出一段已结束的完整语音时返回 true 并消费之（可继续下一句）。
func (d *Detector) EndOfSpeech(chunk []int16) bool {
	for _, s := range chunk {
		d.pending = append(d.pending, float32(s)/32768.0)
	}
	for len(d.pending) >= windowSize {
		d.vad.AcceptWaveform(d.pending[:windowSize])
		d.pending = d.pending[windowSize:]
		if d.vad.IsSpeech() {
			d.speech = true
		}
		if d.speech && !d.vad.IsEmpty() {
			d.vad.Pop() // 消费完整语音段，为下一句复位
			d.speech = false
			return true
		}
	}
	return false
}

// Reset 丢弃进行中的状态（上一句被打断、未等到段尾时调用）。
func (d *Detector) Reset() {
	d.vad.Flush() // 强制把"进行中"的语音出段
	for !d.vad.IsEmpty() {
		d.vad.Pop()
	}
	d.speech = false
	d.pending = d.pending[:0]
}

// Close 释放底层资源。
func (d *Detector) Close() error {
	if d.vad != nil {
		sherpa.DeleteVoiceActivityDetector(d.vad)
		d.vad = nil
	}
	return nil
}
