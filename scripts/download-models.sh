#!/usr/bin/env bash
set -euo pipefail

# 下载 Friday 机器人语音模型（sherpa-onnx 中文 ASR / KWS / TTS）
# 用法: scripts/download-models.sh [目标目录]
# 默认下载到项目根目录下的 models/ 子目录。

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODELS_DIR="${1:-$PROJECT_ROOT/models}"

ASR_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-paraformer-zh-2023-09-14.tar.bz2"
KWS_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/kws-models/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01.tar.bz2"
TTS_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/sherpa-onnx-vits-zh-ll.tar.bz2"

ASR_DIR="sherpa-onnx-paraformer-zh-2023-09-14"
KWS_DIR="sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01"
TTS_DIR="sherpa-onnx-vits-zh-ll"

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

create_keywords_file() {
    local kws_model_dir="$MODELS_DIR/wake/$KWS_DIR"
    local kw_file="$kws_model_dir/keywords.txt"

    if [ -f "$kw_file" ]; then
        log "keywords.txt 已存在，跳过"
        return 0
    fi

    log "创建默认唤醒词文件: $kw_file"
    cat > "$kw_file" <<'EOF'
# 格式: 拼音标注 @中文词
# 每行一个唤醒词，拼音之间用空格分隔
# 参考: https://k2-fsa.github.io/sherpa/onnx/kws/pretrained_models/index.html
x i ǎo zhù sh ǒu @小助手
EOF
    log "默认唤醒词: 小助手（如需自定义，编辑 $kw_file）"
}

main() {
    log "模型下载目录: $MODELS_DIR"
    mkdir -p "$MODELS_DIR"

    echo ""
    log "=== 下载 ASR 模型（Paraformer 中文离线识别）==="
    download_and_extract "$ASR_URL" "$ASR_DIR" "asr" "ASR"

    echo ""
    log "=== 下载 KWS 模型（Zipformer 中文唤醒词检测）==="
    download_and_extract "$KWS_URL" "$KWS_DIR" "wake" "KWS"

    echo ""
    log "=== 下载 TTS 模型（VITS 中文语音合成）==="
    download_and_extract "$TTS_URL" "$TTS_DIR" "tts" "TTS"

    echo ""
    log "=== 创建唤醒词配置 ==="
    create_keywords_file

    echo ""
    log "全部模型下载完成！"
    log ""
    log "模型目录结构:"
    log "  models/asr/$ASR_DIR/    — ASR 识别模型"
    log "  models/wake/$KWS_DIR/   — KWS 唤醒词模型"
    log "  models/tts/$TTS_DIR/    — TTS 合成模型"
    log ""
    log "请确保 config.yaml 中的路径指向这些目录。"
}

main "$@"
