// Package voice 编排语音交互链路：唤醒词 → 录音 → 语音识别 → 交给对话处理。
// 还提供 TTS+喇叭 组装成 output.Speaker 的适配器。
package voice

import (
	"context"
	"strings"
	"time"

	"github.com/capyflow/friday/archive/voice-mode/asr"
	"github.com/capyflow/friday/archive/voice-mode/tts"
	"github.com/capyflow/friday/archive/voice-mode/vad"
	"github.com/capyflow/friday/archive/voice-mode/wake"
	"github.com/capyflow/friday/internal/audio"
	"github.com/capyflow/friday/internal/logx"
)

// Loop 是语音交互主循环。
type Loop struct {
	Wake     wake.Detector
	Recorder audio.Recorder
	ASR      asr.Recognizer
	// VAD 非 nil 时说完自动断句；nil 时录满 MaxRecord。
	VAD *vad.Detector
	// MaxRecord 单次录音时长上限。
	MaxRecord time.Duration
	// LeadSilence 唤醒后的静音等待上限：期间 VAD 始终没检测到语音就提前收兵
	// 回待机（避免干等 MaxRecord）。0 视为 3 秒；仅 VAD 启用时生效。
	LeadSilence time.Duration
	// SilenceRMS 静音门限：整段录音 RMS 低于该值视为纯静音，跳过 ASR 直接
	// 走"没说话"路径（离线 ASR 对近静音会幻觉出重复文本）。0 视为默认 150。
	SilenceRMS float64
	// WakeChime 唤醒命中后的提示音回调（nil 不播），告知用户"我在听了"。
	WakeChime func(ctx context.Context) error
	// Misheard 听到了语音但没识别出内容时的反馈回调（nil 不播）。
	Misheard func(ctx context.Context) error
	// BargeIn 为 true 时 handle 执行期间（LLM/播报）并行检测唤醒词，
	// 再次叫唤醒词即可打断当前回复，随后继续听下一句。
	BargeIn bool
	// StopSignal 在每轮回答结束后调用，返回 true 表示用户已要求结束连续对话
	// （stop_listening 工具置位，实现方须保证读即清除）。对话随之回待机。
	StopSignal func() bool
}

// Run 持续执行"等待唤醒 → 连续对话"，直到 ctx 取消。
// 一次唤醒开启一轮对话：识别 → handle 回答 → 不需唤醒词继续听下一句；
// 直到静音超时、或用户通过 stop_listening 明确结束，才回待机等唤醒。
func (l *Loop) Run(ctx context.Context, handle func(ctx context.Context, text string) error) error {
	for {
		if err := l.Wake.Wait(ctx); err != nil {
			return err
		}
		logx.Printf(ctx, "[语音] 检测到唤醒")
		if l.WakeChime != nil {
			if err := l.WakeChime(ctx); err != nil {
				logx.Printf(ctx, "[语音] 提示音播放失败: %v", err)
			}
		}
		for first := true; ; first = false {
			start := time.Now()
			samples, heard, err := l.record(ctx)
			if err != nil {
				return err
			}
			// 能量门：纯静音不送识别（防 ASR 对噪声的幻觉），日志带实测 RMS 便于调门限。
			if rms := audio.RMS(samples); rms < l.silenceFloor() {
				if first {
					logx.Printf(ctx, "[语音] 录音 RMS=%.0f 低于静音门限 %.0f，视为没说话，返回待机", rms, l.silenceFloor())
				} else {
					logx.Printf(ctx, "[语音] 连续对话静音超时，返回待机")
				}
				break
			}
			logx.Printf(ctx, "[语音] 录音结束：%.1f 秒，开始识别", time.Since(start).Seconds())
			text, err := l.ASR.Recognize(ctx, samples)
			if err != nil {
				return err
			}
			if strings.TrimSpace(text) == "" {
				if heard {
					logx.Printf(ctx, "[语音] 听到了语音但未识别出内容")
					if l.Misheard != nil {
						if err := l.Misheard(ctx); err != nil {
							logx.Printf(ctx, "[语音] 反馈播报失败: %v", err)
						}
					}
					continue // 留在对话里再听一句
				}
				logx.Printf(ctx, "[语音] 唤醒后没有说话，返回待机")
				break
			}
			interrupted, err := l.dispatch(ctx, handle, text)
			if err != nil {
				return err
			}
			if interrupted {
				continue // 叫停是为了说下一句：打断后继续听
			}
			if l.StopSignal != nil && l.StopSignal() {
				logx.Printf(ctx, "[语音] 用户要求结束对话，返回待机")
				break
			}
		}
	}
}

