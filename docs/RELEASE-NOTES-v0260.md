# EdgeFlow v0.26.0 Release Notes —— MQTT QoS2 ＋ client mTLS ＋ mapper 配置文件化

- 日期：2026-08-31
- 基线：v0.25.0（5e0c732）
- 主题：MQTT QoS2（EXACTLY ONCE 完整握手）＋ client mTLS（双向认证）＋ mapper 配置文件化
- 兼容性：HTTP 端点保持 42；go.mod 零新依赖（YAML/JSON 均手写极简 parser）；默认行为与 v0.25.0 逐字兼容（QoS2 与 mTLS 均 opt-in）；既有测试零改动全绿（含冻结 v0240_sim_test.go）

## 1. MQTT QoS2：EXACTLY ONCE 完整握手（opt-in 门控）

### 1.1 codec 层（pkg/mqtt）

- `message.go`：补齐 PacketType 常量 `PUBREC=5` / `PUBREL=6` / `PUBCOMP=7`；新增 `Pubrec` / `Pubrel` / `Pubcomp` 三类型（可变头 PacketID uint16，无 payload）。
- `packet.go`：
  - `fixedByte1` 零 flags 列表纳入三类型；`PUBREL` 按规范特例 flags=0x02，PUBREC/PUBCOMP flags=0；
  - 三类型 encodeUA（writeU16）与 decodePacket 分支（flags 不符 → ErrMalformed）；
  - `decodePubRelID` 等解码函数沿用 decodePuback 模式（readUint16 + remaining() 校验，尾随字节拒绝）。
- QoS1/QoS0 编码路径零改动。

### 1.2 client 层（pkg/mqtt/client.go）

- **门控**：新增 `Options.EnableQoS2`（默认 false）。关闭时 `Publish(qos=2)` 仍按 v0.24.0 语义拒绝（`mqtt: invalid QoS level`），既有行为逐字一致；开启后 QoS2 上行走完整四次握手。
- **上行状态机**（复用 pendingAcks 通道机制）：PUBLISH → 等 PUBREC → 发 PUBREL → 等 PUBCOMP；任一步超时（`ackTimeout`）或收到非预期类型即报错返回。exactly-once 语义由「消息在 PUBCOMP 确认后才算投递完成」保证。
- **下行处理**（readPump）：收到 QoS2 PUBLISH → 回 PUBREC 并将消息暂存 `pendingDownQoS2`；收到 PUBREL → 从暂存取出分发 handler 并回 PUBCOMP（保证 handler 恰好执行一次）；PUBREC/PUBCOMP 走 resolveAck 唤醒上行等待者。

### 1.3 sim broker（pkg/mqttsim/sim.go）

- `Broker` 新增 `pendingQoS2` 暂存（锁保护，**按连接隔离**：外层 `*simClient`、内层 PacketID——MQTT PacketID 仅在单连接内唯一，避免多客户端并发同 id 串扰；独立复核 P1 项已修复，`unregister` 时清理该连接的全部 parked 交换）；serve() PUBLISH QoS2 分支：回 PUBREC + 暂存；新增 `case *mqtt.Pubrel`：取出消息 → recordPublish → fanout → 回 PUBCOMP。**消息在 PUBREL 确认后才投递**（broker 视角 exactly-once）。
- QoS0/QoS1 分发路径逐字未动；冻结测试 v0240_sim_test.go 零改动通过。

### 1.4 测试

- `pkg/mqtt/v0260_qos2_test.go` 五用例：完整握手成功、门控拒绝（默认关闭）、下行 QoS2 分发恰好一次、握手超时错误路径、codec roundtrip（含 PUBREL flags=0x02 字节级断言）。
- `pkg/mqttsim/v0260_qos2_test.go`：裸 socket QoS2 全程（PUBLISH→PUBREC→PUBREL→PUBCOMP，fanout 在 PUBREL 后）+ QoS1 回归 + **多客户端同 PacketID 并发隔离回归**（TestV0260SimQoS2PerConnIsolation，复核 P1 修复配套）。

## 2.1 独立复核结论（PASS-with-notes → P1 已修）

