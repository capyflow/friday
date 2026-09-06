package audio

import "math"

// RMS 返回样本的均方根振幅（0~32768），用于判断一段录音是否接近纯静音：
// paraformer 等离线 ASR 对近静音输入会幻觉出重复文本（如一串"没有"），
// 送识别前先用 RMS 门限过滤。
func RMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		v := float64(s)
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(samples)))
}
