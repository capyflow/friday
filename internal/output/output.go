// Package output 定义机器人的输出抽象：语音播报（Speaker）与屏幕显示（Displayer）。
// agent 工具（speak_now / display_now）与提醒到点回调都依赖这两个接口，
// 开发阶段用 Console 实现，后续换 TTS 引擎 / OLED 屏幕只需替换实现。
package output

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Speaker 负责把文字播报出来。
type Speaker interface {
	Speak(ctx context.Context, text string) error
}

// Displayer 负责把文字显示到屏幕。
type Displayer interface {
	Display(ctx context.Context, text string) error
}

// ConsoleSpeaker 是开发用播报：打印到指定输出。
type ConsoleSpeaker struct {
	mu  sync.Mutex
	out io.Writer
}

func NewConsoleSpeaker(out io.Writer) *ConsoleSpeaker {
	return &ConsoleSpeaker{out: out}
}

func (s *ConsoleSpeaker) Speak(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := fmt.Fprintf(s.out, "[TTS] %s\n", text)
	return err
}

// ConsoleDisplayer 是开发用显示：打印到指定输出。
type ConsoleDisplayer struct {
	mu  sync.Mutex
	out io.Writer
}

func NewConsoleDisplayer(out io.Writer) *ConsoleDisplayer {
	return &ConsoleDisplayer{out: out}
}

func (d *ConsoleDisplayer) Display(_ context.Context, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := fmt.Fprintf(d.out, "[屏幕] %s\n", text)
	return err
}
