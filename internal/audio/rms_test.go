package audio

import (
	"math"
	"testing"
)

func TestRMS(t *testing.T) {
	cases := []struct {
		name    string
		samples []int16
		want    float64
	}{
		{"空", nil, 0},
		{"满幅方波", []int16{32767, -32767, 32767, -32767}, 32767},
		{"纯静音", make([]int16, 1024), 0},
	}
	for _, c := range cases {
		if got := RMS(c.samples); got != c.want {
			t.Errorf("%s: RMS = %v, 期望 %v", c.name, got, c.want)
		}
	}

	// 正弦波 RMS ≈ 峰值/√2
	sine := make([]int16, 1600)
	for i := range sine {
		sine[i] = int16(16000 * math.Sin(float64(i)/1600*2*math.Pi))
	}
	got := RMS(sine)
	want := 16000 / 1.41421356
	if got < want*0.98 || got > want*1.02 {
		t.Errorf("正弦波 RMS = %v, 期望约 %v", got, want)
	}
}
