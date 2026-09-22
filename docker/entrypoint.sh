#!/bin/sh
# friday 容器入口：首次运行播种默认配置，然后启动机器人。
set -e

CONFIG="${CONFIG_PATH:-/data/config.yaml}"

if [ ! -f "$CONFIG" ]; then
    cp /usr/share/friday/config.yaml.example "$CONFIG"
    echo "=============================================================="
    echo " 已生成默认配置：$CONFIG"
    echo " 请编辑该文件补全 provider 段（baseURL / model / apiKeyEnv），"
    echo " 并以 -e <apiKeyEnv 指定的环境变量名>=sk-... 注入 API 密钥，"
    echo " 然后重启容器。"
    echo "==============================================================" >&2
fi

exec /usr/local/bin/robot -config "$CONFIG"
