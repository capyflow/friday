package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// encodeWAV 构造最小 WAV 字节流（fmt + data），用于测试解码器。
func encodeWAV(channels, rate, bits, format int, pcm []byte) []byte {
	buf := make([]byte, 0, 44+len(pcm))
	buf = append(buf, "RIFF"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(36+len(pcm)))
	buf = append(buf, "WAVE"...)
	buf = append(buf, "fmt "...)
	buf = binary.LittleEndian.AppendUint32(buf, 16)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(format))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(channels))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(rate))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(rate*channels*bits/8))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(channels*bits/8))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(bits))
	buf = append(buf, "data"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(pcm)))
	buf = append(buf, pcm...)
	return buf
}

func pcmBytes(samples []int16) []byte {
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return buf
}

func TestDecodeWAVMono(t *testing.T) {
	raw := encodeWAV(1, 16000, 16, 1, pcmBytes([]int16{1, -2, 3, -4}))
	got, rate, err := decodeWAVBytes(raw)
	if err != nil {
		t.Fatalf("decodeWAVBytes: %v", err)
	}
	if rate != 16000 {
		t.Fatalf("rate = %d, want 16000", rate)
	}
	if len(got) != 4 || got[0] != 1 || got[3] != -4 {
		t.Fatalf("样本不符: %v", got)
	}
}

func TestDecodeWAVStereoDownmix(t *testing.T) {
	raw := encodeWAV(2, 22050, 16, 1, pcmBytes([]int16{100, 200, -300, 100}))
	got, rate, err := decodeWAVBytes(raw)
	if err != nil {
		t.Fatalf("decodeWAVBytes: %v", err)
	}
	if rate != 22050 || len(got) != 2 {
		t.Fatalf("应为 2 个单声道样本 @22050，got %d @%d", len(got), rate)
	}
	if got[0] != 150 || got[1] != -100 {
		t.Fatalf("降混结果不符: %v", got)
	}
}

func TestDecodeWAVOddSizeChunkPadding(t *testing.T) {
	// 在 fmt 前插入一个奇数长度的 LIST 块，验证 2 字节对齐跳过。
	pcm := pcmBytes([]int16{7, 8})
	raw := []byte("RIFF")
	body := append([]byte("LIST"), 5, 0, 0, 0) // size=5（奇数）
	body = append(body, 'I', 'N', 'F', 'O', 'x')
	body = append(body, 0) // 奇数块后 RIFF 规范要求的对齐填充字节
	body = append(body, encodeWAV(1, 8000, 16, 1, pcm)[12:]...)
	raw = binary.LittleEndian.AppendUint32(raw, uint32(4+len(body)))
	raw = append(raw, "WAVE"...)
	raw = append(raw, body...)

	got, rate, err := decodeWAVBytes(raw)
	if err != nil {
		t.Fatalf("decodeWAVBytes: %v", err)
	}
	if rate != 8000 || len(got) != 2 || got[0] != 7 {
		t.Fatalf("解码不符: rate=%d samples=%v", rate, got)
	}
}

func TestDecodeWAVErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"非 wav", []byte("hello world ..........")},
		{"8-bit", encodeWAV(1, 16000, 8, 1, []byte{0, 1})},
		{"非 PCM 编码", encodeWAV(1, 16000, 16, 6, pcmBytes([]int16{0}))},
		{"缺 data 块", func() []byte {
			w := encodeWAV(1, 16000, 16, 1, nil)
			return w[:36] // 截掉 data 块
		}()},
	}
	for _, c := range cases {
		if _, _, err := decodeWAVBytes(c.raw); err == nil {
			t.Errorf("%s 应报错", c.name)
		}
	}
}

func TestDecodeWAVFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.wav")
	if err := os.WriteFile(path, encodeWAV(1, 16000, 16, 1, pcmBytes([]int16{5, 6})), 0o644); err != nil {
		t.Fatal(err)
	}
	samples, rate, err := DecodeWAV(path)
	if err != nil {
		t.Fatalf("DecodeWAV: %v", err)
	}
	if rate != 16000 || len(samples) != 2 || samples[1] != 6 {
		t.Fatalf("结果不符: rate=%d samples=%v", rate, samples)
	}
}
