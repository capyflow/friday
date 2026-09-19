//go:build !sherpa

package main

import "fmt"

// 未启用 sherpa 编译标签时给出明确指引（语音链路需要 cgo + 本地模型）。
func runASR([]string) error { return errNeedSherpa }

func runTTS([]string) error { return errNeedSherpa }

func runDevices() error { return errNeedSherpa }

func runRec([]string) error { return errNeedSherpa }

func runPlay([]string) error { return errNeedSherpa }

func runKWS([]string) error { return errNeedSherpa }

var errNeedSherpa = fmt.Errorf("voicecheck 需要 sherpa-onnx 支持：go run -tags sherpa ./cmd/voicecheck ...（依赖安装见 README-voice.md）")
