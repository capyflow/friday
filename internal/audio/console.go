package audio

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// ConsolePlayer 是文本模式的播放兜底：不发声，打印将要播放的内容，
// 与 output.ConsoleSpeaker/ConsoleDisplayer 同属开发期实现。
type ConsolePlayer struct {
	mu  sync.Mutex
	out io.Writer
}

func NewConsolePlayer(out io.Writer) *ConsolePlayer {
	return &ConsolePlayer{out: out}
}

func (p *ConsolePlayer) Play(_ context.Context, samples []int16, sampleRate int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	dur := time.Duration(float64(len(samples)) / float64(sampleRate) * float64(time.Second)).Round(time.Millisecond)
	_, err := fmt.Fprintf(p.out, "[播放] %d 采样 @%dHz（约 %v）\n", len(samples), sampleRate, dur)
	return err
}

// Close 无需操作。
func (p *ConsolePlayer) Close() error { return nil }
