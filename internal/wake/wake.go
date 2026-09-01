// Package wake 定义唤醒词检测抽象：机器人平时待机监听，听到唤醒词才进入对话。
package wake

import "context"

// Detector 持续监听麦克风，检测到唤醒词后返回。
type Detector interface {
	// Wait 阻塞直到检测到唤醒词（返回 nil）或 ctx 取消。
	Wait(ctx context.Context) error
	Close() error
}