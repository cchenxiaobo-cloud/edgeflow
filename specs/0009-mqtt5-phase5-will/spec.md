# Spec 0009：MQTT 5.0 阶段五（遗嘱消息 Will 面）

- 版本归属：v0.36.0（基线 dd48437 = v0.35.0）
- 规范依据：spec 0008 边界登记「will 面（Will/Will Retain/Will Delay）留后续版本」+ KNOWN-ISSUES §36「will 面…仍为后续」+ 用户方向「继续开发后续功能」
- 裁定结论（本 spec 前置决策）：will 是 MQTT 5.0 核心功能最后一块（规范 3.1.2.5/3.1.2.11）。本版全链交付：v5 Will Properties 编解码（Will Delay Interval）+ sim 存储与触发发布（异常断连发布/正常断开抑制）+ v5 Will Delay（含重连取消）+ Will Retain×v0.35 retained store 交叉 + client 侧 will 配置。零依赖、全在 pkg/mqtt + pkg/mqttsim。

## 用户故事与验收

### US-1 v5 Will Properties 编解码（pkg/mqtt）
- v5 CONNECT willFlag=1 时 payload 顺序为 ClientID, Will Properties, Will Topic, Will Message：补齐 Will Properties 区段编解码（varint 长度 + 白名单解析）。
- 解析字段：Will Delay Interval（0x18，四字节整型秒）；白名单外/重复属性拒绝（延续属性区白名单语义；不透传）。
- 编码：WillDelay>0 → 写 0x18 属性；否则写空属性区（长度 0x00）。
- 校验：3.1.1（V5=false）非零 WillDelay 拒绝；willFlag=0 携带非零 WillDelay 拒绝（均为编码校验；wire 上 willFlag=0 无 Will Properties 区，解码侧不作该组合判定）。
- 验收：v5 roundtrip（delay 0/非 0）、手工字节解码（空属性区与 0x18 形态）、3.1.1 字节不变（v0240 冻结测试继续通过）、v5 无 will 连接字节不变。

### US-2 sim 存储与触发发布
- CONNECT 接受（鉴权通过、会话处理后）存储 will（topic/payload 副本/QoS/retain/delay）；鉴权失败、CONNECT 拒绝、未 CONNECT 连接不存储不发布。
- 断连判定（唯一收口 unregister，锁外发布）：
  - 未收到 DISCONNECT 或 v5 DISCONNECT reason≠0（含 0x04）→ 异常 → 发布 will。
  - 正常 DISCONNECT（3.1.1 空体 / v5 reason=0x00）→ 抑制。
  - 会话接管踢连接 → 未正常断开 → 发布（规范 takeover 语义）。
  - broker 已关闭（b.closed）→ 不发布。
- 发布路径：QoS 0/1/2 走 fanoutBytes（在线 v5 granted≥1 收 QoS1 下行、离线 QoS1 暂存等既有语义）；Will Retain=1 → updateRetained 存储（顺序仿 PUBLISH 路径）+ 转发 retain 标志（RAP 语义复用）。
- 恰好一次：willSent CAS 防重（shutdown 可重入路径）。
- 验收：异常发布（QoS0）/正常抑制/无 will 零变化/v3.1.1 发布/QoS1 下行/离线暂存/retain 交叉/鉴权拒绝不发布。

### US-3 v5 Will Delay
- delay=0：断连立即发布。
- delay>0：挂 broker.pendingWills 定时器延迟发布；期间同 ClientID 新连接建立（恢复或重建）→ 取消（stop+delete；先于接管踢人）。
- broker.Close：停止全部 pending 定时器，不再发布。
- 定时回调竞态：回调与取消以 b.mu 内 pendingWills 归属判定（先到者赢，恰好一次）。
- 验收：delay=0 立即/delay=1s 延迟（等待窗内无消息、到期发布）/重连取消（超过 delay 仍无消息）/Close 清理。

### US-4 client 侧 will 配置
- Options 增：WillTopic / WillMessage / WillQoS / WillRetain / WillDelaySec（v5 生效；3.1.1 非 0 将被拒绝——Dial 报错）。
- Dial CONNECT 构造按配置写入；v5 编码带 will properties 区。
- 正常 Close 仍是 DISCONNECT（rc=0）→ 不触发 will（与 US-2 联动）。
- 验收：配置后正常关闭无 will；接管踢连接触发 will 发布（e2e）；will retain 落 store。

