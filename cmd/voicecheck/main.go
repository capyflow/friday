// Command voicecheck 是语音链路的文件回放测试工具：不依赖麦克风/喇叭，
// 用 wav 文件驱动 ASR 识别、把文本合成导出为 wav，用于在无音频设备的环境
// 验证 sherpa-onnx 模型集成是否正常。
//
// 用法（需 -tags sherpa 编译，依赖安装见 README-voice.md）：
//
//	go run -tags sherpa ./cmd/voicecheck asr [-model <ASR模型目录>] <wav文件>...
//	go run -tags sherpa ./cmd/voicecheck tts [-model <TTS模型目录>] [-voice <说话人ID>] -out <输出.wav> <文本>...
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "asr":
		err = runASR(os.Args[2:])
	case "tts":
		err = runTTS(os.Args[2:])
	case "devices":
		err = runDevices()
	case "rec":
		err = runRec(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法:
  voicecheck asr [-model <ASR模型目录>] <wav文件>...
  voicecheck tts [-model <TTS模型目录>] [-voice <说话人ID>] -out <输出.wav> <文本>...
  voicecheck devices              # 列出 portaudio 设备与默认输入/输出
  voicecheck rec [-device <名称子串>] -out <输出.wav> -sec <秒数>   # 用与 robot 相同的路径录音自检

需要 -tags sherpa 编译：go run -tags sherpa ./cmd/voicecheck ...`)
}
