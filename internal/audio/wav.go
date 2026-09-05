package audio

import (
	"encoding/binary"
	"fmt"
	"os"
)

// DecodeWAV 读取并解码 WAV 文件，返回单声道 16-bit PCM 样本与采样率。
// 仅支持 PCM 编码（format 1，或声明为 WAVE_FORMAT_EXTENSIBLE 的 16-bit）；
// 立体声自动降混为单声道（Player 输出约定为单声道）。
func DecodeWAV(path string) ([]int16, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("读取音频文件: %w", err)
	}
	samples, rate, err := decodeWAVBytes(raw)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", path, err)
	}
	return samples, rate, nil
}

func decodeWAVBytes(raw []byte) ([]int16, int, error) {
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("不是 WAV 文件（缺少 RIFF/WAVE 头）")
	}

	var (
		rate     int
		channels int
		bits     int
		format   int
		data     []byte
		hasFmt   bool
		hasData  bool
	)
	for pos := 12; pos+8 <= len(raw); {
		id := string(raw[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(raw[pos+4 : pos+8]))
		if pos+8+size > len(raw) {
			size = len(raw) - pos - 8 // 声明的块大小超出实际数据时按实际处理
		}
		body := raw[pos+8 : pos+8+size]
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, fmt.Errorf("fmt 块过短: %d 字节", size)
			}
			format = int(binary.LittleEndian.Uint16(body[0:2]))
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			rate = int(binary.LittleEndian.Uint32(body[4:8]))
			bits = int(binary.LittleEndian.Uint16(body[14:16]))
			hasFmt = true
		case "data":
			data = body
			hasData = true
		}
		pos += 8 + size
		if size%2 == 1 {
			pos++ // RIFF 块按 2 字节对齐
		}
	}
	if !hasFmt || !hasData {
		return nil, 0, fmt.Errorf("缺少 fmt/data 块")
	}
	if format != 1 && format != 0xFFFE {
		return nil, 0, fmt.Errorf("仅支持 PCM 编码的 wav（format=%d，mp3 等需先转换）", format)
	}
	if bits != 16 {
		return nil, 0, fmt.Errorf("仅支持 16-bit wav（当前 %d bit）", bits)
	}
	if channels < 1 || channels > 2 {
		return nil, 0, fmt.Errorf("仅支持单声道/立体声（当前 %d 声道）", channels)
	}
	if rate <= 0 {
		return nil, 0, fmt.Errorf("无效采样率: %d", rate)
	}

	n := len(data) / 2
	if n == 0 {
		return nil, 0, fmt.Errorf("音频数据为空")
	}
	samples := make([]int16, n)
	for i := 0; i < n; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
	}
	if channels == 2 {
		mono := make([]int16, n/2)
		for i := range mono {
			mono[i] = int16((int32(samples[2*i]) + int32(samples[2*i+1])) / 2)
		}
		samples = mono
	}
	return samples, rate, nil
}
