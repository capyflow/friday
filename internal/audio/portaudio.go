//go:build sherpa

package audio

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gordonklaus/portaudio"
)

// portaudioOnce ensures portaudio.Initialize/Terminate are called exactly once.
var (
	portaudioOnce    sync.Once
	portaudioInitErr error
)

// EnsurePortaudio 确保 PortAudio 全局初始化恰好一次（供本包与其他包共用）。
func EnsurePortaudio() error {
	portaudioOnce.Do(func() {
		portaudioInitErr = portaudio.Initialize()
	})
	return portaudioInitErr
}

// ---- device selection helpers ----

// findInputDevice 按名称子串查找麦克风设备；name 为空时返回默认设备。
func findInputDevice(name string) (*portaudio.DeviceInfo, error) {
	if err := EnsurePortaudio(); err != nil {
		return nil, fmt.Errorf("初始化 PortAudio 失败: %w", err)
	}
	if name == "" {
		d, err := portaudio.DefaultInputDevice()
		if err != nil {
			return nil, fmt.Errorf("获取默认输入设备失败: %w", err)
		}
		return d, nil
	}
	devs, err := portaudio.Devices()
	if err != nil {
		return nil, fmt.Errorf("枚举音频设备失败: %w", err)
	}
	lower := strings.ToLower(name)
	for _, d := range devs {
		if d.MaxInputChannels > 0 && strings.Contains(strings.ToLower(d.Name), lower) {
			return d, nil
		}
	}
	return nil, fmt.Errorf("未找到匹配 %q 的输入设备", name)
}

// findOutputDevice 按名称子串查找播放设备；name 为空时返回默认设备。
func findOutputDevice(name string) (*portaudio.DeviceInfo, error) {
	if err := EnsurePortaudio(); err != nil {
		return nil, fmt.Errorf("初始化 PortAudio 失败: %w", err)
	}
	if name == "" {
		d, err := portaudio.DefaultOutputDevice()
		if err != nil {
			return nil, fmt.Errorf("获取默认输出设备失败: %w", err)
		}
		return d, nil
	}
	devs, err := portaudio.Devices()
	if err != nil {
		return nil, fmt.Errorf("枚举音频设备失败: %w", err)
	}
	lower := strings.ToLower(name)
	for _, d := range devs {
		if d.MaxOutputChannels > 0 && strings.Contains(strings.ToLower(d.Name), lower) {
			return d, nil
		}
	}
	return nil, fmt.Errorf("未找到匹配 %q 的输出设备", name)
}

// ---- Recorder ----

// PortaudioRecorder 通过 PortAudio 采集 16kHz 单声道 int16 PCM。
// 每次 Record 调用独立打开/关闭设备，避免与唤醒词检测器争抢麦克风。
type PortaudioRecorder struct {
	deviceName string
}

// NewPortaudioRecorder 创建录音器。deviceName 为空时自动选择默认麦克风。
func NewPortaudioRecorder(deviceName string) *PortaudioRecorder {
	return &PortaudioRecorder{deviceName: deviceName}
}

// Record 录制 duration 时长的音频，返回 16kHz 单声道 int16 PCM。
func (r *PortaudioRecorder) Record(ctx context.Context, duration time.Duration) ([]int16, error) {
	device, err := findInputDevice(r.deviceName)
	if err != nil {
		return nil, err
	}

	const (
		sampleRate = 16000
		channels   = 1
	)
	totalSamples := int(float64(sampleRate) * duration.Seconds())
	if totalSamples <= 0 {
		return nil, fmt.Errorf("录音时长 %v 无效", duration)
	}

	param := portaudio.StreamParameters{
		Input: portaudio.StreamDeviceParameters{
			Device:   device,
			Channels: channels,
			Latency:  device.DefaultHighInputLatency,
		},
		SampleRate:      float64(sampleRate),
		FramesPerBuffer: 1024,
	}

	buf := make([]int16, 1024)
	stream, err := portaudio.OpenStream(param, &buf)
	if err != nil {
		return nil, fmt.Errorf("打开录音流失败: %w", err)
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		return nil, fmt.Errorf("启动录音流失败: %w", err)
	}

	samples := make([]int16, 0, totalSamples)
	for len(samples) < totalSamples {
		// 检查 ctx 取消
		select {
		case <-ctx.Done():
			_ = stream.Stop()
			return samples, ctx.Err()
		default:
		}
		if err := stream.Read(); err != nil {
			_ = stream.Stop()
			return samples, fmt.Errorf("读取音频数据失败: %w", err)
		}
		n := len(buf)
		remaining := totalSamples - len(samples)
		if n > remaining {
			n = remaining
		}
		samples = append(samples, buf[:n]...)
	}

	if err := stream.Stop(); err != nil {
		return samples, fmt.Errorf("停止录音流失败: %w", err)
	}
	return samples, nil
}

// Close 无需操作（每次 Record 独立管理流）。
func (r *PortaudioRecorder) Close() error { return nil }

// ---- Player ----

// PortaudioPlayer 通过 PortAudio 播放 int16 PCM 音频。
type PortaudioPlayer struct {
	deviceName string
}

// NewPortaudioPlayer 创建播放器。deviceName 为空时自动选择默认喇叭。
func NewPortaudioPlayer(deviceName string) *PortaudioPlayer {
	return &PortaudioPlayer{deviceName: deviceName}
}

// Play 播放 int16 PCM 音频。sampleRate 为采样率（如 16000、22050）。
func (p *PortaudioPlayer) Play(ctx context.Context, samples []int16, sampleRate int) error {
	if len(samples) == 0 {
		return nil
	}
	if sampleRate <= 0 {
		return fmt.Errorf("无效采样率: %d", sampleRate)
	}

	device, err := findOutputDevice(p.deviceName)
	if err != nil {
		return err
	}

	param := portaudio.StreamParameters{
		Output: portaudio.StreamDeviceParameters{
			Device:   device,
			Channels: 1,
			Latency:  device.DefaultHighOutputLatency,
		},
		SampleRate:      float64(sampleRate),
		FramesPerBuffer: 1024,
	}

	buf := make([]int16, 1024)
	stream, err := portaudio.OpenStream(param, &buf)
	if err != nil {
		return fmt.Errorf("打开播放流失败: %w", err)
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		return fmt.Errorf("启动播放流失败: %w", err)
	}

	for offset := 0; offset < len(samples); {
		select {
		case <-ctx.Done():
			_ = stream.Stop()
			return ctx.Err()
		default:
		}
		n := copy(buf, samples[offset:])
		// 最后一块不足 buf 长度时用零填充，确保 portaudio 读到完整帧
		for i := n; i < len(buf); i++ {
			buf[i] = 0
		}
		if err := stream.Write(); err != nil {
			_ = stream.Stop()
			return fmt.Errorf("播放音频数据失败: %w", err)
		}
		offset += n
	}

	if err := stream.Stop(); err != nil {
		return fmt.Errorf("停止播放流失败: %w", err)
	}
	return nil
}

// Close 无需操作。
func (p *PortaudioPlayer) Close() error { return nil }
