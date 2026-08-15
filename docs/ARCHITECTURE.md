# EdgeFlow 架构设计文档（ARCHITECTURE）

> **WBS**：9.1 架构文档（设计文档、模块说明）
> **版本**：v1.0（已评审，2026-08-15 全面回写）
> **状态说明**：本文档描述 EdgeFlow **v0.1.0 实际实现的架构**（不再使用"目标架构/待开发"框架），并保留少量已显式决策的延后项（见 §12）。实现进度核对至 2026-08-15（仓库 head `f6b4898`），逐节与代码、docs/API-SPEC.md（v0.1.0 定稿）、docs/ROADMAP.md（§1.2 状态列）交叉核验。
> **阅读对象**：零基础用户（本人）与专业评审。每个章节先给"一句话版本"，再给细节；陌生术语附通俗解释。

---

## 评审记录（2026-08-15）

| 项 | 内容 |
|----|------|
| 评审日期 | 2026-08-15 |
| 评审人 | 收尾核对员（子代理），交叉核验视角 |
| 前置问题 | audit-m02 S11-S13：文档标注 v0.1 草案未评审；NATS→MQTT、Token→mTLS 未回写；实现进度滞后（原文档称"当前实现仅到 M0"） |
| 评审结论 | ✅ **通过**：全文档回写至 2026-08-15 代码现状；§4 协议由"草案"改为"已实现契约"；§6 安全按实际演进（M1-M3 无认证 → M4 完整 mTLS + Token 认证中间件默认 off + 7.3 设备认证）重写；残留缺口集中登记于 §12 |
| 与 ROADMAP 状态一致性声明 | 本文档各组件状态列与 ROADMAP §1.2（2026-08-14 audit-m02/audit-m35 回写 + 2026-08-15 收尾轮闭环）一致；数字口径（心跳 30s / CloudHub 失活 90s / NodeController 180s / 重连退避 1s→60s / 幂等缓存 1000 条 / 13 REST 端点 / 可靠投递 5s×最多 3 次尝试）与 docs/API-SPEC.md、代码常量逐项核对一致。ROADMAP §1.2 的 9.1 行"存在但未评审、内容滞后"标记随本文档本次评审闭环（该行状态列的更新超出本文档修改范围）。 |
| 历史版本 | v0.1（2026-08-13 草案）：本文档前身，NATS/Token/进度等断言已按实际情况修订或删除 |

---

## 1. 系统概述与设计目标

### 1.1 这是什么（一句话）

EdgeFlow 是一个**云边两级架构**的边缘计算平台：云端（CloudCore）负责统一管理，边缘（EdgeCore）负责就近处理，即使**断网也能继续干活**。

通俗类比：云端像"总部"，边缘节点像"分公司"。总部下发任务、收集汇报；分公司平时与总部通信，但**断网时分公司也能自己运转**，网络恢复后再向总部补报。

### 1.2 与 KubeEdge 的关系

EdgeFlow 借鉴 KubeEdge 的整体架构（CloudHub/EdgeHub 云边通信、Edged 边缘运行时、DeviceTwin 设备影子、MetaManager 本地存储等概念，参考 <https://kubeedge.io/docs/architecture/>），但**以本项目自身设计为准**，差异点：

| 维度 | KubeEdge | EdgeFlow（本项目 v0.1.0） |
|------|----------|---------------------------|
| 边缘消息总线 | MQTT Broker（mosquitto） | **MQTT（mosquitto broker + paho 客户端）**，仅作边缘设备数据面，不跨云边（WBS 3.6，commit `2a0d0a3`） |
| 云端元数据 | apiserver + etcd | **无真实 K8s 接入**：内存态 registry + REST API 适配（WBS 2.3/2.6，决策记录 R1） |
| 消息序列化 | Protobuf | **JSON（信封 + payload 全 JSON）**；gzip 已实现（WBS 4.4，2026-08-15，协商式兼容）；Protobuf 显式延后（决策记录 R9） |
| 安全演进 | 默认 mTLS | **M1-M3 通道无认证（历史事实）→ M4 完整 mTLS**；7.3 设备认证（Register.token）2026-08-15 闭环；API Token 认证中间件默认 off（决策记录 R6/R7） |

### 1.3 三大设计目标

| 目标 | 含义 | 对应模块（WBS） | 验收锚点（ROADMAP §5）与实际状态 |
|------|------|----------------|----------------------------------|
| **云边协同** | 云端统一管控：节点注册、应用下发、状态收集 | 2.3/2.4/2.1/3.1 | 节点注册后 `kubectl get nodes` Ready（**REST 化适配**：`GET /api/v1/edgenodes` Running/Offline 流转）；心跳 ≤30s ✅ |
| **边缘自治** | 断网期间边缘继续运行，恢复后自动同步 | 3.4、3.3 | 断网 30min 容器持续运行（**E2E 以 60s 短时窗口模拟验证**，30min 真实长跑未验证，见 §12）；恢复 120s 内同步 ✅（实测 cloudcore 重启后 8s 内重连注册） |
| **设备管理** | 设备接入、数字孪生（设备影子）、远程控制 | 5.x、2.2、3.5、3.6 | 端到端设备状态上报链路 ✅；**延迟 ≤5s 从未测量**（见 §12） |

### 1.4 总体设计原则

1. **云边同源**：云侧 API 形态对标 Kubernetes 生态（EdgeNode CRD 类型 + K8s List 风格响应），但 v0.1.0 未接入真实 apiserver，以内存 registry + REST 适配实现（决策记录 R1）。
2. **通道单一、总线解耦**：云边管理面只走一条 WebSocket 通道（CloudHub↔EdgeHub）；组件之间不直接跨进程调用。边缘设备数据面走 MQTT（EventBus），与云边管理面分离。
3. **零依赖起步，按需引入**：核心逻辑纯标准库（Go 1.26.2）；已引入的第三方依赖均经过评审（gorilla/websocket v1.5.3、eclipse/paho.mqtt.golang v1.5.1、modernc.org/sqlite 纯 Go 驱动、goburrow/modbus 等）。
4. **协议先行**：云边通信协议（WBS 4.1）是关键路径第一步，先定义后实现（ROADMAP §2.1 关键路径）。

---

## 2. 总体架构与分层

### 2.1 分层架构图（v0.1.0 实际架构）

四层：**云层 → 云边通道 → 边缘层 → 设备层**。

