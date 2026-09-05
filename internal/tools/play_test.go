package tools

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/capyflow/friday/internal/audio"
)

type fakePlayer struct {
	rates []int
	sizes []int
	err   error
}

func (f *fakePlayer) Play(_ context.Context, samples []int16, rate int) error {
	if f.err != nil {
		return f.err
	}
	f.sizes = append(f.sizes, len(samples))
	f.rates = append(f.rates, rate)
	return nil
}

func (f *fakePlayer) Close() error { return nil }

// writeTestWAV 写一个 16-bit 单声道 wav 供 play_audio 测试。
func writeTestWAV(t *testing.T, rate int, samples []int16) string {
	t.Helper()
	pcm := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	buf := make([]byte, 44+len(pcm))
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+len(pcm)))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1) // 单声道
	binary.LittleEndian.PutUint32(buf[24:], uint32(rate))
	binary.LittleEndian.PutUint32(buf[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(buf[32:], 2)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(len(pcm)))
	copy(buf[44:], pcm)
	path := filepath.Join(t.TempDir(), "test.wav")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlayAudioCall(t *testing.T) {
	p := &fakePlayer{}
	tool := &PlayAudioTool{Player: p}
	path := writeTestWAV(t, 8000, []int16{1, 2, 3, 4, 5})

	res, err := tool.Call(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("返回为空")
	}
	if len(p.rates) != 1 || p.rates[0] != 8000 || p.sizes[0] != 5 {
		t.Fatalf("应播放 5 个样本 @8000Hz，got %v @%v", p.sizes, p.rates)
	}
}

func TestPlayAudioErrors(t *testing.T) {
	p := &fakePlayer{}
	tool := &PlayAudioTool{Player: p}
	if _, err := tool.Call(context.Background(), nil); err == nil {
		t.Fatal("缺少 path 应报错")
	}
	if _, err := tool.Call(context.Background(), map[string]any{"path": "/nonexistent/x.wav"}); err == nil {
		t.Fatal("文件不存在应报错")
	}
	if _, err := (&PlayAudioTool{}).Call(context.Background(), map[string]any{"path": "x.wav"}); err == nil {
		t.Fatal("无播放设备应报错")
	}
}

func TestPlayEffectCall(t *testing.T) {
	p := &fakePlayer{}
	tool := &PlayEffectTool{Player: p}

	if _, err := tool.Call(context.Background(), map[string]any{"name": "ding"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(p.sizes) != 1 || p.sizes[0] == 0 {
		t.Fatalf("应播放非空音效，got %v", p.sizes)
	}
	if _, err := tool.Call(context.Background(), map[string]any{"name": "不存在的"}); err == nil {
		t.Fatal("未知音效应报错")
	}
	if _, err := tool.Call(context.Background(), nil); err == nil {
		t.Fatal("缺少 name 应报错")
	}
	if _, err := (&PlayEffectTool{}).Call(context.Background(), map[string]any{"name": "ding"}); err == nil {
		t.Fatal("无播放设备应报错")
	}
}

// ConsolePlayer 满足 audio.Player 接口（文本模式兜底）。
func TestConsolePlayerIsPlayer(t *testing.T) {
	var _ audio.Player = audio.NewConsolePlayer(os.Stdout)
}
