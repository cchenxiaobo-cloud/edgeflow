#!/usr/bin/env bash
#
# EdgeFlow 开发环境清理脚本（WBS 1.3，与 hack/dev-up.sh 配套）
#
# 功能（幂等，按反向顺序拆除，不存在的东西自动跳过）：
#   1. 停止 dev-up 托管的 cloudcore（先 SIGTERM 优雅退出，5s 内不退再 SIGKILL）
#   2. 删除边缘节点模拟容器（edgeflow-edge-*）
#   3. 删除 kind 集群（--keep-cluster 可跳过）
#   4. 清理运行时目录（仅限临时目录，安全校验）
#
# 用法：
#   ./hack/dev-down.sh                # 全部拆除
#   ./hack/dev-down.sh --keep-cluster # 保留 kind 集群（只停进程/删容器）
#
# 环境变量（与 dev-up.sh 保持一致）：
#   EDGEFLOW_CLUSTER_NAME    kind 集群名，默认 edgeflow-dev
#   EDGEFLOW_RUN_DIR         运行时目录（PID/日志），默认 ${TMPDIR:-/tmp}/edgeflow-dev
#
# 说明：本脚本只做环境编排，不包含任何业务逻辑。
set -euo pipefail

CLUSTER_NAME="${EDGEFLOW_CLUSTER_NAME:-edgeflow-dev}"
RUN_DIR="${EDGEFLOW_RUN_DIR:-${TMPDIR:-/tmp}/edgeflow-dev}"
KEEP_CLUSTER=0

log()  { printf '\033[1;32m[dev-down]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[dev-down]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[dev-down]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- 1. 停止 dev-up 托管的 cloudcore ----------
stop_cloudcore() {
  local pid_file="$RUN_DIR/cloudcore.pid"
  if [ ! -f "$pid_file" ]; then
    log "cloudcore PID 文件不存在（$pid_file），跳过"
    return 0
  fi
  local pid
  pid="$(cat "$pid_file")"
  if ! kill -0 "$pid" 2>/dev/null; then
    log "cloudcore（pid $pid）未在运行，清理 PID 文件"
    rm -f "$pid_file"
    return 0
  fi
  log "停止 cloudcore（pid $pid，SIGTERM 优雅退出）..."
  kill "$pid" 2>/dev/null || true
  # 最多等 5 秒（cloudcore 优雅退出窗口），未退出则强制结束
  for _ in $(seq 1 5); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
  done
  if kill -0 "$pid" 2>/dev/null; then
    warn "cloudcore 5 秒内未退出，SIGKILL 强制结束"
    kill -9 "$pid" 2>/dev/null || true
  else
    log "cloudcore 已退出"
  fi
  rm -f "$pid_file"
}

# ---------- 2. 删除边缘节点模拟容器 ----------
stop_edge_nodes() {
  if ! command -v docker >/dev/null 2>&1; then
    log "docker 未安装，跳过边缘容器清理"
    return 0
  fi
  docker ps -a --format '{{.Names}}' | grep -E '^edgeflow-edge-[0-9]+$' | while read -r cname; do
    log "删除边缘节点容器 $cname ..."
    docker rm -f "$cname" >/dev/null
  done
  # 无匹配时 grep 返回 1，管道在 if 外会触发 set -e？不会：while 循环退出码取自循环体
  log "边缘节点容器清理完成"
}

# ---------- 3. 删除 kind 集群 ----------
cluster_down() {
  if ! command -v kind >/dev/null 2>&1; then
    log "kind 未安装，跳过集群删除"
    return 0
  fi
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
    log "删除 kind 集群 $CLUSTER_NAME ..."
    kind delete cluster --name "$CLUSTER_NAME"
    log "kind 集群已删除"
  else
    log "kind 集群 $CLUSTER_NAME 不存在，跳过"
  fi
}

# ---------- 4. 清理运行时目录（仅限临时目录） ----------
cleanup_run_dir() {
  case "$RUN_DIR" in
    /tmp/*|/private/tmp/*|/var/folders/*)
      rm -rf "$RUN_DIR"
      log "已清理运行时目录 $RUN_DIR"
      ;;
    *)
      warn "运行目录 $RUN_DIR 不是临时目录，跳过清理（请手动处理）"
      ;;
  esac
}

# ---------- 主流程 ----------
for arg in "$@"; do
  case "$arg" in
    --keep-cluster)  KEEP_CLUSTER=1 ;;
    -h|--help)
      echo "用法: ./hack/dev-down.sh [--keep-cluster]（--keep-cluster: 保留 kind 集群）"
      exit 0
      ;;
    *) die "未知参数: $arg" ;;
  esac
done

log "EdgeFlow 开发环境清理（集群: $CLUSTER_NAME）"
stop_cloudcore
stop_edge_nodes
if [ "$KEEP_CLUSTER" = "1" ]; then
  log "已指定 --keep-cluster，跳过集群删除"
else
  cluster_down
fi
cleanup_run_dir
log "清理完成"
