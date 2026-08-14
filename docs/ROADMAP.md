# EdgeFlow ROADMAP — 开发路线图与任务拆解

> **依据**：`EdgeFlow-开发计划.md` v1.0（2026-08-12，经可执行性审查修正）
> **编制日期**：2026-08-13
> **定位**：将开发计划拆解为可执行任务列表，明确首个开发模块的任务边界与完成标准。
> **约定**：所有"完成标准"均为可验证命令/操作；标注 ⚠️ 的依赖关系为基于计划中架构数据流与里程碑描述的推断（计划未显式声明），需在开发中确认。

---

## 0. 现状基线（2026-08-13 检查）

> 注：本节为 2026-08-13 的**历史快照**（首个模块拆解时的基线）；最新状态见 §1.1/§1.2 状态列（2026-08-14 按 audit-m02/audit-m35 核对回写）。

以下内容已在先前会话完成（git commit `4093fe3 chore: project skeleton`），首个模块任务拆解基于此现状：

| 项 | 状态 | 说明 |
|----|------|------|
| go.mod / 目录结构（cmd、pkg、apis、build、docs、hack） | ✅ 完成 | `module edgeflow`，Go 1.26.2 |
| Makefile（build/test/run/lint/clean） | ✅ 完成 | 已注入 `-ldflags` 占位 |
| cmd/cloudcore、cmd/edgecore 占位程序 | ✅ 完成 | edgecore 打印版本后退出，**非常驻服务** |
| .github/workflows/ci.yml | ✅ 完成 | vet + build + test -race + lint（强制）+ 覆盖率门槛 ≥70%（commit `ab73062`） |
| README.md / .gitignore / git | ✅ 完成 | |
| 共享库（日志/配置/版本） | ✅ 完成 | pkg/log、pkg/config、pkg/version（commit `98a50a6`、`f49ce32`） |
| cloudcore 常驻服务 + /healthz | ✅ 完成 | 默认端口 8080（commit `98a50a6`） |
| 单元测试 | ✅ 完成 | pkg 四包 100% + cmd/cloudcore 88%，全仓总覆盖率 87.5%（commit `98a50a6`、`f49ce32`） |
| golangci-lint 配置 | ✅ 完成 | .golangci.yml v2（standard 组），0 issues；CI 中强制（commit `98a50a6`、`ab73062`） |
| CRD 定义 / Helm 骨架 / 架构文档 | ⬜ 未开始 | apis/、build/、docs/ 为空 |

---

## 1. 模块清单（WBS 一级 + 二级）

模块 ID 直接沿用开发计划 WBS 编号。状态占位：⬜ 未开始 ｜ 🟨 进行中 ｜ ✅ 已完成 ｜ ⛔ 依赖 P0 决策。

> **状态核对（2026-08-14）**：本节与 §1.2 状态列已按 audit-m02.md / audit-m35.md 逐项结论回写（M0-M2 44 项 + M3-M5 31 项核对）；里程碑归属偏移（2.4/4.5/5.2/8.6 → M4）见 §3 注。

### 1.1 一级模块总览

| 模块 ID | 名称 | 人天 | 依赖（⚠️ 为推断） | 状态 |
|---------|------|------|-------------------|------|
| WBS 1 | 基础架构搭建 | 20 | 无（M0 无依赖） | ✅ 完成（2026-08-14 核对，audit-m02 §1.1） |
| WBS 2 | CloudCore 开发 | 63 | WBS 1、WBS 4（⚠️ CloudHub 需消息协议与连接管理） | 🟨 主体完成；2.8 NodeJob、2.9 可观测性未做（audit-m02 §2.1、audit-m35 G2） |
| WBS 3 | EdgeCore 开发 | 65 | WBS 1、WBS 4（⚠️ EdgeHub 需消息协议与连接管理） | 🟨 主体完成；3.7 ServiceBus、3.8 可观测性未做 |
| WBS 4 | 云边通信层 | 28 | WBS 1（协议定义在 WebSocket 通道之前，计划 §7.4 关键路径） | ✅ 完成（4.4 压缩计划内延后；4.5 实际在 M4 完成） |
| WBS 5 | 设备映射器框架 | 22 | WBS 1（CRD）、WBS 2.2、WBS 3.5/3.6（⚠️ 数据流：DeviceTwin ← EventBus ← MetaManager） | 🟨 主体完成；5.2 OPC-UA 未做（Modbus 实际在 M4 完成）、5.3 控制器未做 |
| WBS 6 | 应用管理 | 20 | WBS 3（⚠️ Edged/MetaManager 就绪后） | 🟨 主体完成；6.4 滚动更新+回滚、6.5 调度/超卖未做（audit-m02 §2.3） |
| WBS 7 | 安全与认证 | 18 | WBS 1；7.4 依赖 WBS 4.5（⚠️） | 🟨 仅 7.1（部分）+7.4；7.2 RBAC、7.3 设备认证、7.5 审计日志未做（audit-m35 G1） |
| WBS 8 | 测试与部署 | 30 | 随各模块并行；8.5 依赖 WBS 2 产物；8.6 依赖 WBS 4 协议（⚠️） | 🟨 8.1/8.5 完成；8.2 单节点、8.3 E2E 完整场景、8.4 压测未做；8.6 基础+升级回滚（实际在 M4） |
| WBS 9 | 文档与示例 | 14 | WBS 1（可全程并行） | 🟨 9.2-9.5 完成；9.1 架构文档存在但未评审（audit-m02 S11-S13） |
| WBS 10 | 审查补充项 | 24 | WBS 2/3（可观测性）、WBS 8.6（升级回滚，⚠️） | 🟨 10.2 完成、10.4 部分（SBOM）；10.1 可观测性、10.3 兼容矩阵未做（audit-m35 G2/G4） |

