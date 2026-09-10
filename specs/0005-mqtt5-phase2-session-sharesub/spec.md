# Spec 0005 — MQTT 5.0 阶段二：会话解耦 + 共享订阅（v0.32.0）

- **FR**：FR-S2-07（DEVELOPMENT-SPEC 缺口表 v0.32.0 草案行）
- **版本**：v0.32.0
- **边界**：pkg/mqtt（codec/客户端）+ pkg/mqttsim（内置 broker）。云端 CloudHub MQTT 面不在本轮（契约 42 端点冻结不动）。$queue/ 隐式共享订阅不支持（5.0 以 $share 为准，登记）。

## 背景

v0.30.0 交付 MQTT 5.0 阶段一（版本参数化 + 原因码 + Receive Maximum 流控 + 属性最小层）。
当前会话与连接完全耦合：client `CleanSession` 恒 true（硬编码），broker 订阅表挂连接对象、
断连即丢、无离线消息面；无共享订阅。本阶段把会话从连接解耦，并补组内负载均衡订阅。

## 用户故事与验收

### US-1 会话属性 codec（pkg/mqtt/packet.go）

- CONNECT v5 属性区支持 Session Expiry Interval（0x11，u32，秒）：`Connect.SessionExpiry`
  字段，>0 时编码进属性区。
- 属性区编解码从"单条 RM（3B）"扩展为通用白名单解析：0x21 RM（u16）与 0x11 SE（u32），
  任意组合与顺序循环解析（编码端固定按 0x11 → 0x21 次序）；未知属性仍拒绝（阶段一语义延续）。
- CONNACK v5 可携带 SE 回显（`Connack.SessionExpiry`，broker 接受值）。
- **冻结**：3.1.1 路径逐字节不动；V5=false 不触碰属性区。

### US-2 客户端持久会话 opt-in（pkg/mqtt/client.go）

- `Options.PersistentSession`（默认 false = 现状）：true 时 v5 连接发 CleanStart=0 +
  SessionExpiry 属性；3.1.1 连接发 CleanSession=0。
- `Options.SessionExpiryMs`（v5 专用，毫秒 → 秒换算向上取整；0 = 断连即毁的持久订阅，
  仅订阅表保留）。
- CONNACK 的 Session Present 暴露：`Client.SessionPresent()`。
- 默认（false）连接逐字节现状。

### US-3 broker 会话状态机（pkg/mqttsim）

- 按 ClientID 维护会话（订阅表 + 离线 QoS1 队列 + 过期时刻）：会话生命周期与连接解耦。
- CleanStart/ CleanSession=1 → 丢弃旧会话新建（SessionPresent=0）；=0 且会话存在 →
  恢复（SessionPresent=1、订阅保留、离线消息下发）；=0 且无会话 → 新建（present=0）。
- v5 SessionExpiry>0：断连后会话保留至到期（惰性清理：CONNECT/fanout/检索时扫除）；
  expiry=0：断连即毁（现状默认行为）。3.1.1 CleanSession=0：保留至 broker 关闭（无限期）。
- CONNACK 回 Session Present + 接受的 Session Expiry（v5）。
- 同 ClientID 新连接接管：旧连接被踢下线（shutdown），会话转接新连接。

### US-4 离线 QoS1 暂存与恢复下发

- 会话离线期间匹配其订阅的 QoS1 消息入会话离线队列（上限 64 条，超限丢最旧并计数）；
  QoS0 不暂存（best-effort，登记 as-built）。
- 重连（CleanStart=0）后按 QoS1 下发（dup=0），收到该连接 PUBACK 后出队；无重发定时器
  （PUBACK 未达不重推，as-built 登记为阶段二简化）。
- 共享订阅不参与离线暂存（见 US-5）。

### US-5 共享订阅（$share/{group}/{filter}）

- SUBSCRIBE 校验：`$share/{group}/{filter}` 形态，group 非空、inner filter 合法
  （ValidateTopicFilter）；非法形态该 filter 回 SUBACK 0x80。
- 组路由：组键 = (ShareName, inner filter)。消息 fanout 时组内**在线**成员 round-robin
  轮转（每条消息恰一人收到）；离线共享成员不参与轮转、不暂存。
- 普通订阅独立于共享订阅分发（同一连接可同时持有两种）。
- `$queue/` 前缀不支持（SUBACK 0x80，登记）。

## 非目标（阶段三/登记）

- 下行 QoS1/2 inflight 重发状态机（PUBACK 未达重推、报文 ID 管理）——broker 侧完整
  QoS1 下行属阶段三。
- 云端 CloudHub 的 MQTT 会话面（契约冻结）。
- $queue/、订阅选项（Subscription Options：No Local / Retain As Published / Retain
  Handling）、Will 延迟、Topic Alias。

## As-Built 登记位

实现偏差在本节登记（完成后回填）。

- **US-2/US-3 范围修订**：3.1.1 CleanSession=false 持久会话不支持——3.1.1 连接整体退出会话状态机（恒 clean 语义、断连即毁、无接管仲裁），冻结兼容优先（v0240–v0300 全量回归零触碰）。会话解耦仅 v5 生效。
- **US-3 接管仲裁收窄**：仅任一方持有持久会话意图时踢旧连接（新连接 CleanStart=0/SE>0 或旧连接 SE>0）；双方 clean 连接并存保持 v0.24.0 现状（v0260 冻结测试依赖同 ID 并存）。
- **US-4 语义登记**：离线队列 ≤64 丢最旧；恢复下发 dup=0、broker 侧无重发定时器；恢复消息在 client handler 注册前到达时经 pendingRecovered（≤32 丢最旧）补投，确认与补投解耦（至多一次）。
- **US-5 语义登记**：下行 QoS 恒 0（恢复下发 QoS1 除外）；组内成员 fanout 前按 ClientID 排序保证轮转确定性；hasSubscriber 计入共享订阅。
- **确认类报文属性区宽容**：PUBACK/PUBREC/PUBREL/PUBCOMP/DISCONNECT 的属性区仍走白名单解析（0x21/0x11），与阶段一宽容策略一致；未知属性拒绝。
- **v5 连接 SUBSCRIBE 属性区**：client 栈出站 Subscribe/Puback/Pubrec/Disconnect 补 V5 形态标注（阶段一遗漏——v5 连接上 SUBSCRIBE 曾以 3.1.1 形态发出，broker 宽容路径错位；本轮修复并经 e2e 覆盖）。
- **复核修复（v0320 复核轮）**：① fanoutBytes 持锁路径改内联入队（enqueueQoS 队列满分支会锁 b.mu，持锁调用 = 非重入自死锁；慢消费者回归锚）；② client 恢复缓冲限定 QoS<2（QoS2 park/PUBREC 状态机不受拦截）；③ sim pump 写截止（正常 5s / drain 1s）——死消费者不再可挂起接管与关停；④ decodeProps 拒绝白名单内重复属性；⑤ v5 出站 Subscribe 等 V5 标注（见上）。