```
┌───────────────────────────────────────────────────────────────────────────────┐
│                               云层 Cloud Side                                  │
│   kubectl/curl ──► CloudCore（单进程）                                          │
│     ├─ HTTP :8080：/healthz · /metrics · 13 个 /api/v1/* 端点                   │
│     │     （Token 认证中间件，env 开关默认 off；审计中间件审计台账）                │
│     ├─ registry（内存节点注册表）──► EdgeNode CRD 视图（K8s List 风格）           │
│     ├─ NodeController（心跳超时扫描：30s 周期 / 180s 阈值）                      │
│     ├─ podstatus / devicestatus（内存态状态存储：Pod 状态、设备影子云端视图）      │
│     ├─ CloudHub（WebSocket 服务端 :10000，路径 /v1/edge）                       │
│     │     └─ 会话管理（同 nodeID 踢旧连接）· 心跳失活判定（90s）· 可靠投递        │
│     │        ReliableSend（QoS1：5s 超时 × 最多 3 次尝试，Ack 关联）              │
│     └─ audit（audit-ledger.jsonl 审计台账）· metrics（Prometheus 五指标）        │
│   （无 apiserver/etcd：内存态，重启清空，边缘重连后重新注册恢复）                  │
└─────────────────────────────────────┬──────────────────────────────────────────┘
                                      │  云边通道：WebSocket :10000（/v1/edge）
                                      │  M1-M3 无认证 → M4 起 mTLS 可选（wss://，
                                      │  env 开关，自动生成/加载证书）
                                      │  7.3（2026-08-15）：Register.token 校验
┌─────────────────────────────────────▼──────────────────────────────────────────┐
│                               边缘层 Edge Side                                  │
│   EdgeCore（单进程）                                                            │
│     ├─ EdgeHub（WS 客户端：注册/心跳 30s/重连退避 1s→60s/自动 Ack + 幂等去重）     │
│     ├─ MetaManager（SQLite WAL：KV/节点信息/Pod/配置落盘 + 增量订阅）             │
│     ├─ Edged（方案 A：DockerRuntime + Mock 双实现，声明式 reconcile，             │
│     │     多副本/健康自愈/CrashLoopBackOff/镜像漂移检测重建）                     │
│     ├─ DeviceTwin（设备影子：desired/reported 合并，字段级合并保 desired）        │
│     ├─ EventBus（paho MQTT 客户端：QoS1、自动重连、OnConnect 恢复订阅）           │
│     └─ Mapper（mock_sensor 内置 / modbus mapper + op_ledger 操作台账）           │
│   MQTT broker（mosquitto，可选，默认 tcp://127.0.0.1:1883）：设备数据面          │
│   边缘容器网络：Docker bridge（无 CNI/Flannel，决策记录 R2）                     │
└────────────────────────────────────────────────────────────────────────────────┘
        ▲ MQTT telemetry/command ｜ Modbus TCP/串口（Mapper 侧）
┌───────┴────────────────────────────────────────────────────────────────────────┐
│                               设备层 Device Side                                │
│   MQTT 设备（传感器等，直连 EventBus 主题）｜ Modbus 设备（经 modbus mapper）      │
└────────────────────────────────────────────────────────────────────────────────┘
```

**读图要点（新手向）**：

- **云侧一个进程**：CloudCore 同时承载 HTTP 管理 API 与 CloudHub（WS 服务端）。不直连 etcd/apiserver，节点/Pod/设备状态均为内存态（重启清空，边缘重连后自愈）。
- **边侧一个进程**：EdgeCore 内部多模块（EdgeHub/MetaManager/Edged/DeviceTwin/EventBus/Mapper）。所有"对外通信"分两条：**云边管理面**（EdgeHub ↔ CloudHub 的 WebSocket）与**设备数据面**（EventBus ↔ mosquitto，不出边缘）。
- **设备层**：真实设备通过 MQTT（遥测/指令主题）或 Modbus（mapper 适配）接入，Mapper 是协议适配的插件位。

### 2.2 组件职责表

> 状态列：✅ 已实现（括号内为实际完成里程碑，标注 ⚠️ 的为归属偏移项，见 §9 R6）｜ 🔒 已关闭（产品决策）｜ ⬜ 未实现（见 §12）

