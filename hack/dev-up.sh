#!/usr/bin/env bash
#
# EdgeFlow 开发环境一键启动脚本（WBS 1.3，M1 起完整可用）
#
# 功能（幂等，可重复执行）：
#   1. preflight：检查 docker/kind/kubectl/go/make 等依赖
#   2. 创建 kind 集群（已存在则跳过，并等待就绪）
#   3. make build 构建 cloudcore / edgecore
#   4. --cloud：后台启动本机 cloudcore（PID 写入临时目录，dev-down 清理）
#   5. --edge ：拉起 N 个边缘节点模拟容器（Linux 版 edgecore，M1 起有效）
#
# 用法：
#   ./hack/dev-up.sh                          # 集群 + 构建（M0 即可用）
#   ./hack/dev-up.sh --cloud                  # 额外后台启动 cloudcore
#   ./hack/dev-up.sh --cloud --edge           # M1 后：完整开发环境
#   EDGEFLOW_EDGE_NODES=3 ./hack/dev-up.sh --edge
#
# 环境变量（均有默认值）：
#   EDGEFLOW_CLUSTER_NAME     kind 集群名，默认 edgeflow-dev
#   EDGEFLOW_CLOUDCORE_PORT   cloudcore healthz 端口，默认 8080
#   EDGEFLOW_EDGE_NODES       边缘模拟容器数量，默认 1
#   EDGEFLOW_EDGE_IMAGE       边缘容器镜像，默认 alpine:3.20
#   EDGEFLOW_EDGE_ARGS        追加给容器内 edgecore 的参数（M1 后传 --cloudhub ...）
#   EDGEFLOW_RUN_DIR          运行时目录（PID/日志），默认 ${TMPDIR:-/tmp}/edgeflow-dev
#
# 说明：本脚本只做环境编排，不包含任何业务逻辑。
set -euo pipefail

# ---------- 配置（可被环境变量覆盖） ----------
CLUSTER_NAME="${EDGEFLOW_CLUSTER_NAME:-edgeflow-dev}"
KIND_CONTEXT="kind-${CLUSTER_NAME}"
CLOUDCORE_PORT="${EDGEFLOW_CLOUDCORE_PORT:-8080}"
EDGE_NODES="${EDGEFLOW_EDGE_NODES:-1}"
EDGE_IMAGE="${EDGEFLOW_EDGE_IMAGE:-alpine:3.20}"
EDGE_ARGS="${EDGEFLOW_EDGE_ARGS:-}"
RUN_DIR="${EDGEFLOW_RUN_DIR:-${TMPDIR:-/tmp}/edgeflow-dev}"
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

START_CLOUDCORE=0
START_EDGE=0

# ---------- 工具函数 ----------
log()  { printf '\033[1;32m[dev-up]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[dev-up]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[dev-up]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
用法: ./hack/dev-up.sh [选项]
选项:
  --cloud   后台启动本机 cloudcore（healthz 健康检查）
  --edge    拉起边缘节点模拟容器（M1 起有效，edgecore 需已实现 EdgeHub）
  -h|--help 显示本帮助
环境变量: EDGEFLOW_CLUSTER_NAME / EDGEFLOW_CLOUDCORE_PORT /
          EDGEFLOW_EDGE_NODES / EDGEFLOW_EDGE_IMAGE / EDGEFLOW_EDGE_ARGS
EOF
}

require() {
  command -v "$1" >/dev/null 2>&1 || die "缺少依赖: $1（安装方式见 docs/DEV-ENV.md §3.1）"
}

# ---------- 1. preflight ----------
preflight() {
  log "检查依赖 ..."
  require docker
  require kind
  require kubectl
  require go
  require make
  if [ "$START_CLOUDCORE" = "1" ]; then
    require curl
  fi
  docker info >/dev/null 2>&1 || die "Docker 未运行，请先启动 Docker Desktop"
  log "依赖检查通过"
}

# ---------- 2. kind 集群（幂等创建 + 等待就绪） ----------
cluster_up() {
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
    log "kind 集群 $CLUSTER_NAME 已存在，跳过创建"
  else
    log "创建 kind 集群 $CLUSTER_NAME ..."
    kind create cluster --name "$CLUSTER_NAME"
  fi

  log "等待集群就绪（context: $KIND_CONTEXT）..."
  # 先等节点 Ready；kubectl 版本过旧不支持 wait 时回退到 cluster-info
  if ! kubectl --context "$KIND_CONTEXT" wait --for=condition=Ready node --all --timeout=120s >/dev/null 2>&1; then
    kubectl --context "$KIND_CONTEXT" cluster-info >/dev/null 2>&1 \
      || die "kind 集群 $CLUSTER_NAME 创建了但无法访问，试试 ./hack/dev-down.sh 后重跑"
  fi
  log "集群就绪: kubectl --context $KIND_CONTEXT get nodes"
}

# ---------- 3. 构建二进制 ----------
build_binaries() {
  log "构建二进制（make build）..."
  (cd "$PROJECT_ROOT" && make build)
  log "构建完成: bin/cloudcore, bin/edgecore"
}

