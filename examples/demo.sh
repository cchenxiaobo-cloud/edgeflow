#!/usr/bin/env bash
#
# EdgeFlow 温度传感器端到端 Demo（WBS 9.5 示例，M5 前置）
#
# 一键演示「云端下发 Pod + 设备数据流 + MQTT 数据面（可选）」：
#
#   1. 构建：make build（bin/cloudcore + bin/edgecore）
#   2. 临时目录：mktemp（SQLite 元数据库 / 证书 / 日志全部落在临时目录）
#   3. 启动 cloudcore（随机空闲端口，healthz 就绪等待）
#   4. 启动 edgecore（本地 Docker 运行时 + mock_sensor 模拟传感器）
#   5. 验证节点注册：GET /api/v1/nodes 出现 Ready 节点
#   6. 下发 Pod：POST /api/v1/nodes/{nodeID}/podsync（nginx:1.25-alpine）
#   7. 验证容器：docker ps 可见 edgeflow-default-<pod>-0 运行中
#   8. 验证 Pod 状态：GET /api/v1/pods 出现 Running
#   9. 验证设备数据：GET /api/v1/devices 出现 sensor-01（temperature/humidity）
#  10. 设备指令：POST .../device-command（targetTemp=25）→ 期望值写入
#  11. MQTT 数据面（可选）：检测到 mosquitto 才执行——订阅遥测 + 下发指令
#  12. 清理：podsync delete 回收容器 → 停止进程 → 删除临时目录
#
# 幂等性：每次运行使用随机 node-id / 随机端口 / 随机 Pod 名，
# 重复运行互不冲突；失败即退出（set -euo pipefail）并在最后输出日志路径。
#
# 用法：
#   bash examples/demo.sh            # 仓库内任意目录运行
#   ./examples/demo.sh
#
# 环境变量（均有默认值，一般无需设置）：
#   EDGEFLOW_DEMO_SKIP_BUILD=1       跳过 make build（复用已有 bin/）
#   EDGEFLOW_DEMO_HTTP_PORT=18080    固定 cloudcore HTTP 端口（默认自动挑选空闲端口）
#   EDGEFLOW_DEMO_HUB_PORT=20000     固定 CloudHub 端口（默认自动挑选空闲端口）
#   EDGEFLOW_DEMO_MQTT_PORT=21883    固定 mosquitto 端口（默认自动挑选空闲端口）
#   EDGEFLOW_DEMO_KEEP_RUN=1         结束时保留进程与临时目录（排障用，默认清理）
#   EDGEFLOW_DEMO_POD_IMAGE=nginx:1.25-alpine  下发 Pod 的镜像
#
# 依赖：
#   必选：docker（daemon 运行中）、curl、make、go
#   可选：mosquitto + mosquitto_sub/pub（仅 MQTT 数据面段；缺失自动跳过）
#
# 退出码：0 = DEMO PASS；非 0 = DEMO FAIL（见脚本输出与日志）。
set -euo pipefail

# ---------- 路径与配置 ----------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
POD_IMAGE="${EDGEFLOW_DEMO_POD_IMAGE:-nginx:1.25-alpine}"

# 每次运行的唯一后缀：node-id / Pod 名共用，保证重复运行互不冲突
RUN_SUFFIX="$(date +%s)-$$"
NODE_ID="demo-node-${RUN_SUFFIX}"
POD_NAME="demo-nginx-${RUN_SUFFIX}"

# 随机空闲端口（环境变量可固定覆盖）
HTTP_PORT="${EDGEFLOW_DEMO_HTTP_PORT:-}"
HUB_PORT="${EDGEFLOW_DEMO_HUB_PORT:-}"
MQTT_PORT="${EDGEFLOW_DEMO_MQTT_PORT:-}"

# 进程与临时目录
CLOUD_PID=""
EDGE_PID=""
MQTT_PID=""
RUN_DIR=""
CREATED_CONTAINERS=()   # 本次运行创建的容器名（清理时精确删除）

# ---------- 工具函数 ----------
log()  { printf '\033[1;32m[demo]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[demo]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[demo]\033[0m %s\n' "$*" >&2; exit 1; }
step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

