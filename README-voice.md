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

# 指定下载目录
./scripts/download-models.sh /opt/friday-models
```

下载后目录结构：

```
models/
├── asr/sherpa-onnx-paraformer-zh-2023-09-14/    — Paraformer 离线中文识别
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
└── tts/sherpa-onnx-vits-zh-ll/            — VITS 中文语音合成（5 说话人）
    ├── model.onnx
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
  modelDir: models/tts/sherpa-onnx-vits-zh-ll
  voice: ""

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