# ---------- 4. 后台启动 cloudcore ----------
cloudcore_up() {
  mkdir -p "$RUN_DIR"

  # 已运行则跳过（幂等）
  if [ -f "$RUN_DIR/cloudcore.pid" ] && kill -0 "$(cat "$RUN_DIR/cloudcore.pid")" 2>/dev/null; then
    log "cloudcore 已在运行（pid $(cat "$RUN_DIR/cloudcore.pid")），跳过"
    return 0
  fi

  # 端口被占用则提示并跳过（不抢占其他服务）
  if lsof -iTCP:"$CLOUDCORE_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    warn "端口 $CLOUDCORE_PORT 已被占用，cloudcore 未启动；可用 EDGEFLOW_CLOUDCORE_PORT 换端口"
    return 0
  fi

  log "后台启动 cloudcore（端口 $CLOUDCORE_PORT，日志: $RUN_DIR/cloudcore.log）..."
  nohup "$PROJECT_ROOT/bin/cloudcore" --port "$CLOUDCORE_PORT" \
    >"$RUN_DIR/cloudcore.log" 2>&1 &
  echo $! >"$RUN_DIR/cloudcore.pid"

  # 健康检查（最多等 10 秒）
  for _ in $(seq 1 10); do
    if curl -fsS "http://127.0.0.1:${CLOUDCORE_PORT}/healthz" >/dev/null 2>&1; then
      log "cloudcore 已就绪: http://127.0.0.1:${CLOUDCORE_PORT}/healthz（pid $(cat "$RUN_DIR/cloudcore.pid")）"
      return 0
    fi
    sleep 1
  done
  warn "cloudcore 10 秒内未就绪，查看日志: $RUN_DIR/cloudcore.log"
  return 0
}

# ---------- 5. 边缘节点模拟（Docker 容器，M1 起有效） ----------
edge_nodes_up() {
  require docker

  # Mac 二进制无法在 Linux 容器中运行：交叉编译 Linux 版 edgecore
  local host_arch
  host_arch="$(uname -m)"                     # arm64 / x86_64
  case "$host_arch" in
    arm64)  linux_arch="arm64" ;;
    x86_64) linux_arch="amd64" ;;
    *)      die "不支持的架构: $host_arch" ;;
  esac
  local linux_bin="$PROJECT_ROOT/bin/edgecore-linux-${linux_arch}"
  if [ ! -x "$linux_bin" ] || [ "$PROJECT_ROOT/bin/edgecore" -nt "$linux_bin" ]; then
    log "交叉编译 Linux/$linux_arch 版 edgecore ..."
    (cd "$PROJECT_ROOT" && GOOS=linux GOARCH="$linux_arch" go build -ldflags "-s -w" -o "$linux_bin" ./cmd/edgecore)
  fi

  for i in $(seq 1 "$EDGE_NODES"); do
    local cname="edgeflow-edge-${i}"
    if docker ps -a --format '{{.Names}}' | grep -qx "$cname"; then
      if docker ps --format '{{.Names}}' | grep -qx "$cname"; then
        log "边缘节点容器 $cname 已在运行，跳过"
      else
        log "边缘节点容器 $cname 已存在但已停止，重新启动 ..."
        docker start "$cname" >/dev/null
      fi
    else
      log "创建边缘节点容器 $cname（镜像 $EDGE_IMAGE）..."
      # shellcheck disable=SC2086  # EDGE_ARGS 需按词拆分，故意不加引号
      # host-gateway 是 Linux 特性；macOS Docker Desktop 不支持时回退（其自带 host.docker.internal）
      if ! docker run -d --name "$cname" \
        -v "$PROJECT_ROOT/bin:/edgeflow/bin:ro" \
        --add-host host.docker.internal:host-gateway \
        "$EDGE_IMAGE" /edgeflow/bin/edgecore-linux-${linux_arch} $EDGE_ARGS >/dev/null 2>&1; then
        log "host-gateway 不可用（macOS Docker Desktop），改用默认方式重试 ..."
        docker run -d --name "$cname" \
          -v "$PROJECT_ROOT/bin:/edgeflow/bin:ro" \
          "$EDGE_IMAGE" /edgeflow/bin/edgecore-linux-${linux_arch} $EDGE_ARGS >/dev/null
      fi
      sleep 2
      if docker ps --format '{{.Names}}' | grep -qx "$cname"; then
        log "边缘节点容器 $cname 运行中"
      else
        warn "容器 $cname 已退出（M0 阶段 edgecore 为占位程序属正常；M1 后请 docker logs $cname 排查）"
      fi
    fi
  done
}

# ---------- 主流程 ----------
for arg in "$@"; do
  case "$arg" in
    --cloud)       START_CLOUDCORE=1 ;;
    --edge)        START_EDGE=1 ;;
    -h|--help)     usage; exit 0 ;;
    *)             die "未知参数: $arg（--help 查看用法）" ;;
  esac
done

log "EdgeFlow 开发环境启动（集群: $CLUSTER_NAME）"
preflight
cluster_up
build_binaries
[ "$START_CLOUDCORE" = "1" ] && cloudcore_up
[ "$START_EDGE" = "1" ] && edge_nodes_up

cat <<EOF

✔ 完成。当前环境：
  · kind 集群: $CLUSTER_NAME（context: $KIND_CONTEXT）
  · 二进制: bin/cloudcore, bin/edgecore
EOF
[ "$START_CLOUDCORE" = "1" ] && echo "  · cloudcore: http://127.0.0.1:${CLOUDCORE_PORT}/healthz（后台运行，日志 $RUN_DIR/cloudcore.log）"
[ "$START_EDGE" = "1" ] && echo "  · 边缘模拟容器: edgeflow-edge-1 .. edgeflow-edge-${EDGE_NODES}（docker ps 查看）"

cat <<'EOF'

下一步：
  kubectl --context kind-edgeflow-dev get nodes
  ./hack/dev-down.sh     # 清理（停进程/删容器/删集群）
M1 后：
  ./hack/dev-up.sh --cloud --edge   # 完整云边开发环境
EOF
