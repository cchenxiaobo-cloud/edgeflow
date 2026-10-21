#!/usr/bin/env bash
# dr-drill.sh — EdgeFlow 灾备演练编排（v0.52.0，spec 0025 裁决 5，DR-ASSESSMENT
# 差距 #4「演练流程/预案」承接）。SOP 全文见 docs/DR-RUNBOOK.md。
#
# 三模式：
#   backup  —— 触发一次治理面快照备份（调 cloudcore API）并立即校验
#   verify  —— 对指定备份执行 sha256 完整性复核（调 API ?verify=true）
#   restore —— 演练恢复：确认 cloudcore 已停止 → 用备份副本覆盖回源快照路径
#              → 提示重启（本脚本不代启停生产进程，避免误操作）
#
# 用法：
#   dr-drill.sh backup  [base_url] [token]
#   dr-drill.sh verify  [base_url] [token] [backup_id]   # backup_id 缺省=最新一份
#   dr-drill.sh restore [dr_dir]   [source_path] [backup_id] [copy_name]
#
# 退出码：0 成功 / 非 0 失败（步骤日志带 [drill] 前缀）。
set -euo pipefail

log() { echo "[drill] $*"; }
die() { echo "[drill] 错误: $*" >&2; exit 1; }

MODE="${1:?用法: dr-drill.sh backup|verify|restore ...}"

api() { # api METHOD URL TOKEN [body]
  local method="$1" url="$2" token="$3" body="${4:-}"
  if [ -n "$body" ]; then
    curl -fsS -X "$method" -H "Authorization: Bearer $token" \
      -H "Content-Type: application/json" -d "$body" "$url"
  else
    curl -fsS -X "$method" -H "Authorization: Bearer $token" "$url"
  fi
}

case "$MODE" in
  backup)
    BASE="${2:-http://127.0.0.1:8080}"
    TOKEN="${3:?缺少 API token}"
    log "创建备份（POST $BASE/api/v1/dr/backups）…"
    RESP="$(api POST "$BASE/api/v1/dr/backups" "$TOKEN")"
    ID="$(printf '%s' "$RESP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
    [ -n "$ID" ] || die "响应缺少备份 id: $RESP"
    log "备份 $ID 已创建，立即复核完整性…"
    V="$(api GET "$BASE/api/v1/dr/backups/$ID?verify=true" "$TOKEN")"
    printf '%s' "$V" | grep -q '"verified":true' || die "复核未通过: $V"
    log "OK：备份 $ID 创建且 sha256 复核通过"
    ;;

  verify)
    BASE="${2:-http://127.0.0.1:8080}"
    TOKEN="${3:?缺少 API token}"
    BID="${4:-}"
    if [ -z "$BID" ]; then
      LIST="$(api GET "$BASE/api/v1/dr/backups" "$TOKEN")"
      BID="$(printf '%s' "$LIST" | sed -n 's/.*"backups":\[{"id":"\([^"]*\)".*/\1/p')"
      [ -n "$BID" ] || die "清单为空或解析失败，请显式指定 backup_id"
      log "未指定 id，取最新一份：$BID"
    fi
    V="$(api GET "$BASE/api/v1/dr/backups/$BID?verify=true" "$TOKEN")"
    printf '%s' "$V" | grep -q '"verified":true' || die "备份 $BID 复核未通过: $V"
    log "OK：备份 $BID sha256 复核通过"
    ;;

  restore)
    DR_DIR="${2:?缺少备份目录}"
    SRC="${3:?缺少源快照路径}"
    BID="${4:?缺少 backup_id}"
    COPY="${5:?缺少副本文件名（如 region.snapshot.json）}"
    pgrep -f cloudcore >/dev/null 2>&1 && \
      die "检测到 cloudcore 仍在运行——恢复前必须先停止进程（防写竞争，见 RUNBOOK 步骤 2）"
    COPY_PATH="$DR_DIR/$BID/$COPY"
    [ -f "$COPY_PATH" ] || die "副本不存在: $COPY_PATH"
    log "恢复 $COPY_PATH → $SRC"
    cp "$COPY_PATH" "$SRC.tmp.restore"
    mv "$SRC.tmp.restore" "$SRC"
    log "OK：快照已恢复；请按 RUNBOOK 步骤 4 重启 cloudcore 并执行 API 健康验证"
    ;;

  *)
    die "未知模式: $MODE（支持 backup|verify|restore）"
    ;;
esac