| 组件 | 所属侧 | 职责（一句话） | WBS | 状态 |
|------|--------|---------------|-----|------|
| cloudcore 进程（入口） | 云 | 云端程序入口：加载配置、初始化日志、装配 HTTP API + CloudHub + 控制器 | 1.1、1.5 | ✅ M0（commit `98a50a6` 起） |
| pkg 共享库（log/config/version/httpx） | 双 | 日志、配置加载、版本注入、HTTP 工具，全部组件复用 | 1.5 | ✅ M0 |
| pkg/certs | 双 | 纯标准库证书管理：CA/服务端/客户端证书幂等生成、LoadTLSConfig 双向强制、TLS1.2+、私钥 0600 | 7.1 | ✅ M4（commit `0a7fcc2`；轮换人工编排、吊销未实现） |
| **CloudHub** | 云 | WebSocket **服务端**（:10000/v1/edge）：会话管理（同 nodeID 踢旧连接）、注册、心跳失活判定（90s）、可靠投递 ReliableSend、mTLS Option、Register.token 校验 | 2.1、4.2、4.6、4.5、7.3 | ✅ M1 基础 + M4 TLS + 2026-08-15 token 校验 |
| **EdgeController**（registry + EdgeNode 映射） | 云 | 边缘节点**注册**（NodeInfo 注册表 + EdgeNode CRD 对象映射，`GET /api/v1/edgenodes`） | 2.3、2.6 | ✅ M1（REST 化适配，commit `3c7b99d`/`641863e`） |
| **NodeController** | 云 | 心跳监控、节点上线/下线判定（扫描 30s / 超时 180s，SIGSTOP 冻结→Offline→Ready 状态机闭环） | 2.4 | ✅ M4 ⚠️（commit `f71684e`，原计划 M1） |
| 云端元数据层 | 云 | **内存 registry + REST API**（无 apiserver/etcd，已文档化为适配决策） | 2.6 | ✅ M1（适配） |
| CloudCore API 层 | 云 | 面向管理员的 RESTful API（**13 个端点** + /healthz + /metrics，见 docs/API-SPEC.md）；Token 认证中间件 | 2.5、7.2 | ✅ M1 端点 + ✅ M4 认证中间件（默认 off，commit `4c5b9c6`） |
| DeviceController（云端设备状态） | 云 | 设备影子云端视图 + 查询/指令 API（内存态 devicestatus，字段级合并保 desired） | 2.2 | ✅ M3（commit `744afaa`；无 K8s 控制器，见 5.3） |
| NodeJob 任务管理 | 云 | 任务 CRD、任务分发与结果回收 | 2.8 | 🔒 **已关闭**（v0.1.0 范围外产品决策，协议占位标注"已关闭"，commit `4c5b9c6`） |
| 可观测性（云） | 云 | /metrics Prometheus 五指标（nodes/pods/devices_total、http_requests_total、active_connections） | 2.9、10.1 | ✅ M4（commit `4c5b9c6`，与 3.8/10.1 合并） |
| **EdgeHub** | 边 | WebSocket **客户端**：注册、心跳保活（30s）、断线重连（退避 1s→60s）、自动 Ack + 幂等去重（缓存 1000 条 FIFO）、wss 支持 | 3.1、4.2、4.6、4.5 | ✅ M1 基础 + M4 wss（commits `7b1c27a`/`19dd66f`/`0a7fcc2`） |
| **MetaManager** | 边 | 本地元数据存储（SQLite WAL）：KV/节点信息/Pod/ConfigMap/Secret 落盘、重启不丢、增量订阅 | 3.3、6.2 | ✅ M1/M2（commits `3aaaf28`/`089c358`/`5403daa`） |
| **Edged** | 边 | 轻量容器运行时管理：方案 A（DockerRuntime + Mock 双实现）、声明式 reconcile、多副本、健康自愈、CrashLoopBackOff、镜像漂移检测+重建 | 3.2、6.1、6.4、6.5 | ✅ M2（P0 决策定案：方案 A；containerd CRI 为 P2 延后） |
| **自治引擎** | 边 | 断网时容器持续运行、恢复后重连注册并恢复上报（周期上报驱动云端收敛；无独立待同步队列） | 3.4 | ✅ M2 基础（60s E2E 短时模拟验证；30min 真实长跑未验证） |
| **DeviceTwin** | 边 | 设备影子（desired/reported 双状态、字段级合并、深拷贝、自动创建） | 3.5 | ✅ M3（覆盖率 100%，commit `744afaa`） |
| **EventBus** | 边 | 设备消息总线：paho MQTT 客户端（QoS1、AutoReconnect、OnConnect 恢复订阅），连接 mosquitto | 3.6 | ✅ M3（commit `2a0d0a3`；NATS 方案放弃，决策记录 R4） |
| ServiceBus | 边 | 边缘服务发现与路由（云边 HTTP 调用，Phase 3） | 3.7 | ⬜ 未实现 |
| Mapper SDK / 框架 | 边 | 设备协议适配标准接口：DeviceMapper 接口 + MapperRegistry（注册/注销/启停幂等） | 5.1 | ✅ M3（commit `7d82c0c`，覆盖率 96.4%） |
| Modbus Mapper | 边 | Modbus 协议适配：自实现模拟器（modbussim）+ goburrow Mapper + op_ledger 操作台账 | 5.2 | ✅ M4 ⚠️（commit `a290686`，原计划 M3；OPC-UA 未做） |
| 可观测性（边） | 边 | 与 10.1 合并（云端 /metrics 暴露，边缘不独立暴露） | 3.8 | ✅ M4（合并交付） |
| 云边通信协议/连接管理 | 双 | 消息格式（JSON 信封）、类型枚举、心跳、重连退避、可靠投递 | 4.1-4.6 | ✅ M1（pkg/protocol + 各侧实现，详见 §4） |
| 安全（证书/mTLS/认证/审计） | 双 | CA 生成、双向 mTLS、API Token 认证中间件、设备认证（Register.token）、审计日志 | 7.1-7.5、4.5 | ✅ M4（7.1/7.4/4.5 mTLS + 7.2 Token 中间件 + 7.5 审计）；7.3 设备认证 ✅ 2026-08-15 闭环 |
| Helm Chart | 部署 | 云端组件一键部署（build/charts/edgeflow，helm lint 0 failed） | 8.5 | ✅ M4 |
| keadm | 部署 | 边缘节点安装/注册工具：init/join/reset/version + upgrade/rollback/ops-ledger + batch（2026-08-15） | 8.6、10.2 | ✅ M4 基础 ⚠️ + M5 升级回滚 + 2026-08-15 batch |
| Flannel/CNI | 边 | 边缘节点容器网络 | —（ROADMAP 缺口 6） | 🔒 关闭：**Docker bridge 方案**（决策记录 R2） |

### 2.3 基础设施选型

| 设施 | 选型 | 位置 | 说明 |
|------|------|------|------|
| 云端元数据存储 | **内存 registry**（无 etcd/apiserver） | 云 | CloudCore 状态为内存态，重启清空；对接 K8s apiserver 列入后续版本（API-SPEC §7） |
| 设备消息总线 | **MQTT：mosquitto（broker，可选）+ paho（客户端）** | 边 | 仅设备数据面，不出边缘；broker 缺席时设备链路降级本地模式（决策记录 R4） |
| 边缘本地存储 | SQLite（modernc 纯 Go 驱动，WAL） | 边 | MetaManager 持久化（KV/Pod/配置/op_ledger）；免 CGO，交叉编译友好 |
| 边缘容器运行时 | **Docker（方案 A 定案）** | 边 | DockerRuntime + Mock 双实现；containerd CRI 为 P2 延后（决策记录 R3） |
| 云边传输 | WebSocket :10000（/v1/edge） | 云↔边 | 边缘主动拨号；M4 起 mTLS 可选（env 开关，ws:// 自动归一化 wss://） |
| 部署 | Helm（云侧）+ keadm（边侧） | — | 分别对应 WBS 8.5、8.6；发布制品见 §10 |

---

## 3. 核心数据流

### 3.1 下行：云 → 边（PodSync/ConfigSync）

```
用户 curl POST /api/v1/nodes/{nodeID}/podsync（或 config-sync）
   │  （操作：add/update/delete；五态响应 200/400/404/502/504）
   ▼
CloudHub ReliableSend（可靠投递：5s 超时 × 最多 3 次尝试，重发保持同 msg.ID）
   │  WebSocket 通道（:10000）
   ▼
EdgeHub（自动 Ack + 幂等去重：成功处理入 1000 条 FIFO 缓存）
   │
   ▼
MetaManager（先落盘 SQLite：pods/<ns>/<name>、configs/<ns>/<name>，再触发）
   │  增量订阅（Subscribe/Event，缓冲满丢弃，声明式收敛兜底）
   ▼
Edged（声明式 reconcile：创建/更新/删除 Docker 容器，命名 edgeflow-<ns>-<name>-<index>）
```

要点：
- **MetaManager 先落盘再执行**：即使 Edged 还没就绪，数据也不会丢；重启后从 SQLite 恢复。
- **可靠投递语义**：200 = 边缘已确认（Ack ok）；404 = 节点离线（不重试）；502 = 边缘回 error Ack（重试无意义）；504 = 确认超时重试耗尽（可重试，边缘有幂等去重）。详见 §4.5。