### 1.2 二级模块明细

| 模块 ID | 名称 | 子任务（计划原文） | 人天 | 依赖（⚠️ 为推断） | 状态 |
|---------|------|-------------------|------|-------------------|------|
| 模块 ID | 名称 | 子任务（计划原文） | 人天 | 依赖（⚠️ 为推断） | 状态 |
|---------|------|-------------------|------|-------------------|------|
| 1.1 | 项目骨架 | Go 项目结构、Makefile、go.mod | 2 | 无 | ✅ 完成（commit `4093fe3`） |
| 1.2 | CI/CD 流水线 | GitHub Actions、多架构构建 | 3 | 1.1 | ✅ 完成（基础 CI commit `ab73062`；多架构 release.yml 于 M4 补齐，commit `301daee`） |
| 1.3 | 开发环境 | 本地 K8s 集群、边缘节点模拟 | 2 | 1.1 | ✅ 完成（hack/dev-up.sh + DEV-ENV.md，模拟方案已文档化） |
| 1.4 | API 规范定义 | CRD 定义、OpenAPI schema | 3 | 1.1 | 🟨 类型定义完成（commit `a541128`，EdgeNode/DeviceModel/Device + 11 测试）；CRD manifest 补于 `config/crd/`（2026-08-14，G6）；无 OpenAPI 独立 schema（内嵌于 CRD manifest） |
| 1.5 | 共享库与工具包 | 日志、配置、错误处理 | 4 | 1.1 | ✅ 完成（pkg/log、pkg/config、pkg/version、pkg/httpx） |
| 1.6 | 构建与发布脚本 | 交叉编译、版本管理 | 3 | 1.1 | ✅ 完成（Makefile cross-build + release.yml + M5 发布制品） |
| 1.7 | 代码质量基建 | golangci-lint、覆盖率、安全扫描 | 3 | 1.1 | ✅ 完成（.golangci.yml v2 + CI 强制 lint + 覆盖率门槛 70%） |
| 2.1 | CloudHub 模块 | WebSocket 服务端、消息编解码、会话管理、连接池 | 12 | 1.x、4.1、4.2（⚠️） | ✅ 完成（commit `6241a78` 起多轮） |
| 2.2 | DeviceController | 设备 CRD 控制器、状态同步 | 10 | 1.4、5.3（⚠️ 同一 CRD） | 🟨 部分：云端设备状态存储 + 查询/指令 API（内存态，commit `744afaa`）；无 K8s 控制器（audit-m35 G6） |
| 2.3 | EdgeController | 边缘节点注册、状态管理 | 8 | 2.6（⚠️ 需 apiserver 交互） | ✅ 完成（REST 化适配：registry + EdgeNode 映射，commits `3c7b99d`/`641863e`） |
| 2.4 | NodeController | 心跳监控、节点上下线 | 6 | 2.3（⚠️ 注册后才有心跳） | ✅ 完成（commit `f71684e`，覆盖率 96.8%；**实际在 M4 完成**，M1 仅 cloudhub 内嵌断线检测） |
| 2.5 | CloudCore API 层 | RESTful API、认证中间件 | 5 | 2.6（⚠️） | ✅ 完成（13 个 REST 端点 + /healthz，见 API-SPEC.md；**认证中间件未做**，归 WBS 7.2 RBAC 缺口） |
| 2.6 | 云端元数据存储 | K8s apiserver 交互、缓存层 | 6 | 1.x | ✅ 完成（内存 registry + REST API，无 apiserver/etcd，已文档化为适配） |
| 2.7 | CloudCore 配置管理 | 动态配置、热重载 | 4 | 1.5（⚠️） | 🟨 文件/flag/env 配置完成；热重载未实现 |
| 2.8 | 云端任务管理 | NodeJob CRD、任务分发 | 8 | 2.1、2.3（⚠️） | ⬜ 未实现（协议仅占位"待定"，audit-m02 §2.1） |
| 2.9 | 可观测性基建 | 监控指标、日志收集、告警 | 4 | 2.1（⚠️） | ⬜ 未实现（与 10.1 合并未做，audit-m35 G2） |
| 3.1 | EdgeHub 模块 | WebSocket 客户端、消息路由、断线重连 | 12 | 1.x、4.1、4.2（⚠️） | ✅ 完成（commits `7b1c27a`/`19dd66f`/`0a7fcc2`） |
| 3.2 | Edged 模块 | 轻量容器运行时管理、Pod 生命周期 | 30 | ⛔ **Edged 策略 P0 决策**（方案 A/B/C）；3.3（⚠️ 数据流 EdgeHub→MetaManager→Edged） | ✅ 完成（方案 A：DockerRuntime + Mock + 声明式 reconcile + 多副本 + 健康自愈 + CrashLoopBackOff；containerd CRI 实现 P2 延后） |
| 3.3 | MetaManager | 本地元数据存储(SQLite)、数据同步 | 8 | 1.5（⚠️） | ✅ 完成（SQLite WAL + KV + Pod 持久化 + 增量订阅，commits `3aaaf28`/`089c358`） |
| 3.4 | 边缘自治引擎 | 断网独立运行、重连后同步、冲突解决 | 15 | 3.2、3.3、4.6（⚠️） | ✅ 完成（基础：断网 40s 容器持续运行 + 恢复同步；**30min 时长未验证**，归 WBS 8.3 缺口，audit-m02 #40） |
| 3.5 | DeviceTwin 模块 | 设备影子、状态同步 | 6 | 3.6、3.3（⚠️） | ✅ 完成（devicetwin 覆盖率 100%，commit `744afaa`） |
| 3.6 | EventBus 模块 | MQTT 消息总线 | 4 | ⚠️ NATS MQTT 兼容性 POC（开发前 1-2 天，计划 §4.2.4） | ✅ 完成（mosquitto + paho，commit `2a0d0a3`；**NATS POC 未执行**，改用 mosquitto，已文档化偏差） |
| 3.7 | ServiceBus 模块 | 边缘服务发现、路由 | 4 | 4.x（⚠️ 云边 HTTP 调用，Phase 3） | ⬜ 未实现（ROADMAP §7 缺口 9 未处置） |
| 3.8 | 可观测性(边缘) | 指标暴露、日志上报 | 6 | 3.1（⚠️） | ⬜ 未实现（与 10.1 合并未做，audit-m35 G2） |
| 4.1 | 消息协议设计 | 消息格式(Protobuf)、类型枚举 | 3 | 1.x | ✅ 完成（pkg/protocol JSON 信封 + 类型枚举，commit `e569ea1`） |
| 4.2 | 连接管理 | 连接池、心跳保活、重连退避 | 6 | 4.1（⚠️） | ✅ 完成（注册/心跳 30s/指数退避 2/4/8s/自动重连） |
| 4.3 | 消息路由引擎 | 主题路由、消息过滤 | 5 | 4.1、4.2（⚠️） | ✅ 完成（SendToNode/Broadcast/Deliver，commit `081eb0f`） |
| 4.4 | 数据压缩与序列化 | 消息压缩、增量同步 | 4 | 4.1（⚠️） | 🟨 序列化（JSON）✅、增量同步部分实现；**压缩未实现**（计划内延后：gzip M2+、Protobuf M4/规模化，audit-m02 #16） |
| 4.5 | 安全传输层 | TLS/MTLS、证书轮换 | 5 | 7.1（⚠️） | ✅ 完成（完整 mTLS，commit `0a7fcc2`；**实际在 M4 完成**，与 7.1/7.4 合并交付，M1-M3 通道无认证） |
| 4.6 | 消息可靠投递 | ACK、重试、幂等 | 5 | 4.1、4.2（⚠️） | ✅ 完成（ReliableSend + Ack + 同 ID 重试 + 幂等去重 + P2 收尾，commits `3197ad3`/`19dd66f`/`8321b0e`） |
| 5.1 | Mapper 框架 | Mapper SDK、设备注册/注销 | 6 | 1.4（⚠️） | ✅ 完成（DeviceMapper + MapperRegistry，commit `7d82c0c`，覆盖率 96.4%） |
| 5.2 | 设备协议适配 | Modbus、OPC-UA、MQTT 适配器 | 5 | 5.1（⚠️） | 🟨 部分：Modbus ✅（commit `a290686`，**实际在 M4 完成**）；OPC-UA ⬜；MQTT 仅 mock_sensor 数据面模式（audit-m35 G7） |
| 5.3 | 设备 CRD 控制器 | Device/DeviceModel CRD | 4 | 1.4（⚠️） | 🟨 部分：CRD 类型定义 ✅（commit `a541128`）+ manifest（`config/crd/`，2026-08-14）；K8s 控制器 ⬜（audit-m35 G6） |
| 5.4 | 设备数据流水线 | 采集→转换→上报→持久化 | 4 | 5.1、3.3（⚠️） | ✅ 完成（采集→Twin 合并→周期上报→云端存储 + op_ledger） |
| 5.5 | 设备示例 Mapper | 完整 Mapper 示例 | 3 | 5.2（⚠️） | ✅ 完成（mock_sensor 91.1% + modbus mapper 示例） |
| 6.1 | 边缘应用部署 | Pod 部署、Deployment 支持 | 5 | 3.2（⚠️） | ✅ 完成（PodSync 下发→订阅触发→Edged 调谐→真实 Docker 容器，commits `15366e7`/`9fe47c1`） |
| 6.2 | 配置下发 | ConfigMap/Secret 同步 | 3 | 3.3、4.x（⚠️） | ✅ 完成（五态 + 可靠投递 + 落盘 + 自动 Ack + Secret 脱敏，commit `5403daa`） |
| 6.3 | 状态上报 | Pod 状态上报、健康检查 | 3 | 3.2（⚠️） | ✅ 完成（30s 周期上报 + Absent 终态保留 90s + 云端收敛，commits `707128f`/`bc58e40`） |
| 6.4 | 边缘应用升级 | 滚动更新、回滚 | 4 | 6.1（⚠️） | 🟨 部分：健康自愈/重启 ✅（commit `47d9e21`）；**镜像更新滚动策略 + 回滚未实现**（P1 缺口，audit-m02 §2.3） |
| 6.5 | 资源管理 | 边缘调度、资源超卖 | 5 | 6.1（⚠️） | 🟨 部分：Replicas 补齐/收缩 ✅；**调度/资源超卖未实现**（audit-m02 §4 P2） |
| 7.1 | 证书管理 | CA 初始化、签发、轮换 | 5 | 1.x | 🟨 部分：CA/签发/校验/幂等 ✅；**轮换人工编排、吊销（CRL/OCSP）未实现**（audit-m35 G9） |
| 7.2 | RBAC | 角色/权限定义 | 3 | 1.4（⚠️） | ⬜ 未实现（全仓无 RBAC/authz 代码，audit-m35 G1） |
| 7.3 | 设备认证 | 设备身份注册、Token 认证 | 4 | 5.x（⚠️） | ⬜ 未实现（token 为预留字段，edgecore 未消费，audit-m35 G1） |
| 7.4 | 云边认证 | MTLS 握手、节点验证 | 3 | 7.1、4.5（⚠️） | ✅ 完成（双向强制 + peer CN 审计 + 拒绝路径，commit `0a7fcc2`） |
| 7.5 | 审计日志 | 操作审计、安全事件 | 3 | 7.1（⚠️） | ⬜ 未实现（仅 TLS 连接单行日志；keadm ops-ledger 为运维台账非安全审计，audit-m35 G1） |
| 8.1 | 单元测试 | 核心模块测试、覆盖率≥70% | 8 | 随各模块并行 | ✅ 完成（24 包 race 全绿，总覆盖率 77.8% ≥70%） |
| 8.2 | 集成测试 | 云边联调、设备管理端到端 | 6 | 2.x、3.x（⚠️） | 🟨 云边联调/设备链路单节点 ✅；**多节点（10+）E2E 未做**（audit-m35 G11） |
| 8.3 | E2E 测试 | 多节点、故障恢复、自治 | 5 | 3.4（⚠️） | ⬜ 完整场景未做（多节点/故障恢复/自治 30min；仅 M5 demo.sh 演示脚本，audit-m02 §2.4） |
| 8.4 | 性能测试 | 节点规模模拟、延迟测试 | 4 | 8.2（⚠️） | ⬜ 未做（100 节点/延迟/内存指标全未验证，audit-m35 G3） |
| 8.5 | Helm Chart | Chart 编写、多环境 values | 3 | 2.x 产物（⚠️） | ✅ 完成（骨架 `9d78246` + 完整化 `3bb60ce`，helm lint 0 failed） |
| 8.6 | keadm 安装工具 | 安装工具、注册流程、升级 | 4 | 4.x（⚠️ 注册协议） | 🟨 基础 init/join/reset/version（**实际在 M4 完成**，commit `2386fa3`）+ upgrade/rollback/ops-ledger ✅（`7aa035c`/`fe093e1`）；**批量操作 ⬜、配置迁移=备份保全**（audit-m35 #23/G8） |
| 9.1 | 架构文档 | 设计文档、模块说明 | 3 | 1.x | 🟨 存在但标注 v0.1 草案、**未评审**、内容滞后（NATS→MQTT、Token→mTLS 未回写，audit-m02 S11-S13） |
| 9.2 | API 文档 | API 参考、CRD 文档 | 2 | 1.4（⚠️） | ✅ 完成（API-SPEC.md 13 端点 + 错误码 + CRD 全字段，commit `f090a30`） |
| 9.3 | 部署指南 | 快速开始、生产部署 | 3 | 8.5（⚠️） | ✅ 完成（DEPLOYMENT.md，REVIEWS.md R9.3 已处理） |
| 9.4 | 开发者指南 | 贡献指南、代码规范 | 2 | 1.x | ✅ 完成（HANDOFF.md，REVIEWS.md R9.4 已处理） |
| 9.5 | 示例与教程 | 设备接入、应用部署示例 | 4 | 5.x、6.x（⚠️） | ✅ 完成（examples/demo.sh DEMO PASS×3，REVIEWS.md §5） |
| 10.1 | 可观测性基建 | 监控(Prometheus)、日志(Fluent Bit)、告警 | 10 | 2.9、3.8（⚠️ 职责需与 2.9/3.8 对齐） | ⬜ 未实现（无指标端点、无 Prometheus/Fluent Bit 代码，audit-m35 G2） |
| 10.2 | 边缘节点升级与回滚 | OTA 升级、灰度发布 | 6 | 8.6（⚠️） | ✅ 完成（备份模型 + ops-ledger + simulate-failure + 事务化 restore + 白名单；**灰度发布未实现**，audit-m35 G8） |
| 10.3 | API 兼容性测试 | K8s API 版本兼容矩阵 | 4 | 2.x（⚠️） | ⬜ 未实现（无兼容矩阵文档/测试，audit-m35 G4） |
| 10.4 | 安全扫描与修复 | 镜像扫描、依赖漏洞 | 4 | 8.5（⚠️） | 🟨 部分：SBOM ✅（33 组件）；**镜像漏洞扫描 ⬜、cosign 签名延后**（audit-m35 G10） |

