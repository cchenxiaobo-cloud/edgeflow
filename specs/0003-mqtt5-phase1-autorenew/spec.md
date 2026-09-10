# Spec 0003：MQTT 5.0 阶段一（版本参数化 + 原因码 + 流控）与登记项收口

- 版本归属：v0.30.0（基线 c7e04ee）
- 规范依据：FR-S2-07（DEVELOPMENT-SPEC 缺口表）、docs/MQTT5-EVALUATION.md（评估文档分期建议一）、KNOWN-ISSUES §29/§30 登记项
- 外部协议依据：MQTT Version 5.0, OASIS Standard（属性区/原因码/Receive Maximum 语义）
- 分段声明：本 spec 为 MQTT 5.0 **阶段一**；会话解耦（Session Expiry/Clean Start）、共享订阅、User Properties、mapper 配置项为**阶段二及后续轮**，不承诺版本。

## 用户故事与验收

### US-1 协议版本参数化（opt-in）
- `mqtt.Options` 新增 ProtocolVersion5（bool，默认 false）。默认路径下所有报文构造/解析产出与 v0.29.0 **逐字节一致**（冻结验收）。
- v5 模式 CONNECT：协议名 "MQTT"、级别 0x05，flags/keepalive/Payload 与 3.1.1 同布局；sim 侧按级别识别：4=既有路径，5=v5 路径（CONNACK 回 v5 形态），其余级别拒绝（3.1.1 sim 收 5 以外未知级别按既有拒绝语义断连）。
- 验收：单测——默认 OPTIONS 编码逐字节等价断言（对 3.1.1 路径）；v5 CONNECT 编码含级别 0x05。

### US-2 属性最小层
- 新增属性编解码单元（VBI 变长整数 + 属性 ID 分发），阶段一仅接 **Receive Maximum（0x21）**：CONNECT（客户端→服务器）与 CONNACK（服务器→客户端）可携带。
- 属性区仅存在于 v5 报文（3.1.1 路径无属性段，冻结保证）。VBI 1–4 字节编解码含边界拒绝。
- 验收：VBI 编解码往返 + 边界值（0 / 127 / 128 / 16383 / 16384 / 2097151 / 2097152 / 溢出拒绝）。

### US-3 原因码（v5 帧形态）
- CONNACK（v5）：`rc(1B) + PropertyLength + [RM]`；失败码语义化：0x84 不支持的协议版本、0x81 畸形报文、0x87 未授权、0x86 用户名密码错误、0x88 服务器不可用。
- PUBACK/PUBREC/PUBREL/PUBCOMP（v5）：可变头后追加 `rc(1B)`（Remaining Length 2 时 rc 视为 0x00，规范 §3.4.2.1）；成功 0x00，QoS1 无匹配订阅者 0x10（PUBACK）。
- SUBACK（v5）：属性段（PropertyLength，通常 0）+ 载荷逐订阅原因码（0x00/0x01/0x02 授予 QoS、0x8F 主题过滤器无效）。
- DISCONNECT（v5）：客户端可发 `rc + PropertyLength`（0x00 正常断开，Remaining Length 0 视为 0x00）；服务器流控违规回 DISCONNECT 0x93（Receive Maximum exceeded）后断连。
- 注：本仓 3.1.1 栈不含 UNSUBSCRIBE/UNSUBACK 包型，本轮不为其新增 3.1.1 能力（范围外）。
- 验收：编解码往返 + 失败码透出（客户端 Connect 错误信息带 0x84/0x87 码语义）。

### US-4 流控 Receive Maximum（client 出站）
- 客户端：v5 模式 CONNECT 携带自身 RM（Options.ReceiveMax，默认 65535=无限制）；发送侧 QoS1/2 在途（PUBACK/PUBCOMP 未回）超过 CONNACK 下发的 server-RM 时**暂停发送**（不阻塞 QoS0），按 server-RM 节流。
- sim：上行方向按 QoS2 暂存深度强制——v5 客户端在途超 server-RM → DISCONNECT 0x93 断连（规范语义）；**下行推送面阶段一仅 QoS0**（fanout 不产生 QoS1/2 下行），按 client RM 的下行强制随阶段二下行 QoS 推送面交付（as-built 修订，原 US-4 文本的下行子句移出本轮验收）。
- 验收：单测——客户端节流窗口（RM=2 时第三条 QoS1 等待前两条回执）；e2e——sim 强制 0x93 断连路径。

### US-5 e2e（v5 全链路 + 向下兼容）
- v5 client ↔ v5 sim：connect（RM 协商）→ sub → pub QoS0/1/2（PUBACK rc=0x00、QoS1 无订阅者 rc=0x10）→ disconnect(0x00)。TLS/mTLS 路径不在本轮（与 v5 正交）。
- 向下兼容：3.1.1 client 连 v5-capable sim 仍走既有 3.1.1 路径（sim 按级别分派），冻结测试零改动。
- 验收：新增 e2e 至少 2 例（全链路 + 流控 0x93）。

