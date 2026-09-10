# spec 0006 — MQTT 5.0 阶段三：QoS1 可靠下行 + 订阅选项 + Topic Alias（v0.33.0）

上游：FR-S2-07 阶段三段（DEVELOPMENT-SPEC 缺口表）。前置：0003（阶段一）、0005（阶段二）。
冻结底线：v3.1.1 字节路径零触碰；契约 42 端点零改动；零第三方依赖；v0240–v0320 测试零改动全绿。

## US-1 下行 QoS 按订阅授予（仅 v5）
v5 SUBSCRIBE 携带订阅 QoS 请求，broker 授予 granted = min(req, 1)（请求 2 → 授予 1，不拒绝；
SUBACK 逐订阅回 granted）。下行 PUBLISH 按**成员订阅条目**的 granted 发送：granted=0 维持
best-effort QoS0 现状；granted≥1 走 QoS1（分配 pktID、等 PUBACK）。v3.1.1 订阅/下行行为
逐字节现状冻结（SUBACK granted 现状、下行恒 QoS0）。

## US-2 PUBACK 确认与 inflight 窗口（broker 会话级）
统一 v0320 恢复下发窗口模型：会话下行队列（offline 头部 dispatched 条 = 已入连接队列在途）。
- 新 QoS1 下行：窗口未满（dispatched < recoverWindow=16）→ 直接入连接队列 + dispatched++；
  窗口满 → 进会话下行队列尾部（≤64 丢最旧语义不变）。
- PUBACK：dispatched-- 并从队列补发下一条（滑动）。
- QoS1 消息在 PUBACK 前保留（重发面）；QoS0 不占窗口（现状）。

## US-3 重连重发（DUP）
持久会话（Clean Start=0 未过期）重连恢复时：在途（已下发未 PUBACK）条目以 DUP=1 重新下发
（pktID 重新分配），随后按窗口滑动补发队列剩余条目。3.1.1 CleanSession=false 仍不支持（冻结）。

## US-4 订阅选项 NoLocal / RAP（v5 选项字节）
SUBSCRIBE v5 的 Subscription Options 字节：bit2=NoLocal、bit3=RAP、bit4-5=Retain Handling
（0/1/2 之外拒绝；本仓 sim 无 retain 面故仅存储登记）、bit6-7 保留（非 0 拒绝 ErrMalformed）。
- NoLocal=1：该订阅条目不接收**发布者自身**发出的匹配消息（他人正常）。
- RAP=1：转发时保留消息原 QoS（cap 到 granted）；RAP=0：按 granted 发送。
- 选项仅 v5 订阅生效；v3.1.1 QoS 字节解析不变。

## US-5 Topic Alias 入站（v5 PUBLISH 属性 0x23）
- codec：propsV5 白名单加 0x23 (u16)；Publish.TopicAlias 字段（0=不携带）。
- client 出站 opt-in：Options.PublishTopicAlias=true 时同主题第二包起仅发 alias（首包建映射）。
- broker：per-session alias→topic 映射（≤16，超限断开）；alias=0 或未建立即用 → 断开该连接
  （DISCONNECT 0x94 形态，登记简化）；断连后映射随会话保留/销毁语义走。

## US-6 client API（不破坏既有签名）
- SubscribeWithOpts(filter, SubOpts{QoS, NoLocal, RAP, RH}, handler)；既有 Subscribe 签名/行为冻结。
- Options.PublishTopicAlias（默认 false）；TopicAlias 上限常量导出登记。

## 边界登记（as-built 承诺）
1. 周期定时重发不做（sim 连接可靠；重发仅触发于重连恢复）。
2. client 出站 QoS1 断线重发不做（qos2 持久化语义外延，登记阶段四候选）。
3. Topic Alias 出站方向（server→client 分配）不做。
4. RH 语义仅校验+存储（无 retain 转发面）。
5. broker alias 违规断开采用 DISCONNECT 0x94 + 关连接（无等待）。
6. 请求 QoS2 授予 1（登记，不回 0x9B）。

## 验收锚
codec 3（alias 属性往返/选项字节+保留位拒绝/alias 0 拒绝）+ broker 9（授予 cap/端到端
QoS1 闭环/窗口背压/重连重发 DUP/NoLocal/RAP/alias 映射路由/alias 未知断开/v3 冻结锚）+
client e2e 2（SubscribeWithOpts/alias 出站两包）。v0320 回归锚（慢消费者死锁、OfflineQoS1Cap）
与 v0240–v0310 全量零改动绿。

