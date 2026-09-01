// Package asr 定义语音识别抽象：把麦克风采集的 PCM 转成文字。
package asr

import "context"

// Recognizer 把 16kHz 单声道 int16 PCM 语音识别为文字。
type Recognizer interface {
	Recognize(ctx context.Context, samples []int16) (string, error)
	Close() error
}