### US-5 兼容与冻结
- 无 will 连接的编解码与运行时路径零变化（v5/3.1.1 均）。
- 3.1.1 带 will 连接：新增规范行为（此前 sim 忽略 will，现在发布）——既有测试无此场景，零冻结破坏。
- 契约 42 端点零改动；零新依赖；edgecore/云/edge 零触碰；v0240–v0350 冻结测试零改动。

## 冻结与兼容
- 全仓核查：v5+will 编码字节此前无任何用例（grep WillTopic 仅 v0240 3.1.1 冻结面），格式修正零破坏。
- will 发布仅对「CONNECT 携带 will 的连接」生效 → 默认路径（无 will）逐字节不变。
- 会话过期清理路径不触碰（登记：过期先于 delay 的「取小发布」不做）。

## 测试锚（预估，实现后核对）
- sim（pkg/mqttsim/v0360_test.go，预估 13-15 例）：正常抑制/异常发布/QoS1 下行/离线暂存/retain 交叉/无 will 零变化/v3.1.1/鉴权拒绝/delay 0/delay 延迟/重连取消/Close 清理/rc=0x04 发布。
- packet（pkg/mqtt/v0360_test.go，预估 3-4 例）：v5 roundtrip（delay 0/非 0）、手工字节解码、编码拒绝（3.1.1 delay、willFlag=0 带 0x18）。
- client e2e（pkg/mqtt/v0360_e2e_test.go，package mqtt_test，预估 3 例）：正常关闭无 will/接管踢触发 will/will retain 落 store。

## 边界与非目标（登记 KNOWN-ISSUES §37）
- will 消息的 v5 属性（content type/response topic 等）不透传（sim Publish 无属性模型）——属性区跳过不解析。
- 会话过期先于 Will Delay 的「取小发布」不做；接管/踢连接的 delay 走通用延迟路径（规范「接管立即发布」细节简化）。
- 取消条件=同 ClientID 新连接建立（不区分恢复/重建，取消检查先于接管踢人）；被踢旧连接在 kick 时新生成的 will 不受该次连接抑制（走通用延迟路径）。同 ClientID 旧待发 will 被新 pending 注册替换（注册覆盖）。
- QoS2 will 在线下行简化引用既有登记（QoS1 下行语义）。
- will 空 payload+retain 走 v0.35 既有语义（清除+照常转发）。
- will 持久化（跨 broker 重启）不做。

## as-built 登记（开发完成，2026-09-13）

- 实现面：pkg/mqtt/packet.go（v5 Will Properties 编解码 + Connect.WillDelay）；
  pkg/mqttsim/sim.go（will 存储 / 异常断连发布 / pendingWills 定时器 / 同 ClientID
  新连接取消 / Close 清理）；pkg/mqtt/client.go（Options will 配置 + v5 编码）。
- 测试锚（23 例，逐条核实）：sim 15（AbnormalPublishBasic / NormalDisconnectSuppressed /
  V311WillPublish / V311NormalDisconnectSuppressed / NoWillNoPublish / DisconnectRC04Publishes / WillQoS1Downlink /
  WillOfflineStoreReplay / WillRetainStored / WillDelayDeferred / WillDelayCancelOnReconnect /
  WillDelayBrokerClose / TakeoverPublish / AuthRejectNoWill / WillEmptyPayloadRetain）+
  packet 5（WillPropsRoundTrip / WillPropsManualDecode / WillPropsReject /
  WillPropsEncodeReject / NoWillByteIdentical）+ client e2e 3（WillNormalCloseNoPublish /
  WillTakeoverPublish / WillRetainStoreE2E）。
- 实现调整（开发期）：取消逻辑从「恢复分支」调整为「同 ClientID 新连接统一取消」
  （规范：delay 内新连接抑制 will，不区分恢复/重建）。
- 开发期调试记录（测试自身，非产品缺陷）：① 会话快照等待条件（filters>0）不覆盖
  无订阅的 will 发送方 → v0360WaitDetached；② 鉴权 broker 下订阅者连接需带凭证；
  ③ 空 payload 用例需先读掉先前的普通 fanout 消息。
