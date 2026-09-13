# EdgeFlow v0.35.0 发布说明 — MQTT 5.0 阶段四：保留消息（Retain）面 + RH 语义

发布日期：2026-09-13 ｜ 上游规格：specs/0008-mqtt5-phase4-retain/spec.md（spec 0006 阶段四候选登记收口）

## 亮点

MQTT 5.0 协议栈补齐**保留消息（Retain）全链语义**——这是 MQTT 核心功能三大家
（QoS / 会话 / Retain）的最后一块：发布侧存储/覆盖/清除、订阅侧下发（含 v5 订阅
选项 Retain Handling 0/1/2）、转发侧 RAP（Retain As Published）生效、3.1.1 基础
支持，并补齐 `client.PublishRetain` 发布 API 与订阅在途缓冲竞态修复。零依赖、
契约 42 端点零改动。

### N1 保留存储（sim 发布侧）

- retain=1 发布存储/覆盖（QoS0/1/2 均存储——QoS0 为「SHOULD store」选择路径）；
  QoS2 在 PUBREL 交付完成时存储。
- 空 payload + retain=1：删除该主题保留消息；空消息本身照常转发给匹配订阅者。
- retain=0 不存储、不动既有保留消息；迭代按主题字典序（确定性）。

### N2 订阅下发 + RH 语义

- 新订阅（SUBACK 之后）下发匹配保留消息：通配匹配、QoS=min(存储, granted)、
  RETAIN=1；一次 SUBSCRIBE 内按主题去重。
- RH（v5 订阅选项）：0=总是下发；1=仅订阅不存在时（覆盖前判定）；2=不发送。
  3.1.1 无选项字节 → RH=0 行为；下行维持 QoS0 现状。
- 下发 QoS1（min≥1 且 granted≥1）：纳入会话在途窗口模型（与 v0330 同构，
  offline 队列 + PUBACK 推进）。

### N3 RAP 转发与发布 API

- v5 订阅 RAP=1：转发 retain=1 消息时保留 RETAIN=1（含离线条目——恢复下发按
  条目字段重放）；RAP=0（默认）：转发恒 RETAIN=0。
- `client.PublishRetain(topic, qos, payload, retain)`：发布侧 API 补齐
  （空 payload + retain=true = 清除）；QoS/Ack 路径与 Publish 一致。

### N4 开发期修复（测试暴露的真实缺陷）

- sim permissive 订阅解析丢弃 RH 位（只校验不存储）→ RH 传入 TopicFilter
  （v0330 遗留的对称缺口）。
- client 订阅在途窗口竞态：SUBACK 后紧随的 retained 消息可能先于 handler 注册
  到达被丢弃 → 在途 filter 登记（pendingSubs）+ 缓冲补投（扩展 v0320 恢复
  缓冲机制：订阅在途期间匹配消息进缓冲，注册后按 filter 补投）。
- QoS2 parked 结构保留 Retain 字段（此前 PUBREL 交付/记录丢失 retain 标志）。
- RAP 离线重放缺口（复核 P1-1）：离线会话暂存条目未填 Retain → 重连恢复重放丢
  标志；补 `Retain: retain && 命中 filter 的 RAP`（rapHit）——在线直发/离线暂存/
  恢复重放三路标志完整。

### N5 测试与兼容

- 新增 18 例测试：sim 14（存储/覆盖/清除/QoS0/QoS2/RH 0/1/2/通配排序去重/
  RAP 在线+离线重放/不存储/3.1.1/QoS min/畸形选项拒绝）+ client e2e 4
  （全链/清除/RH=2/QoS1 链路）。含 -race 与多轮复跑。
- 冻结兼容：无 retain 消息路径逐字节不变；默认 3.1.1 行为零变化；契约 42 端点
  零改动；零新依赖。

### N6 边界登记（KNOWN-ISSUES §36）

- will 面（Will / Will Retain / Will Delay）整体不做（留后续版本）；共享订阅不
  触发 retained 下发；retained 不跨 broker 重启持久化（sim 内存）；无上限/淘汰
  策略（测试 broker）；handler 面不带 retain 标志（富 handler 后续）。