**合计**：374 人天（原始）→ 340（Vibe Coding 1.1×）→ 425（含 25% 缓冲，计划 §5.2/5.3）。

---

## 2. 依赖关系与开发顺序（拓扑排序）

### 2.1 依赖关系要点（依据计划）

1. **里程碑级**：M0 → M1 → M2 → M3 → M4 → M5（计划 §7.4，串行关键路径）。
2. **关键路径活动**（计划 §7.4 原文）：`云边通信协议定义 → WebSocket 通道 → 边缘自治 → 设备管理 → 规模化验证 → 发布`。
3. **架构数据流**（计划 §3.2）：下行 `K8s API → Controller → CloudHub →(WS)→ EdgeHub → MetaManager → Edged/DeviceTwin`；上行 `设备 →(MQTT)→ EventBus → DeviceTwin → MetaManager → EdgeHub →(WS)→ CloudHub → Controller → K8s API`。
4. **P0 前置**（计划 §4.2.2/附录 B）：Edged 实现策略必须在开发启动前确定（默认按方案 A，30 人天）；NATS MQTT 兼容性 POC 在 Phase 1 前 1-2 天执行。

### 2.2 拓扑排序

```
阶段 0 (M0)  WBS 1（1.1 → [1.4 ∥ 1.5] → [1.2 ∥ 1.6 ∥ 1.7] → 1.3）   ← 无依赖，起点
阶段 1 (M1)  4.1 消息协议设计（关键路径第一步）
             └→ [2.1 CloudHub ∥ 3.1 EdgeHub ∥ 4.2 连接管理]
                 └→ [4.3 ∥ 4.4 ∥ 4.6]（消息路由/压缩/可靠投递）
                 └→ [2.3/2.4/2.6（Cloud 侧控制器）∥ 3.3 MetaManager]
                 └→ 3.2 Edged 基础版（POC，方案 A 验证）
阶段 2 (M2)  [3.2 Edged 完整 ∥ 3.4 边缘自治] → 6.1~6.5 应用管理
阶段 3 (M3)  [5.1~5.5 Mapper 框架 ∥ 2.2 DeviceController ∥ 3.5 DeviceTwin ∥ 3.6 EventBus]
阶段 4 (M4)  [7.1~7.5 安全认证 ∥ 10.1~10.4 审查补充 ∥ 2.8/2.9/3.8 ∥ 8.4/8.5]
阶段 5 (M5)  [8.6 keadm 完整 ∥ 9.2~9.5 文档示例] → 发布
```

