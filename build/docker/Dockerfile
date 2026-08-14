# syntax=docker/dockerfile:1

# ============================================================================
# EdgeFlow 镜像构建文件（WBS 8.5）
#
# 用法（在仓库根目录执行，构建上下文 = 仓库根目录）：
#   docker build -f build/docker/Dockerfile --target cloudcore -t edgeflow/cloudcore:v0.1.0 .
#   docker build -f build/docker/Dockerfile --target edgecore  -t edgeflow/edgecore:v0.1.0 .
#
# 可选构建参数（版本注入，与 Makefile LDFLAGS 保持一致）：
#   docker build --build-arg VERSION=v0.2.0 --build-arg GIT_COMMIT=abc123 ...
#
# 设计说明：
#   1. 多阶段构建：
#      阶段 1（builder）在 golang:1.26-alpine 中编译；
#      阶段 2 分别产出 cloudcore / edgecore 两个运行镜像 target。
#   2. 静态编译 CGO_ENABLED=0：SQLite 使用 modernc.org/sqlite（纯 Go 实现），
#      无需 cgo，产物为无外部依赖的静态二进制。
#   3. 运行镜像选择 gcr.io/distroless/static-debian12:nonroot 的理由：
#       - 无 shell、无包管理器、无系统库，攻击面最小，符合生产安全基线；
#       - 内置非 root 用户（uid 65532），配合 Chart securityContext 双重保险；
#       - 代价：容器内无法 exec 调试，排障需 docker cp 拉日志；
#         如需调试可临时把运行阶段基镜像换成 alpine:3.20（自带 shell）。
#   4. 版本/提交/构建时间通过 ARG + -ldflags 注入，docker run --version 可验证。
# ============================================================================

# ---------------------------------------------------------------------------
# 阶段 1：构建阶段
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS builder

# ca-certificates：静态二进制校验 TLS 证书需要（edgecore 连云端 wss / HTTPS 时使用）
# tzdata：时区数据，供日志时间戳本地化（TZ 环境变量生效）
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src

# 先复制依赖清单，利用 Docker 层缓存：依赖未变更时不会重新下载
COPY go.mod go.sum ./
RUN go mod download

# 复制全部源码（.dockerignore 已排除 bin/ dist/ .git docs/ 等非构建内容）
COPY . .

# 版本注入参数：默认与 Makefile VERSION 一致，可用 --build-arg 覆盖
ARG VERSION=v0.1.0
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

# 静态编译 cloudcore（-trimpath 可复现；-s -w 剥离符号表缩小体积）
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w \
        -X edgeflow/pkg/version.Version=${VERSION} \
        -X edgeflow/pkg/version.GitCommit=${GIT_COMMIT} \
        -X edgeflow/pkg/version.BuildTime=${BUILD_TIME}" \
    -o /out/cloudcore ./cmd/cloudcore

# 静态编译 edgecore
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w \
        -X edgeflow/pkg/version.Version=${VERSION} \
        -X edgeflow/pkg/version.GitCommit=${GIT_COMMIT} \
        -X edgeflow/pkg/version.BuildTime=${BUILD_TIME}" \
    -o /out/edgecore ./cmd/edgecore

# 预创建 edgecore 的 SQLite 数据目录并赋予 nonroot（uid 65532）写权限
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# ---------------------------------------------------------------------------
# 阶段 2a：cloudcore 运行镜像
#   监听端口：
#     - 8080  HTTP API + /healthz 健康检查
#     - 10000 CloudHub（云边 WebSocket 通道）
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS cloudcore

# 非 root 用户：distroless nonroot 镜像内置 uid 65532，无需额外 USER 指令
COPY --from=builder /out/cloudcore /cloudcore
# CA 证书与时区数据（静态二进制不自带，需随镜像分发）
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# 端口默认值（环境变量可覆盖，与 Chart values.env 对齐）
ENV EDGEFLOW_CLOUDCORE_PORT=8080 \
    EDGEFLOW_CLOUDCORE_HUB_PORT=10000

EXPOSE 8080 10000

ENTRYPOINT ["/cloudcore"]

# ---------------------------------------------------------------------------
# 阶段 2b：edgecore 运行镜像
#   无入站端口（主动出站连接云端 CloudHub）；
#   本地 SQLite 元数据库写入 /data（生产建议挂载持久卷/emptyDir）
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS edgecore

COPY --from=builder /out/edgecore /edgecore
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# 数据目录：预先授权 nonroot 用户写（COPY --chown 在 distroless 无 shell 下同样生效）
COPY --from=builder --chown=65532:65532 /out/data /data
VOLUME ["/data"]

# 数据库路径固定指向 /data，避免依赖进程工作目录
ENV EDGEFLOW_EDGECORE_DB_PATH=/data/edgeflow.db

# 可选：若以 sidecar 方式后接 MQTT broker（如 Eclipse Mosquitto），
# broker 自身监听 1883 —— 本镜像不监听任何入站端口，以下仅作文档注释：
# EXPOSE 1883

ENTRYPOINT ["/edgecore"]