- 复核报告：`.cluster/edgeflow-v0260/review.md`——QoS2 编解码规范符合、冻结兼容逐项 ✓、锁保护完整无死锁、三包测试全绿。
- P1（sim pendingQoS2 全局键串扰）已修复并附回归测试；4 项 P2（inbound QoS2 不跨连接、出站超时重试可能重复、ackTimeout 包级 var 等）以文档边界形式登记于 KNOWN-ISSUES §26.2。

## 2. client mTLS：双向认证（opt-in）

| 层 | 变更 |
|---|---|
| mappers/mqtt（Mapper） | 新增 `EDGEFLOW_MQTT_TLS_CERT` / `EDGEFLOW_MQTT_TLS_KEY`（PEM 文件路径）；env 缺省时回退配置文件字段；两路径必须成对（只给其一 → connect() fail-fast 明确报错）；`tls.X509KeyPair` 加载失败 fail-fast。证书对注入 `cfg.Certificates` 后，客户端在 TLS 握手出示证书，由服务端（RequireAndVerifyClientCert）校验 |
| pkg/mqttsim（测试 broker） | 零新 API：`NewBrokerTLS` 本就接受完整 `tls.Config`，测试直接传 `ClientAuth: tls.RequireAndVerifyClientCert` + `ClientCAs` 构造严格 mTLS broker |
| 测试 | `v0260_mtls_test.go`：mTLS 全链路（订阅/上报在双向认证通道闭环）+ fail-fast 五例（cert 缺失/key 缺失/文件不存在/坏 PEM/不成对）+ 无证书客户端被拒 |

- 排障注记（教学价值）：测试 CA 模板若带 `ExtKeyUsage:[ServerAuth]`，Go x509 EKU 嵌套检查会要求 CA EKU 覆盖叶子用途，ClientAuth 叶子链将被服务器判定 `bad certificate`；表现为客户端侧 `read CONNACK: malformed packet: fixed header`（TLS alert 被解码层吞噬）。修复：CA 不设 EKU 约束 + 服务器用专用叶子（leaf+CA 链 PEM，X509KeyPair 支持多 CERTIFICATE 段）。

## 3. mapper 配置文件化（EDGEFLOW_MQTT_CONFIG）

- 新增 `mappers/mqtt/config.go`：`EDGEFLOW_MQTT_CONFIG` 指定 `.yaml` / `.yml` / `.json` 配置文件路径（扁平 key: value；手写极简 parser，零第三方依赖）。
- **优先级**：With 选项 > 环境变量 > 配置文件 > 内置默认——与既有 New() 回退链一致。
- **软失败**：文件不存在/解析失败 → `log.Errorf` 记录后忽略继续（与 mapper「告警不失败」哲学一致），不打断设备接入。
- `tls_insecure` 接受 "1"/"true"/"on"（大小写不敏感）；`EDGEFLOW_MQTT_TLS_CA` / `_TLS_CERT` / `_TLS_KEY` 均可写入配置文件。
- 测试：`v0260_config_test.go`（YAML/JSON 解析、优先级覆盖、坏文件软失败、tls_insecure 取值）。

## 4. 验证与门禁

- 五门禁：gofmt / go build ./... / go vet 全净；三包（mappers/mqtt、pkg/mqtt、pkg/mqttsim）`-count=1` 与 `-race` 全绿。
- 全仓 `go test ./...` EXIT=0 零失败（tests/e2e 351.167s ok）；契约测试 42 端点 `-count=1` ok 12.552s。
- 冻结测试（v0240_sim_test.go、v0250_* 全套）零改动零失败。
- 改动规模：5 文件修改（+285/-28）＋ 5 新增文件；零新依赖。

## 5. 已知边界（详见 KNOWN-ISSUES §26）

- client 无自动重连（历轮边界不变，由上层监管循环负责）；QoS2 重发不做消息级去重持久化（进程内 exactly-once）。
- mqttsim 的 mTLS 为测试定位；生产 broker（EMQX/Mosquitto）承载真实 CA 与认证体系。
- OPC-UA Basic256Sha256 加密通道未含，登记 ROADMAP §21 下轮候选。
