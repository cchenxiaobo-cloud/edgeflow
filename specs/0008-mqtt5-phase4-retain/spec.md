# Spec 0008：MQTT 5.0 阶段四（保留消息 Retain 面 + RH 语义）

- 版本归属：v0.35.0（基线 ee743d4 = v0.34.0）
- 规范依据：spec 0006 阶段四候选登记（RH 行为）+ ROADMAP §29 登记（「RH 仅校验+存储（无 retain 转发面）」）+ KNOWN-ISSUES §34 + 用户方向「继续开发后续功能」
- 裁定结论（本 spec 前置决策）：retain 是 MQTT 最后一块核心功能面（QoS/会话/订阅选项已做）。本版全链交付：发布侧存储语义 + 订阅侧下发（含 RH）+ 转发侧 RAP 生效 + v3.1.1 基础支持。零依赖、全在 pkg/mqttsim + pkg/mqtt；will 面（含 Will Retain）与共享订阅 retained 下发不做（登记 §36）。

## 用户故事与验收

### US-1 保留存储（sim 发布侧）
- retain=1 的 PUBLISH：存储（或覆盖同主题）retained 消息（含 QoS）；QoS0/1/2 均存储（规范 SHOULD 路径；QoS2 在 PUBREL 完成时存储）。
- 空 payload + retain=1：删除该主题 retained；空消息本身照常转发给当前匹配订阅者。
- retain=0：不存储、不删除既有 retained。
- 存储无上限（测试 broker，登记）；迭代按主题字典序（确定性）。
- 验收：存储/覆盖/清除/空消息转发/QoS0/QoS2 存储。

### US-2 订阅下发 + RH（v5）+ 3.1.1 基础
- 新订阅（SUBACK 后）下发匹配 retained：按 filter 通配匹配，QoS=min(存储 QoS, granted)，RETAIN=1。
- RH 语义（v5）：0=总是；1=仅订阅不存在时（按 filter 键、覆盖前判定）；2=不发送。（3.1.1 无选项字节 → RH=0 行为。）
- 顺序：SUBACK 先于 retained 消息（实现语义；规范允许两种顺序）。
- 一次 SUBSCRIBE 内多 filter 通配重叠：按主题去重（登记）。
- 共享订阅：不下发（登记）。
- 验收：RH 0/1/2、通配、QoS min、SUBACK 顺序、v3.1.1 基础。

### US-3 RAP 转发生效
- v5 普通订阅（聚合语义）：RAP=1 → 转发 retain=1 发布时保留 RETAIN=1；RAP=0（默认）→ 转发恒 RETAIN=0。
- 非 retain 发布恒 RETAIN=0；3.1.1 恒 0（冻结）。
- 订阅下发场景 RETAIN=1 不受 RAP 影响（规范 [MQTT-3.3.1-8]）。
- 验收：RAP=1/RAP=0/非 retain/3.1.1。

### US-4 会话集成与存量修复
- retained 下发 QoS1（min≥1 且 granted≥1）：纳入会话在途窗口模型（与 v0330 一致：offline 队列 + 窗口 + PUBACK 推进）。
- 存量修复：QoS2 parked 结构保留 Retain 字段（此前 PUBREL 交付/记录丢失 retain 标志）。
- 验收：QoS1 下发链路 + QoS2 retain 存储。

## 冻结与兼容
- 无 retain 消息路径逐字节不变：普通发布/转发/订阅/会话零行为差异（新面从无到有；全仓核查无 retain 行为断言）。
- 3.1.1：retain 基础语义启用（协议共有）；选项字节/RH 仅 v5。
- 契约 42 端点零改动；零新依赖；edgecore/云/edge 零触碰。
- v0240–v0340 冻结测试零改动。

## 测试锚（预估，实现后核对）
- sim（pkg/mqttsim/v0350_test.go，预估 12 例）：存储/覆盖/清除/空消息、QoS0/QoS2、RH 0/1/2、通配、RAP 转发、非 retain 不存、v3.1.1、SUBACK 顺序、QoS min。
- client e2e（pkg/mqtt/v0350_e2e_test.go，package mqtt_test，预估 2-3 例）：跨 client retain 全链（发布→订阅收 retained）、RH=2 抑制、QoS1 下发。

## 边界与非目标（登记 KNOWN-ISSUES §36）
- will 面（Will/Will Retain/Will Delay）整体不做（留后续版本）。
- 共享订阅 retained 下发不做（登记）。
- retained 跨 broker 重启持久化不做（sim 内存 store）。
- retained 上限/淘汰策略不做（测试 broker）。
- QoS0 retained「MAY discard」选择存储路径（登记）。
- retained 下发为「受控内部发布」：不经过 hasSubscriber/fanout，直接按订阅者构造。

## as-built 登记（开发完成，2026-09-13）

- 实现面：pkg/mqttsim/sim.go（retained store / updateRetained / deliverRetained /
  queueDownlinkQoS1 / RAP 转发标志 / permissive 解析 RH）；pkg/mqtt/client.go
  （PublishRetain 发布 API + 订阅在途缓冲 pendingSubs）；pkg/mqtt QoS2 parked
  保留 Retain 字段。
- 测试锚（18 例，逐条核实）：sim 14（StoreDeliverBasic / Overwrite /
  ClearEmptyPayload / QoS0 / QoS2 / RH1NewOnly / RH2Suppress /
  WildcardOrderedDedup / RAPForward / PlainNoStore / V311 / GrantMin /
  RAPOfflineReplay / SubscribeMalformedOptionsRejected）+
  client e2e 4（RetainE2E / RetainClearE2E / RetainRH2E2E / RetainQoS1E2E）。
- 开发期修复（测试暴露，均含锚）：① permissive 订阅解析丢弃 RH 位（v0330 只校验
  不存储的对称缺口）；② client 订阅在途竞态——SUBACK 后紧随的 retained 可能先于
  handler 注册到达被丢弃 → pendingSubs 在途登记 + 缓冲补投（扩展 v0320 恢复缓冲）；
  ③ QoS2 parked 结构丢 Retain 字段；④ 离线会话暂存条目丢 RAP 标志（复核
  P1-1，恢复重放场景）→ rapHit 补标志。
- 实现语义登记：SUBACK 先于 retained（单写者 FIFO）；一次 SUBSCRIBE 内按主题
  去重；恢复下发按条目 Retain 字段重放（离线条目同样保留标志）；v3.1.1 下行恒
  QoS0（含 retained）；RH=1「订阅已存在」按 filter 键覆盖前判定；共享订阅不触发
  下发。补充登记：NoLocal 与 retained 下发未交叉判定（规范留白、维持现状）；
  pendingSubs 残留缓冲（订阅失败后 ≤32 滞留、至多一次补投）与同 filter 并发订阅
  微边界。
