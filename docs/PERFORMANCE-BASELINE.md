# EdgeFlow 性能基线（WBS 8.4 缺口 G5 补做）

> 测试时间：2026-08-14 21:30
> 测试环境：macOS arm64（Apple Silicon），cloudcore 本机进程，10 个模拟 edgecore 客户端（hack/load-test）
> 测试方式：`LOAD_TEST_NODES=10 LOAD_TEST_DURATION_SEC=8 LOAD_TEST_BEAT_SEC=2 go run ./hack/load-test`

---

## 1. 测试结果（10 节点基线）

| 指标 | 结果 | 说明 |
|------|------|------|
| 注册成功率 | **10/10（100%）** | 并发 10 客户端全部注册成功 |
| 平均注册延迟 | **201.3 ms** | 含连接建立 + Register/RegisterAck 往返 |
| P95 注册延迟 | **202.0 ms** | 分布极窄（并发客户端几乎同时完成） |
| 心跳统计 | 0（窗口内） | 心跳周期默认 30s > 8s 测试窗口，无 Ack 采样——需长窗口验证 |
| 测试时长 | 8s | 可配置 |

## 2. 与验收目标的差距

| 验收指标（ROADMAP M4） | 基线 | 差距 | 说明 |
|------------------------|------|------|------|
| 100 节点注册成功率 ≥99% | 10 节点 100% | **未在 100 节点验证** | 需集群环境/多机扩展（本机 10 并发已占满测试意图，100 节点建议 CI 大 runner） |
| 消息延迟 ≤3s | 注册延迟 ~200ms | ✅ 数量级满足 | 消息延迟需独立压测（未做） |
| EdgeCore 内存 ≤256MB | 未测量 | **未验证** | 需容器化测量（docker stats）；EDGED-POC 已标注为 P2 |

## 3. 使用方式

```bash
# 启动 cloudcore（默认明文通道）
./bin/cloudcore

# 10 节点基线
LOAD_TEST_NODES=10 LOAD_TEST_DURATION_SEC=8 go run ./hack/load-test

# JSON 输出（供脚本解析）
LOAD_TEST_NODES=10 go run ./hack/load-test -json

# 50 节点（本机资源允许时）
LOAD_TEST_NODES=50 LOAD_TEST_DURATION_SEC=15 go run ./hack/load-test
```

环境变量：LOAD_TEST_CLOUD（云端地址，默认 ws://127.0.0.1:10000/v1/edge）、LOAD_TEST_NODES（节点数，默认 10）、LOAD_TEST_BEAT_SEC（心跳间隔，默认 2s）、LOAD_TEST_DURATION_SEC（测试时长，默认 10s）。

## 4. 已知限制

- 单机测试：所有客户端与本机 cloudcore 同机，网络延迟为回环（真实网络延迟会更高）
- 心跳指标需测试时长 > 心跳周期（30s）才能采样
- 内存测量未做（需 docker stats 或容器化运行）
- 100 节点规模验证建议在 CI 大 runner 或多机环境执行
