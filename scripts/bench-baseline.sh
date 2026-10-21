#!/usr/bin/env bash
# bench-baseline.sh — 采集管道性能基线刷新（v0.52.0，spec 0025 裁决 7，NFR-04）。
# 跑 hack/collect-bench 并把结果以日期分节追加进 docs/PERFORMANCE-BASELINE.md。
# 机制可重复执行；历史分节保留（append-only），回归对比看相邻两次数字。
set -euo pipefail
cd "$(dirname "$0")/.."
DOC=docs/PERFORMANCE-BASELINE.md
STAMP="$(date '+%Y-%m-%d %H:%M %Z')"
GO_VER="$(go version | awk '{print $3}')"

echo "[bench] 运行 collect-bench（200 设备 × 10 属性，10s）…"
OUT="$(go run ./hack/collect-bench -devices 200 -props 10 -interval 1s -duration 10s)"

{
  echo ""
  echo "## 采集管道基线（collect-bench）— $STAMP"
  echo ""
  echo "| 项 | 值 |"
  echo "| --- | --- |"
  echo "| 参数 | 200 设备 × 10 属性，1s 周期，10s 采样 |"
  echo "| 环境 | $(uname -sm) / $GO_VER |"
  echo "| 触发 | make bench-baseline（人工触发，CI 化留档不承诺） |"
  echo ""
  echo '```'
  printf '%s\n' "$OUT"
  echo '```'
} >> "$DOC"
echo "[bench] 已追加至 $DOC（分节：$STAMP）"
