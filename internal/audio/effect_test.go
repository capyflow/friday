package audio

import (
	"math"
	"testing"
)

func TestEffectPresets(t *testing.T) {
	for _, name := range EffectNames() {
		samples, rate, err := Effect(name)
		if err != nil {
			t.Fatalf("Effect(%q): %v", name, err)
		}
		if rate != EffectSampleRate {
			t.Fatalf("Effect(%q) 采样率 = %d, want %d", name, rate, EffectSampleRate)
		}
		if len(samples) == 0 {
			t.Fatalf("Effect(%q) 样本为空", name)
		}
		var peak float64
		for _, s := range samples {
			peak = math.Max(peak, math.Abs(float64(s)))
		}
		if peak < 1000 {
			t.Fatalf("Effect(%q) 峰值过低（%.0f），像静音", name, peak)
		}
		if peak > 32767 {
			t.Fatalf("Effect(%q) 峰值 %0.f 溢出 int16", name, peak)
		}
	}
}

func TestEffectUnknown(t *testing.T) {
	if _, _, err := Effect("rock"); err == nil {
		t.Fatal("未知音效应报错")
	}
}

// ding 约比 dingdong 短一截（单音 vs 双音）。
func TestEffectRelativeLength(t *testing.T) {
	ding, _, _ := Effect("ding")
	dong, _, _ := Effect("dingdong")
	if len(ding) >= len(dong) {
		t.Fatalf("ding(%d) 应短于 dingdong(%d)", len(ding), len(dong))
	}
}
