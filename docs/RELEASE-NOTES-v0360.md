# EdgeFlow v0.36.0 发布说明（MQTT 5.0 阶段五：遗嘱消息 Will 面）

- 发布日期：2026-09-13
- 基线：v0.35.0（dd48437）
- 主题：MQTT 5.0 阶段五——遗嘱消息（Will）：v5 Will Properties 编解码 + sim 存储与触发发布（异常断连发布/正常断开抑制）+ Will Delay（含重连取消）+ client 侧配置。零依赖、契约 42 端点不变。

## N1 能力（新增）

### 1. v5 Will Properties 编解码（pkg/mqtt）
- v5 CONNECT willFlag=1 时 payload 规范顺序 ClientID, Will Properties, Will Topic, Will Message：补齐 Will Properties 区编解码（此前直读 topic/message，与真实 v5 客户端不互操作）。
- Will Delay Interval（0x18，u32 秒）解析与编码；空属性区（长度 0x00）合法；白名单外/重复属性拒绝。
- 校验：3.1.1 携带 WillDelay 编码拒绝；willFlag=0 携带 WillDelay 编码拒绝。
- 兼容锚：v5 无 will 连接编码逐字节不变（TestV0360NoWillByteIdentical）；3.1.1 路径（含 will 字段）字节不变（v0240 冻结测试继续通过）。

### 2. sim 存储与触发发布（pkg/mqttsim）
- CONNECT 接受后存储 will（topic/payload 副本/QoS/retain/delay）；鉴权失败与 CONNECT 拒绝路径不存储（规范：拒绝连接不发送 will）。
- 断连判定（唯一收口 unregister，锁外发布）：
  - 未收到 DISCONNECT 或 v5 DISCONNECT rc≠0（含 0x04 disconnect-with-will）→ 异常 → 发布；
  - 正常 DISCONNECT（3.1.1 空体 / v5 rc=0x00）→ 抑制；
  - 会话接管踢连接 → 发布（规范 takeover 语义）；
  - broker 已关闭 → 不发布；恰好一次（willSent CAS 防重，shutdown 路径可重入）。
- 发布路径复用既有语义：QoS 0/1/2 经 fanoutBytes（v5 granted≥1 收 QoS1 下行；离线持久会话 QoS1 暂存）；Will Retain=1 落 retained store（与 v0.35.0 交叉）。
- 3.1.1 连接带 will：同样支持（规范共有行为；无 will 连接零变化）。

### 3. v5 Will Delay（pkg/mqttsim）
- delay=0 立即发布；delay>0 挂 broker.pendingWills 定时器延迟发布。
- delay 内同 ClientID 新连接建立（恢复或重建）→ 取消待发（规范：重连抑制 will；取消先于接管踢人）。
- Broker.Close 停止全部待发定时器（不再发布）。

### 4. client 侧配置（pkg/mqtt）
- Options 增 WillTopic/WillMessage/WillQoS/WillRetain/WillDelaySec；Dial 校验（WillQoS≤2；WillDelaySec 仅 v5）。
- 正常 Close 保持 DISCONNECT rc=0（不触发 will，与 US-2 联动）。

## N2 兼容与冻结
- 无 will 连接：编解码与运行时路径零变化（v5/3.1.1 均）；默认路径逐字节不变。
- 全仓核查：v5+will 编码字节此前无任何既有用例（grep 仅 v0240 3.1.1 冻结面）→ 格式修正零破坏。
- 契约 42 端点零改动；零新依赖；edgecore/云/edge 零触碰；v0240–v0350 冻结测试零改动。

## N3 测试（23 例）
- sim 15：AbnormalPublishBasic / NormalDisconnectSuppressed / V311WillPublish / V311NormalDisconnectSuppressed / NoWillNoPublish / DisconnectRC04Publishes / WillQoS1Downlink / WillOfflineStoreReplay / WillRetainStored / WillDelayDeferred / WillDelayCancelOnReconnect / WillDelayBrokerClose / TakeoverPublish / AuthRejectNoWill / WillEmptyPayloadRetain。
- packet 5：WillPropsRoundTrip / WillPropsManualDecode / WillPropsReject / WillPropsEncodeReject / NoWillByteIdentical。
- client e2e 3：WillNormalCloseNoPublish / WillTakeoverPublish / WillRetainStoreE2E。

## N4 边界（登记 KNOWN-ISSUES §37）
- will 消息的 v5 属性（content type/response topic 等）不透传（属性区白名单外拒绝）。
- 会话过期先于 Will Delay 的「取小发布」不做；接管/踢连接的 delay 走通用延迟路径（被踢旧连接的 will 在其断连时生成，不受新连接取消影响——边界登记）。
- 同 ClientID 旧待发 will 被新 pending 注册替换（边缘场景，丢失旧 will）。
- QoS2 下行简化引用既有登记；will 空 payload+retain 走 v0.35.0 既有语义（清除+照常转发）。

## N5 门禁（2026-09-13 全绿）
- vet / 全量回归（mqtt 3.4s + mqttsim 17.8s；全仓除 e2e）/ race ×2（mqtt 7.8s + mqttsim 36.3s）
  / 契约 14.7s / e2e 全量 352.3s：全绿。
