//go:build !sherpa

// Package vad 在未启用 sherpa 编译标签时的占位实现：
// VAD 需要 cgo + silero 模型，用 -tags sherpa 编译才会启用。
package vad

import "fmt"

// Detector 占位类型，不持有任何资源。
type Detector struct{}

// New 恒返回错误（见包注释）。
func New(string, float32, float32, float32) (*Detector, error) {
	return nil, fmt.Errorf("VAD 需要 sherpa-onnx：请以 -tags sherpa 编译（见 README-voice.md）")
}

func (*Detector) EndOfSpeech([]int16) bool { return false }

// HasSpeech 占位实现：恒为 false（VAD 未启用，无语音检测能力）。
func (*Detector) HasSpeech() bool { return false }

func (*Detector) Reset() {}

func (*Detector) Close() error { return nil }
