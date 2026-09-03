//go:build sherpa

package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gordonklaus/portaudio"

	"github.com/capyflow/friday/internal/asr"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/tts"
)

// runASR 逐个识别 wav 文件，打印识别文本与耗时。
func runASR(args []string) error {
	fs := flag.NewFlagSet("asr", flag.ExitOnError)
	model := fs.String("model", "models/asr/sherpa-onnx-paraformer-zh-2023-09-14", "ASR 模型目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("用法: voicecheck asr [-model <模型目录>] <wav文件>...")
	}

	rec, err := asr.NewSherpaRecognizer(*model)
	if err != nil {
		return err
	}
	defer rec.Close()

	for _, path := range fs.Args() {
		samples, rate, err := readWav(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		start := time.Now()
		text, err := rec.Recognize(context.Background(), samples)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		fmt.Printf("%s（%.1fs 音频，识别耗时 %.2fs）: %s\n",
			filepath.Base(path), float64(len(samples))/float64(rate), time.Since(start).Seconds(), text)
	}
	return nil
}

// runTTS 把文本合成为 wav 文件并打印响度统计，无喇叭也能确认产物有效。
func runTTS(args []string) error {
	fs := flag.NewFlagSet("tts", flag.ExitOnError)
	model := fs.String("model", "models/tts/sherpa-onnx-vits-zh-ll", "TTS 模型目录")
	voice := fs.String("voice", "", "说话人 ID，留空用默认")
	out := fs.String("out", "", "输出 wav 路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" || *out == "" {
		return fmt.Errorf("用法: voicecheck tts [-model <目录>] [-voice <ID>] -out <输出.wav> <文本>...")
	}

	synth, err := tts.NewSherpaSynthesizer(*model, *voice)
	if err != nil {
		return err
	}
	defer synth.Close()

	start := time.Now()
	pcm, rate, err := synth.Synthesize(context.Background(), text)
	if err != nil {
		return err
	}
	elapsed := time.Since(start).Seconds()

	if err := writeWav(*out, pcm, rate); err != nil {
		return err
	}

	// 响度统计：peak/rms 全为 0 说明产物是静音，正常应有千级数值
	var peak int
	var sum float64
	for _, s := range pcm {
		v := int(s)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
		sum += float64(v) * float64(v)
	}
	rms := 0.0
	if len(pcm) > 0 {
		rms = math.Sqrt(sum / float64(len(pcm)))
	}
	fmt.Printf("已写出 %s：%.2f 秒（%dHz），合成耗时 %.2fs，peak=%d rms=%.0f\n",
		*out, float64(len(pcm))/float64(rate), rate, elapsed, peak, rms)
	return nil
}

// runDevices 列出 portaudio 枚举到的音频设备与默认输入/输出，排查设备选择问题。
func runDevices() error {
	if err := audio.EnsurePortaudio(); err != nil {
		return err
	}
	defIn, _ := portaudio.DefaultInputDevice()
	defOut, _ := portaudio.DefaultOutputDevice()
	devs, err := portaudio.Devices()
	if err != nil {
		return err
	}
	for _, d := range devs {
		mark := ""
		if defIn != nil && d.Index == defIn.Index {
			mark += " <== 默认输入"
		}
		if defOut != nil && d.Index == defOut.Index {
			mark += " <== 默认输出"
		}
		fmt.Printf("#%-2d %-50s in:%d out:%d%s\n", d.Index, d.Name, d.MaxInputChannels, d.MaxOutputChannels, mark)
	}
	return nil
}

// runRec 用与 robot 语音模式完全相同的 PortaudioRecorder 路径录音，验证采集链路。
func runRec(args []string) error {
	fs := flag.NewFlagSet("rec", flag.ExitOnError)
	device := fs.String("device", "", "输入设备名称子串，留空用默认")
	out := fs.String("out", "rec-check.wav", "输出 wav 路径")
	sec := fs.Float64("sec", 3, "录音时长（秒）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rec := audio.NewPortaudioRecorder(*device)
	samples, err := rec.Record(context.Background(), time.Duration(*sec*float64(time.Second)), nil)
	if err != nil && len(samples) == 0 {
		return err
	}
	if err := writeWav(*out, samples, 16000); err != nil {
		return err
	}
	var peak int
	var sum float64
	for _, s := range samples {
		v := int(s)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
		sum += float64(v) * float64(v)
	}
	rms := math.Sqrt(sum / math.Max(1, float64(len(samples))))
	fmt.Printf("已录 %.1fs → %s，peak=%d rms=%.0f（err=%v）\n",
		float64(len(samples))/16000, *out, peak, rms, err)
	return nil
}

// readWav 读取 16-bit PCM wav；立体声自动下混为单声道。
// 采样率非 16kHz 时报错（paraformer 只接受 16k 输入），并提示 ffmpeg 转换命令。
func readWav(path string) ([]int16, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("不是 RIFF/WAVE 文件")
	}

	var (
		rate     int
		channels int
		raw      []byte
	)
	for pos := 12; pos+8 <= len(data); {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		if pos+8+size > len(data) {
			size = len(data) - pos - 8
		}
		body := data[pos+8 : pos+8+size]
		switch id {
		case "fmt ":
			if len(body) < 16 {
				return nil, 0, fmt.Errorf("fmt 块过短")
			}
			format := binary.LittleEndian.Uint16(body[0:2])
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			rate = int(binary.LittleEndian.Uint32(body[4:8]))
			bits := binary.LittleEndian.Uint16(body[14:16])
			if format != 1 || bits != 16 {
				return nil, 0, fmt.Errorf("仅支持 16-bit PCM，实际 format=%d bits=%d", format, bits)
			}
		case "data":
			raw = body
		}
		pos += 8 + size
		if size%2 == 1 {
			pos++ // RIFF 块按 2 字节对齐
		}
	}
	if raw == nil || rate == 0 {
		return nil, 0, fmt.Errorf("缺少 fmt/data 块")
	}

	n := len(raw) / 2
	samples := make([]int16, n)
	for i := 0; i < n; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(raw[i*2 : i*2+2]))
	}
	if channels == 2 {
		mono := make([]int16, n/2)
		for i := range mono {
			mono[i] = int16((int32(samples[2*i]) + int32(samples[2*i+1])) / 2)
		}
		samples = mono
	}
	if rate != 16000 {
		return nil, rate, fmt.Errorf("采样率 %dHz ≠ 16kHz（ffmpeg -i in -ar 16000 -ac 1 out.wav 转换后再试）", rate)
	}
	return samples, rate, nil
}

// writeWav 写出 16-bit 单声道 PCM wav（标准 44 字节头）。
func writeWav(path string, samples []int16, rate int) error {
	buf := make([]byte, 44+len(samples)*2)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+len(samples)*2))
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
	binary.LittleEndian.PutUint32(buf[40:], uint32(len(samples)*2))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(s))
	}
	return os.WriteFile(path, buf, 0o644)
}