### US-6 OPC-UA 自动续期（75% 寿命，§30 收口）
- `Client.WithAutoRenew(ratio)`（0<r≤1，默认 0=关闭保持 v0.29.0 行为）：令牌寿命 RevisedLifetime 的 ratio 时刻自动触发 Renew（复用显式 Renew 全部换钥窗口语义）；None 通道不触发；续期失败按剩余寿命的 1/10 退避重试，不中断订阅。
- 并发约束：自动 Renew 与显式 Renew/在途请求互斥（同一 roundTrip 通道），不产生双续期。
- 验收：单测/e2e——短寿命令牌下自动续期发生、跨续期订阅持续收通知、关闭开关时行为与 v0.29.0 一致。

### US-7 mappers/opcua race flake 修复（§29/§30 收口）
- 定位 TestStopNilClientCollectErrors 偶发 DATA RACE 根因（stop 路径与采集 goroutine 的共享字段无同步），按所有权/锁纪律修复；不改变公开行为。
- 验收：`go test ./mappers/opcua -race -count=20` 稳定全绿。

## 冻结与兼容声明
- 3.1.1 默认路径（Options 零值 + 全部既有构造函数）报文字节、时序、错误语义逐字节不变；v0240–v0290 冻结测试零改动。
- v5 新行为全部经显式 opt-in 进入；5.0 测试与冻结带物理隔离（v0300_* 命名）。
- 契约 42 端点、零第三方依赖不受影响。

## Out-of-Scope（登记去向）
- 会话解耦 / Session Expiry Interval / Clean Start / 共享订阅 → MQTT 5.0 阶段二（v0.31.0 草案，非承诺）
- User Properties / Content Type / Payload Format → 阶段二评估
- mapper 环境变量（PROTOCOL_VERSION/RECEIVE_MAXIMUM 等）→ 阶段二随配置面统一扩展
- OPC-UA 互操作向量比对 → 仍登记 §30，接真实第三方服务器前必须补

## 实现偏差登记（as-built）
- **US-4 下行子句修订（复核 P1-1）**：原 US-4 承诺「sim 按 client RM 限制下行 QoS1/2 推送」，但阶段一 sim 下行 fanout 仅产生 QoS0（无 QoS1/2 推送面，client 侧 con.ReceiveMax 解码后即弃）；上行方向的实际执行点 = QoS2 暂存深度强制（0x93）。US-4 已修订为出站单向 + 登记边界；client-RM 下行强制归阶段二。
- **Renew 共享态安全重构（v0300 轮，-race 暴露）**：原 v0290 Renew 在 timeout>0 时无锁改写共享 c.timeout，与并发 roundTrip 的读取构成数据竞争（自动续期使显式/自动 Renew 与业务请求并发成为常态而暴露）；修复 = Renew 拆分内部 renewLocked（持 renewMu）+ roundTripTimeout 超时参数化（不再改写共享态）+ 修订寿命经返回值传递（消除 tokenLifetime 跨 goroutine 字段）。公开 API 签名不变（Renew 返回 error）。
- **nextReqID 原子化 + sendCLO None 分支补锁（flake 修复）**：Client 各服务调用构造 RequestHandle 均在 sendMu 之外调用 nextReqID（含 Close 的 CloseSession/DeleteSubscriptions），与订阅泵 PubAck→sendSecure 持锁自增构成数据竞争（§29/§30 flake 根因）；修复 = reqId 改原子自增（RequestHandle 与帧 RequestID 无需同值，语义不变）+ sendCLO None 分支同样持 sendMu（v0290 P2-1 只修了 B256 分支）+ 消除 sc.reqId 混合原子/非原子读。线格式不变；sendCLO None 分支文本重排按行为口径登记（延续 spec 0002 同名先例）。
- **v5 SUBSCRIBE 解码边界**：pkg/mqtt 严格解码器 decodeSubscribe 不支持 v5 形态（属性段）；v5 SUBSCRIBE 解码由 sim 宽容路径承担（跳过属性段后按 3.1.1 同形载荷解析）。客户端不接收 SUBSCRIBE，无影响面。
- **sim 关停报文队列化（单写者时序修复）**：修复前 serve 直写关停报文（鉴权 CONNACK / 流控 DISCONNECT 0x93）与 pump 异步确认队列可乱序上线，违背 MQTT 单字节流语义；修复 = 关停报文一律入队 + pump 关停时先清空既有队列 + shutdown 等泵退出后再关连接。行为变化：关停前已入队报文由「可能丢弃」变为「确定性上线」，冻结包测试全绿。
- **MQTT 属性区最小面（阶段一边界）**：PUBLISH/SUBSCRIBE 属性区仅接受空属性；CONNECT/CONNACK 仅 Receive Maximum（0x21）；未知属性拒绝。详见 KNOWN-ISSUES §31。