### 2.3 可并行项

| 并行组 | 条件 | 依据 |
|--------|------|------|
| WBS 2（CloudCore）∥ WBS 3（EdgeCore） | 4.1 消息协议定义就绪 | 计划 §3.2 数据流、§7.4 关键路径 |
| WBS 9 文档 | 全程 | 计划 M5 为文档交付，9.x 可全程并行 |
| WBS 8.1 单元测试 | 随各模块 | 计划 §8.1 每个 PR 必须含单测 |
| 7.1 证书管理 | 与 M1 并行（不阻塞） | 计划 §7.1 里程碑依赖仅到 M4，证书为 M4 交付 |
| 5.x ∥ 6.x | 各自依赖就绪后 | 设备与应用两条线独立（⚠️ 推断） |
| 1.4 API 规范 ∥ 1.5 共享库 | 1.1 骨架就绪后 | 无相互依赖（⚠️ 推断） |

---

## 3. 里程碑与模块映射

| 里程碑 | 名称 | 覆盖 WBS 模块 | 计划验收标准（摘要） |
|--------|------|--------------|---------------------|
| M0 | 架构基线与开发就绪 | **WBS 1 全部**、9.1 | `make build` 本地+CI 通过；CRD 可 `kubectl apply`；CI PR 反馈 ≤10min；架构文档评审通过 |
| M1 | 云边核心通信链路打通 | **4.1-4.4、4.6**（4.5 基础版 ※）、**2.1、2.3、2.4 ※、2.6**、**3.1、3.3**、3.2 基础版、8.2 部分、8.6 基础 ※ | keadm join 注册；`kubectl get nodes` Ready；心跳 ≤30s；断线 60s 重连；MetaManager 持久化 SQLite；单测 ≥70% |
| M2 | 应用部署与边缘自治 MVP | **3.2 完整、3.4**、**6.1-6.5**、8.3 部分、Flannel（⚠️ WBS 无对应条目，见 §7 缺口 6） | kubectl apply 部署到边缘；断网 30min 容器持续运行；恢复 120s 同步；ConfigMap/Secret 下发；E2E 自治全过 |
| M3 | 设备管理与数字孪生 | **2.2、3.5、3.6、5.1-5.5**（5.2 ※）、8.2/8.3 设备部分 | CRD 定义 DeviceModel/Device；Twin desired 变更同步并触发设备操作；设备状态上报；MQTT Mapper 读写模拟设备；端到端延迟 ≤5s |
| M4 | 生产化与规模化 | **4.5 完整、7.1-7.5、10.1-10.4**、2.8（⚠️ 归属待确认）、2.9、3.8、8.4、8.5 | `helm install` 一键部署；keadm join 含证书签发；amd64+arm64 镜像可运行；滚动升级零停机；100 节点注册 ≥99%、延迟 ≤3s；EdgeCore 内存 ≤256MB |
| M5 | MVP 发布与文档交付 | **8.6 完整、9.2-9.5**、发布产物（Release Notes、二进制、Chart 包、多架构镜像） | 新用户 15min 部署；CRD 字段文档齐全；温度传感器 Demo 跑通；验收演示通过 |

