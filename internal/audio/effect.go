package audio

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// 内置提示音：运行期合成正弦音，无需随仓库/设备携带音频资产。

// EffectSampleRate 音效合成采样率。
const EffectSampleRate = 22050

// EffectNames 返回全部内置音效名称（字母序）。
func EffectNames() []string { return []string{"alert", "ding", "dingdong"} }

// Effect 按名称合成提示音，返回 16-bit 单声道 PCM 与采样率。
// ding：单音"叮"；dingdong：双音"叮咚"；alert：三声急促提示。
func Effect(name string) ([]int16, int, error) {
	switch name {
	case "ding":
		return synthTones([]tone{{880, 350 * time.Millisecond}}, 8, 0), EffectSampleRate, nil
	case "dingdong":
		return synthTones([]tone{{880, 220 * time.Millisecond}, {660, 320 * time.Millisecond}}, 6, 30*time.Millisecond), EffectSampleRate, nil
	case "alert":
		return synthTones([]tone{{1050, 110 * time.Millisecond}, {1050, 110 * time.Millisecond}, {1050, 110 * time.Millisecond}}, 0, 70*time.Millisecond), EffectSampleRate, nil
	default:
		return nil, 0, fmt.Errorf("未知音效 %q，可选：%s", name, strings.Join(EffectNames(), "/"))
	}
}

// tone 一个音符：频率与时长。
type tone struct {
	freq float64
	dur  time.Duration
}

// synthTones 依次合成音符。decay 为指数衰减系数（0 表示平直方波式哔声），
// 每个音符首尾 3ms 线性过渡防爆音，音符间留 gap 静音。
func synthTones(notes []tone, decay float64, gap time.Duration) []int16 {
	const (
		amplitude = 0.6
		edgeSec   = 0.003
	)
	rate := float64(EffectSampleRate)
	edge := rate * edgeSec
	gapN := int(gap.Seconds() * rate)

	out := make([]int16, 0, int(rate)*2)
	for _, nt := range notes {
		n := int(nt.dur.Seconds() * rate)
		for i := 0; i < n; i++ {
			t := float64(i) / rate
			env := math.Exp(-decay * t)
			switch {
			case float64(i) < edge:
				env *= float64(i) / edge
			case float64(n-i) < edge:
				env *= float64(n-i) / edge
			}
			v := amplitude * env * math.Sin(2*math.Pi*nt.freq*t)
			out = append(out, int16(v*32767))
		}
		out = append(out, make([]int16, gapN)...)
	}
	return out
}