### 3.2 上行：边 → 云（状态上报/设备遥测）

**Pod 状态**（周期 30s，env 可配）：

```
Edged 调谐结果 → EdgeHub（PodStatus 消息）→ CloudHub → podstatus 存储
   → GET /api/v1/pods（phase: Running/Stopped/Absent/Error/Unknown；
      Absent 终态保留 90s，云端 Absent→Delete 收敛）
```

**设备数据**：

```
Mapper 采集（mock_sensor 2s 采样 / modbus mapper 轮询）
   → DeviceTwin 影子合并（UpsertReported 字段级合并，reported 落影子）
   → 周期上报（DeviceReport，env 可配，默认 30s）
   → CloudHub → devicestatus 存储（字段级合并保 desired——设备上报不覆盖云端期望值）
   → GET /api/v1/devices（properties + desired 双视图）
```

**设备指令（下行设备面）**：

```
POST /api/v1/nodes/{nodeID}/device-command（deviceName/property/value）
   → ReliableSend DeviceCommand → EdgeHub → mapperCommandExecutor → Mapper 执行
   → 结果快照写回 Twin → 随周期上报云端（云端 desired 已同步写入）
```

### 3.3 MQTT 设备数据面（设备 ↔ EventBus，不出边缘）

```
MQTT 设备 ── publish devices/<ns>/<deviceName>/telemetry ──► mosquitto
                                                            │
                                    EventBus（paho 订阅，QoS1）──► Mapper 消费
Mapper ── publish devices/<ns>/<deviceName>/command ──► mosquitto ──► MQTT 设备执行
```

- 主题约定：`devices/<namespace>/<deviceName>/telemetry`（遥测）、`devices/<namespace>/<deviceName>/command`（指令）、`edgeflow/<module>/<event>`（边缘内部模块事件，预留）。
- 可靠性：Publish/Subscribe 默认 **QoS 1**（至少一次，不保证去重，消费方需幂等）；paho 自动重连（ConnectRetry 1s），断线期间订阅关系在重连成功后自动恢复（不依赖 broker 会话持久化）。
- 降级路径：broker 缺席（如未装 mosquitto）→ Warn + 本地模式（mapper 直连本地回环），主链路不受影响。

### 3.4 自治模式：断网期间

```
断网事件
   │
   ▼
EdgeHub 检测连接断开（云端失活判定 90s 是云端侧视角；边缘侧立即进入重连退避）
   │
   ├─► Edged 继续按 SQLite 中的元数据维持容器运行（本地调谐不受影响）
   ├─► 设备采集照常：Mapper → DeviceTwin（本地影子继续更新）
   └─► 状态变更记入本地（SQLite 落盘）；无独立"待同步队列"——
        恢复后靠周期全量上报（PodStatus/DeviceReport）驱动云端收敛
   │
网络恢复
   │
   ▼
EdgeHub 重连成功（指数退避 1s/2s/4s/…上限 60s，注册成功后重置）
   → 重新 Register（+token 校验）→ 周期上报恢复 → 云端状态收敛
```

验收锚点（ROADMAP §5 WBS 3.4）：断网 30min 容器持续运行（E2E 以 **60s 短时窗口**验证同语义，见 `tests/e2e/autonomy_test.go`；30min 真实长跑未验证，见 §12）；恢复后 120s 内同步（实测 cloudcore 重启后 8s 内重连注册并恢复上报）。

---

## 4. 云边通信协议（v1，已实现）

> 本节为 **WBS 4.1 交付物**：v0.1.0 已按此契约实现并双向联调通过（`pkg/protocol` + `cloud/pkg/cloudhub` + `edge/pkg/edgehub`）。消息类型与字段矩阵另见 docs/API-COMPATIBILITY.md §2。

### 4.1 传输通道

- 传输层：WebSocket，端口 **10000**，路径 `/v1/edge`（云边唯一管理面通道；与 HTTP 管理端口 8080 分离）。
- 方向：边缘主动发起连接（EdgeHub 客户端 → CloudHub 服务端），云端不反向拨号——边缘常处于 NAT 之后，只能主动出网（与 KubeEdge 一致）。
- 心跳：EdgeHub 每 **30s** 发 Heartbeat，云回 HeartbeatAck（`edge/pkg/edgehub` `DefaultHeartbeatInterval`）。
- 断线判定（两级）：
  - **CloudHub 连接级**：超过 **90s** 未收到任何消息即断开连接（`cloud/pkg/cloudhub` `HeartbeatTimeout`，监控扫描周期 = timeout/3）。
  - **NodeController 节点级**：心跳超时阈值默认 **180s**（约 6 个心跳周期）、扫描周期 30s（`cloud/pkg/nodecontroller`，env 可配）。
- 重连：断线后指数退避（1s/2s/4s/…上限 **60s**），注册成功后重置退避。
- 同节点单活跃连接：同 nodeID 重复注册时踢掉旧连接（发 conflict Ack 后关闭），防双连接脑裂。

### 4.2 消息信封格式（JSON，v1）

所有消息统一外层信封，业务数据放 `payload`（`pkg/protocol.Message`）：

```json
{
  "id": "9f8c1e2a-...",        // 消息唯一 ID（UUID），ACK 关联 + 幂等去重键
  "type": "Heartbeat",         // 消息类型（见 §4.3 枚举）
  "version": "v1",             // 协议版本，平滑升级锚点（JSON → Protobuf 时靠它兼容）
  "source": "node-edge-001",   // 来源标识：cloud 或节点 ID
  "target": "cloud",           // 目标：cloud / 节点 ID / 广播组
  "timestamp": 1755168000000,  // 毫秒时间戳
  "correlationId": "",         // 关联 ID：请求-响应配对（如 Register ↔ RegisterAck、Ack 关联）
  "payload": { }               // 类型相关负载（见下表示例）
}
```

### 4.3 消息类型枚举（已实现）

