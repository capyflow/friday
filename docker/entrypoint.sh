#!/bin/sh
# friday 容器入口：首次运行播种默认配置，然后启动机器人。
set -e

CONFIG="${CONFIG_PATH:-/data/config.yaml}"
ADMIN_BIND_PORT="${ADMIN_PORT:-8080}"

# 容器健康检查依赖固定端口，禁止 0（随机端口）或无效值导致检测地址失配。
case "$ADMIN_BIND_PORT" in
    *[!0-9]*)
        echo "ADMIN_PORT 必须是 1 到 65535 的端口号" >&2
        exit 2
        ;;
esac
if [ "${#ADMIN_BIND_PORT}" -gt 5 ] || [ "$ADMIN_BIND_PORT" -lt 1 ] || [ "$ADMIN_BIND_PORT" -gt 65535 ]; then
    echo "ADMIN_PORT 必须是 1 到 65535 的端口号" >&2
    exit 2
fi

if [ ! -f "$CONFIG" ]; then
    cp /usr/share/friday/config.yaml.example "$CONFIG"
    echo "=============================================================="
    echo " 已生成默认配置：$CONFIG"
    echo " 请编辑该文件补全 provider 段（baseURL / model / apiKeyEnv），"
    echo " 并以 -e <apiKeyEnv 指定的环境变量名>=sk-... 注入 API 密钥，"
    echo " 然后重启容器。"
    echo "==============================================================" >&2
fi

# 显式覆盖播种配置和既有配置，让监听地址、映射与健康检查保持一致。
exec /usr/local/bin/robot -config "$CONFIG" -host "${ADMIN_HOST:-0.0.0.0}" -port "$ADMIN_BIND_PORT"