require() {
  command -v "$1" >/dev/null 2>&1 || die "缺少依赖: $1"
}

# pick_port 输出一个当前空闲的 TCP 端口（python3 优先，回退 lsof 探测）。
pick_port() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
    return 0
  fi
  local p
  for _ in $(seq 1 20); do
    p=$(( (RANDOM % 20000) + 20000 ))
    if ! lsof -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
      echo "$p"
      return 0
    fi
  done
  die "无法找到空闲端口（需 python3 或 lsof 之一）"
}

# 挑选三个互不相同的空闲端口（HTTP / CloudHub / MQTT）
pick_ports() {
  [ -n "$HTTP_PORT" ] || HTTP_PORT="$(pick_port)"
  while [ -z "$HUB_PORT" ] || [ "$HUB_PORT" = "$HTTP_PORT" ]; do
    HUB_PORT="$(pick_port)"
  done
  while [ -z "$MQTT_PORT" ] || [ "$MQTT_PORT" = "$HTTP_PORT" ] || [ "$MQTT_PORT" = "$HUB_PORT" ]; do
    MQTT_PORT="$(pick_port)"
  done
}

# wait_for <描述> <超时秒> <命令...>：轮询直到命令成功或超时
wait_for() {
  local desc="$1" timeout="$2"
  shift 2
  local deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  warn "等待超时（${timeout}s）: $desc"
  return 1
}

# stop_proc <pid> <名字>：SIGTERM 优雅退出，10s 内不退再 SIGKILL
stop_proc() {
  local pid="$1" name="$2"
  [ -n "$pid" ] || return 0
  if ! kill -0 "$pid" 2>/dev/null; then
    return 0
  fi
  log "停止 $name（pid $pid）..."
  kill "$pid" 2>/dev/null || true
  local i
  for i in $(seq 1 10); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 1
  done
  warn "$name 10 秒内未退出，SIGKILL 强制结束"
  kill -9 "$pid" 2>/dev/null || true
  return 0
}