## as-built 登记（实现后核实，2026-09-10）

- **US-1** ✅：SUBSCRIBE granted=min(req,1)（serve 普通订阅分支）；v3 分支 codes=req QoS 原样（冻结）。SUBACK 逐订阅 granted。
- **US-2** ✅：fanoutBytes(publisher, topic, data, qos)——v5 granted≥1 且入站 qos≥1 走 QoS1 流（append offline + 窗口未满直发）；QoS0/3.1.1/共享订阅维持 inlineEnqueue QoS0。PUBACK 无条件窗口释放（在途条被 64 上限挤掉的防御）。
- **US-3** ✅（复核 P1-1 修正）：恢复批次按断连时刻真实在途数标注——前 dispatched 条 DUP=1 重发，离线暂存条目（append 不动 dispatched、从未下发）DUP=0 首传；enqueueQoS 补 dup 参数并按 m.Dup 传递。回归锚：TestV0330ReconnectReplayDUP（在途→1）+ TestV0330OfflineBacklogFirstDeliveryDUP0（暂存→0）。
- **US-4** ✅：subOptsByte 编码（QoS|0x04|0x08|RH<<4）；permissive 解析拆解（NoLocal/RAP 透传、保留位/RH>2 拒绝）；v3 字节不变。fanout NoLocal：全部命中条目均 NoLocal 且发布者为自己才跳过；RAP 存储（granted cap 1 下与按 granted 同值，登记）。
- **US-5** ✅：propsV5+0x23（重复位标 bit2）；Publish.TopicAlias；alias-only 帧（Topic=""）codec 放行（decodePublish 延后校验、encodeUA 豁免）；serve 解映射/建表（≤16）+ 未知/超限 DISCONNECT 0x94 断开；aliases 随会话保留。alias=0 与未携带等同（登记）。
- **US-6** ✅：SubOpts/SubscribeWithOpts（旧 Subscribe 委托）；Options.PublishTopicAlias（默认 false、表 ≤16 降级）。
- **存量修复**：decodeSubscribe 补 v5 propsLen 读取（阶段一起 encode/decode 不对称，通用路径修复）；decodePermissiveSubscribe 补选项字节拆解。
- **测试锚**（复核 P1-2 补齐后）：12 例——codec/broker 9（授予 cap+v3 冻结锚、QoS1 e2e、窗口背压、重连重发 DUP、离线暂存 DUP=0、NoLocal、alias 映射、alias 超限、alias-only 往返、0x23 作用域拒绝、$share+NoLocal 拒绝）+ client e2e 2（PublishTopicAlias 端到端、SubscribeWithOpts NoLocal）+ propsLen 修复锚 1；v0320 回归零改动绿；-race count=2 绿。

## 复核处置记录（v0330 复核轮，2026-09-10）

- **P0-1 → 已修复**：aliases 纳入 sess.mu（原 serve-goroutine confined 论证不成立：shutdown 只等 pumpDone 不等旧 serve 退出，接管时新旧 serve 短暂并发；DISCONNECT 入队移出锁外维持锁序纪律）。
- **P1-1 → 已修复**：恢复批次改回精确 inflight 标注（前 dispatched 条 DUP=1；离线暂存条目 DUP=0）。补充回归锚 TestV0330OfflineBacklogFirstDeliveryDUP0。四处错误文本（注释/本 spec/RELEASE-NOTES/KI）同步更正。
- **P1-2 → 已修复**：补 client e2e ×2（TestV0330ClientPublishTopicAliasE2E、TestV0330ClientSubscribeOptsE2E）+ TestV0330SubscribeV5PropsLenDecode（decodeSubscribe propsLen 修复锚）；测试计数更正为 12 例。
- **P2 → 处置**：①0x23 属性作用域——CONNECT/CONNACK/SUBACK 属性区携带即拒绝（已修，TestV0330TopicAliasPropScopeRejected 锚）；②$share+NoLocal → 0x80 拒绝（已修，锚测试）；③CONNACK 无 Topic Alias Maximum/client 不读服务端上限——登记阶段四（真实 broker 互通风险，KI §34）；④跨连接残留 alias 映射——登记（会话语义固有；client 重连首包带主题覆盖）；⑤"RH 校验+存储"更正为"校验+丢弃"（无 retain 面）；⑥"QoS2 下行仅 QoS1 触发"文本失准更正（实现按 min(req,1) 语义与发布 QoS 无关地作用于 granted 流）；⑦注释错误两处随手修。