// dispatch 执行 handle；BargeIn 时并行跑唤醒检测，听到唤醒词即取消 actx——
// 中断进行中的 LLM 调用与播报。返回 interrupted 表示回答被打断
// （调用方应继续听下一句而不是结束对话）。退出前等监听 goroutine 释放麦克风，
// 避免与下一轮录音争抢输入设备。
func (l *Loop) dispatch(ctx context.Context, handle func(context.Context, string) error, text string) (bool, error) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		hit        bool
		listenDone = make(chan struct{})
	)
	if l.BargeIn {
		go func() {
			defer close(listenDone)
			if werr := l.Wake.Wait(actx); werr == nil {
				hit = true
				logx.Printf(actx, "[语音] 检测到唤醒，打断当前回复")
			}
		}()
	} else {
		close(listenDone)
	}

	err := handle(actx, text)

	cancel()
	<-listenDone
	if hit {
		logx.Printf(ctx, "[语音] 已打断，请讲")
	}
	return hit, err
}

// record 录一句指令：有 VAD 时说完自动断句、唤醒后始终没开口则等满
// LeadSilence 提前收兵，否则录满 MaxRecord；heard 报告期间是否出现过语音。
func (l *Loop) record(ctx context.Context) (samples []int16, heard bool, err error) {
	var onChunk func(chunk []int16) bool
	if l.VAD != nil {
		l.VAD.Reset() // 清理上一句可能残留的进行中状态
		lead := 0     // 自录音起的样本计数
		leadMax := int(l.leadSilence().Seconds() * recordSampleRate)
		onChunk = func(chunk []int16) bool {
			if l.VAD.EndOfSpeech(chunk) {
				return true // 说完断句
			}
			lead += len(chunk)
			return lead >= leadMax && !l.VAD.HasSpeech() // 始终没开口，提前收兵
		}
	}
	samples, err = l.Recorder.Record(ctx, l.MaxRecord, onChunk)
	heard = l.VAD != nil && l.VAD.HasSpeech()
	return samples, heard, err
}

// leadSilence 返回生效的静音等待上限。
func (l *Loop) leadSilence() time.Duration {
	if l.LeadSilence <= 0 {
		return 3 * time.Second
	}
	return l.LeadSilence
}

// silenceFloor 返回生效的静音 RMS 门限（0 视为默认值）。
func (l *Loop) silenceFloor() float64 {
	if l.SilenceRMS <= 0 {
		return defaultSilenceRMS
	}
	return l.SilenceRMS
}

// defaultSilenceRMS 默认静音 RMS 门限：安静室内噪声通常 <100，近距说话 >500。
const defaultSilenceRMS = 150

// recordSampleRate 录音采样率（与 audio 包的 Record 实现一致）。
const recordSampleRate = 16000

// SpeakerAdapter 把 Synthesizer + Player 组装成 output.Speaker。
type SpeakerAdapter struct {
	Synth  tts.Synthesizer
	Player audio.Player
}

func (s *SpeakerAdapter) Speak(ctx context.Context, text string) error {
	start := time.Now()
	logx.Printf(ctx, "[tts] 开始播报：%s", logx.Truncate(text, 60))
	samples, rate, err := s.Synth.Synthesize(ctx, text)
	if err != nil {
		return err
	}
	logx.Printf(ctx, "[tts] 合成完成：约 %.1f 秒音频 @%dHz（合成耗时 %s），开始播放",
		float64(len(samples))/float64(rate), rate, time.Since(start))
	if err := s.Player.Play(ctx, samples, rate); err != nil {
		return err
	}
	logx.Printf(ctx, "[tts] 播报完成，总耗时 %s", time.Since(start))
	return nil
}