> ※ 里程碑归属偏移（2026-08-14 审计回写，audit-m02/audit-m35）：**2.4 NodeController、4.5 TLS、8.6 keadm 基础均实际在 M4 完成**（原计划 M1）；**5.2 Modbus 实际在 M4 完成**（原计划 M3）。原 M1 范围"4.5 基础版"从未实现（M1-M3 通道无认证，M4 直接完整 mTLS）；M2 范围 Flannel 从未实现（缺口 6 未处置）。

> ⚠️ 3.7 ServiceBus（云边 HTTP 调用，Phase 3）计划未明确归属里程碑，建议随 M4 完成（推断）。
> ⚠️ 2.8 NodeJob 未出现在任何里程碑交付物中，建议 M1 后启动、M4 前完成（推断），见 §7 缺口 1。

---

## 4. 首个开发模块：M0 核心（项目骨架 + 基础共享库 + cloudcore 最小可运行）

### 4.1 选择与理由

**首个模块 = WBS 1 中「项目骨架 + 基础共享库」+ 延伸验证「cloudcore 最小可运行程序（/healthz）」**，对应计划模块 1.1（项目骨架，2 人天）+ 1.5（共享库与工具包，4 人天）+ 1.2/1.7 的启动部分。

理由：
1. **无依赖**：M0 在计划 §7.1 中依赖列为"无"，是拓扑排序起点；WBS 1 是 M0 里程碑主体（计划 §7.2 M0 交付物）。
2. **全项目前提**：1.1 项目骨架是所有模块的载体；1.5 共享库（日志/配置/错误处理）被所有模块复用，先做避免各模块各自实现（计划 WBS 1.5 原文）。
3. **最早可验证**：cloudcore 最小常驻程序把 M0 验收标准"`make build` 在本地和 CI 均通过"从"能编译"提升为"能运行 + 可探活"，为 M1 的 CloudHub 开发提供可运行底座。
4. **Vibe Coding 友好度最高**：计划 §5.4 将"项目骨架"列为 🟢 高加速（1.3-1.5×），适合新手起步建立信心与流程。