# cleanup 由 trap 触发：任何路径（成功/失败）都清理本次运行的全部资源
cleanup() {
  local rc=$?
  set +e

  # 1. 停止 edgecore（先停边再停云，避免断线告警刷屏）
  [ -n "$EDGE_PID" ] && stop_proc "$EDGE_PID" "edgecore"
  # 2. 停止 cloudcore
  [ -n "$CLOUD_PID" ] && stop_proc "$CLOUD_PID" "cloudcore"
  # 3. 停止 mosquitto（仅本次启动的）
  [ -n "$MQTT_PID" ] && stop_proc "$MQTT_PID" "mosquitto"

  # 4. 删除本次创建的容器（podsync delete 未回收时的兜底）
  if [ "${#CREATED_CONTAINERS[@]}" -gt 0 ]; then
    for c in "${CREATED_CONTAINERS[@]}"; do
      if docker ps -a --format '{{.Names}}' | grep -qx "$c"; then
        log "删除遗留容器 $c ..."
        docker rm -f "$c" >/dev/null 2>&1
      fi
    done
  fi

  # 5. 删除临时目录（仅限 mktemp 生成的 /var/folders 或 /tmp 路径）
  if [ -n "$RUN_DIR" ] && [ "${EDGEFLOW_DEMO_KEEP_RUN:-0}" != "1" ]; then
    case "$RUN_DIR" in
      /tmp/*|/private/tmp/*|/var/folders/*)
        rm -rf "$RUN_DIR"
        ;;
      *)
        warn "运行目录 $RUN_DIR 不是临时目录，跳过删除（请手动清理）"
        ;;
    esac
  fi

  if [ "$rc" -ne 0 ]; then
    printf '\n\033[1;31m=== DEMO FAIL（退出码 %d）===\033[0m\n' "$rc"
    if [ -n "$RUN_DIR" ]; then
      printf '日志保留在: %s（cloudcore.log / edgecore.log / mqtt.log）\n' "$RUN_DIR"
      printf '重跑提示: 修复问题后重新执行 bash examples/demo.sh 即可（幂等）\n'
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT

# ---------- 主流程 ----------
main() {
  step "1/11 前置检查与构建"
  require docker
  require curl
  require make
  require go
  docker info >/dev/null 2>&1 || die "Docker daemon 未运行，请先启动 Docker Desktop"

  if [ "${EDGEFLOW_DEMO_SKIP_BUILD:-0}" != "1" ]; then
    log "make build（产物: bin/cloudcore, bin/edgecore）..."
    (cd "$PROJECT_ROOT" && make build)
  else
    log "已设置 EDGEFLOW_DEMO_SKIP_BUILD=1，复用现有 bin/"
  fi
  [ -x "$PROJECT_ROOT/bin/cloudcore" ] || die "bin/cloudcore 不存在（去掉 EDGEFLOW_DEMO_SKIP_BUILD 或先 make build）"
  [ -x "$PROJECT_ROOT/bin/edgecore" ]  || die "bin/edgecore 不存在（去掉 EDGEFLOW_DEMO_SKIP_BUILD 或先 make build）"

  # 运行时目录：SQLite 元数据库 / 日志全在临时目录，与仓库隔离
  RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/edgeflow-demo.XXXXXX")"
  log "运行时目录: $RUN_DIR（临时，结束后自动清理）"

  # 端口：随机空闲端口，避免与任何已运行服务冲突
  pick_ports
  log "端口分配: HTTP=$HTTP_PORT CloudHub=$HUB_PORT MQTT=$MQTT_PORT"

  # MQTT 数据面（可选段）：检测 mosquitto broker（含 brew 常见路径）
  MQTT_BROKER=""
  for b in "$(command -v mosquitto 2>/dev/null || true)" \
           "/opt/homebrew/sbin/mosquitto" "/usr/local/sbin/mosquitto"; do
    if [ -n "$b" ] && [ -x "$b" ]; then
      MQTT_BROKER="$b"
      break
    fi
  done

  step "2/11 启动 cloudcore（HTTP $HTTP_PORT / CloudHub $HUB_PORT）"
  EDGEFLOW_CLOUDCORE_HUB_PORT="$HUB_PORT" \
    "$PROJECT_ROOT/bin/cloudcore" --port "$HTTP_PORT" \
    >"$RUN_DIR/cloudcore.log" 2>&1 &
  CLOUD_PID=$!
  wait_for "cloudcore healthz" 15 curl -fsS "http://127.0.0.1:${HTTP_PORT}/healthz" \
    || die "cloudcore 未就绪，日志: $RUN_DIR/cloudcore.log"
  log "cloudcore 就绪: http://127.0.0.1:${HTTP_PORT}/healthz（pid $CLOUD_PID）"

  step "3/11 启动 edgecore（nodeID=$NODE_ID，本地 Docker 运行时 + mock_sensor）"
  # MQTT 数据面装配：broker 存在则先起 broker 并把地址注入 edgecore；
  # 不存在则 edgecore 快速进入降级路径（MQTT 段跳过，云边链路不受影响）
  if [ -n "$MQTT_BROKER" ]; then
    if command -v mosquitto_sub >/dev/null 2>&1 && command -v mosquitto_pub >/dev/null 2>&1; then
      log "检测到 mosquitto（$MQTT_BROKER），启动 MQTT broker（端口 $MQTT_PORT）"
      "$MQTT_BROKER" -p "$MQTT_PORT" >"$RUN_DIR/mqtt.log" 2>&1 &
      MQTT_PID=$!
      wait_for "mosquitto 监听 $MQTT_PORT" 10 \
        lsof -iTCP:"$MQTT_PORT" -sTCP:LISTEN \
        || die "mosquitto 未就绪，日志: $RUN_DIR/mqtt.log"
      log "MQTT broker 就绪: tcp://127.0.0.1:${MQTT_PORT}"
    else
      warn "找到 mosquitto broker 但缺少 mosquitto_sub/mosquitto_pub，跳过 MQTT 数据面段"
      MQTT_BROKER=""
    fi
  else
    warn "未检测到 mosquitto broker，跳过 MQTT 数据面段（可选）"
  fi

  local edge_env=(
    EDGEFLOW_EDGECORE_NODE_ID="$NODE_ID"
    EDGEFLOW_EDGECORE_CLOUD_ADDR="ws://127.0.0.1:${HUB_PORT}"
    EDGEFLOW_EDGECORE_DB_PATH="$RUN_DIR/edgeflow.db"
    EDGEFLOW_EDGECORE_REPORT_INTERVAL=3s
    EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=3s
    EDGEFLOW_EDGECORE_MQTT_CONNECT_TIMEOUT=2s
  )
  if [ -n "$MQTT_BROKER" ]; then
    edge_env+=(EDGEFLOW_EDGECORE_MQTT_ADDR="tcp://127.0.0.1:${MQTT_PORT}")
  fi
  env "${edge_env[@]}" "$PROJECT_ROOT/bin/edgecore" >"$RUN_DIR/edgecore.log" 2>&1 &
  EDGE_PID=$!
  log "edgecore 启动（pid $EDGE_PID），等待注册..."

  step "4/11 验证节点注册（GET /api/v1/nodes）"
  wait_for "节点 $NODE_ID Ready" 20 \
    bash -c "curl -fsS 'http://127.0.0.1:${HTTP_PORT}/api/v1/nodes' | grep -q '\"nodeID\":\"${NODE_ID}\"' && curl -fsS 'http://127.0.0.1:${HTTP_PORT}/api/v1/nodes' | grep -q '\"status\":\"Ready\"'" \
    || die "节点未在 20s 内注册为 Ready，日志: $RUN_DIR/edgecore.log"
  curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/nodes" | python3 -m json.tool 2>/dev/null \
    || curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/nodes"
  log "节点注册成功: $NODE_ID（Ready）"

  step "5/11 下发 Pod（podsync: $POD_NAME / nginx:1.25-alpine / replicas=1）"
  local sync_resp
  sync_resp="$(curl -fsS -X POST "http://127.0.0.1:${HTTP_PORT}/api/v1/nodes/${NODE_ID}/podsync" \
    -d "{\"operation\":\"add\",\"pod\":{\"name\":\"${POD_NAME}\",\"namespace\":\"default\",\"image\":\"${POD_IMAGE}\",\"replicas\":1}}")"
  echo "$sync_resp"
  echo "$sync_resp" | grep -q '"acked":true' || die "podsync 未被边缘确认: $sync_resp"

  step "6/11 验证容器运行（docker ps edgeflow-*）"
  local cname="edgeflow-default-${POD_NAME}-0"
  CREATED_CONTAINERS+=("$cname")
  # 首次拉取镜像可能耗时（nginx:1.25-alpine 已缓存时秒级）；docker ps 可见即通过
  wait_for "容器 $cname 运行" 60 \
    bash -c "docker ps --format '{{.Names}}' | grep -qx '${cname}'" \
    || die "容器 $cname 未出现，日志: $RUN_DIR/edgecore.log"
  docker ps --filter label=edgeflow.pod --format 'table {{.Names}}\t{{.Status}}'
  log "容器运行中: $cname"

  step "7/11 验证 Pod 状态上报（GET /api/v1/pods → Running）"
  wait_for "Pod 状态 Running" 30 \
    bash -c "curl -fsS 'http://127.0.0.1:${HTTP_PORT}/api/v1/pods' | grep -q '\"podName\":\"${POD_NAME}\"' && curl -fsS 'http://127.0.0.1:${HTTP_PORT}/api/v1/pods' | grep -q '\"phase\":\"Running\"'" \
    || die "Pod 状态未在 30s 内上报 Running，日志: $RUN_DIR/edgecore.log"
  curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/pods" | python3 -m json.tool 2>/dev/null \
    || curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/pods"
  log "Pod 状态: Running（云端已确认）"

  step "8/11 验证设备数据流（GET /api/v1/devices → sensor-01）"
  wait_for "设备 sensor-01 上报" 20 \
    bash -c "curl -fsS 'http://127.0.0.1:${HTTP_PORT}/api/v1/devices' | grep -q '\"deviceName\":\"sensor-01\"'" \
    || die "设备数据未上报，日志: $RUN_DIR/edgecore.log"
  curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/devices" | python3 -m json.tool 2>/dev/null \
    || curl -fsS "http://127.0.0.1:${HTTP_PORT}/api/v1/devices"
  log "设备数据流正常: sensor-01（temperature/humidity 周期上报）"

  step "9/11 下发设备指令（device-command: targetTemp=25）"
  local cmd_resp
  cmd_resp="$(curl -fsS -X POST "http://127.0.0.1:${HTTP_PORT}/api/v1/nodes/${NODE_ID}/device-command" \
    -d '{"deviceName":"sensor-01","property":"targetTemp","value":25}')"
  echo "$cmd_resp"
  echo "$cmd_resp" | grep -q '"acked":true' || die "设备指令未被边缘确认: $cmd_resp"
  wait_for "云端期望值写入（desired.targetTemp=25）" 10 \
    bash -c "curl -fsS 'http://127.0.0.1:${HTTP_PORT}/api/v1/devices' | grep -q '\"targetTemp\":25'" \
    || die "desired.targetTemp 未写入云端设备状态，日志: $RUN_DIR/edgecore.log"
  log "设备指令生效: sensor-01.targetTemp=25（云端 desired 已写入）"

  # MQTT 数据面（可选段）：仅在本次启动了 broker 时执行
  if [ -n "$MQTT_BROKER" ]; then
    step "10/11 MQTT 数据面（mosquitto，端口 $MQTT_PORT）"
    log "订阅遥测主题 devices/default/sensor-01/telemetry（最多等 10s 收 1 条）..."
    local tele
    tele="$(mosquitto_sub -p "$MQTT_PORT" -t 'devices/default/sensor-01/telemetry' -C 1 -W 10 -v 2>/dev/null || true)"
    if [ -z "$tele" ]; then
      die "10s 内未收到遥测消息，broker 日志: $RUN_DIR/mqtt.log"
    fi
    echo "  收到: $tele"
    echo "$tele" | grep -q 'temperature' || die "遥测消息缺少 temperature 字段: $tele"
    log "遥测数据面正常（MQTT 每 2s 发布一次温湿度）"

    log "通过 MQTT 下发指令（devices/default/sensor-01/command: targetTemp=23）..."
    mosquitto_pub -p "$MQTT_PORT" -t 'devices/default/sensor-01/command' \
      -m '{"property":"targetTemp","value":23}'
    wait_for "edgecore 日志出现数据面指令生效" 10 \
      bash -c "grep -q '数据面指令生效 targetTemp=23' '$RUN_DIR/edgecore.log'" \
      || die "MQTT 指令未生效，日志: $RUN_DIR/edgecore.log"
    log "MQTT 指令生效: sensor-01.targetTemp=23（数据面闭环）"
  else
    step "10/11 MQTT 数据面（可选段，已跳过）"
    warn "本机未检测到 mosquitto broker，跳过 MQTT 数据面验证（不影响主链路结论）"
  fi

  step "11/11 清理演示资源（podsync delete → 容器回收）"
  local del_resp
  del_resp="$(curl -fsS -X POST "http://127.0.0.1:${HTTP_PORT}/api/v1/nodes/${NODE_ID}/podsync" \
    -d "{\"operation\":\"delete\",\"pod\":{\"name\":\"${POD_NAME}\",\"namespace\":\"default\"}}")"
  echo "$del_resp"
  echo "$del_resp" | grep -q '"acked":true' || warn "podsync delete 未确认（清理阶段继续兜底）"
  # 期望：Edged 调谐回收容器（reconcile 周期 5s + docker rm）
  if wait_for "容器 $cname 回收" 30 \
    bash -c "! docker ps -a --format '{{.Names}}' | grep -qx '${cname}'"; then
    log "容器已回收: $cname"
  else
    warn "容器 30s 内未回收，交由 cleanup 兜底删除"
  fi

  printf '\n\033[1;32m========================================\033[0m\n'
  printf '\033[1;32m  DEMO PASS ✅（%s）\033[0m\n' "$(date '+%Y-%m-%d %H:%M:%S')"
  printf '\033[1;32m========================================\033[0m\n'
  printf '演示链路：cloudcore+edgecore 启动 → 节点注册 → Pod 下发/运行 → 设备数据上报 → 设备指令 → %s\n' \
    "$([ -n "$MQTT_BROKER" ] && echo 'MQTT 数据面 → 资源清理' || echo '资源清理（MQTT 段跳过）')"
}

main "$@"