| type | 方向 | 用途 | 关键 payload 字段（v0.1.0） | 里程碑 |
|------|------|------|------------------------------|--------|
| `Register` | 边→云 | 节点注册（连接建立后第一条） | nodeID、arch、os、edgecoreVersion、cpu、memory、**token**（7.3，2026-08-15 新增） | M1 |
| `RegisterAck` | 云→边 | 注册结果 | accepted、nodeName、message | M1 |
| `Heartbeat` | 边→云 | 心跳保活 | timestamp（毫秒） | M1 |
| `HeartbeatAck` | 云→边 | 心跳应答 | nodeStatus（Ready/Unknown） | M1 |
| `PodSync` | 云→边 | Pod 配置下发（add/update/delete） | nodeID、namespace、podName、image、replicas、action | M1/M2 |
| `ConfigSync` | 云→边 | ConfigMap/Secret 下发 | nodeID、kind、name、data、operation | M2 |
| `PodStatus` | 边→云 | Pod 状态上报 | nodeID、podName、namespace、phase、restartCount 等 | M1/M2 |
| `DeviceReport` | 边→云 | 设备数据/状态上报 | nodeID、deviceID、properties（含 direction/regAddr/value/result/message） | M3 |
| `DeviceCommand` | 云→边 | 设备操作指令（Twin desired 变更） | nodeID、deviceID、property、value | M3 |
| `NodeJob` / `NodeJobResult` | 云→边 / 边→云 | 任务分发与结果回收 | —（**已关闭**：v0.1.0 范围外，保留协议占位） | 🔒 关闭 |
| `Ack` | 双向 | 通用确认（配合 §4.5 可靠投递） | code（ok/error）、message | M1 |

> 枚举是**开放集合**：`type` 字段按字符串匹配，新增类型不改信封结构、不破坏旧版本（协议兼容性，WBS 10.3 API 兼容矩阵的通信侧对应物，见 docs/API-COMPATIBILITY.md）。

### 4.4 路由机制（无 NATS 主题）

v0.1.0 **未采用 NATS**（决策记录 R4），路由按两侧分工实现：

- **云端**（`cloud/pkg/cloudhub/router.go`）：按 target/type 分发——`SendToNode`（定向下发）、`Broadcast`（广播，Target="*"，路由层已支持，API 层未暴露）、`Deliver`（上行消息投递到对应处理器）。
- **边缘**：EdgeHub 按 type 分发给处理器（handleDownlink → msgHandler）；设备数据面按 MQTT 主题路由（§3.3）。
- 原草案的 NATS 主题体系（`edgeflow.cloud.edge.{nodeID}.*`）随 NATS 放弃而废弃。

### 4.5 可靠投递（WBS 4.6，已实现）

- 语义：**QoS 1（至少一次）+ 幂等去重**（与 ROADMAP 完成标准"QoS 1 消息不丢失（ACK/重试生效）"一致）。
- **云端**（`cloud/pkg/cloudhub/reliable.go`）：`ReliableSend(nodeID, msg, ReliableOptions{Timeout:5s, MaxRetries:2})`——发送后按 `msg.ID`（Ack 的 CorrelationID）匹配确认；超时重发且**保持原 msg.ID**（幂等键）；**5s 超时 × 最多 3 次尝试**；重试耗尽返回导出错误 `ErrAckTimeout`；节点回 `code="error"` 返回 `ErrAckFailed`（不重试）；节点离线返回 `ErrNodeOffline`（立即失败，不消耗重试）。并发在途消息按 ID 关联，互不干扰。
- **边缘**（`edge/pkg/edgehub/ack.go`）：收到下发类消息（非 Ack 且非注册/心跳类，如 PodSync/ConfigSync/DeviceCommand）后自动回 `Ack{code:ok}`（CorrelationID=被确认消息的 ID）；成功处理过的 ID 记入幂等缓存（上限 **1000 条**，FIFO 淘汰），云端重发同 ID 直接回 ok 不重复执行；handler 返回 error 时回 `Ack{code:error}` 且**不入缓存**（云端重试同 ID 允许重新执行）。
- **离线缓冲未实现**：原草案的"边侧待确认队列持久化到 SQLite"未实现；云端重启会丢失在途未确认消息，由上层控制器重新下发兜底（边缘侧幂等去重保证不重复执行）。

### 4.6 序列化现状与演进

```
M1（现状）: JSON（信封 + payload 全 JSON）        ← 已实现，零依赖可读
M2+/规模化: gzip/snappy 压缩（WBS 4.4）          ← gzip 已实现（2026-08-15），snappy 未做
M4/规模化: Protobuf 编码（信封保留 version 字段） ← 未实现，显式延后（决策记录 R9）
```

**gzip 压缩（WBS 4.4，2026-08-15 落地，`pkg/protocol/compress.go`）**：

- **帧格式**：`"EFGZ"(4B) + flags(1B) + gzip(明文信封)`；明文信封恒以 `{` 开头，与 magic 无碰撞，接收端按前缀无损区分。
- **兼容策略（协商式，v1.0 兼容性优先）**：Register/RegisterAck 恒为明文协商信道；边缘在 Register 声明 `compression="gzip"`，云端仅对已声明连接的应答回带同字段——旧云不回带（新边保持明文上行）、旧边不声明（新云保持明文下发），四象限互操作不受影响；小消息（<256B）或压缩不缩小时自动回落明文。
- **开关与 ReadLimit 交互**：cloudcore 配置 `compress`（缺省 true，单开关控双向，变更需重启）；WebSocket 层 1 MiB 读限制作用于线缆（压缩）字节，解压层对明文输出设同样 1 MiB 上限防压缩炸弹——消息体量上限与 v1.0 明文通道一致。

---

## 5. 关键机制

### 5.1 注册与心跳

- 连接建立后第一条消息是 `Register`（含 nodeID/arch/os/version/资源量；v0.1.0 起含 **token**，云端 `EDGEFLOW_CLOUDCORE_NODE_TOKEN` 非空时校验，常数时间比较，失败拒绝注册且不污染注册表）。
- 心跳：EdgeHub 每 **30s** 发 Heartbeat；云端两级失活判定（CloudHub 90s 连接级 / NodeController 180s 节点级，见 §4.1）。
- 云端节点状态：`Ready` / `Unknown` / `Offline`（NodeController 状态机：SIGSTOP 冻结→Offline→SIGCONT→Ready 闭环验证）。

### 5.2 断线检测与重连退避

- 退避序列 1s/2s/4s/…上限 **60s**，注册成功后重置；E2E 实测 cloudcore 重启后 8s 内重连并重新注册。
- 重连窗口防串扰：同 nodeID 新连接注册后踢掉旧连接；幂等去重在两条连接短暂并存时由 downlinkMu 串行化保护（防重）。

### 5.3 边缘自治与恢复同步

- 断网期间：Edged 本地调谐不中断（容器持续运行）；设备采集照常（影子本地更新）。
- 恢复后：重连 → 重新 Register → **周期全量上报**（PodStatus 30s / DeviceReport 周期可配）驱动云端收敛（Absent 终态保留 90s 后云端删除）。
- 无独立"待同步队列/增量补报"：以周期上报 + 声明式收敛替代（与 §3.4 一致）。

### 5.4 设备影子与合并