### 4.2 任务拆解（每项：做什么 → 产出文件 → 验收标准）

> 状态列：✅ 已完成（2026-08-13 全部完成，commit 见各行）｜ ⬜ 待做。括号内为对应 WBS 条目。

| # | 任务（WBS） | 做什么 | 产出文件 | 验收标准 | 状态 |
|---|------------|--------|----------|----------|------|
| T0 | 项目骨架初始化（1.1） | go.mod、目录结构、Makefile、占位程序、README、git 初始化 | go.mod、Makefile、cmd/cloudcore/main.go、cmd/edgecore/main.go、.gitignore、README.md | `go build ./...` 通过；`make build` 产出 bin/ 下两个二进制 | ✅ |
| T1 | 共享库-日志（1.5） | 日志初始化包（klog 或标准 log 封装），支持级别控制与格式化输出；cloudcore 启动日志接入 | pkg/log/log.go、pkg/log/log_test.go | `go test ./pkg/log/...` 通过；`./bin/cloudcore` 启动日志走该库且可分级 | ✅（commit `98a50a6`） |
| T2 | 共享库-配置（1.5） | 配置文件解析（JSON）+ flag/环境变量覆盖 + 默认值兜底 + 端口 1-65535 校验 | pkg/config/config.go、pkg/config/config_test.go、config/cloudcore.json（示例） | `go test ./pkg/config/...` 通过（覆盖默认值/文件/env/flag/非法端口/解析错误）；缺省配置时启动使用默认值；覆盖用例（文件/flag/环境变量）通过 | ✅（commit `f49ce32`） |
| T3 | 共享库-版本信息（1.6 启动） | pkg/version 包；Makefile 通过 `-ldflags` 注入 version/gitCommit/buildTime；替换 main.go 硬编码 const | pkg/version/version.go、pkg/version/version_test.go、Makefile（VERSION/LDFLAGS 扩展）、cmd 改造 | `make build` 后 `./bin/cloudcore --version` 输出 `v0.1.0 + gitCommit + buildTime`；`go test ./pkg/version/...` 通过 | ✅（commit `98a50a6`） |
| T4 | cloudcore 最小可运行程序（1.1 延伸） | main.go 改为常驻服务：加载配置、初始化日志、启动 HTTP server 提供 `/healthz`（含 handler 单元测试）；edgecore 保持可编译 | cmd/cloudcore/main.go（重写）、pkg/httpx/healthz.go、pkg/httpx/healthz_test.go（healthz 实现于 pkg/httpx，替代计划的 internal/server，见 CODE-REVIEW.md） | `./bin/cloudcore` 常驻运行；`curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz` 返回 `200`（healthz 管理端口默认 8080，见 §4.3 端口口径说明） | ✅（commit `98a50a6`、`f49ce32`） |
| T5 | 单元测试与覆盖率基线（8.1 启动） | 为 pkg/log、pkg/config、pkg/version、healthz handler 编写测试；Makefile test 目标加 `-cover` | 各包 `_test.go`、Makefile | `go test -race -cover ./...` 全部通过；全仓总覆盖率 87.5%（pkg 四包 100%，cmd/cloudcore 88.2%；计划 §8.1：核心包 ≥70%） | ✅（commit `98a50a6`、`f49ce32`） |
| T6 | golangci-lint 接入（1.7） | .golangci.yml 配置（启用 errcheck、govet、staticcheck 等基础 linter）；Makefile lint 目标改为强制 | .golangci.yml、Makefile | `golangci-lint run ./...` 无 error（0 条）；`make lint` 通过；CI 中 lint 失败即失败（commit `ab73062`） | ✅（commit `98a50a6`、`ab73062`） |
| T7 | CI 完善（1.2 启动） | CI 增加 lint job、覆盖率检查（阈值 70%）、缓存 Go 模块 | .github/workflows/ci.yml | push 后 CI 全绿；PR 反馈 ≤10min（M0 验收标准）；CI 中 lint 失败即失败 | ✅（commit `ab73062`） |
| T8 | 首模块验收与记录 | 按 §4.3 完成标准逐项验证；记录结果到 docs/（本 ROADMAP 勾选状态） | docs/ROADMAP.md（状态更新）、docs/CODE-REVIEW.md | §4.3 全部通过（7/7 条件全部满足，见 CODE-REVIEW.md 复验） | ✅（commit `c4d417b`） |

