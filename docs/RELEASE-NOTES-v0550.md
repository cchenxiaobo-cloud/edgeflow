# EdgeFlow v0.55.0 发布说明（MQTT 出站重发）

> 版本：v0.55.0 ｜ 日期：2026-10-21 ｜ spec：0028-mqtt-resend ｜ 缺口：replan 持续项 B1/B2
> 基线：v0.54.0（a0783ab）。**本版为客户端库能力：零 API 变更（契约 128 维持、消息 19 不变）。**

## N1 能力

- **QoS>0 出站重发定时器**（pkg/mqtt，spec 0028 裁决 1-2）：Options 新增
  `ResendInterval`（单轮确认等待与重发间隔；0=禁用）+ `MaxResends`（≤0 默认 3）。
  启用后 PUBLISH/PUBREL 写出按间隔轮询确认，超时即重发，总尝试 ≤ 1+MaxResends，
  额度耗尽返回 ack timeout。
- **三路径重发语义**：QoS1 = PUBLISH DUP=1 重发；QoS2 phase1（等 PUBREC）=
  PUBLISH DUP=1 重发；phase2（等 PUBCOMP）= PUBREL 重发（规范无 DUP 位）。
- **断连/关闭立即返回**：ErrClientClosed 路径不重发（重连后由既有机制承接）。
- **B3 销项**：出站 Topic Alias 已于 v0.33.0 交付（PublishTopicAlias）——侦察
  确认后本版不再改造，持续项登记闭环。

## N2 兼容

- 端点 128 不变、消息 19 不变；契约四件套零改动。
- **默认禁用逐字节兼容**：ResendInterval=0（默认）时 attempts=1、单轮
  ackTimeout，错误文本保持 "mqtt: ack timeout after <d>" 形态（errAckTimeout
  哨兵包装，既有调用方 errors.Is 无感）。
- **SUBACK 语义不变**：waitAck 拆分 waitAckFor——Subscribe 仍单轮 ackTimeout，
  重发轮仅作用于 publish 确认路径（spec 裁决 3）。

## N3 测试（新增 4 例）

- `pkg/mqtt/v0550_test.go` 4 例（scriptBroker 脚本化替身，锁内帧序记录）：
  - QoS1 重发至确认：broker 第 2 次才 PUBACK → 首发 DUP=0、重发 DUP=1。
  - 额度耗尽：broker 永不确认 → 3 帧（DUP false,true,true）后 ack timeout。
  - 默认禁用：单轮 ackTimeout 仅首发 1 帧（逐字节既有行为锚）。
  - QoS2 phase2：PUBREC 后 PUBREL 静默 → 重发 PUBREL 第 2 帧后 PUBCOMP 闭环。
- e2e 轨迹：run1 Dial 缺 ClientID → 补齐；run2 -race 抓到测试替身收集切片无锁
  访问 → 帧序记录移入 broker 锁内 → 复跑全绿。

## N4 边界（已知限制，详见 KI §56）

- 重发固定间隔（无退避曲线）；SUBSCRIBE/PINGREQ 无重发（库能力范围裁决）。
- 生产装配面未 env 化（代码 opt-in）；broker 侧下行重发不在库范围。

## N5 门禁

- `go build ./...` / vet / gofmt：净。
- `go test -race -count=1 ./pkg/mqtt/`：**ok 4.963s**（新增 4 例 + 既有全部通过
  ——重发默认禁用路径兼容性实证）。

## N6 复核

- R1（v0.52+v0.53）PASS 已处置；**R2（v0.54–v0.56 合并复核 + 全量 e2e 终验）**
  随 v0.56 执行，覆盖本版重发语义与既有兼容锚。
