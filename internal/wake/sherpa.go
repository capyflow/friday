//go:build sherpa

package wake

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/gordonklaus/portaudio"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/capyflow/friday/internal/audio"
)

// SherpaDetector 通过 sherpa-onnx KeywordSpotter 实现唤醒词检测。
type SherpaDetector struct {
	spotter    *sherpa.KeywordSpotter
	deviceName string
}

// NewSherpaDetector 创建唤醒词检测器。
// modelDir 包含 KWS 模型文件（encoder/decoder/joiner/tokens）与 keywords.txt（唤醒词列表）。
func NewSherpaDetector(modelDir string, _ []string, deviceName string) (*SherpaDetector, error) {
	log.Printf("[wake] 加载模型中: %s", modelDir)
	config := sherpa.KeywordSpotterConfig{}
	config.FeatConfig.SampleRate = 16000
	config.FeatConfig.FeatureDim = 80

	config.ModelConfig.Transducer.Encoder = modelDir + "/encoder-epoch-12-avg-2-chunk-16-left-64.onnx"
	config.ModelConfig.Transducer.Decoder = modelDir + "/decoder-epoch-12-avg-2-chunk-16-left-64.onnx"
	config.ModelConfig.Transducer.Joiner = modelDir + "/joiner-epoch-12-avg-2-chunk-16-left-64.onnx"
	config.ModelConfig.Tokens = modelDir + "/tokens.txt"
	config.ModelConfig.NumThreads = 2
	config.ModelConfig.Provider = "cpu"
	// 唤醒词来自模型目录下的 keywords.txt（拼音注音格式，见 scripts/download-models.sh）。
	config.KeywordsFile = modelDir + "/keywords.txt"

	spotter := sherpa.NewKeywordSpotter(&config)
	if spotter == nil {
		return nil, fmt.Errorf("创建 KeywordSpotter 失败，请检查模型目录 %q 与 keywords.txt", modelDir)
	}
	log.Printf("[wake] 模型加载完成")

	return &SherpaDetector{
		spotter:    spotter,
		deviceName: deviceName,
	}, nil
}

// Wait 阻塞直到检测到唤醒词（返回 nil）或 ctx 取消（返回 ctx.Err()）。
// 内部持续打开麦克风读取音频并送入 KWS，检测到或取消后立即释放音频流。
func (d *SherpaDetector) Wait(ctx context.Context) error {
	device, err := findWakeDevice(d.deviceName)
	if err != nil {
		return err
	}

	const (
		sampleRate = 16000
		channels   = 1
	)

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
		return fmt.Errorf("打开唤醒词检测音频流失败: %w", err)
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		return fmt.Errorf("启动唤醒词检测音频流失败: %w", err)
	}
	defer stream.Stop()

	// 创建 KWS 流（使用 keywords.txt 中的预定义唤醒词）
	kwsStream := sherpa.NewKeywordStream(d.spotter)
	defer sherpa.DeleteOnlineStream(kwsStream)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := stream.Read(); err != nil {
			return fmt.Errorf("读取唤醒词检测音频失败: %w", err)
		}

		// int16 PCM → float32 [-1, 1]
		floats := make([]float32, len(buf))
		for i, s := range buf {
			floats[i] = float32(s) / 32768.0
		}

		kwsStream.AcceptWaveform(sampleRate, floats)

		for d.spotter.IsReady(kwsStream) {
			d.spotter.Decode(kwsStream)
			result := d.spotter.GetResult(kwsStream)
			if result.Keyword != "" {
				d.spotter.Reset(kwsStream)
				return nil
			}
		}
	}
}

// Detect 对一段离线 PCM（16kHz 单声道）做唤醒词检测，按顺序返回命中的唤醒词。
// 与 Wait 走同一套 KWS 配置，用于不占麦克风的诊断与测试。
func (d *SherpaDetector) Detect(samples []int16) []string {
	kwsStream := sherpa.NewKeywordStream(d.spotter)
	defer sherpa.DeleteOnlineStream(kwsStream)

	var hits []string
	const chunk = 1024
	for off := 0; off < len(samples); off += chunk {
		end := off + chunk
		if end > len(samples) {
			end = len(samples)
		}
		floats := make([]float32, end-off)
		for i, s := range samples[off:end] {
			floats[i] = float32(s) / 32768.0
		}
		kwsStream.AcceptWaveform(16000, floats)
		for d.spotter.IsReady(kwsStream) {
			d.spotter.Decode(kwsStream)
			if result := d.spotter.GetResult(kwsStream); result.Keyword != "" {
				hits = append(hits, result.Keyword)
				d.spotter.Reset(kwsStream)
			}
		}
	}
	return hits
}

// Close 释放 KeywordSpotter 资源。
func (d *SherpaDetector) Close() error {
	if d.spotter != nil {
		sherpa.DeleteKeywordSpotter(d.spotter)
		d.spotter = nil
	}
	return nil
}

// findWakeDevice 按名称子串查找唤醒词检测用的输入设备。
func findWakeDevice(name string) (*portaudio.DeviceInfo, error) {
	if err := audio.EnsurePortaudio(); err != nil {
		return nil, err
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
