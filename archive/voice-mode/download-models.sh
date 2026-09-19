#!/usr/bin/env bash
set -euo pipefail

# 下载 Friday 机器人语音模型（sherpa-onnx 中文 ASR / KWS / TTS）
# 用法: scripts/download-models.sh [--small-asr] [--matcha-tts] [目标目录]
#   --small-asr   ASR 换用小模型（int8 约 82MB，默认全量版约 230MB），适合 1GB 内存开发板
#   --matcha-tts  TTS 换用 Matcha（matcha-icefall-zh-baker + vocos vocoder，约 125MB，
#                 发音更清晰、合成更快；默认为 vits-zh-ll）
# 默认下载到项目根目录下的 models/ 子目录。

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

SMALL_ASR=0
MATCHA_TTS=0
POSITIONAL=()
for arg in "$@"; do
    case "$arg" in
        --small-asr) SMALL_ASR=1 ;;
        --matcha-tts) MATCHA_TTS=1 ;;
        *) POSITIONAL+=("$arg") ;;
    esac
done
MODELS_DIR="${POSITIONAL[0]:-$PROJECT_ROOT/models}"

ASR_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-paraformer-zh-2023-09-14.tar.bz2"
ASR_SMALL_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-paraformer-zh-small-2024-03-09.tar.bz2"
KWS_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/kws-models/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01.tar.bz2"
TTS_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/sherpa-onnx-vits-zh-ll.tar.bz2"
MATCHA_TTS_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/matcha-icefall-zh-baker.tar.bz2"
VOCOS_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/vocoder-models/vocos-22khz-univ.onnx"
VAD_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx"

ASR_DIR="sherpa-onnx-paraformer-zh-2023-09-14"
ASR_SMALL_DIR="sherpa-onnx-paraformer-zh-small-2024-03-09"
KWS_DIR="sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01"
TTS_DIR="sherpa-onnx-vits-zh-ll"
MATCHA_TTS_DIR="matcha-icefall-zh-baker"

log() { echo "==> $*"; }

download_and_extract() {
    local url="$1" expected_dir="$2" target="$3" label="$4"

    if [ -d "$MODELS_DIR/$target/$expected_dir" ]; then
        log "$label 模型已存在，跳过: $MODELS_DIR/$target/$expected_dir"
        return 0
    fi

    log "下载 $label 模型..."
    local tmpfile
    tmpfile=$(mktemp /tmp/sherpa-model-XXXXXX.tar.bz2)

    if ! curl -fSL -o "$tmpfile" "$url"; then
        rm -f "$tmpfile"
        log "错误: $label 模型下载失败"
        return 1
    fi

    mkdir -p "$MODELS_DIR/$target"
    log "解压 $label 模型..."
    tar xjf "$tmpfile" -C "$MODELS_DIR/$target"
    rm -f "$tmpfile"

    if [ ! -d "$MODELS_DIR/$target/$expected_dir" ]; then
        log "错误: 解压后未找到 $MODELS_DIR/$target/$expected_dir"
        return 1
    fi

    log "$label 模型就绪: $MODELS_DIR/$target/$expected_dir"
}

download_vad() {
    local target="$MODELS_DIR/vad/silero_vad.onnx"
    if [ -f "$target" ]; then
        log "VAD 模型已存在，跳过: $target"
        return 0
    fi
    log "下载 VAD 模型（silero，录音断句用）..."
    mkdir -p "$MODELS_DIR/vad"
    curl -fSL -o "$target" "$VAD_URL"
    log "VAD 模型就绪: $target"
}

download_vocoder() {
    local target="$MODELS_DIR/tts/$MATCHA_TTS_DIR/vocos-22khz-univ.onnx"
    if [ -f "$target" ]; then
        log "vocoder 已存在，跳过: $target"
        return 0
    fi
    log "下载 vocoder（vocos 22kHz，matcha 解码用）..."
    curl -fSL -o "$target" "$VOCOS_URL"
    log "vocoder 就绪: $target"
}

