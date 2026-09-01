# EdgeFlow v0.27.0 发布说明

版本主题：**QoS2 会话恢复（in-flight 持久化）** —— 把 v0.26.0 的「进程内 exactly-once」推进到「跨重连/跨重启可恢复」，并附 MQTT 5.0 特性评估文档。

- 日期：2026-09-01
- 基线：v0.26.0（`41ccc6f`）
- 规模：3 新文件 + 4 修改（代码），1 新评估文档 + 5 文档批
- 硬约束遵守：零第三方依赖；HTTP 端点 42 不变；冻结测试（v0240/v0250/v0260 全套）零改动全绿；`PersistenceDir` 默认空 = v0.26.0 行为逐字一致

## 1. 新特性

### 1.1 client 端 QoS2 in-flight 持久化（pkg/mqtt）

- `Options.PersistenceDir`（string，默认空 = 禁用，v0.26.0 内存态行为逐字一致）。
- 落盘对象（JSON 单文件，`qos2-<4位hex PacketID>.json`，temp+rename 原子写）：
  - **上行**：PUBLISH 已发等 PUBREC（Phase 1）；PUBREL 已发等 PUBCOMP（Phase 2）。
  - **下行 parked**：已回 PUBREC 等 broker PUBREL 的消息（Kind 'i'）。
- 生命周期：PUBCOMP 收到即删；下行 PUBREL 完成投递即删；断连/崩溃**不删**（这正是恢复凭据）。
- 回放入口 `(*Client).Resume()`（新连接上调用，可选）：Phase 1 记录重发 `PUBLISH(Dup=1)` → PUBREC → PUBREL → PUBCOMP；Phase 2 直接重发 PUBREL；下行记录跳过（等 broker 的 PUBREL，若到达走 readPump 正常投递路径并清记录）。
- `ResumePending` / `ResumeComplete` 导出 API：供上层自定义恢复策略。
- 单条损坏记录：删除跳过，不阻塞其余恢复。
- 持久化写失败（上行 Publish 路径）fail-fast 返回错误；下行 park 路径软失败（协议应答不依赖磁盘健康）。

### 1.2 broker 端 QoS2 暂存持久化（pkg/mqttsim）

- `NewBrokerWithOptions(persistDir)` 新入口（`NewBroker` / `NewBrokerTLS` 签名与语义不变，冻结安全）。
- park 时落盘、PUBREL 完成投递后删记录；客户端断开/连接消失**不删**记录。
- **重启恢复**：新进程启动时把记录装载进孤儿表 `orphanQoS2`；重连后的 release leg（同 PacketID PUBREL）内存 miss 时回退查孤儿表，完成恰好一次投递并清记录。per-connection park 永远优先于孤儿表（v0.26.0 隔离语义不变）。
- `BrokerResumePending(dir)` 导出 API：列出可恢复消息（坏记录删除，client 侧外来记录跳过且保留——两端可能共目录，broker 无权删除对方凭据）。

### 1.3 MQTT 5.0 特性评估文档（docs/MQTT5-EVALUATION.md，仅评估）

- 七项 5.0 核心特性逐条价值判断（属性系统/原因码/会话过期/共享订阅/主题别名/流控/增强认证）。
- 架构适配点：指出「QoS2 按连接隔离 → 会话级隔离」是 5.0 化最大改造面。
- 结论：**v0.27.0 不实现 5.0**；分期草案（非承诺）：v0.29.0 版本参数化+原因码+流控 → v0.30.0 会话解耦+共享订阅。
- 规范依据：MQTT 5.0 OASIS 官方规范，未引用任何未核实第三方来源。

## 2. 测试与验证

- `pkg/mqtt/v0270_persistence_test.go`（7 例）：上行记录生命周期（PUBCOMP 清记录）、默认禁用零文件、Phase1 回放（Dup=1 重发+PUBREL）、Phase2 回放（仅 PUBREL 不重发 PUBLISH）、下行 park 持久化+release 清记录、混合恢复（下行保留/损坏删除/上行回放）、ResumePending/ResumeComplete API。
- `pkg/mqttsim/v0270_persistence_test.go`（4 例）：park 落盘+release 清记录、默认 broker 零文件、**重启恢复全链路**（crash → 记录存活 → 新 broker → PUBREL → 恰好一次投递 → 记录清除）、损坏/外来记录处理。
- 三包 `-race` 全绿；全仓 `go test ./...` EXIT=0；契约 42 端点不变。

## 3. 兼容性

| 场景 | v0.26.0 | v0.27.0 |
|---|---|---|
| `PersistenceDir` 空（默认） | 内存态 QoS2 | **逐字一致**（零文件、零 I/O、零行为差） |
| `PersistenceDir` 设置 | 不存在该选项 | 新增持久化+恢复能力 |
| `NewBroker()` / `NewBrokerTLS()` | 现有语义 | 签名不变、行为不变 |
| sim QoS0/QoS1 路径 | — | 零改动 |
| HTTP 端点 | 42 | 42 |

## 4. 已知边界（详见 KNOWN-ISSUES §27）

- MQTT 3.1.1 无跨连接会话：client 端下行 parked 记录若 broker 永不重发 PUBREL，则该交换无法完成（记录保留，不影响新交换）——协议边界，非实现缺陷。
- broker 重启恢复依赖「发送方以同 PacketID 重发 PUBREL」；sim 不做自动 fanout（重启后订阅关系已失）。
- 记录目录未做加密/加锁：与 v0.26.0 的内存态同级信任域（本机磁盘），多进程同目录并发写未支持。
