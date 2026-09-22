# friday（陪伴机器人"小五"）镜像：
#   构建：docker build -t friday .
#   运行：docker run -d --name friday \
#           -p 8080:8080 \
#           -v friday-data:/data \
#           -e DEEPSEEK_API_KEY=sk-... \
#           friday
#
# 说明：
#   - 所有可变数据（config.yaml、session.json、tasks.json、memories.json、
#     admin-account.json、uploads/、HEARTBEAT.md）都落在 /data（相对路径基于
#     工作目录 /data），挂载卷即可持久化。
#   - 首次启动若 /data/config.yaml 不存在，会从镜像内示例复制一份，按注释补全
#     provider 段后重启；API 密钥经 provider.apiKeyEnv 指定的环境变量注入，不落盘。
#   - 运行镜像用 alpine（自带 busybox sh）：exec_command 工具靠 sh -c 执行命令。
#   - MCP 服务器：远程 URL（Streamable HTTP）开箱即用；本地 stdio 服务器需要
#     自行扩展镜像安装对应运行时（如 node/npx）。
#   - 时区默认 Asia/Shanghai（定时任务/心跳的 cron 依赖它），可用 -e TZ 覆盖。

FROM golang:1.25-alpine AS build
# 构建时需拉取 Go 依赖：capyflow/* 为私有仓库，经 GOPRIVATE 走 git+SSH 直连，
# 因此构建镜像要传 SSH 凭据：docker build --ssh default -t friday .
#（CI 里确保 ssh-agent 已加载密钥）。公共依赖仍走 goproxy.cn。
RUN apk add --no-cache git openssh-client
ENV GOPRIVATE=github.com/capyflow/* \
    GOPROXY=https://goproxy.cn,direct
RUN git config --global url."git@github.com:".insteadOf https://github.com/ \
    && mkdir -p /root/.ssh \
    && ssh-keyscan -t rsa,ecdsa,ed25519 github.com >> /root/.ssh/known_hosts 2>/dev/null
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=ssh \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags "-s -w" -o /out/robot ./cmd/robot

FROM alpine:3.22
# 非 root 运行；UID 固定 1000，与宿主机普通用户对齐，bind mount /data 时无需调权限
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 1000 -S friday \
    && adduser -S -u 1000 -G friday -H -h /data friday \
    && mkdir -p /data && chown friday:friday /data

COPY --from=build /out/robot /usr/local/bin/robot
COPY config.yaml.example /usr/share/friday/config.yaml.example
COPY docker/entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# 管理后台/网页聊天端口（改了 config 里 admin.port 的话同步调整映射，
# 并用 -e ADMIN_PORT=<端口> 让健康检查跟随）。
ENV TZ=Asia/Shanghai \
    CONFIG_PATH=/data/config.yaml \
    ADMIN_PORT=8080
WORKDIR /data
VOLUME /data
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${ADMIN_PORT}/" || exit 1

USER friday
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