- 边缘 TwinStore（`edge/pkg/devicetwin`）：`SetDesired`（云端 DeviceCommand 落点）/ `UpsertReported`（Mapper 采样落点），**字段级合并**语义（只更新上报字段，不覆盖其他属性）、深拷贝、自动创建；影子持久化到 SQLite（写路径追加落盘）。
- 云端 devicestatus（`cloud/pkg/devicestatus`）：同样字段级合并且**保 desired**——设备上报（properties）不会覆盖云端期望值（desired）；nodeID 权威。
- 冲突解决（已实现策略 = 原草案 D5 定案）：**云端期望为准 + 本地补报**（设备上报不覆盖 desired；desired 由 device-command 显式写入）。

### 5.5 op-ledger 操作台账

- Modbus Mapper 的写操作（如 targetTemp 设置）记入操作台账（`edge/pkg/metamanager.Ledger` 接口，SQLite 持久化，**保留 30 天**，按条件查询）。
- keadm 另有 ops-ledger.jsonl（升级/回滚等运维操作台账，见 §10），两者职责不同：前者设备指令审计，后者安装运维审计。

### 5.6 可观测性

- 云端 `/metrics`（Prometheus 文本格式）五指标：`edgeflow_cloudcore_nodes_total`、`pods_total`、`devices_total`（gauge）+ `active_connections`（gauge）+ `http_requests_total`（counter，按路由模式+状态码分桶）；metrics 覆盖 96.6%。
- 日志/告警链路（Fluent Bit 等）为 P2 未做；边缘侧不独立暴露指标（与 10.1 合并）。

---

## 6. 安全模型

### 6.1 演进史（实际）

| 阶段 | 通道安全 | 说明 |
|------|---------|------|
| M0 | 无云边通道 | `/healthz` 仅健康检查，默认绑定 :8080（开发期） |
| M1-M3 | **无认证**（历史事实） | 原计划"M1 Token 过渡"**从未实现**（audit-m02 §2.2 确认，cloudhub 注释自认）；通道明文 WS |
| M4 | **完整 mTLS** | 一次到位：pkg/certs + CloudHub TLS + EdgeHub wss（commit `0a7fcc2`，与 7.1/7.4 合并交付）；TLS off 完全向后兼容 |
| v0.1.0（2026-08-15） | mTLS + Token 认证中间件（默认 off）+ 设备认证 | 7.2 API Token 中间件（默认 off 向后兼容）、7.3 Register.token 云边双向校验、7.5 审计台账全部闭环 |

### 6.2 mTLS（WBS 7.1/7.4/4.5，M4 交付）

- CA/服务端/客户端证书幂等生成（纯标准库），`LoadTLSConfig` 双向强制、TLS1.2+、私钥权限 0600、半套 fail-fast（`pkg/certs`）。
- CloudHub `WithTLS` Option + tls.NewListener；mTLS 审计日志记录 peer CN；未认证连接拒绝路径已验证。
- EdgeHub 注入 TLSConfig，`ws://` 自动归一化 `wss://`；TLS off 完全向后兼容。
- 证书轮换：**人工编排**（gen-certs.sh 重新生成 + 重启），吊销（CRL/OCSP）未实现（G9，见 §12）。
- 跨主机 CA 分发（2026-08-15 闭环）：`hack/gen-certs.sh` 支持 `CERT_DIST_DIR` 生成分发包（`cloud/` + `edge/<CN>/`，含 README 部署说明），openssl verify 链验证通过。

### 6.3 API Token 认证中间件（WBS 7.2，M4 交付）

- 开关：`EDGEFLOW_CLOUDCORE_AUTH=on` 启用，**默认 off**（向后兼容，仅限受信网络）；启用时必须设置 `EDGEFLOW_CLOUDCORE_API_TOKEN`（否则启动报错）。
- 校验：`Authorization: Bearer <token>`，**常数时间比较**防时序侧信道；失败 401 + `WWW-Authenticate`；身份写入审计上下文（operator=token / anonymous）。
- 边界：v0.1.0 为单共享令牌模型（"持有令牌即管理员"），多角色 RBAC 为 P2。

### 6.4 设备认证（WBS 7.3，2026-08-15 闭环）

- `keadm join --token=<token>` 将 token 写入 edgecore env；注册时 Register 消息携带 token。
- 云端 `EDGEFLOW_CLOUDCORE_NODE_TOKEN` 非空时校验（常数时间比较）：正确 token 注册成功；错误/缺失被拒且**无注册表污染**（单测 6 项 + `hack/token-auth-check.sh` 真实进程验证）。

### 6.5 审计日志（WBS 7.5，M4 交付）

- `cloud/pkg/audit`：审计中间件将 API 操作写入 `audit-ledger.jsonl`（JSONL 追加写、失败不阻断 API、启动期 fail-fast）；认证失败 401 记录 operator=anonymous；审计覆盖 76.3%。
- 查询 API 为 P2 未做。

### 6.6 端口与暴露边界

| 端口 | 用途 | 暴露边界 |
|------|------|----------|
| 8080 | HTTP 管理（healthz/metrics/13 REST 端点） | 仅绑定 127.0.0.1 或内网；生产经 Helm values 控制；建议启用 Token 认证 |
| 10000 | 云边 WebSocket | 仅对边缘节点网段开放；M4 起可启用 mTLS（env 开关） |
| 1883 | MQTT broker（可选） | 边缘本机/内网；仅设备数据面，不出边缘 |
| 15020 | Modbus 模拟器（MODBUS_SIM_PORT 可覆盖） | 开发/测试用（`pkg/modbussim`，unit ID 1-247、连接数上限 8，均按规范校验） |

---

## 7. 配置管理策略

### 7.1 现状（已实现，pkg/config + env）

- 优先级模型（**命令行 > 环境变量 > 配置文件 > 默认值**）——cloudcore 端口示例（`--port` / `EDGEFLOW_CLOUDCORE_PORT` / `config/cloudcore.json` / 默认 8080；文件存在但解析失败**报错退出**）。
- edgecore 配置全走环境变量（`EDGEFLOW_EDGECORE_*`）：`NODE_ID`、`CLOUD_ADDR`（默认 ws://127.0.0.1:10000）、`MQTT_ADDR`（默认 tcp://127.0.0.1:1883）、`DB_PATH`、`TLS`/`CERT_DIR`、`DEVICE_REPORT_INTERVAL`、`NODE_TOKEN` 等。
- 云端敏感配置走 env：`EDGEFLOW_CLOUDCORE_API_TOKEN`（API 认证）、`EDGEFLOW_CLOUDCORE_NODE_TOKEN`（设备/节点认证）、`EDGEFLOW_CLOUDCORE_TLS`/`CERT_DIR`/`TLS_SAN`（mTLS）、`EDGEFLOW_CLOUDCORE_NODE_SCAN_INTERVAL`/`NODE_TIMEOUT`（NodeController）。

