# spec 0028 — MQTT 出站重发（v0.55.0，持续项 B1/B2）

> 版本：v0.55.0 ｜ 缺口：replan 持续项 B1「重发定时器」+ B2「client 出站重发」｜
> 上游：spec 0003–0005/0030 系列（MQTT 5.0 五阶段）、spec 0033（v0.33 Topic Alias）
> 说明：本版为客户端库能力（pkg/mqtt），零 API 变更（契约 128 维持、消息 19 不变）。

## 背景与范围裁决

持续项侦察结论：
- **B3「Topic Alias 出站方向」已于 v0.33.0 交付**（Options.PublishTopicAlias，
  client.go aliasMap/alias-only 帧）——本版销项，无改造。
- **B1/B2 缺口确认**：publish() QoS1/2 写出后 waitAck 单轮阻塞（ackTimeout 超时
  即报错返回），无 DUP=1 重发——静默丢帧场景（broker 收到但 ACK 丢失/网络抖动）
  下消息丢失且上层无感。

**裁决：QoS>0 出站重发定时器——Options opt-in，三路径（QoS1 PUBLISH DUP=1 /
QoS2 phase1 PUBLISH DUP=1 / phase2 PUBREL），默认禁用逐字节兼容。**

## 裁决

1. **Options 字段**：`ResendInterval`（单轮确认等待与重发间隔；0=禁用=既有单轮
   ackTimeout 行为）+ `MaxResends`（重发上限，≤0 取默认 3）。启用后总尝试
   ≤ 1+MaxResends，额度耗尽返回 ack timeout（errAckTimeout 哨兵包装，错误文本
   保持 "mqtt: ack timeout after <d>" 形态）。
2. **三路径语义**：QoS1 = PUBLISH DUP=1 重发；QoS2 phase1（等 PUBREC）= PUBLISH
   DUP=1 重发；phase2（等 PUBCOMP）= PUBREL 重发（规范无 DUP 位）。断连/关闭
   （ErrClientClosed）立即返回不重发。
3. **SUBACK 语义不变**：waitAck 拆分 waitAckFor——Subscribe 仍单轮 ackTimeout，
   重发轮仅作用于 publish 确认路径。
4. **默认兼容**：ResendInterval=0（默认）时 attempts=1，waitAckFor(ackTimeout)，
   逐字节等价既有行为；既有错误文本不变。
5. **B3 销项**：出站 Topic Alias 已于 v0.33.0 交付（PublishTopicAlias），本版
   如实登记不再改造。
6. **非目标**：SUBSCRIBE/PINGREQ 重发、断连后离线重排队（QoS2 已有
   PersistenceDir/ResumePending 机制）、mqttsim broker 侧下行重发（B1 的 broker
   侧解读——broker 为外部组件/仿真器，重发责任在客户端）、重发退避曲线（固定间隔）。

## 端点

无新增（128 维持）。库能力：`mqtt.Options{ResendInterval, MaxResends}`（代码层
opt-in，生产装配面按需接线）。

## 交付锚

- 实际交付（2026-10-21）：单测 4 例（重发至确认/额度耗尽/默认禁用锚/QoS2 phase2
  PUBREL 重发）；race 全包 4.963s 绿（新增 4 例 + 既有全部通过——默认禁用路径
  兼容性实证）；零 API 变更（契约 128 维持）。
- B3 销项：出站 Topic Alias 已于 v0.33.0 交付（PublishTopicAlias），持续项登记
  闭环。
- R2（v0.54–v0.56 合并复核 + 全量 e2e 终验）随 v0.56 执行。