create_keywords_file() {
    local kws_model_dir="$MODELS_DIR/wake/$KWS_DIR"
    local kw_file="$kws_model_dir/keywords.txt"
    # 格式: 声母 + 带调韵母（合成一个 token），如 小 = "x iǎo"
    local kw_lines=("x iǎo zh ù sh ǒu @小助手" "x iǎo w ǔ @小五")

    if [ ! -f "$kw_file" ]; then
        log "创建默认唤醒词文件: $kw_file"
        printf '# 格式: 声母 带调韵母 @中文词（每行一个唤醒词）\n# 参考: https://k2-fsa.github.io/sherpa/onnx/kws/pretrained_models/index.html\n' > "$kw_file"
    fi
    # 模型包自带的演示唤醒词保留，逐个追加我们的默认词（已注册的跳过）
    local line word
    for line in "${kw_lines[@]}"; do
        word="${line##*@}"
        if grep -q "@${word}\$" "$kw_file"; then
            log "唤醒词 ${word} 已注册，跳过"
        else
            echo "$line" >> "$kw_file"
            log "已追加默认唤醒词: ${word}（如需自定义，编辑 $kw_file）"
        fi
    done
}

main() {
    log "模型下载目录: $MODELS_DIR"
    mkdir -p "$MODELS_DIR"

    echo ""
    if [ "$SMALL_ASR" -eq 1 ]; then
        log "=== 下载 ASR 模型（Paraformer 中文小模型，低内存设备）==="
        download_and_extract "$ASR_SMALL_URL" "$ASR_SMALL_DIR" "asr" "ASR"
        ASR_CHOSEN_DIR="$ASR_SMALL_DIR"
    else
        log "=== 下载 ASR 模型（Paraformer 中文离线识别）==="
        download_and_extract "$ASR_URL" "$ASR_DIR" "asr" "ASR"
        ASR_CHOSEN_DIR="$ASR_DIR"
    fi

    echo ""
    log "=== 下载 KWS 模型（Zipformer 中文唤醒词检测）==="
    download_and_extract "$KWS_URL" "$KWS_DIR" "wake" "KWS"

    echo ""
    local tts_chosen_dir
    if [ "$MATCHA_TTS" -eq 1 ]; then
        log "=== 下载 TTS 模型（Matcha 中文，含 vocoder）==="
        download_and_extract "$MATCHA_TTS_URL" "$MATCHA_TTS_DIR" "tts" "TTS"
        download_vocoder
        tts_chosen_dir="$MATCHA_TTS_DIR"
    else
        log "=== 下载 TTS 模型（VITS 中文语音合成）==="
        download_and_extract "$TTS_URL" "$TTS_DIR" "tts" "TTS"
        tts_chosen_dir="$TTS_DIR"
    fi

    echo ""
    log "=== 下载 VAD 模型（录音断句）==="
    download_vad

    echo ""
    log "=== 创建唤醒词配置 ==="
    create_keywords_file

    echo ""
    log "全部模型下载完成！"
    log ""
    log "模型目录结构:"
    log "  models/asr/$ASR_CHOSEN_DIR/    — ASR 识别模型"
    log "  models/wake/$KWS_DIR/   — KWS 唤醒词模型"
    log "  models/tts/$tts_chosen_dir/    — TTS 合成模型"
    log "  models/vad/silero_vad.onnx — VAD 断句模型"
    log ""
    log "请确保 config.yaml 中的路径指向这些目录"
    if [ "$SMALL_ASR" -eq 1 ]; then
        log "（--small-asr 模式下 asr.modelDir 应为 models/asr/$ASR_CHOSEN_DIR）"
    fi
    if [ "$MATCHA_TTS" -eq 1 ]; then
        log "（--matcha-tts 模式下 tts.modelDir 应为 models/tts/$MATCHA_TTS_DIR）"
    fi
}

main "$@"
