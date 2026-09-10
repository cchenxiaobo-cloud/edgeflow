# Release Notes — v0.30.0（2026-09-09）

**主题：MQTT 5.0 阶段一（协议版本参数化 + 原因码 + 流控）与登记项收口**——FR-S2-07 分期一落地（评估文档 docs/MQTT5-EVALUATION.md 分期建议一），同轮收口 OPC-UA 自动续期（§30）与 mappers race flake（§29/§30）。

开发规范：本版按《EdgeFlow 开发规范 v1.0.0》SDD 流程交付（spec-kit：specs/0003-mqtt5-phase1-autorenew/spec.md）。

---

## 1. 新特性

### 1.1 MQTT 5.0 阶段一（FR-S2-07 分期一，client 与 sim 成对交付）
- **协议版本参数化**（US-1）：`mqtt.Options.ProtocolVersion5`（opt-in，默认 false = 逐字节 3.1.1 冻结）；CONNECT 以级别 0x05 编码，sim 按级别字节自动分派（4=既有路径逐字不变，5=v5）；client 与 sim 必须同轮升级（评估文档 §4.1 判定成立）。
- **属性最小层**（US-2）：VBI 变长整数 + 属性 ID 分发；阶段一仅 Receive Maximum（0x21）载于 CONNECT/CONNACK；未知属性拒绝。
- **原因码**（US-3）：CONNACK v5（0x84/0x81/0x86/0x87/0x88 语义化透出）；PUBACK/PUBREC/PUBREL/PUBCOMP v5 追加 rc（2B 形态容忍 rc=0）；SUBACK v5 逐订阅码（0x8F 等）；DISCONNECT v5（0x93 流控违规）。QoS1 无匹配订阅者回 0x10（警告级，Publish 容忍返回 nil）。
- **Receive Maximum 双向流控**（US-4）：客户端出站 QoS1/2 以服务器 CONNACK 下发的 RM 为在途窗口（`flowSlots` 信号量，QoS0 不占槽，全部退出路径释放）；sim 服务端强制——上行 QoS2 暂存深度达 RM 即 DISCONNECT 0x93 断连；sim 下行仅 QoS0（无 QoS1/2 下行面，下行限制归阶段二）。
- **e2e**（US-5）：v5 全链路（RM 协商/QoS0-2/0x10 容忍/下行 v5 形态）、流控违规 0x93、鉴权失败码与 3.1.1 向下兼容。

### 1.2 OPC-UA 自动续期（§30 收口，US-6）
- `OpenSecureChannelOptions.AutoRenewRatio`（0=关闭保持 v0.29.0 冻结；(0,1] 合法）：令牌寿命 ratio 比例点自动 `Renew`；续期失败按寿命 1/10（下限 1s）退避重试；Close 收口循环；自动/显式 Renew 经 `renewMu` 互斥；仅 Basic256Sha256 通道启动。
- sim 新增 `WithTokenLifetime(ms)`（测试注入短寿命；默认 600000 不变）。

### 1.3 既有缺陷修复
- **mappers/opcua race flake（§29/§30 收口，US-7）**：（修复后回填根因与处置）
- **MQTT sim 双写者时序缺陷（本轮发现修复）**：PUBREC 走异步出站队列、关停报文由 serve 直写，两者可乱序——违背 MQTT 单字节流语义。修复 = 恢复单写者不变量（关停报文一律入队）+ 泵关停时先清空既有队列、shutdown 等泵退出后再关连接（鉴权 CONNACK/0x93 关停报文确定性上线）。

## 2. 兼容性

- **3.1.1 默认路径逐字节冻结**：V5=false 的 CONNECT/CONNACK/确认报文/PUBLISH 编码与 v0.29.0 完全一致（冻结锚单测钉住）；`DecodePacket` 导出语义不变（仍拒绝级别 5 CONNECT）；v0240–v0290 冻结测试零改动。
- v5 新行为全部经显式 opt-in；5.0 测试与冻结带物理隔离（v0300_* 命名）。
- 契约 42 端点不变；零第三方依赖。
- 公开 API 增量：`Options.ProtocolVersion5/ReceiveMax`、`DecodePacketV`、`NewBrokerWithConfig/BrokerConfig`、MQTT 5.0 报文 V5 字段与原因码常量、`OpenSecureChannelOptions.AutoRenewRatio`、`opcuasim.WithTokenLifetime`。

## 3. 质量证据

| 门禁 | 结果 |
|---|---|
| v0300 新测试 | 12 例全绿（pkg/mqtt 5：VBI/冻结锚/v5 CONNECT/CONNACK/确认码+PUBLISH/流控节流；pkg/mqttsim e2e 3：全链路/0x93/鉴权+向下兼容；pkg/opcua 4：自动续期 B256/关闭冻结/None noop/ratio 校验） |
| -race | pkg/mqtt、pkg/mqttsim、pkg/opcua、mappers/opcua 全绿（flake 修复后 count=30 稳定） |
| gofmt / build / vet | 净 |
| 全仓 go test ./... | EXIT=0（39 包 + e2e） |
| 契约 | 42 端点冻结不变 |

## 4. 边界与登记（KNOWN-ISSUES §31）

- PUBLISH/SUBSCRIBE v5 属性区仅接受空属性（User Properties/Subscription Identifier 拒绝）——阶段二扩展
- sim 上行流控执行点 = QoS2 暂存深度（QoS1 即时回执不构成在途积累）；下行 QoS1/2 推送面阶段二实现
- MQTT 5.0 会话解耦（Session Expiry/Clean Start）、共享订阅、mapper 配置项 → 阶段二（v0.31.0 草案，非承诺）
- OPC-UA 互操作向量比对仍 pending（§30 沿用）

## 5. 下一版本候选（v0.31.0+，非承诺）

- MQTT 5.0 阶段二：会话与连接解耦 + 共享订阅 + mapper 配置面
- OPC 基金会互操作向量比对（接真实第三方服务器前必须补）
- OPC-UA 令牌过期前平滑切换增强（自动续期失败通道级告警）