### 4.3 首模块完成标准（全部可命令验证）

| # | 完成标准 | 验证命令 | 状态 |
|---|----------|----------|------|
| 1 | 编译通过 | `go build ./...` 无 error | ✅ |
| 2 | 测试通过且覆盖率达标 | `go test -race -cover ./...` 全部通过，核心包覆盖率 ≥70%（实测全仓总覆盖率 87.5%） | ✅ |
| 3 | cloudcore 可运行且可探活 | `./bin/cloudcore` 启动后 `curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/healthz` 返回 `200`；进程保持常驻（Ctrl+C 优雅退出） | ✅ |
| 4 | 静态检查无 error | `make lint`（golangci-lint run ./...）0 error；CI 中 lint 失败即失败 | ✅ |
| 5 | 版本信息可注入 | `./bin/cloudcore --version` 输出版本号（非硬编码 const） | ✅ |
| 6 | 不回归 | `make build` 同时产出 cloudcore + edgecore 两个二进制；CI 全绿（lint + vet + build + test -race + 覆盖率门槛） | ✅ |
| 7 | 配置生效 | 修改配置文件 `config/cloudcore.json` 中端口后重启，healthz 监听新端口（验证配置库真实可用） | ✅ |

> **端口口径说明**：**healthz 管理端口默认 8080**（可用 `config/cloudcore.json` / 环境变量 `EDGEFLOW_CLOUDCORE_PORT` / `--port` 覆盖）；**云边通信端口 10000 是 M1 的内容，不属于 M0**。

> 首模块**边界之外**（属于 M0 后续任务，不做）：CRD 定义（1.4）、完整 CI 多架构构建（1.2 剩余）、开发环境搭建（1.3）、Helm 骨架（8.5）、架构文档评审（9.1）。

---

## 5. 各模块完成标准（可验证）

### WBS 1 基础架构（M0）
- `make build` 本地 + CI 均通过
- 首个 CRD 定义可 `kubectl apply` 创建成功（`kubectl get crd` 可见）
- CI PR 反馈 ≤10 分钟
- 架构文档经评审通过（有评审记录）
- Helm Chart 骨架存在且 `helm lint` 通过

### WBS 2 CloudCore（M1-M4）
- `./bin/cloudcore` 常驻运行，`/healthz` 返回 200
- 边缘节点注册后 `kubectl get nodes` 状态 Ready（配合 WBS 3）
- 心跳上报间隔 ≤30s（计划 §7.2 M1 验收）
- 单元测试覆盖率 ≥70%（计划 §8.1）
- NodeJob 任务分发可下发到边缘并返回执行结果（2.8，⚠️ 归属待确认）

### WBS 3 EdgeCore（M1-M3）
- `./bin/edgecore` 启动并成功连接云端 CloudHub
- MetaManager 将元数据持久化到 SQLite，重启后数据仍在（`sqlite3` 可查询 / 重启验证）
- 断网 30 分钟容器持续运行无中断、恢复后 120s 内状态同步完成（3.4，计划 §7.2 M2 验收）
- 单元测试覆盖率 ≥70%

### WBS 4 云边通信层（M1-M4）
- `keadm join` 完成节点注册（计划 §7.2 M1 验收）
- 断线 60s 内自动重连成功（计划 §7.2 M1 验收）
- QoS 1 消息不丢失（ACK/重试生效；配合 NATS/MQTT POC 结论）
- mTLS 握手成功、证书轮换后连接不中断（4.5，M4）

### WBS 5 设备映射器（M3）
- DeviceModel/Device CRD 可创建并驱动 Mapper 实例（计划 §7.2 M3 验收）
- MQTT Mapper 可连接模拟设备完成读写
- 设备状态端到端上报延迟 ≤5s（计划 §7.2 M3 验收）

