# EdgeFlow v0.33.0 发布说明 — MQTT 5.0 阶段三：QoS1 可靠下行 + 订阅选项 + Topic Alias

发布日期：2026-09-10 ｜ 上游规格：specs/0006-mqtt5-phase3-qos1-reliability/spec.md（FR-S2-07 阶段三段）

## 亮点

本版本补齐 MQTT 5.0 下行链路的可靠传输语义，v5 订阅者获得端到端 QoS1：broker 按订阅授予发送、
确认出队、断连重发；同时交付 NoLocal 订阅选项与入站 Topic Alias（重复主题带宽压缩）。
3.1.1 路径继续逐字节冻结（订阅选项/属性/alias/QoS1 下行全部 v5-only）。

### N1 下行 QoS 按订阅授予（v5）

- v5 SUBSCRIBE 授予 granted = min(req, 1)：请求 2 授予 1（不回 0x9B，spec 0006 边界登记）；
  SUBACK 逐订阅回 granted。
- fanout 按成员订阅条目的 granted 发送：granted=0 维持 QoS0 best-effort；granted≥1 走 QoS1。
- v3.1.1 订阅/下行行为零变化（冻结锚测试断言）。

### N2 PUBACK 确认与 inflight 窗口（会话级）

- 统一 v0.32.0 恢复下发窗口模型：会话下行队列头部 dispatched 条 = 在途（窗口 16 =
  outQueueSize/2，防连接队列溢出）；窗口满后新消息积压队列尾部（≤64 丢最旧）。
- PUBACK 出队并滑动补发下一条；在途条被 64 上限挤掉时窗口仍正确释放（防御）。

### N3 重连重发（DUP 精确标注）

- 持久会话重连：断连前真实在途（已入连接队列未 PUBACK）条目以 DUP=1 重发；
  离线期间暂存的条目（从未下发）以 DUP=0 首次下发（复核 P1-1 修正——
  暂存 append 不动 dispatched，不适用在途重发语义）。
- 窗口后的积压条目经后续 PUBACK 滑动补发（DUP=0 首次下发）。

### N4 订阅选项 NoLocal / RAP（v5）

- NoLocal=1：该订阅条目不接收发布者自身的匹配消息（全部命中条目均 NoLocal 才跳过，逐条目语义）。
- RAP=1：保留消息原 QoS（cap 到 granted；granted cap 1 下与按 granted 同值，存储登记）。
- RH 仅校验（0/1/2 之外拒绝）+ 存储（本仓 sim 无 retain 转发面）。
- 选项字节 bit6-7 保留位非 0 拒绝（ErrMalformed）。

### N5 Topic Alias 入站（v5 属性 0x23）

- codec 属性白名单 +Topic Alias (u16)，重复/未知属性拒绝（与阶段一/二语义一致）；
  0x23 为 PUBLISH 专用——CONNECT/CONNACK/SUBACK 属性区携带即拒绝（复核 P2 修复）。
- client 出站 opt-in（Options.PublishTopicAlias）：同主题第二包起仅携带 16 位别名
  （alias-only 帧 Topic="" + alias），表 ≤16 表满降级全主题帧。
- broker per-session 映射（≤16）：alias-only 解映射路由；未建立即用 / 新键超限 →
  DISCONNECT 0x94 断开；映射随会话保留/销毁语义走。

### 存量缺陷修复（本轮门禁/测试暴露）

1. codec decodeSubscribe 从不读取 v5 属性长度字节（阶段一起 encode/decode 不对称）——
   经通用 codec 解析 v5 SUBSCRIBE 必失败；sim broker 走 permissive 手工解析未暴露。
   本轮补齐（decodeSubscribe + v5 参数，与 encode 对称）。
2. sim permissive SUBSCRIBE 解析把 v5 选项字节直接当 QoS 校验（NoLocal=0x04 > 2 → 断连）——
   本轮拆解选项字节（NoLocal/RAP 透传、保留位拒绝）。
3. 恢复下发路径 enqueueQoS 重建 Publish 结构体丢失 Dup 字段——补 dup 参数。

## 变更清单

| 包 | 变更 |
|---|---|
| pkg/mqtt | packet.go：propsV5+0x23、decodeProps 重复属性位标补齐（v0320 遗漏）、decodeSubscribe v5 化、Publish.TopicAlias、Suback 注释；message.go：TopicFilter 订阅选项字段、Publish.TopicAlias、MQTTV5TopicAliasInvalid(0x94)；client.go：SubOpts/SubscribeWithOpts、Options.PublishTopicAlias、出站 alias 表 |
| pkg/mqttsim | sim.go：filters granted 化 + subOpts、simSession.aliases、SUBSCRIBE 授予 cap、fanoutBytes publisher 参数 + granted-QoS1 下行流（NoLocal/RAP）、恢复批次 DUP=1、enqueueQoS dup 参数、PUBACK 窗口释放防御、alias 解映射/超限断开 |
| specs/0006 | 新规格（US-1..US-6 + as-built 登记八条） |
| docs | RELEASE-NOTES/README/Chart 0.32.0→0.33.0/DEVELOPMENT-SPEC 缺口表/ROADMAP §29/KNOWN-ISSUES §34 |

## 测试

- 新增 12 例：codec/broker 9（SUBSCRIBE v5 选项字节+propsLen 修复锚、alias-only 帧往返、
  授予 cap+v3 冻结锚、QoS1 端到端、窗口背压、重连重发 DUP、离线暂存 DUP=0、NoLocal、
  alias 映射路由、alias 超限断开、0x23 作用域拒绝、$share+NoLocal 拒绝）+ client e2e 2
  （PublishTopicAlias 端到端、SubscribeWithOpts NoLocal）。`-race -count=2` 绿。
- 回归：v0240–v0320 全部零改动绿；契约 42 端点零改动；e2e 包全量绿。

## 升级兼容

默认参数行为与 v0.32.0 逐字节一致（PersistentSession/PublishTopicAlias/NoLocal 均默认关闭）；
3.1.1 连接行为零变化。阶段边界（周期定时重发、client 出站断线重发、alias 出站方向、
RH 行为、共享订阅 granted-QoS1）见 KNOWN-ISSUES §34 与 spec 0006 边界登记。
