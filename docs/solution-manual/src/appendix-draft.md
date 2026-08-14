# 附录 A 常见客户问题 FAQ

> 以下问答全部基于 EdgeFlow v0.1.0（2026-08-14 发布）已核验实现作答；涉及规划中能力均明确标注。

**A1. 云边断网期间，采集到的数据会丢吗？**

断网期间边缘本地采集不受影响：设备 → Mapper → 设备影子的链路完全在边缘本地完成，影子始终保留最新状态快照。但平台不保留历史遥测样本，断网期间产生的中间样本不会回补，重连后通过周期上报（默认 30s，可配 1s–10min）补报**最新状态快照**。如业务需要完整历史序列，需在 EventBus 侧或应用侧自行订阅持久化（v0.1.0 未内置遥测历史存储）。

**A2. 弱网下发给设备的指令会丢吗？**

不会静默丢失。下行指令走可靠投递：应用层 Ack 确认、同一命令 ID 自动重试（5s 超时 × 最多 3 次）、边缘侧幂等去重（缓存 1000 条 FIFO），通道恢复后重发指令仍可正确投递且不重复执行；超时最终返回 504 便于调用方感知。

**A3. 数据是否可追溯？**

可追溯。三类台账覆盖全链路：审计台账（云端 API 操作，含 401 拒绝记录，JSONL）、ops-ledger（keadm 升级/回滚，含备份 id 与失败原因）、op-ledger（设备级采集/下发流水，SQLite，30 天保留）。统一口径：时间戳（毫秒/RFC3339）+ 设备标识（nodeID/deviceName/namespace）+ 同步状态（result/status）。

**A4. 支持哪些设备协议？**

v0.1.0 支持 MQTT（QoS1，遥测/指令主题）与 Modbus TCP（显式设置 EDGEFLOW_MODBUS_ADDR 启用，读写保持寄存器/线圈，写后回读验证）；另提供 mock_sensor 模拟器用于演示联调。OPC-UA 接入规划中/即将上线。

**A5. 能管理多少边缘节点？**

支持多节点统一纳管。性能基线上 10 节点并发注册实测 100% 成功（平均 201.3ms）；更大规模请以实际环境压测为准。

**A6. 平台安全机制有哪些？**

生产加固四项：mTLS 云边通道（启用后自动升级 wss）、接入 Token 鉴权（默认关闭、开启后 401 fail-fast）、审计台账（文件 0600/目录 0700，认证失败也记录）、镜像多架构与 keadm 升级备份校验。完整 RBAC（当前为单 Token 鉴权）与镜像安全扫描规划中/即将上线。

**A7. 模型如何部署到边缘？**

v0.1.0 以"容器镜像 + 配置"为载体：模型与推理运行时打包为版本化镜像 → podsync 下发 Pod 声明（image + replicas）→ 边缘 Edged 拉取运行并持续自治（多副本/健康自愈/防漂移）；推理参数经 config-sync 下发并落盘。模型仓库/版本管理/灰度发布平台规划中/即将上线，当前以镜像 Tag + 配置参数化作为过渡路径。

**A8. 升级和回滚有保障吗？**

有。keadm upgrade 自动备份（manifest.json + sha256 校验）→ staging 原子替换 → 支持 --simulate-failure 演练；失败自动回滚，全程 ops-ledger 留痕（operator/from/to/result/note）。配合镜像 Tag 切换可完成应用级升级与回滚。

**A9. 边缘重启或云端重启会怎样？**

边缘重启：期望状态（节点元数据/Pod/配置）已落盘 MetaManager SQLite（WAL），启动后自动恢复并按声明调谐。云端重启：云端状态为内存态，边缘感知断线（读超时约 90s）后退避重连并自动重新注册（首条消息为 Register），随后周期上报自动重建节点/设备/Pod 状态，无需人工介入（E2E 已实测）。

**A10. 设备离线如何感知？**

v0.1.0 提供**节点级**离线判定（CloudHub 90s 无消息断开 + NodeController 心跳停滞 180s 双路径），状态呈现 Ready/Unknown/Offline；**设备级**离线检测规划中/即将上线，当前 Mapper 采集失败仅记 Warn 并落 op-ledger（result=error），可通过台账查询错误历史。

---

# 附录 B 术语表

| 术语 | 定义 |
|---|---|
| CloudCore | EdgeFlow 云端组件，承担节点/设备管控、REST API、审计台账与运行指标 |
| EdgeCore | EdgeFlow 边缘组件，由 EdgeHub/Edged/DeviceTwin/EventBus/Mapper/MetaManager 组成 |
| EdgeHub | 边缘云边通道客户端，负责注册、心跳、消息收发与幂等去重 |
| Edged | 边缘容器自治引擎，按声明式调谐（默认 5s）维护容器期望状态 |
| DeviceTwin | 设备影子，维护设备属性最新快照（内存态），支撑上报与指令闭环 |
| Mapper | 设备接入框架，提供 DeviceMapper 接口与注册路由，实现协议适配（MQTT/Modbus） |
| EventBus | 边缘 MQTT 数据面，承载遥测/指令主题（QoS1），broker 缺失时降级本地模式 |
| MetaManager | 边缘元数据管理，SQLite（WAL）落盘节点/Pod/配置，提供本地持久化 |
| 设备影子 | 设备属性在平台侧的期望/实际状态视图（desired/reported） |
| 声明式调谐 | 以期望状态为基准周期对账（reconcile），持续收敛实际状态 |
| 调谐周期 | 默认 5s，Edged 每次对账的时间间隔 |
| 镜像漂移 | 容器实际镜像与期望镜像不一致，触发自动重建还原 |
| CrashLoopBackOff | 连续重启 3 次进入 30s 退避窗口，稳定运行 60s 后重置计数 |
| 指数退避重连 | 断线后 1s 起步、逐次翻倍、封顶 60s 的自动重连策略 |
| 可靠投递 | 应用层 pending+Ack 确认、同 ID 重试（5s×3）、幂等去重（1000 条） |
| 幂等 | 同一命令/消息重复投递不产生重复副作用 |
| DeviceReport | 设备状态周期上报消息（30s，可配），无 Ack 单向流式 |
| podsync | 云端向边缘下发 Pod 声明（image/replicas）的 API |
| config-sync | 云端向边缘下发配置并持久化的 API |
| 节点离线 | 云端判定：CloudHub 90s 断开或 NodeController 180s 心跳停滞 |
| 设备离线 | 设备级离线检测（规划中/即将上线）；当前仅 op-ledger 记录采集错误 |
| 台账 | 追加式审计/操作记录（审计台账/ops-ledger/op-ledger 三类） |
| JSONL | JSON Lines，每行一条 JSON 记录的追加式日志格式 |
| mTLS | 双向 TLS，云边通道启用后自动升级 wss |
| QoS1 | MQTT 服务质量级别"至少一次"，不保证去重，消费方需幂等 |
| keadm | EdgeFlow 部署工具（init/join/upgrade/rollback/ops-ledger 等） |
| ops-ledger | keadm 升级/回滚操作台账（JSONL，含备份 id 与失败原因） |
| op-ledger | 设备操作台账（SQLite 追加流水，30 天保留） |
| 审计台账 | 云端 API 操作台账（audit-ledger.jsonl，含 401 记录） |
| 状态快照周期补报 | 弱网恢复后按周期补报最新状态快照（非历史回补） |
