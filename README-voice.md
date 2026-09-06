# Friday 语音模式安装指南

语音模式基于 [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx)（离线语音识别/合成）和
[portaudio](https://github.com/gordonklaus/portaudio)（音频采集/播放），需要 cgo 编译。

## 系统依赖安装

### macOS (arm64)

```bash
# PortAudio（麦克风/喇叭驱动）
brew install portaudio

# sherpa-onnx Go 绑定通过 go.mod 自动拉取，无需手动安装 C 库
# 预编译的 libsherpa-onnx-c-api.a / libonnxruntime.a 已包含在 Go 模块中
```

### Linux (amd64/arm64)

```bash
# Ubuntu/Debian
sudo apt-get install -y portaudio19-dev

# CentOS/RHEL
sudo yum install -y portaudio-devel

# sherpa-onnx Go 绑定同上，预编译库随 Go 模块自动下载
```

## 模型下载

```bash
# 一键下载全部中文模型（ASR + KWS + TTS + VAD，约 371MB）
./scripts/download-models.sh

# 1GB 内存开发板（香橙派 Zero 3 / Radxa Zero 3W 等）建议 ASR 换小模型：
# Paraformer zh-small（int8 约 82MB，精度略降），下载后 config.yaml 的
# asr.modelDir 改为 models/asr/sherpa-onnx-paraformer-zh-small-2024-03-09
./scripts/download-models.sh --small-asr

# 指定下载目录
./scripts/download-models.sh --small-asr /opt/friday-models

# 换更好的 TTS（matcha：发音更清晰、合成更快；模型+vocoder 约 125MB）
./scripts/download-models.sh --matcha-tts
```

下载后目录结构：

```
models/
├── asr/sherpa-onnx-paraformer-zh-2023-09-14/    — Paraformer 离线中文识别
│   │                                              （--small-asr 时为
│   │                                               sherpa-onnx-paraformer-zh-small-2024-03-09）
│   ├── model.int8.onnx
│   ├── tokens.txt
│   └── ...
├── wake/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01/  — 中文唤醒词检测
│   ├── encoder-epoch-12-avg-2-chunk-16-left-64.onnx
│   ├── decoder-epoch-12-avg-2-chunk-16-left-64.onnx
│   ├── joiner-epoch-12-avg-2-chunk-16-left-64.onnx
│   ├── tokens.txt
│   ├── keywords.txt                       — 自定义唤醒词（拼音注音格式）
│   └── test_wavs/
└── tts/matcha-icefall-zh-baker/           — Matcha 中文合成（--matcha-tts 下载，含 vocos vocoder）
    │                                        代码按目录布局自动识别 Matcha/VITS 与文本前端
    ├── model-steps-3.onnx
    ├── vocos-22khz-univ.onnx
    ├── tokens.txt
    ├── lexicon.txt
    ├── dict/                              — jieba 分词词典（文本前端）
    └── *.fst                              — 数字/日期/电话等规则
vad/silero_vad.onnx                         — 录音断句（说完自动停止录音）
```

## 配置

编辑 `config.yaml`，将语音引擎从 `none` 改为 `sherpa-onnx`：

```yaml
audio:
  inputDevice: ""           # 留空自动选择，或填设备名子串如 "MacBook"
  outputDevice: ""
  sampleRate: 16000
  channels: 1

asr:
  engine: sherpa-onnx
  modelDir: models/asr/sherpa-onnx-paraformer-zh-2023-09-14

wake:
  engine: sherpa-onnx
  modelDir: models/wake/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01
  keywords: ["小助手"]

tts:
  engine: sherpa-onnx
  modelDir: models/tts/matcha-icefall-zh-baker   # 默认 matcha；换回 VITS 改为 models/tts/sherpa-onnx-vits-zh-ll
  voice: ""                    # matcha 单说话人；VITS（zh-ll）有 0-4 五个可选
  speed: 1.0                   # 语速倍率（越大越快；听不清可试 0.8）

vad:
  modelPath: models/vad/silero_vad.onnx   # 说完自动断句；缺失则固定时长录音
  threshold: 0.5
  minSilence: 0.6              # 静音多久判定说完（秒）
  minSpeech: 0.25
```

### 自定义唤醒词

编辑 `models/wake/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01/keywords.txt`，
格式为「拼音标注 @中文词」，每行一个：

```
x i ǎo zhù sh ǒu @小助手
n ǐ h ǎo @你好
```

拼音标注规则参见 [sherpa-onnx KWS 文档](https://k2-fsa.github.io/sherpa/onnx/kws/pretrained_models/index.html)。

## 编译与运行

```bash
# 语音模式编译（必须加 -tags sherpa）
go build -tags sherpa -o friday ./cmd/robot

# 运行
./friday

# 或直接运行
go run -tags sherpa ./cmd/robot
```

默认编译（不含语音）不受影响：

```bash
go build ./...        # ✅ 文本模式，无 cgo 依赖
go build -tags sherpa ./...  # 需要 portaudio + sherpa-onnx
```

## 语音交互行为

语音循环（唤醒 → 连续对话 → 回待机）的完整状态机，日志按 `[req-N]` 请求 ID 串联（见 `internal/logx`）：

- **唤醒**：命中唤醒词后播放一声"叮"（`internal/audio/effect.go` 运行期合成），随即开始录音。
- **连续对话**：回答完后不要求再次唤醒，直接继续听下一句；直到以下任一情况才回待机——静音超过 3 秒（`voice.Loop.LeadSilence`，兼作跟进等待窗口）、或 LLM 调用 `stop_listening` 工具（用户明确说"不用了/别说了/再见"时模型会调用）。
- **说完断句**：VAD（silero）检测到语音结束后静音 0.6s 即停止录音，无需等满上限（8s）。
- **静音能量门**：录音 RMS 低于门限（`audio.silenceRMS`，默认 150）视为没人说话，跳过 ASR——离线识别对近静音会幻觉出重复文本（如一串"没有"）。
- **没听清**：听到了语音（过能量门）但 ASR 识别为空时，TTS 反馈"我没听清，再叫我一声"后继续听。
- **打断（barge-in）**：LLM 思考与播报期间并行运行唤醒检测，再次叫唤醒词即打断——播报立刻停、进行中的 LLM 请求取消，然后继续听你说下一句。注意：无回声消除，若回复文本恰好包含唤醒词（如"我是小五"）可能自我打断。

## 故障排查

### PortAudio 设备错误

```
错误: 打开录音流失败: ...
```

- 检查麦克风权限（macOS: 系统设置 → 隐私与安全性 → 麦克风）
- 列出可用设备：在 Go 代码中调用 `portaudio.Devices()` 或用 `portaudio-info` 命令
- 填写 `config.yaml` 的 `audio.inputDevice` / `audio.outputDevice` 指定设备

### 模型加载失败

```
错误: 创建 OfflineRecognizer 失败，请检查模型目录 "..."
```

- 确认模型目录存在且包含所需文件
- ASR 需要: `model.int8.onnx` + `tokens.txt`
- KWS 需要: `encoder-*.onnx` + `decoder-*.onnx` + `joiner-*.onnx` + `tokens.txt`
- TTS 需要: `model.onnx` + `tokens.txt` + `lexicon.txt` + `dict/`（jieba 词典）；
  多语种 espeak 前端模型则为 `espeak-ng-data/`（代码按目录自动适配）

### CGO 编译错误

```
cgo: exec gcc: ... (原因: 找不到 portaudio 头文件)
```

- macOS: `brew install portaudio` 确保 `/opt/homebrew/include/portaudio.h` 存在
- Linux: `apt-get install portaudio19-dev` 确保 `/usr/include/portaudio.h` 存在
- 如果 brew 装在非标准路径，设置 `CGO_CFLAGS` 和 `CGO_LDFLAGS`：

```bash
export CGO_CFLAGS="-I/opt/homebrew/include"
export CGO_LDFLAGS="-L/opt/homebrew/lib"
```

### sherpa-onnx Go 模块下载

sherpa-onnx Go 绑定通过 `github.com/k2-fsa/sherpa-onnx-go` 拉取，包含预编译 C 库
（~200MB，含 libsherpa-onnx-c-api + libonnxruntime）。首次编译需下载：

```bash
# 拉取平台特定模块（macOS arm64 约 200MB）
go get github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx@v1.13.7

# 如果网络受限，可手动设置 Go 代理
export GOPROXY=https://goproxy.cn,direct
go get github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx@v1.13.7
```

下载完成后，`go build -tags sherpa ./...` 即可编译。
