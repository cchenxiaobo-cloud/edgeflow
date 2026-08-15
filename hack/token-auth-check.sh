#!/usr/bin/env bash
#
# WBS 7.3 设备认证（B1）真实进程验证：
#   cloudcore 启用节点令牌 → edgecore 携带正确 token 注册成功；
#   携带错误/缺失 token 注册被拒（节点不出现在 API）。
#
# 用法：bash hack/token-auth-check.sh（仓库根目录执行）
# 依赖：go、docker daemon（edgecore 本地 Docker 运行时）、curl、lsof
# 退出码：0 = 全部通过；非 0 = 验证失败
set -euo pipefail

cd "$(dirname "$0")/.."
RUN_DIR="$(mktemp -d)"
HTTP_PORT="$((20000 + RANDOM % 20000))"
HUB_PORT="$((HTTP_PORT + 1))"
NODE_ID="token-check-$(date +%s)"
TOKEN="n0des3cret"
CLOUD_PID=""
EDGE_PID=""

cleanup() {
  [ -n "$EDGE_PID" ] && kill "$EDGE_PID" 2>/dev/null || true
  [ -n "$CLOUD_PID" ] && kill "$CLOUD_PID" 2>/dev/null || true
  sleep 1
  rm -rf "$RUN_DIR"
}
trap cleanup EXIT

log() { printf '\033[1;34m[token-auth]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[token-auth]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- 1. 构建 ----------
log "构建 bin/cloudcore + bin/edgecore ..."
make build >/dev/null 2>&1 || die "make build 失败"

# ---------- 2. 启动 cloudcore（启用节点令牌）----------
log "启动 cloudcore（HTTP $HTTP_PORT / Hub $HUB_PORT，EDGEFLOW_CLOUDCORE_NODE_TOKEN 已设置）..."
EDGEFLOW_CLOUDCORE_HUB_PORT="$HUB_PORT" \
EDGEFLOW_CLOUDCORE_NODE_TOKEN="$TOKEN" \
  bin/cloudcore --port "$HTTP_PORT" >"$RUN_DIR/cloudcore.log" 2>&1 &
CLOUD_PID=$!
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:${HTTP_PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -fsS "http://127.0.0.1:${HTTP_PORT}/healthz" >/dev/null 2>&1 || die "cloudcore 未就绪，日志: $RUN_DIR/cloudcore.log"

# ---------- 3. 正确 token：应注册成功 ----------
log "启动 edgecore（正确 token）..."
env EDGEFLOW_EDGECORE_NODE_ID="$NODE_ID" \
  EDGEFLOW_EDGECORE_CLOUD_ADDR="ws://127.0.0.1:${HUB_PORT}" \
  EDGEFLOW_EDGECORE_DB_PATH="$RUN_DIR/edgeflow.db" \
  EDGEFLOW_EDGECORE_REPORT_INTERVAL=3s \
  EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=3s \
  EDGEFLOW_EDGECORE_MQTT_CONNECT_TIMEOUT=2s \
  EDGEFLOW_EDGECORE_TOKEN="$TOKEN" \
  bin/edgecore >"$RUN_DIR/edgecore-ok.log" 2>&1 &
EDGE_PID=$!

registered=0
for _ in $(seq 1 40); do
  if curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/nodes" 2>/dev/null | grep -q "$NODE_ID"; then
    registered=1; break
  fi
  sleep 0.5
done
[ "$registered" = 1 ] || die "正确 token 未能注册成功（日志: $RUN_DIR/edgecore-ok.log）"
log "✅ 正确 token 注册成功：$NODE_ID 出现在 /api/v1/nodes"

# ---------- 4. 错误 token：应被拒绝（用独立 node-id，避免首次成功注册项残留）----------
kill "$EDGE_PID" 2>/dev/null || true
sleep 2
BAD_NODE="bad-token-$(date +%s)"
log "启动 edgecore（错误 token，node=$BAD_NODE），预期注册被拒..."
env EDGEFLOW_EDGECORE_NODE_ID="$BAD_NODE" \
  EDGEFLOW_EDGECORE_CLOUD_ADDR="ws://127.0.0.1:${HUB_PORT}" \
  EDGEFLOW_EDGECORE_DB_PATH="$RUN_DIR/edgeflow.db" \
  EDGEFLOW_EDGECORE_REPORT_INTERVAL=3s \
  EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=3s \
  EDGEFLOW_EDGECORE_MQTT_CONNECT_TIMEOUT=2s \
  EDGEFLOW_EDGECORE_TOKEN="wrong-token" \
  bin/edgecore >"$RUN_DIR/edgecore-bad.log" 2>&1 &
EDGE_PID=$!

rejected=0
for _ in $(seq 1 30); do
  if grep -q "接入令牌校验失败" "$RUN_DIR/cloudcore.log" 2>/dev/null; then
    rejected=1; break
  fi
  sleep 0.5
done
[ "$rejected" = 1 ] || die "错误 token 未被拒绝（cloudcore 日志未见拒绝记录）"
log "✅ 错误 token 被云端拒绝（日志: 接入令牌校验失败）"

# 错误 token 的 node-id 不得出现在注册表
if curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/nodes" 2>/dev/null | grep -q "$BAD_NODE"; then
  die "被拒节点 $BAD_NODE 出现在 /api/v1/nodes（不应注册）"
fi
log "✅ 被拒节点 $BAD_NODE 未出现在节点列表"

# ---------- 5. 缺失 token：应被拒绝（独立 node-id）----------
kill "$EDGE_PID" 2>/dev/null || true
sleep 2
NONE_NODE="no-token-$(date +%s)"
log "启动 edgecore（缺失 token，node=$NONE_NODE），预期注册被拒..."
env EDGEFLOW_EDGECORE_NODE_ID="$NONE_NODE" \
  EDGEFLOW_EDGECORE_CLOUD_ADDR="ws://127.0.0.1:${HUB_PORT}" \
  EDGEFLOW_EDGECORE_DB_PATH="$RUN_DIR/edgeflow.db" \
  EDGEFLOW_EDGECORE_REPORT_INTERVAL=3s \
  EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=3s \
  EDGEFLOW_EDGECORE_MQTT_CONNECT_TIMEOUT=2s \
  bin/edgecore >"$RUN_DIR/edgecore-none.log" 2>&1 &
EDGE_PID=$!

rejected2=0
for _ in $(seq 1 30); do
  if grep -q "接入令牌校验失败" "$RUN_DIR/cloudcore.log" 2>/dev/null; then
    rejected2=1; break
  fi
  sleep 0.5
done
[ "$rejected2" = 1 ] || die "缺失 token 未被拒绝（cloudcore 日志未见拒绝记录）"
log "✅ 缺失 token 被云端拒绝"

if curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/nodes" 2>/dev/null | grep -q "$NONE_NODE"; then
  die "被拒节点 $NONE_NODE 出现在 /api/v1/nodes（不应注册）"
fi
log "✅ 被拒节点 $NONE_NODE 未出现在节点列表"

# ---------- 6. 回归：正确 token 节点仍在列表 ----------
sleep 1
if ! curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/nodes" 2>/dev/null | grep -q "$NODE_ID"; then
  die "首次注册节点 $NODE_ID 丢失（错误 token 验证不应影响已注册节点）"
fi
log "✅ 首次注册节点 $NODE_ID 仍在列表（无回归）"

log "全部通过：正确 token 注册成功；错误/缺失 token 被拒且注册表无污染"
