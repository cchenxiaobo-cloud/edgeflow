# EdgeFlow v0.66.0 发布说明（$queue 队列订阅）

> 版本：v0.66.0 ｜ 日期：2026-10-22 ｜ spec：0040-queue-subscription ｜ 缺口：持续项 B5 相邻
> 基线：v0.65.0（16be79a）。**本版为 MQTT 语义扩展（mqttsim 仿真器面）：零 API 变更（契约 139 维持）、零新增消息（19 不变）。**

## N1 能力

- **$queue 队列订阅**（mqttsim 仿真器）：parseShareFilter 支持
  "$queue/{inner}" → 固定组（"$queue"）共享订阅——复用既有 $share 轮转
  机制，多订阅者负载均衡轮转接收。
- **granted-QoS1**：$queue 订阅的 SUBACK 回显请求 QoS（与 $share 一致）；
  v5 面 granted≥1 → QoS1 下行（对齐 $share 语义）。
- **隔离语义**：$queue 与 $share/{group} 各自独立轮转（互不干扰）。

## N2 兼容

- 零 API 变更（契约 139 维持）；消息 19 不变。
- **行为变更**：$queue filter 从 SUBACK 失败（0x80）变为正常 granted——
  既有客户端如果依赖 $queue 被拒绝的行为需适配（登记 KI）。
- v3.1.1 路径行为不变（$queue 仍拒绝——冻结锚保留）。

## N3 测试（新增 2 例 + 1 既有适配）

- `pkg/mqttsim/v0660_test.go` 2 例：$queue 轮转（双订阅者各收一份 QoS1）/
  $queue 与 $share 隔离（同主题独立组互不干扰）。
- 既有适配：v0320 TestV0320ShareSubscriptionRejects——$queue 从拒绝列表
  移出，单独验证 granted 为请求 QoS。

## N4 边界（详见 KI §68）

- $queue 为非标准 MQTT 扩展（部分 broker 支持）——真实 broker 部署时
  需确认对端支持。
- $queue 不指定组名（固定组 "$queue"）——无法像 $share 那样创建多个
  独立消费组。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race -count=1 ./pkg/mqttsim/`：**19.457s** 全绿（新增 2 例 +
  既有适配 + 全过）。

## N6 复核

- R5/R6 已完成；**全量 e2e 终验（-timeout 30m）**随本版推送执行。
