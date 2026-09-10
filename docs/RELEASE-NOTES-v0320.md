# RELEASE-NOTES v0.32.0 — MQTT 5.0 阶段二：会话解耦与共享订阅

发布日期：2026-09-10 ｜ 上一版本：v0.31.0 ｜ spec：specs/0005-mqtt5-phase2-session-sharesub/spec.md（FR-S2-07）

## 亮点

- **会话解耦（v5）**：会话从网络连接解耦——Clean Start=0 + Session Expiry Interval 下，断连后订阅表与离线 QoS1 消息按 ClientID 保留，重连 CONNACK 回 Session Present=1 并恢复下发；SE 到期惰性清理；SE=0 断连即毁（现状默认）。client 侧 `Options.PersistentSession`/`SessionExpiryMs` opt-in、`Client.SessionPresent()` 透出。
- **共享订阅（$share/{group}/{filter}）**：broker 组内 round-robin 负载均衡（组键 = ShareName + 内层 filter，每条消息恰一组一人收到）；client 侧 handler 以内层 filter 注册、$share 形态透传；非法形态（空组/无内层）与 $queue/ 显式拒绝（SUBACK 0x80）。
- **接管仲裁**：同 ClientID 持久连接重入时旧连接被踢下线、会话转接（spec 0005 US-3）；clean 连接间并存行为保持 v0.24.0 以来现状（冻结兼容）。
- **codec 属性区通用化**：v5 属性区从"单条 Receive Maximum"扩展为白名单通用解析（0x21 RM + 0x11 Session Expiry，任意组合顺序，长度自洽校验）；未知属性仍拒绝；CONNACK 支持 SE 回显。

## 变更清单

- pkg/mqtt/packet.go：propsV5 白名单结构（RM/SE）+ decodeProps 通用解析 + encodeConnectProps 双属性编码；Connect.SessionExpiry / Connack.SessionExpiry 字段；decoder.readUint32/consumed、encoder.writeVBI 辅助。
- pkg/mqtt/client.go：Options.PersistentSession + SessionExpiryMs（Dial 构造 CleanStart=0 + SE 属性）；SessionPresent()；恢复会话离线消息"先于 handler 注册"场景的 pendingRecovered 缓冲补投（至多一次，登记 as-built）；$share handler 内层注册；下行确认报文 v5 形态标注（Puback/Pubrec/Disconnect）。
- pkg/mqttsim/sim.go：simSession 会话对象（订阅表快照 + 离线 QoS1 队列 ≤64 + 过期时刻）+ sessions 表；CONNECT 会话建立/恢复/重建 + CONNACK SessionPresent/SE 回显；断连保留/销毁；离线 QoS1 暂存（超限丢最旧）+ 重连恢复下发 + PUBACK 出队；$share 组路由 round-robin（确定性排序）+ SUBSCRIBE 形态校验；hasSubscriber 计入共享订阅与离线保留会话（PUBACK 0x10 判定）。
- specs/0005：SDD 规格（US-1..US-5）+ as-built 登记见下。

## 测试与质量

- 新增 15 例：pkg/mqtt 4 例（SE 往返/属性顺序/白名单拒绝/CONNACK 回显）+ pkg/mqttsim 9 例（会话保留/CleanStart 重建/SE=0 即毁/惰性过期/接管踢旧/离线上限丢旧/$share 轮转均匀/非法形态拒绝/离线成员不暂存）+ client e2e 2 例（持久会话全链路含离线消息补投/$share 双客户端轮转）。
- -race 全绿；v0240–v0300 全量回归净（3.1.1 路径整体退出会话状态机，冻结面归零）；契约 42 端点不变；零第三方依赖。

## 边界与升级兼容（详见 KNOWN-ISSUES §33）

- 3.1.1 CleanSession=false 持久会话不支持（保持现状断连即毁，as-built）——仅 v5 会话解耦生效。
- 下行 QoS1 无重发状态机（PUBACK 未达不重推、恢复下发 dup=0）——broker 完整 QoS1 下行属阶段三。
- 共享订阅离线成员不参与轮转、不暂存；下行 QoS 恒 0（恢复下发除外）。
- 升级兼容：默认参数（无 PersistentSession、无 $share）行为与 v0.31.0 逐字节一致；云端契约 42 端点零改动。