### 7.2 未实现项

- 动态配置/热重载（WBS 2.7，SIGHUP）：**未实现**。
- 下发到边缘的动态配置走 ConfigSync 消息（§4.3），不改配置文件。

---

## 8. 可扩展性设计

### 8.1 模块边界原则

1. **每个组件一个独立包**：`cmd/` 下是进程入口，业务实现放独立包（cloud/pkg/*、edge/pkg/*、pkg/*）；组件之间不直接跨进程调用，管理面走云边 WS 通道，数据面走 MQTT。
2. **协议向后兼容**：新增消息类型 = 加一个枚举值 + 一个处理函数，不破坏旧版本（§4.3；API 兼容矩阵见 docs/API-COMPATIBILITY.md）。
3. **接口先行**：跨模块边界先定义 interface 再实现后端——如 MetaManager 的 `Store`、Edged 的 `ContainerRuntime`（Mock/Docker 双实现）、EventBus 的 Ledger 接口、Mapper 的 DeviceMapper 接口——这是存储/运行时/协议可替换的前提。

### 8.2 MQTT 接入点（替代原 NATS 草案）

| 接入点 | 位置 | 用途 |
|--------|------|------|
| EventBus（paho 客户端） | 边 | 边缘设备数据面：设备/Mapper 以 MQTT 连 EventBus（mosquitto broker），主题见 §3.3 |
| 预留：模块间事件 | 边 | `edgeflow/<module>/<event>` 主题，供 EdgeCore 内部模块解耦（预留，未消费） |

> 原草案"云端 NATS 消息总线 + 边缘 Leaf Node"与"MQTT 兼容层 POC"**均未执行**：POC 未做，直接选型 mosquitto + paho（决策记录 R4）。云端控制器间无消息总线（CloudHub 直接调用各存储/控制器）。

### 8.3 SQLite 接入点

- 使用方：**MetaManager**（KV/节点信息/Pod/ConfigMap/Secret 缓存 + op_ledger 台账）。存储层为 interface（`Store`：Get/List/Watch/Update + 待同步队列——队列未实现），默认实现 SQLite（WAL，modernc 纯 Go 驱动，数据文件默认 `data/edgeflow.db`，env 可重定向）。
- SQLite 数据范围：节点信息、下发的 Pod/ConfigMap/Secret 缓存、设备影子（写路径落盘）、op_ledger。**不含**容器运行数据（由 Docker 自己管）。
- 因接口隔离，未来可换 bbolt/Badger 等嵌入式存储。

### 8.4 CRD 与 Mapper 扩展

- CRD 族（WBS 1.4，`apis/edge/v1alpha1`，Group `edgeflow.io`）：`EdgeNode`、`DeviceModel`、`Device` 三种类型（含 DeepCopy + 11 测试）；manifest 在 `config/crd/`（2026-08-14，kind 集群已 apply 验证）。**NodeJob 已关闭**（v0.1.0 范围外）。
- 新增能力 = 新增 CRD 类型 + 对应控制器，不侵入现有模块；接入真实 Kubernetes 后由 OpenAPI schema/校验取代（API-SPEC §7）。
- Mapper 框架（WBS 5.1）：设备接入以 Mapper 插件形式实现（DeviceMapper 接口 + MapperRegistry），mock_sensor（内置演示）、modbus（goburrow）各是独立 Mapper；OPC-UA 未做。

---

## 9. 已知偏差与决策记录

以下为开发过程中已发生的**实际决策/偏差**（原草案"待确认项"多数已定案）：

| # | 决策/偏差 | 内容 | 依据 |
|---|----------|------|------|
| R1 | **REST 适配替代 K8s apiserver** | 云端节点/Pod/设备状态为内存 registry + REST API；M0 验收"CRD 可 kubectl apply"、M1/M2 验收"kubectl get nodes / kubectl apply"未字面达成，以 REST 端点适配（`/api/v1/edgenodes`、`podsync`、`config-sync`） | audit-m02 §1.1 #10/#27/#39；API-SPEC §8 |
| R2 | **Docker bridge 替代 CNI/Flannel** | ROADMAP 缺口 6 关闭：边缘容器网络走 Docker bridge，无 CNI | audit-m02 §1.3 #44 |
| R3 | **Edged 方案 A 定案** | P0 决策：DockerRuntime + Mock 双实现 + 声明式 reconcile；containerd CRI 为 P2 延后 | EDGED-POC.md；ROADMAP §3.2 状态 |
| R4 | **NATS 放弃 → mosquitto + paho** | 3.6 实现时未执行"NATS MQTT 兼容性 POC"，直接选型 mosquitto（broker）+ paho（客户端）；云端无消息总线 | commit `2a0d0a3`；audit-m02 S12 |
| R5 | **2.8 NodeJob 关闭** | v0.1.0 范围外产品决策：协议占位标注"已关闭"，无 CRD/控制器/API | commit `4c5b9c6`；audit-m35 G7 |
| R6 | **里程碑归属偏移** | 2.4 NodeController、4.5 TLS、8.6 keadm 基础**实际均在 M4 完成**（原计划 M1）；5.2 Modbus 实际在 M4 完成（原计划 M3） | ROADMAP §3 注；audit-m02 §2.2 |
| R7 | **M1-M3 通道无认证（历史事实）** | 原计划"M1 Token 过渡"从未实现；M4 直接完整 mTLS。M1-M3 为明文 WS（仅限开发拓扑） | audit-m02 §2.2/S13 |
| R8 | **验收口径 REST 化** | "kubectl get nodes Ready" 等 K8s 验收以 REST 端点语义适配；真实 K8s 接入排后续版本 | audit-m02 §4 P1（待产品确认） |
| R9 | **序列化仅 JSON，压缩/Protobuf 显式延后** | gzip 于 2026-08-15 落地（协商式兼容，见 §4.6）；Protobuf 仍延后 | audit-m02 §4；commit（gzip 实现） |
| R10 | **自治验收时长口径** | 30min 断网自治以 tests/e2e 60s 短时窗口模拟验证（判定逻辑与 30min 一致）；真实长跑待环境 | tests/e2e/autonomy_test.go |
| R11 | **可观测性合并** | 2.9/3.8 与 10.1 合并为云端 /metrics 五指标，边缘不独立暴露 | commit `4c5b9c6`；ROADMAP §1.1 |
| R12 | **双视图 API 并存** | `/api/v1/nodes`（运行视角 NodeInfo）与 `/api/v1/edgenodes`（CRD 对象视角）并存，属设计取舍 | API-SPEC §3/§8 |

---

## 10. 部署形态

| 形态 | 说明 | 文档 |
|------|------|------|
| 快速开始 | `bash examples/demo.sh` 一键端到端（构建→注册→Pod 下发→设备链路→MQTT 数据面→清理，DEMO PASS×3） | docs/DEPLOYMENT.md §0、examples/README.md |
| keadm | 离线产物生成：`init`（cloudcore.yaml+NOTES）/`join`（edgecore.env+service+install.sh）/`reset`/`version`；`upgrade`/`rollback`（备份模型 backups/<ts>/ + ops-ledger.jsonl + --simulate-failure 演练 + 事务化 restore + manifest 白名单）；**batch**（2026-08-15：join/upgrade/rollback 清单逐节点） | docs/KEADM.md、docs/UPGRADE.md |
| Helm | `helm install edgeflow build/charts/edgeflow`（cloudcore Deployment + Service；values：镜像/端口/探针/资源/env；`service.hubEnabled` 支持集群外边缘节点接入；helm lint 0 failed） | docs/DEPLOYMENT.md §2 |
| 镜像 | 多阶段 distroless（`gcr.io/distroless/static-debian12:nonroot`）：cloudcore 16.7MB / edgecore 22.5MB，nonroot(65532)；**amd64+arm64 双架构 manifest**（本地 registry 闭环，QEMU 交叉运行版本一致）；**Trivy 扫描 0 漏洞**（2026-08-15，修复 golang.org/x/net 后复扫） | docs/MULTIARCH.md、docs/SECURITY-SCAN.md |
| 发布制品 | `release/v0.1.0/`：cloudcore/edgecore/keadm × darwin-arm64/linux-amd64/**linux-arm64**（2026-08-15 补齐）共 9 二进制 + Chart 包 + checksums + SBOM（33 组件）+ images.json | docs/RELEASE-NOTES-v0.1.0.md |

---

## 11. 与 ROADMAP 的映射速查

| 本文档章节 | 对应 WBS / 里程碑 |
|-----------|------------------|
| §2 总体架构与分层 | WBS 1（M0 骨架）、WBS 2/3 组件（M1-M4，状态见 §2.2 表） |
| §3 核心数据流 | 计划 §3.2 架构数据流（下行/上行/MQTT 数据面/自治） |
| §4 通信协议 | WBS 4.1-4.6（M1 已实现；4.5 实际 M4） |
| §5 关键机制 | WBS 3.1/3.4/3.5/4.2/4.6/5.4/10.1 |
| §6 安全 | WBS 7.1-7.5、4.5（M4 + 2026-08-15 7.3 闭环） |
| §7 配置 | WBS 1.5（已实现）、2.7（热重载未实现） |
| §8 可扩展性 | WBS 3.6（MQTT）、3.3（SQLite）、1.4（CRD）、5.1（Mapper） |
| §9 决策记录 | ROADMAP §3 注、§7 缺口 1/6 处置、audit-m02/audit-m35 结论 |
| §10 部署形态 | WBS 8.5（Helm）、8.6/10.2（keadm）、1.6（发布制品） |
| §12 残留缺口 | ROADMAP §1.2 各 ⬜/🟨 项、PROGRESS §5 待办 |

---

## 12. 残留缺口与跟踪（评审时点 2026-08-15）

> 原 v0.1 草案"待确认项清单"（D1-D9）已全部定案（见 §9 决策记录 R1-R12）；以下为**仍开放**的缺口，均可在 ROADMAP §1.2 / PROGRESS §5 找到对应跟踪项。

| # | 缺口 | 状态/影响 | 跟踪 |
|---|------|----------|------|
| G-1 | 3.7 ServiceBus 未实现（云边 HTTP 调用，Phase 3） | 功能缺失（计划外延后） | ROADMAP 3.7 ⬜ |
| G-2 | 6.5 调度/资源超卖未实现（仅 Replicas 伸缩） | 功能缺失 | ROADMAP 6.5 🟨 |
| G-3 | 5.2 OPC-UA 未做；MQTT 仅 mock_sensor 数据面模式，无通用 MQTT 设备适配器 | 功能缺失 | ROADMAP 5.2 🟨 |
| G-4 | 5.3 Device K8s 控制器未做（仅 CRD 类型 + manifest + 云端内存态存储） | 对接真实 K8s 前不阻塞 | ROADMAP 5.3 🟨 |
| G-5 | 2.7 配置热重载未实现 | 功能缺失 | ROADMAP 2.7 🟨 |
| G-6 | 7.1 证书轮换人工编排、吊销（CRL/OCSP）未实现 | 安全运维缺口 | audit-m35 G9 |
| G-7 | 4.4 gzip/Protobuf 压缩与编码升级未实现 | 显式延后，无隐式承诺 | audit-m02 §4 |
| G-8 | 8.2 多节点（10+）E2E 未做；8.4 100 节点压测未做；M3"端到端延迟 ≤5s"从未测量 | 规模化验收未实证（10 节点压测：100% 注册、平均 201ms、P95 202ms） | audit-m35 G11 |
| G-9 | 3.4 自治 30min 真实长跑未验证（E2E 为 60s 短时模拟） | 需真实环境长跑 | audit-m02 #40；PERFORMANCE-BASELINE.md |
| G-10 | 云端重启丢失在途未确认消息（离线缓冲未实现） | 由上层重新下发兜底（边缘幂等去重） | §4.5 说明 |
| G-11 | 边缘资源上报（CPU/内存）未采集（`/api/v1/nodes` memory 恒 0） | 数据缺失 | DEPLOYMENT.md §9 |
| G-12 | config-sync 的 Secret value 明文传输存储 | 生产需加密 | API-SPEC §8 |
| G-13 | CI 从未在 GitHub 运行（远程仓库未关联） | M0 验收"CI PR 反馈 ≤10min"未实证 | PROGRESS §5 P1（需用户操作） |
| G-14 | 生产多节点/多主机部署路径（证书分发、网络差异） | kind 单节点真实集群已跑通；多节点待生产演练 | DRILL-SCHEDULE.md（窗口【需确认】） |

---

*本文档为 WBS 9.1 交付物（v1.0，2026-08-15 评审通过），与 docs/ROADMAP.md、docs/API-SPEC.md、docs/DEPLOYMENT.md、docs/CLOSE-OUT-ACTIONS.md 配套使用。数字口径与状态列均以 2026-08-15 代码现状为准。*
