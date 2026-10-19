# friday（个人管家"小五"）镜像：
#   构建：docker build -t friday .
#   （三阶段：node 构建前端产物 → go 基于 vendor/ 离线编译；前端产物与 Go 依赖
#    均随仓库/镜像走，构建过程零网络依赖，无需 SSH 凭据）
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

# 前端阶段：npm 构建自包含 HTML，产物写到 /src/internal/admin/webdist/
#（vite outDir 指向仓库内的 go:embed 目录）。产物不入库，镜像构建是唯一产物来源。
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ .
RUN npm run build

FROM golang:1.25-alpine AS build
# Go 依赖经仓库内 vendor/ 离线编译（升级依赖：宿主机 go mod vendor 后重新构建），
# 构建零网络依赖、无需任何凭据。
WORKDIR /src
COPY . .
# 用前端阶段产物覆盖（.dockerignore 已排除宿主机本地副本，避免陈旧产物烤进镜像）
COPY --from=web /src/internal/admin/webdist/ ./internal/admin/webdist/
RUN CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=vendor \
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

# 容器绑定全部网卡；入口脚本用 ADMIN_HOST/ADMIN_PORT 覆盖已有配置，
# 服务监听与健康检查共用 ADMIN_PORT。自定义端口时同步调整映射。
ENV TZ=Asia/Shanghai \
    CONFIG_PATH=/data/config.yaml \
    ADMIN_HOST=0.0.0.0 \
    ADMIN_PORT=8080
WORKDIR /data
VOLUME /data
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${ADMIN_PORT}/" || exit 1

USER friday
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
