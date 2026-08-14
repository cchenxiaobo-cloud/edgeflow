# 性能基线（云边通道注册 / 心跳）

> WBS 8.4 / G5 审计缺口关闭：新增并发注册 + 心跳压测工具
> `hack/load-test`，本文档记录本机实测基线。基线用于回归对比——
> 后续任何协议/云边链路改动后重跑 `hack/load-test`，若延迟显著劣化
> （如 P99 翻倍）即视为回归，需复核。

## 测试环境

| 项 | 值 |
| --- | --- |
| 机器 | MacBook Pro（Apple Silicon, arm64），本机回环 |
| OS | macOS（Darwin 25.1.0） |
| Go | 1.26.2 |
| cloudcore | `cmd/cloudcore`，本机编译，HTTP 18080 / CloudHub 19000 |
| 数据库 | 内存/SQLite 默认路径（未做磁盘预热） |
| 工具 | `hack/load-test`（`go run ./hack/load-test`） |

## 方法

- 压测工具按真实云边契约模拟 N 个 edgecore 客户端：WebSocket 拨号
  （`/v1/edge`）→ `Register`（等 `RegisterAck`）→ 连续 H 次 `Heartbeat`
  （等各自 `HeartbeatAck`）。
- 全部节点并发发起（屏障同步），单进程单机回环。
- 注册延迟含拨号耗时；成功率 = 收到 `Accepted=true` 的 `RegisterAck` 数 / 尝试数。
- 复现命令：

```bash
# 启动 cloudcore（独立终端）
EDGEFLOW_CLOUDCORE_PORT=18080 EDGEFLOW_CLOUDCORE_HUB_PORT=19000 \
    EDGEFLOW_CLOUDCORE_AUDIT_PATH=/tmp/ef-audit.jsonl go run ./cmd/cloudcore

# 压测 N=10（另一终端）
LOAD_TEST_NODES=10 LOAD_TEST_HEARTBEATS=5 \
    LOAD_TEST_CLOUD=ws://127.0.0.1:19000 go run ./hack/load-test
```

## 基线结果（2026-08-15 实测）

### N=10（默认档，CI 回归用）

| 指标 | 值 |
| --- | --- |
| 注册成功率 | 10/10 = 100% |
| 注册延迟均值 | 1.85 ms |
| 注册延迟 P50 / P95 / P99 | 1.72 / 2.28 / 2.28 ms |
| 心跳成功率 | 50/50 = 100%（每节点 5 次） |
| 心跳延迟均值 | 0.25 ms |
| 心跳延迟 P50 / P95 / P99 | 0.27 / 0.29 / 0.32 ms |
| 总耗时 | ~4 ms（10 节点并发） |

### N=100（扩展档，本机可跑通，集群环境建议复测）

| 指标 | 值 |
| --- | --- |
| 注册成功率 | 100/100 = 100% |
| 注册延迟均值 | 9.62 ms |
| 注册延迟 P50 / P95 / P99 | 8.72 / 13.24 / 14.37 ms |
| 心跳成功率 | 300/300 = 100%（每节点 3 次） |
| 心跳延迟均值 | 1.74 ms |
| 心跳延迟 P50 / P95 / P99 | 1.54 / 3.39 / 4.53 ms |
| 总耗时 | ~19 ms（100 节点并发） |

## 结论

- 单机回环下，N=10 与 N=100 注册成功率均 100%，心跳 100%；
  延迟均为毫秒级，P99 无异常长尾（N=100 注册 P99 14.37ms，
  心跳 P99 4.53ms）。
- 注册延迟随并发数上升（1.85ms→9.62ms 均值），符合预期：
  瓶颈在 CloudHub 单连接注册处理的串行化与 JSON 编解码，
  无阻塞/死锁迹象。
- **边界说明**：本机单进程回环无法代表生产网络（真实环境存在
  广域网 RTT、TLS 握手、MQTT 设备流量叠加）；100 节点量级在
  本机可跑通，但 1000+ 节点需集群/独立压测环境验证（任务
  边界内仅承诺 N=10 实测）。

## JSON 原始结果（N=10）

```json
{
  "cloud": "ws://127.0.0.1:19000/v1/edge",
  "nodes": 10,
  "heartbeatsPerNode": 5,
  "startedAt": "2026-08-15T00:10:13+08:00",
  "durationMs": 4.142,
  "register": {
    "attempted": 10,
    "succeeded": 10,
    "successRate": 1,
    "latencyMs": {
      "meanMs": 1.8477, "p50Ms": 1.718, "p95Ms": 2.283,
      "p99Ms": 2.283, "minMs": 1.536, "maxMs": 2.286
    }
  },
  "heartbeat": {
    "sent": 50,
    "succeeded": 50,
    "latencyMs": {
      "meanMs": 0.25422, "p50Ms": 0.266, "p95Ms": 0.292,
      "p99Ms": 0.315, "minMs": 0.123, "maxMs": 0.316
    }
  }
}
```