### WBS 6 应用管理（M2）
- `kubectl apply` 可将应用部署到边缘节点（计划 §7.2 M2 验收）
- ConfigMap/Secret 变更可正确下发到边缘
- 滚动更新与回滚可执行（6.4）

### WBS 7 安全与认证（M4）
- 证书 CA 初始化、签发、轮换全流程可执行
- 节点 mTLS 认证通过；未认证节点连接被拒绝
- RBAC 规则生效（越权请求被拒）
- 审计日志产生可查询记录

### WBS 8 测试与部署（贯穿-M5）
- 单测覆盖率 ≥70%（核心包 ≥80%）（计划 §8.1）
- E2E 7 大场景全部通过：基础部署、边缘自治、设备管理、多节点(10+)、故障恢复、升级、安全认证（计划 §8.1）
- `helm install` 一键部署 CloudCore（计划 §7.2 M4 验收）
- `keadm join` 一键注册（含证书自动签发）
- 100 节点压测：注册成功率 ≥99%、消息延迟 ≤3s；EdgeCore 空闲内存 ≤256MB（计划 §7.2 M4 验收）

### WBS 9 文档与示例（M5）
- 新用户按快速入门指南 15 分钟内完成部署（计划 §7.2 M5 验收）
- 所有 CRD 字段有文档说明
- 温度传感器 Demo 完整跑通

### WBS 10 审查补充项（M4）
- 监控/日志/告警链路可用（Prometheus 采集到指标、Fluent Bit 采集到日志）
- 边缘节点 OTA 升级与回滚演练通过（灰度一批、失败回滚）
- K8s API 兼容矩阵验证通过（计划 §9.1 风险 7：Phase 1 确定支持版本范围）
- 镜像扫描无高危漏洞

---

## 6. 开发顺序速查（首 8 周建议，5 人团队参照计划 §7.3 甘特图）

| 周次 | 主任务 | 对应里程碑 |
|------|--------|-----------|
| 第 1 周 | 首模块 T1-T4（共享库 + healthz）；POC：Edged 方案 A 可行性（计划 §9.2） | M0 |
| 第 2 周 | 首模块 T5-T8（测试/lint/CI）；POC：NATS MQTT 兼容性；SQLite 基准测试；Vibe Coding 加速因子校准 | M0 |
| 第 3-4 周 | 1.4 CRD/API 规范、1.3 开发环境、1.6 构建脚本、9.1 架构文档 → **M0 收尾**；启动 4.1 消息协议设计 | M0→M1 |
| 第 5-8 周 | 4.2 连接管理、2.1 CloudHub ∥ 3.1 EdgeHub（并行） | M1 |

---

## 7. 缺口与风险（计划中信息不足项）

| # | 缺口 | 影响 | 建议处理 |
|---|------|------|----------|
| 1 | **2.8 云端任务管理（NodeJob CRD）**：未出现在任何里程碑交付物中，归属不清 | 排期遗漏风险 | M1 后启动、M4 前完成（本 ROADMAP 暂按此排，需确认） |
| 2 | **2.2 DeviceController 与 5.3 设备 CRD 控制器职责重叠** | 重复开发/职责冲突 | 开发前明确：2.2 偏云端状态同步，5.3 偏 CRD 脚手架，共享同一 CRD 定义 |
| 3 | **2.9/3.8 与 10.1 可观测性职责重叠**（4+6+10 人天三处） | 重复建设 | 统一为 10.1 主方案，2.9/3.8 仅做本侧指标暴露点 |
| 4 | **4.5 安全传输层 与 7.1/7.4 证书管理边界** | 责任不清 | 4.5 负责传输实现，7.1/7.4 负责证书生命周期与认证策略 |
| 5 | **3.2 Edged 实现策略未定**（P0 决策项，方案 A/B/C） | 30 人天工作量 ±18 人天，M2 阻塞 | 计划 §9.2：第 1 周 POC 后定案；未定前按方案 A 估算 |
| 6 | **Flannel（M2 交付物）在 WBS 中无对应条目** | 排期遗漏 | 归入 1.3 开发环境扩展或 6.x 实施，需确认 |
| 7 | **模块级依赖关系计划未显式声明**（本表 ⚠️ 项均为推断） | 并行排期可能失真 | 每阶段开工前用 1-2 小时确认依赖后再拆任务 |
| 8 | **8.x 测试人天在里程碑间分配未细化**（30 人天分散 M1-M5） | 里程碑验收可能缺测试资源 | 按"模块完成即带测试"原则，8.1 随模块走，8.2-8.4 按里程碑验收场景分配 |
| 9 | **3.7 ServiceBus 里程碑归属不明**（Phase 3 功能） | 排期遗漏 | 暂排 M4（推断），需确认 |
| 10 | **1.3 开发环境工具链未细化**（kind/Docker 模拟边缘节点的具体方案） | 新手入门门槛 | 首模块完成后立即明确：kind + Docker 方案（计划 §8.2 已有方向） |

---

*本文档仅基于《EdgeFlow-开发计划.md》v1.0 编制；完成标准均取自计划 §7.2 里程碑验收标准、§8.1 测试策略及 WBS 原文；标注 ⚠️ 的依赖与归属推断项需在开发中确认。*
