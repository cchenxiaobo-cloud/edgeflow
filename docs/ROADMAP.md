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
| WBS 2 | CloudCore 开发 | 63 | WBS 1、WBS 4（⚠️ CloudHub 需消息协议与连接管理） | 🟨 主体完成；2.8 NodeJob 已决策关闭（v0.1.0 范围外，协议占位标注，audit-m35 G7）；2.9 可观测性完成（合并 10.1，commit `4c5b9c6`） |
| WBS 3 | EdgeCore 开发 | 65 | WBS 1、WBS 4（⚠️ EdgeHub 需消息协议与连接管理） | 🟨 主体完成；3.7 ServiceBus 决策关闭（Phase 3 范围外）；3.8 可观测性完成（合并 10.1） |
| WBS 4 | 云边通信层 | 28 | WBS 1（协议定义在 WebSocket 通道之前，计划 §7.4 关键路径） | ✅ 完成（4.4 gzip 压缩 2026-08-15 完成、Protobuf 编码延后；4.5 实际在 M4 完成） |
| WBS 5 | 设备映射器框架 | 22 | WBS 1（CRD）、WBS 2.2、WBS 3.5/3.6（⚠️ 数据流：DeviceTwin ← EventBus ← MetaManager） | ✅ 主体完成（v0.2.0 补 5.1 EventBus 装配开关 commit `238b0cc` + Modbus ns 解析 commit `566aff9`）；5.2 OPC-UA ✅ 第三阶段完成（v0.3.0 M1 协议栈核心 → v0.14.0 端到端 Mapper v1 → **v0.15.0 Subscription 订阅推送 + Browse 浏览发现**：值变化即时采集与一键点位发现，缺省行为不变，2026-08-27）；5.3 控制器决策关闭（§8） |
| WBS 6 | 应用管理 | 20 | WBS 3（⚠️ Edged/MetaManager 就绪后） | ✅ 完成（v0.2.0 补齐 6.5 资源调度基础）；6.4 镜像漂移检测+重建（commit `a0a4344`）；6.5 调度基础 P2 全量（commits `5cb7336`/`d3f09fe`） |
| WBS 7 | 安全与认证 | 18 | WBS 1；7.4 依赖 WBS 4.5（⚠️） | ✅ 7.1 证书管理（mTLS）+7.2 Token 认证+7.3 设备认证（2026-08-15 闭环）+7.4+7.5 审计台账完成；吊销闭环完成（CRL 离线 + OCSP 在线，2026-08-16） |
| WBS 8 | 测试与部署 | 30 | 随各模块并行；8.5 依赖 WBS 2 产物；8.6 依赖 WBS 4 协议（⚠️） | 🟨 8.1/8.2/8.3/8.5 完成（E2E 三用例 commit `a0a4344`）；8.4 压测基线完成（10 节点 100%，PERFORMANCE-BASELINE.md）；8.6 基础+升级回滚（实际在 M4） |
| WBS 9 | 文档与示例 | 14 | WBS 1（可全程并行） | ✅ 9.2-9.5 完成；9.1 架构文档 2026-08-15 评审闭环并回写 |
| WBS 10 | 审查补充项 | 24 | WBS 2/3（可观测性）、WBS 8.6（升级回滚，⚠️） | 🟨 10.1 可观测性完成、10.2 升级回滚+灰度分批（2026-08-15）、10.3 兼容矩阵完成（2026-08-15）+ 自动化契约测试（2026-08-16，tests/contract）、10.4 部分（SBOM+镜像扫描完成 2026-08-15；cosign 延后） |

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
| 2.2 | DeviceController | 设备 CRD 控制器、状态同步 | 10 | 1.4、5.3（⚠️ 同一 CRD） | 🟨 部分：云端设备状态存储 + 查询/指令 API（内存态，commit `744afaa`）；K8s 原生控制器不适用（REST 适配架构，§8 5.3 决策关闭） |
| 2.3 | EdgeController | 边缘节点注册、状态管理 | 8 | 2.6（⚠️ 需 apiserver 交互） | ✅ 完成（REST 化适配：registry + EdgeNode 映射，commits `3c7b99d`/`641863e`） |
| 2.4 | NodeController | 心跳监控、节点上下线 | 6 | 2.3（⚠️ 注册后才有心跳） | ✅ 完成（commit `f71684e`，覆盖率 96.8%；**实际在 M4 完成**，M1 仅 cloudhub 内嵌断线检测） |
| 2.5 | CloudCore API 层 | RESTful API、认证中间件 | 5 | 2.6（⚠️） | ✅ 完成（11 个 REST 端点 + /healthz + /metrics，见 API-SPEC.md；认证中间件 2026-08-15 已闭环，归 WBS 7.2） |
| 2.6 | 云端元数据存储 | K8s apiserver 交互、缓存层 | 6 | 1.x | ✅ 完成（内存 registry + REST API，无 apiserver/etcd，已文档化为适配） |
| 2.7 | CloudCore 配置管理 | 动态配置、热重载 | 4 | 1.5（⚠️） | ✅ 文件/flag/env 配置 + 热重载完成（2026-08-15：SIGHUP + 60s mtime，端口/周期热切换，fail-safe，commit `2d0a903`） |
| 2.8 | 云端任务管理 | NodeJob CRD、任务分发 | 8 | 2.1、2.3（⚠️） | 🔒 已关闭（v0.1.0 范围外产品决策，协议占位标注关闭，audit-m35 G7） |
| 2.9 | 可观测性基建 | 监控指标、日志收集、告警 | 4 | 2.1（⚠️） | ✅ 完成（与 10.1 合并：/metrics 端点五指标，commit `4c5b9c6`） |
| 3.1 | EdgeHub 模块 | WebSocket 客户端、消息路由、断线重连 | 12 | 1.x、4.1、4.2（⚠️） | ✅ 完成（commits `7b1c27a`/`19dd66f`/`0a7fcc2`） |
| 3.2 | Edged 模块 | 轻量容器运行时管理、Pod 生命周期 | 30 | ⛔ **Edged 策略 P0 决策**（方案 A/B/C）；3.3（⚠️ 数据流 EdgeHub→MetaManager→Edged） | ✅ 完成（方案 A：DockerRuntime + Mock + 声明式 reconcile + 多副本 + 健康自愈 + CrashLoopBackOff；containerd CRI 实现 P2 延后） |
| 3.3 | MetaManager | 本地元数据存储(SQLite)、数据同步 | 8 | 1.5（⚠️） | ✅ 完成（SQLite WAL + KV + Pod 持久化 + 增量订阅，commits `3aaaf28`/`089c358`） |
| 3.4 | 边缘自治引擎 | 断网独立运行、重连后同步、冲突解决 | 15 | 3.2、3.3、4.6（⚠️） | ✅ 完成（基础：断网 40s 容器持续运行 + 恢复同步；**30min 时长未验证**，归 WBS 8.3 缺口，audit-m02 #40） |
| 3.5 | DeviceTwin 模块 | 设备影子、状态同步 | 6 | 3.6、3.3（⚠️） | ✅ 完成（devicetwin 覆盖率 100%，commit `744afaa`） |
| 3.6 | EventBus 模块 | MQTT 消息总线 | 4 | ⚠️ NATS MQTT 兼容性 POC（开发前 1-2 天，计划 §4.2.4） | ✅ 完成（mosquitto + paho，commit `2a0d0a3`；**NATS POC 未执行**，改用 mosquitto，已文档化偏差） |
| 3.7 | ServiceBus 模块 | 边缘服务发现、路由 | 4 | 4.x（⚠️ 云边 HTTP 调用，Phase 3） | 🔒 已关闭（v0.1.0 范围外产品决策，Phase 3 云边 HTTP 调用后续版本排期；§8 处置登记 2026-08-15） |
| 3.8 | 可观测性(边缘) | 指标暴露、日志上报 | 6 | 3.1（⚠️） | ✅ 完成（与 10.1 合并：/metrics 端点五指标，commit `4c5b9c6`） |
| 4.1 | 消息协议设计 | 消息格式(Protobuf)、类型枚举 | 3 | 1.x | ✅ 完成（pkg/protocol JSON 信封 + 类型枚举，commit `e569ea1`） |
| 4.2 | 连接管理 | 连接池、心跳保活、重连退避 | 6 | 4.1（⚠️） | ✅ 完成（注册/心跳 30s/指数退避 2/4/8s/自动重连） |
| 4.3 | 消息路由引擎 | 主题路由、消息过滤 | 5 | 4.1、4.2（⚠️） | ✅ 完成（SendToNode/Broadcast/Deliver，commit `081eb0f`） |
| 4.4 | 数据压缩与序列化 | 消息压缩、增量同步 | 4 | 4.1（⚠️） | 🟨 序列化（JSON）✅、增量同步部分实现；**gzip 压缩已实现**（2026-08-15：协商式双向，commit `37f34f9`）；Protobuf 编码延后（规模化阶段） |
| 4.5 | 安全传输层 | TLS/MTLS、证书轮换 | 5 | 7.1（⚠️） | ✅ 完成（完整 mTLS，commit `0a7fcc2`；**实际在 M4 完成**，与 7.1/7.4 合并交付，M1-M3 通道无认证） |
| 4.6 | 消息可靠投递 | ACK、重试、幂等 | 5 | 4.1、4.2（⚠️） | ✅ 完成（ReliableSend + Ack + 同 ID 重试 + 幂等去重 + P2 收尾，commits `3197ad3`/`19dd66f`/`8321b0e`） |
| 5.1 | Mapper 框架 | Mapper SDK、设备注册/注销 | 6 | 1.4（⚠️） | ✅ 完成（DeviceMapper + MapperRegistry，commit `7d82c0c`，覆盖率 96.4%；**EventBus 装配开关** `EDGEFLOW_EDGECORE_ENABLE_MAPPER` 默认 true、false/0/off/no 回影子模式，commit `238b0cc`） |
| 5.2 | 设备协议适配 | Modbus、OPC-UA、MQTT 适配器 | 5 | 5.1（⚠️） | 🟨 部分：Modbus ✅（commit `a290686`，**实际在 M4 完成**；DeviceNamespaceResolver + `EDGEFLOW_MODBUS_NAMESPACE` 补齐 commit `566aff9`）；OPC-UA 🟨 独立立项已启动（v0.3.0 M1 交付 pkg/opcua：UA Binary 协议栈核心，零第三方依赖，41 顶层测试，commit `93458d6`；下一里程碑：SecureChannel/Read/Write/第三方 UA 栈互操作 cross-check）；MQTT 仅 mock_sensor 数据面模式（audit-m35 G7） |
| 5.3 | 设备 CRD 控制器 | Device/DeviceModel CRD | 4 | 1.4（⚠️） | 🟨 部分：CRD 类型定义 ✅（commit `a541128`）+ manifest（`config/crd/`，2026-08-14）；K8s 控制器 ⬜（audit-m35 G6） |
| 5.4 | 设备数据流水线 | 采集→转换→上报→持久化 | 4 | 5.1、3.3（⚠️） | ✅ 完成（采集→Twin 合并→周期上报→云端存储 + op_ledger） |
| 5.5 | 设备示例 Mapper | 完整 Mapper 示例 | 3 | 5.2（⚠️） | ✅ 完成（mock_sensor 91.1% + modbus mapper 示例） |
| 6.1 | 边缘应用部署 | Pod 部署、Deployment 支持 | 5 | 3.2（⚠️） | ✅ 完成（PodSync 下发→订阅触发→Edged 调谐→真实 Docker 容器，commits `15366e7`/`9fe47c1`） |
| 6.2 | 配置下发 | ConfigMap/Secret 同步 | 3 | 3.3、4.x（⚠️） | ✅ 完成（五态 + 可靠投递 + 落盘 + 自动 Ack + Secret 脱敏，commit `5403daa`） |
| 6.3 | 状态上报 | Pod 状态上报、健康检查 | 3 | 3.2（⚠️） | ✅ 完成（30s 周期上报 + Absent 终态保留 90s + 云端收敛，commits `707128f`/`bc58e40`） |
| 6.4 | 边缘应用升级 | 滚动更新、回滚 | 4 | 6.1（⚠️） | ✅ 完成（健康自愈/重启 commit `47d9e21`；镜像漂移检测+重建 commit `a0a4344`，批大小 1 逐轮滚动；回滚经 re-deploy 语义覆盖） |
| 6.5 | 资源管理 | 边缘调度、资源超卖 | 5 | 6.1（⚠️） | ✅ 完成（v0.2.0，commits `5cb7336` + 调谐接入 `d3f09fe`）：request/limit 下发与校验、超卖率校验与拒绝（默认 150% 可调）、节点容量探测/覆盖、docker --cpus/--memory 落地、资源漂移检测重建 |
| 7.1 | 证书管理 | CA 初始化、签发、轮换 | 5 | 1.x | ✅ CA/签发/校验/幂等 ✅；轮换自动化（2026-08-15 keadm cert rotate）；吊销闭环完成（2026-08-16）：CRL（RevokeCert/VerifyCertAgainstCRL + mTLS 消费端 + keadm cert revoke [--node/--serial]）+ OCSP（/ocsp responder RFC 6960 + 客户端查询），健壮性补强（文件锁/对账自愈/幂等 CRL Number） |
| 7.2 | RBAC | 角色/权限定义 | 3 | 1.4（⚠️） | ✅ 完成（Token 认证中间件+审计留痕，默认 off 向后兼容，commit `4c5b9c6`；完整 RBAC 角色模型为 P2） |
| 7.3 | 设备认证 | 设备身份注册、Token 认证 | 4 | 5.x（⚠️） | ✅ 已实现（2026-08-15 闭环：Register 携带 token，云端 EDGEFLOW_CLOUDCORE_NODE_TOKEN 常数时间校验，commit `7f5b3d3`） |
| 7.4 | 云边认证 | MTLS 握手、节点验证 | 3 | 7.1、4.5（⚠️） | ✅ 完成（双向强制 + peer CN 审计 + 拒绝路径，commit `0a7fcc2`） |
| 7.5 | 审计日志 | 操作审计、安全事件 | 3 | 7.1（⚠️） | ✅ 完成（audit-ledger.jsonl 审计台账：JSONL 追加写、失败不阻断 API，commit `4c5b9c6`） |
| 8.1 | 单元测试 | 核心模块测试、覆盖率≥70% | 8 | 随各模块并行 | ✅ 完成（24 包 race 全绿，总覆盖率 77.8% ≥70%） |
| 8.2 | 集成测试 | 云边联调、设备管理端到端 | 6 | 2.x、3.x（⚠️） | 🟨 云边联调/设备链路单节点 ✅；**多节点（10+）E2E 未做**（audit-m35 G11） |
| 8.3 | E2E 测试 | 多节点、故障恢复、自治 | 5 | 3.4（⚠️） | ✅ 完成（tests/e2e 三用例：自治 60s 短时模拟/设备链路/多节点隔离，真实进程+真实 Docker，commit `a0a4344`；30min 需真实环境长跑） |
| 8.4 | 性能测试 | 节点规模模拟、延迟测试 | 4 | 8.2（⚠️） | ✅ 完成（hack/load-test：10 节点 100% 注册、均值 1.85ms、P95 2.28ms + N=100 本机回环 100% 注册、均值 9.62ms、P95 13.24ms，PERFORMANCE-BASELINE.md；集群级验收需真实环境复测） |
| 8.5 | Helm Chart | Chart 编写、多环境 values | 3 | 2.x 产物（⚠️） | ✅ 完成（骨架 `9d78246` + 完整化 `3bb60ce`，helm lint 0 failed） |
| 8.6 | keadm 安装工具 | 安装工具、注册流程、升级 | 4 | 4.x（⚠️ 注册协议） | 🟨 基础 init/join/reset/version + upgrade/rollback/ops-ledger ✅；**批量操作 ✅**（2026-08-15 batch join/upgrade/rollback，commit `7f5b3d3`）；配置迁移=备份保全（audit-m35 #23/G8） |
| 9.1 | 架构文档 | 设计文档、模块说明 | 3 | 1.x | ✅ 已评审并回写（2026-08-15：NATS→MQTT、Token→mTLS 演进、实现进度核对，commit `1ae2d81`） |
| 9.2 | API 文档 | API 参考、CRD 文档 | 2 | 1.4（⚠️） | ✅ 完成（API-SPEC.md 13 端点 + 错误码 + CRD 全字段，commit `f090a30`） |
| 9.3 | 部署指南 | 快速开始、生产部署 | 3 | 8.5（⚠️） | ✅ 完成（DEPLOYMENT.md，REVIEWS.md R9.3 已处理） |
| 9.4 | 开发者指南 | 贡献指南、代码规范 | 2 | 1.x | ✅ 完成（HANDOFF.md，REVIEWS.md R9.4 已处理） |
| 9.5 | 示例与教程 | 设备接入、应用部署示例 | 4 | 5.x、6.x（⚠️） | ✅ 完成（examples/demo.sh DEMO PASS×3，REVIEWS.md §5） |
| 10.1 | 可观测性基建 | 监控(Prometheus)、日志(Fluent Bit)、告警 | 10 | 2.9、3.8（⚠️ 职责需与 2.9/3.8 对齐） | ✅ 完成（/metrics 端点：nodes/pods/devices_total、http_requests_total、active_connections，Prometheus 文本格式，metrics 覆盖 96.6%，commit `4c5b9c6`；日志/告警为 P2） |
| 10.2 | 边缘节点升级与回滚 | OTA 升级、灰度发布 | 6 | 8.6（⚠️） | ✅ 完成（备份模型 + ops-ledger + simulate-failure + 事务化 restore + 白名单；**灰度发布已实现** 2026-08-15：upgrade --batch-size/--pause-between，commit `2c877d4`；**服务端模型灰度（v0.7.0）落地**——模型灰度发布与产品升级灰度双轨，见 §12 处置登记） |
| 10.3 | API 兼容性测试 | K8s API 版本兼容矩阵 | 4 | 2.x（⚠️） | ✅ 已完成：兼容矩阵文档（API-COMPATIBILITY.md，2026-08-15）+ 自动化契约测试（tests/contract：13 REST 端点 + 10 消息类型双向断言 + 文档一致性 + 运行时反向探测，2026-08-16） |
| 10.4 | 安全扫描与修复 | 镜像扫描、依赖漏洞 | 4 | 8.5（⚠️） | 🟨 部分：SBOM ✅（33 组件）+ 镜像漏洞扫描 ✅（2026-08-15 Trivy 0 漏洞）；cosign 签名延后（audit-m35 G10） |

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

---

## 8. 未完成项处置登记（2026-08-15 后续开发轮）

> 依据：CLOSE-OUT-ACTIONS.md（2026-08-15 收尾轮）+ 本次后续开发任务执行结果。状态：✅ 本次完成 / 🔒 决策关闭 / ⏸ 环境边界 / ⏳ 延后排期。

| 项 | 对应 WBS | 处置 | 结论/理由 |
|----|----------|------|----------|
| 2.7 配置热重载 | WBS 2.7 | ✅ 本次完成 | SIGHUP + 定时 mtime 重载，端口类变更警告需重启（见 commit） |
| 4.4 gzip 消息压缩 | WBS 4.4 | ✅ 本次完成 | 云边通道压缩开关（见 commit） |
| 7.1 证书轮换自动化 | WBS 7.1 | ✅ 本次完成 | keadm cert rotate（见 commit） |
| 10.2 灰度发布 | WBS 10.2 | ✅ 本次完成 | keadm upgrade 分批/灰度参数（见 commit） |
| M1B/M1C/M1/M3A 审查遗留 P2/P3 | WBS 8.x | ✅ 本次完成 | LIKE 通配/同 ID pending/错误映射/operation 校验/请求体限制/serveWS 竞态/ReadLimit/Memory 类型/newID/Offline TTL-GC（见 commit） |
| 9.1 架构文档评审回写 | WBS 9.1 | ✅ 本次完成 | ARCHITECTURE.md 评审 + NATS→MQTT/Token→mTLS/进度回写（见 commit） |
| 3.7 ServiceBus | WBS 3.7 | 🔒 决策关闭 | Phase 3 云边 HTTP 调用为 v0.1.0 范围外；协议占位标注，后续版本排期 |
| 5.2 OPC-UA 适配器 | WBS 5.2 | 🟨 独立立项已启动（M1 交付） | v0.2.0 确认独立立项只登记不开发（2026-08-18）；v0.3.0 启动并交付第一阶段：pkg/opcua UA Binary 协议栈核心（零第三方依赖，41 测试，commit `93458d6`，2026-08-19），见 §11 |
| 5.3 设备 CRD K8s 控制器 | WBS 5.3 | 🔒 决策关闭 | 项目为 REST 适配架构（无 K8s apiserver），云端状态同步已由 2.2（REST 端点+内存态）覆盖；K8s 原生控制器不适用 |
| 6.5 资源调度/超卖 | WBS 6.5 | ✅ 本次完成 | request/limit 下发与校验、超卖率校验与拒绝（默认 150% 可调）、节点容量探测/覆盖、资源漂移检测重建（commit `5cb7336`；调谐路径接入 `d3f09fe`，2026-08-18） |
| Flannel/CNI（缺口 6） | M2 交付物 | 🔒 决策关闭 | Docker bridge 方案为 v0.1.0 既定网络形态（REAL-CLUSTER-GUIDE.md 已验证）；CNI 排后续版本 |
| 10.4 cosign 签名 | WBS 10.4 | ⏸ 环境边界 | 需镜像仓库+cosign 签名基础设施；已登记 MULTIARCH.md §5 |
| 100 节点压测 | WBS 8.4 | ⏸ 环境边界 | 集群级验收需真实环境复测；本机 N=100 回环已跑通（100% 注册、均值 9.62ms、P95 13.24ms，PERFORMANCE-BASELINE.md） |
| 30min 长跑自治 | WBS 8.3 | ⏸ 环境边界 | 需真实环境长跑；60s E2E 已验证（tests/e2e 三用例 PASS） |
| 8.2 多节点(10+) E2E | WBS 8.2 | ⏸ 环境边界 | 单机资源限制；多节点隔离用例已在 E2E 覆盖（2 边缘节点 + 1 云端进程） |
| M1/M2 "kubectl get nodes/apply" 字面验收 | 验收口径 | 🔒 决策修订 | 无真实 K8s 接入，验收以 REST API 适配 + kind 真实集群路径（REAL-CLUSTER-GUIDE.md，kubectl apply + keadm init + Ready）双重达成 |
| 7.1 证书吊销（CRL） | WBS 7.1 | ✅ 本次完成 | keadm cert revoke + RevokeCert/VerifyCertAgainstCRL（crl.json 记录 + crl.pem 签名产物，幂等/自愈，16 新用例；2026-08-16） |
| 1.4 OpenAPI v3 schema | WBS 1.4 | ✅ 本次完成 | hack/openapi-gen 生成器（确定性）+ docs/openapi/edgeflow-openapi.yaml（17 schema）+ 一致性校验测试（2026-08-16） |
| 10.3 API 兼容性契约测试 | WBS 10.3 | ✅ 本次完成 | tests/contract：13 REST 端点 + 10 消息类型双向断言 + API-SPEC/API-COMPATIBILITY 文档一致性 + 未注册路径运行时反向探测（6 测试；2026-08-16） |
| 7.1 OCSP 在线吊销 | WBS 7.1 | ✅ 本次完成 | cloudcore /ocsp responder（RFC 6960，encoding/asn1 手写 ASN.1，零第三方依赖）+ 客户端 OCSPStatus 查询 + 验签/responderID/CertID 防伪造链（2026-08-16） |
| 7.1 CRL 健壮性补强 | WBS 7.1 | ✅ 本次完成 | 并发文件锁（flock）、keadm cert revoke --serial 按序列号吊销、幂等重生成 CRL Number 递增、verify 侧 json↔pem 对账自愈、NextUpdate 过期策略、小写归一化、损坏组合测试（2026-08-16） |
| 1.4 OpenAPI 语义锁定 | WBS 1.4 | ✅ 本次完成 | 独立 reflect 对照测试（type/format/required 逐字段断言）+ 空 json tag 回落字段名 + 产物零漂移验证（2026-08-16） |
| 10.3 契约测试运行时探测 | WBS 10.3 | ✅ 本次完成 | 未注册路径运行时反向探测（/api/v1/__probe__ 等 404 断言）+ 临时目录清理 + 历史泄漏目录清除（2026-08-16） |

---

## 9. v0.1.1 发布准备轮处置登记（2026-08-18）

> 依据：审计风险清单（.cluster/8401705d/subagent_04.md：中风险 M1/M2/M3）+ 历次 CODE-REVIEW P2 遗留 + 异常路径演练 + 发布保障。状态：✅ 本次完成 / ⏸ 环境边界。

| 项 | 对应 WBS | 处置 | 结论/理由 |
|----|----------|------|----------|
| M1 /ocsp 防滥用 | WBS 7.1 | ✅ 本次完成 | per-IP 令牌桶限流（默认 10 req/s、burst 20、429 语义、map 有界 1 万 IP）+ Cache-Control: max-age=3600；`EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT` 可调（commit `bc8994d`） |
| M2 OCSP 新鲜度校验 | WBS 7.1 | ✅ 本次完成 | ParseOCSPResponseWithFreshness/OCSPStatusAtWithPolicy fail-closed（过期/未来时间拒绝，5 分钟 skew）；旧签名行为不变；生产路径接线时须用 WithPolicy 入口（commit `bc8994d`） |
| M3 CRL 锁降级可观测 | WBS 7.1 | ✅ 本次完成 | 锁失败降级无锁校验（功能语义不变）+ 5 分钟限频 Warn 日志（commit `bc8994d`） |
| P2 遗留闭环（M1B×9/M1C×5/M3A×6） | WBS 8.x | ✅ 本次完成 | WriteTimeout=15s、Encode 日志 17 处、Broadcast 送达计数、Route namespace、LastReportedAt 单调、ReliableSendContext/downlinkMu 核验补测 + 结论记录注释（commit `59dd396`） |
| 异常路径演练 | WBS 8.x | ✅ 本次完成 | 14 条演练 13✅/1⚠️/0❌（空数据/超时/重复提交/并发冲突/回滚触发）；见 .cluster/a3f7c2e1/subagent_03.md |
| 生产发布保障文档 | WBS 10.x | ✅ 本次完成 | release-prep-v011 / monitoring-alerting-v011（11 条告警规则）/ rollback-runbook-v011（见 .cluster/a3f7c2e1/，入档见 S6） |
| 多节点真实集群验证 | WBS 8.2 | ⏸ 环境边界 | 与上轮一致：需真实多节点基础设施（DRILL-SCHEDULE） |
| cosign 签名 | WBS 10.4 | ⏸ 环境边界 | 与上轮一致：需镜像仓库+签名基础设施（MULTIARCH.md §5） |

---

## 10. v0.2.0 开发轮处置登记（2026-08-18）

> 依据：.cluster/a1c29599/plan.md 派单（4 开发项 + 明确不做项）+ 代码复核（0 blocker/1 major 已修）+ 缺陷修复两批 + 全量回归（全仓测试 + 双 e2e 全绿）+ 预发冒烟 8/8 PASS。状态：✅ 本次完成 / ⏳ 独立立项（本轮不开发）。

| 项 | 对应 WBS | 处置 | 结论/理由 |
|----|----------|------|----------|
| 6.5 资源调度基础（P2 全量） | WBS 6.5 | ✅ 本次完成 | pod.resources 四字段（cpuRequest/cpuLimit/memoryRequest/memoryLimit，K8s 风格，omitempty 零值=不限制，云边契约只增不改）；云端前置校验（request>limit → 400）；边缘准入（超卖拒绝不落盘 → 云端 409 `EDGEFLOW_RESOURCE_EXHAUSTED`）；超卖率默认 150%（`EDGEFLOW_EDGECORE_OVERCOMMIT_CPU_RATE/MEMORY_RATE` 可调，非有限值回退）；节点容量探测/覆盖（`EDGEFLOW_EDGECORE_NODE_CPU_MILLI/NODE_MEMORY_BYTES`）；docker --cpus/--memory 落地；资源漂移检测重建（commit `5cb7336`；调谐路径接入 `d3f09fe`） |
| Mapper 框架 EventBus 装配开关 | WBS 5.1 | ✅ 本次完成 | `EDGEFLOW_EDGECORE_ENABLE_MAPPER`（默认 true，与 v0.1.1 制品行为一致；false/0/off/no 大小写不敏感关闭，回纯影子模式）（commit `238b0cc`） |
| Modbus DeviceNamespaceResolver | WBS 5.2（Modbus） | ✅ 本次完成 | 命名空间三级解析：WithNamespace 选项 > `EDGEFLOW_MODBUS_NAMESPACE`（默认 default）> default；注册表按 namespace/deviceName 路由隔离，同名设备按 ns 互不串扰（commit `566aff9`） |
| M1 P3 四项收尾 + log SetLevel | WBS 8.x | ✅ 本次完成 | pkg/log SetLevel/GetLevel/Debugf（atomic 全局级别过滤，默认 LevelInfo 与旧行为逐字节兼容）；被踢标志即时清除（kick 立即 registered.Store(false)）；Shutdown 撞 Start 窗口防护补测；退避重置两段式断言+flakyDialer 注入；newID 兜底经核验前轮（`d9bc9ec`）已闭环（commit `3e0d1ff`） |
| OPC-UA 适配器 | WBS 5.2 | 🟨 已启动（v0.3.0 M1 交付） | 2026-08-19 启动第一阶段：UA Binary 协议栈核心（pkg/opcua，零第三方依赖，41 测试）已交付；下一里程碑：SecureChannel/Read/Write/互操作 cross-check（commit `93458d6`，见 §11） |

---

## 11. v0.3.0 开发轮处置登记（2026-08-19）

> 依据：KNOWN-ISSUES §1 四条闭环派单 + OPC-UA 独立立项启动（WBS 5.2-M1）+ 冒烟 10/10 PASS（基线 HEAD `93458d6`）。状态：✅ 本次完成 / 🟨 进行中。

| 项 | 对应 WBS | 处置 | 结论/理由 |
|----|----------|------|----------|
| ① 采集命名空间同源 | WBS 5.2（采集路径） | ✅ 本次完成 | collectMapperReports 按 mapper 自身命名空间写入影子（与注册路由同源判定），显式 `EDGEFLOW_MODBUS_NAMESPACE` 时才改变 ns，默认行为与 v0.2.0 逐字节一致（commit `714d5ba`） |
| ② 退避测试注入点 | WBS 8.1 | ✅ 本次完成 | `edgehub.Options.BackoffSleepFunc` 注入点（nil=默认退避）+ 计数断言测试，移除实时时间阈值 flake（commit `714d5ba`） |
| ③ syncPod 400 JSON 安全 | WBS 2.5 | ✅ 本次完成 | 400 分支 `json.Marshal` 结构化输出（与 409 同构），畸形输入产出合法 JSON，响应结构不变 `{"error":...}`（commit `714d5ba`） |
| ④ 周期环境变量生效值明示 | WBS 3.x | ✅ 本次完成 | `edgeCoreIntervalFromEnv` 三态启动日志（合法/越界回落 Warn/未设置），启动日志明示生效值（commit `714d5ba`） |
| pkg/log SetOutput | WBS 1.5 | ✅ 本次完成 | `SetOutput(io.Writer)` 新增（nil 恢复 stderr，兼容 stdlib 约定），供测试捕获输出（commit `714d5ba`） |
| OPC-UA 独立立项 M1 | WBS 5.2 | 🟨 第一阶段交付，项目进行中 | 2026-08-19 启动第一阶段：UA Binary 协议栈核心（pkg/opcua，零第三方依赖，41 顶层测试）已交付——内置类型系统（NodeId 全形式/Variant/DataValue/ExtensionObject/Guid/ByteString/DateTime）、UA Binary 大端 codec、MessageHeader/Hello/Ack/Error、Dial TCP→HEL→ACK 三态协商（SecurityPolicy None 明文，仅限可信隔离网络）；下一里程碑：SecureChannel/Read/Write/互操作 cross-check（commit `93458d6`） |

---

## 12. v0.4.0 开发轮处置登记（2026-08-24）

> 依据：RELEASE-NOTES-v040 + KNOWN-ISSUES L6-L9 闭环。状态：✅ 已完成（2026-08-24 发布）。

| 项 | 对应 WBS/手册 | 处置 | 结论/理由 |
|----|----------|------|----------|
| 云端分级持久化（嵌入式 etcd） | WBS 2（CloudCore 存储） | ✅ 已完成 | registry/podstatus/devicestatus 三存储由纯内存改为嵌入式单成员 etcd（v3.5.33，127.0.0.1:12379/12380 只绑回环）写穿持久化：落盘 = 节点注册元数据 + 设备 Desired；心跳/Status/Pod 状态/属性快照不落盘（重启 ≤1 上报周期自愈）；读路径全走内存缓存，启动前缀 Range Load→Seed；GC 级联（CleanupLoop 1h）；坏库降级纯内存 + 大告警（ETCD_STRICT=1 fail-fast）；键空间 /edgeflow/{_meta,registry/nodes,devicestatus}；nodeID 字符约束 ^[A-Za-z0-9._-]+$；env +9（ETCD_ENABLED/DATA_DIR/CLIENT_URL/PEER_URL/QUOTA/COMPACTION/STRICT/NODE_RETENTION 等） |
| 升级与运维约束 | WBS 8（部署） | ✅ 已登记 | 升级后首次启动自动建库（无迁移脚本）；Helm 必须挂 PVC（默认 1Gi，emptyDir 名存实亡）；embed 模式 replicaCount 必须 1（多副本各自 embed=脑裂）；备份恢复走 etcdutl snapshot（文件拷贝≠有效备份）；监控阈值容忍重启短暂清空窗口 |
| 手册同步 | 手册 F09/附录 | ✅ 已完成 | 用户手册/方案手册"云端内存态"口径升级为分级持久化（本次手册轮再全面同步）；KNOWN-ISSUES L6-L9 闭环 |

## 13. v0.5.0 开发轮处置登记（2026-08-24）

> 依据：RELEASE-NOTES-v050 + DEPLOYMENT §10.7（外部模式迁移 runbook）。状态：✅ 已完成（2026-08-24 发布）。

| 项 | 对应 WBS/手册 | 处置 | 结论/理由 |
|----|----------|------|----------|
| 外部 etcd 模式 | WBS 2（CloudCore 存储） | ✅ 已完成 | `EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` 非空 = 外部直连共享 etcd 集群（clientv3，跳过 embed，不建目录不占端口）；业务层零改动；单写者铁律：两种模式 replicaCount 均必须 1（多副本互判离线删键，Chart {{ fail }} 守卫）；env +5（ETCD_ENDPOINTS/TLS_CA/TLS_CERT/TLS_KEY/ALLOW_INSECURE） |
| 安全护栏 | WBS 7（安全） | ✅ 已完成 | 非回环端点+无 TLS → 拒绝启动（逃生门 ETCD_ALLOW_INSECURE=1 仅限可信内网）；TLS_CA 非空启用（全 https）、CERT/KEY 同设即 mTLS（只设其一 fail-fast）、TLS1.2+；启动连通性探活（线性一致读 schemaVersion，至多 3×5s，失败拒绝启动）；schemaVersion 钩子（≠1 Warn） |
| 迁移与运维 | WBS 8（部署） | ✅ 已登记 | 首次切换走 DEPLOYMENT §10.7.5 迁移 runbook（快照恢复或零迁移自愈）；外部集群建议 3 节点奇数/同地域/quota 256MiB/compaction 1h/最小权限角色 readwrite /edgeflow/ |
| 手册同步 | 手册 ch2/ch7/ch8/附录 A | ✅ 已完成 | 部署形态/安全章节补外部模式（本次手册轮再全面同步）；KNOWN-ISSUES L10/L11 登记 |

## 14. v0.6.0 开发轮处置登记（2026-08-25）

> 依据：RELEASE-NOTES-v060 + ARCHITECTURE R15 + KNOWN-ISSUES L12-L17。状态：✅ 已完成（2026-08-25 发布）。

| 项 | 对应 WBS/手册 | 处置 | 结论/理由 |
|----|----------|------|----------|
| 真多活（外部模式多副本放开） | WBS 2（CloudCore 高可用） | ✅ 已完成 | 外部模式 replicaCount>1 active-active：心跳落盘为 etcd 租约（grant-per-heartbeat，/edgeflow/registry/heartbeats/<nodeID>，到期自动删键=软离线）；判活三态 = hb 键存在性（事件源三路互兜：watch 增量/周期重扫/断开事件）；GuardedDelete 守卫（活租约拒绝删台账）；watch 缓存同步（ListByPrefixRev→Watch→断线全量重放）；SetDesired CAS（冲突重试 ≤3）；/healthz 多副本绑定（失联 >TTL → 503）；NodeController 外部模式停用；env +2（NODE_LEASE_TTL 300s/MULTI_REPLICA） |
| 混跑禁令 | WBS 8（升级运维） | ✅ 已登记 | 升级/回滚必须全停再全起（scale 0→1），禁止 v0.5.0×v0.6.0 混跑（旧版 GC 误删活节点，L15）；回滚零脏键（hb 前缀 v0.5.0 扫不到，租约 ≤TTL 自动到期）；多副本前置：同版本/3 节点 quorum/共享 endpoints |
| 语义变化 | WBS 2/手册 | ✅ 已登记 | etcd 故障 >TTL → 全量软离线（有界自愈、零数据删除），与 v0.5.0"判活不受存储故障影响"不同；NODE_TIMEOUT 不再参与判活、NODE_SCAN_INTERVAL 迁用重扫/GC 周期；监控阈值按 ≈2×TTL 折算 |
| 手册同步 | 手册 ch4/ch5/ch7/ch9/附录 A | ✅ 已完成 | 判活机制/healthz 语义/扩缩容入册（本次手册轮再全面同步） |

---

## 15. v0.7.0 开发轮处置登记（2026-08-25）

> 依据：.cluster/edgeflow-v070/plan.md + 设计定稿 subagent_01.md + 审稿复核 subagent_02.md + 主线裁决 decision.md（D1-D11）。状态：✅ 已完成（2026-08-25 发布，Release v0.7.0 15 制品；E2E 全链路验证通过）。

| 项 | 对应 WBS/手册 | 处置 | 结论/理由 |
|----|----------|------|----------|
| 模型仓库与版本管理（F41） | 手册 F41（原规划中） | ✅ v0.7.0 已实现（2026-08-25 发布） | 云模型台账 + 版本状态机 + 部署影子：17 个新端点之一部（模型 5 + 版本 6）；键空间 `/edgeflow/models/`，guard + CAS；版本状态 draft/active/archived（两键 CAS + 补偿）。契约见 API-SPEC §7；架构决策 ARCHITECTURE R16；发布说明 RELEASE-NOTES-v070 |
| 模型灰度发布（F42） | 手册 F42（原部分实现：仅 keadm 升级分批） | ✅ v0.7.0 已实现（2026-08-25 发布） | 服务端灰度执行器：白名单/按比例（创建时快照）、分批、fail-fast、取消、逆序回滚；领跑锁 + 崩溃接管（外部多副本）；下发复用 podsync/config-sync（边缘零改动）。keadm 升级分批保留为**产品升级灰度**（双轨并行）。**v0.16.0 运维纵深**（2026-08-27）：定时维护窗口（notBeforeMs）/ 运行中 pause·resume（批边界生效·保锁续租）/ 模型目录 export·import（幂等 upsert·active 直通灾备），端点 32→36，RELEASE-NOTES-v0116。**v0.17.0 操作纵深收口**（2026-08-27）：发布任务运行中可调参数（PATCH 批边界生效）/ 列表 status 多值过滤 / dryRun 预检（零落盘·TOCTOU 口径明示），端点 36→37，RELEASE-NOTES-v0117。**v0.18.0 智能运维**（2026-08-27）：失败预算自动暂停（failureBudget 达标→paused 可 resume 续跑）/ Events 事件时间线（CAS 并发安全·环形 32 条·随快照迁移）/ 全局部署影子聚合查询（端点 37→38），RELEASE-NOTES-v0118。**v0.19.0 智能运维第二批**（2026-08-27）：PATCH 白名单扩展 failureBudget 运行中可调（批边界生效·0=关闸）/ releases/{id}/snapshot 审计快照一键全景 / GET /api/v1/releases 全局发布查询（端点 38→40），RELEASE-NOTES-v0119。**v0.20.0 生命周期收口**（2026-08-27）：POST .../retry 失败节点克隆重试（终态限定·failed 子集·RetryOf 回指）/ DELETE .../releases/{id} 终态归档删除（与 GC 同源在途绝不删）/ releaseNotes 元数据（≤1024 创建期定死全读取透出）；端点 40→42，RELEASE-NOTES-v0120 |
| 端点口径 | API-SPEC/API-COMPATIBILITY | ✅ 全稿统一 **17 新端点 / 14→31** | 主线裁决 D1（F-1 🔴 修正）；P10 三处一致抽查随实施验证执行 |
| 已知限制 | KNOWN-ISSUES §7 | ✅ 本文档轮登记完成 | L21-L31（设计 §13.1 + 主线 D2/D3/D4/D9/P9 补项；§6 原 L21 更名 L20b 消除编号冲突） |
| 解决方案手册 | 手册 F41/F42 状态翻转 | ✅ 本文档轮完成 | 手册升 v1.1.0：第 4 章重写为正式能力（过渡路径降级为兼容路径）；附录 C 表 C-1 追加 F41/F42、表 C-2 摘除；FAQ A7/术语表/附录 D 同步 |
| Chart | build/charts/edgeflow | ✅ 本文档轮完成 | version/appVersion → 0.7.0；无新增 values 必填项；helm lint 0 failed + 多场景渲染验证 |
| 代码实现与 E2E | WBS 1-10（实施清单） | ✅ 已完成（2026-08-25 发布） | 全量 `go test -race ./...` 33 包全绿、vet 干净；契约测试 31 端点；E2E 全链路实测（发布 v1/v2 succeeded→回滚 rolled_back→影子回退→embed 重启恢复）；E2E 发现并修复回滚链缺陷（cf23ee7：ActivateVersion 记录 PrevActive）；12 制品交叉编译 + helm lint/template 通过；验证摘要已回填 RELEASE-NOTES-v070 §七.7.2 |

---

## 16. v0.21.0 开发轮处置登记（2026-08-28）

> 依据：.cluster/edgeflow-audit/{audit-report.md, ledger-consolidated.md, tasklist.csv}（全量审计 71 条台账）+ .cluster/edgeflow-v0210/plan.md。状态：✅ 已完成（P0 批次 4 任务 / 16 人日：T-01 安全默认值包 5pd + T-02 OPC-UA 预分配修复 4pd + T-03 递归深度限制 2pd + T-04 订阅泵修复 4pd + T-18 mapper 自愈随包交付；P1×12 / P2×13 留后续版本）。

| 项 | 对应审计编号 | 处置 | 结论/理由 |
|----|----------|------|----------|
| 安全默认值包 | SEC-01 / SEC-02 / CHN-06 | ✅ v0.21.0 已实现 | 只提升可见性不改默认行为：SEC-01 auth 关闭启动 WARN（AUTH_WARN=off 可静默）；SEC-02 `EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN=on` 拒绝携带令牌注册（防伪造探测，无令牌注册裸奔兼容）；CHN-06 裸奔组合聚合 WARN；Helm `cloudcore.auth.*`/`cloudcore.cloudhub.*` 默认全关。enforce fail-closed 形态（连无令牌注册也拒）经评审判定与裸奔兼容底线冲突，留待维护窗口协商机制（P1 决策异议） |
| OPC-UA 预分配放大修复 | PRT-01 / PRT-14 | ✅ v0.21.0 已实现 | Variant 数组/StringList 分配前预检（声明×最小元素字节数 vs 剩余缓冲，>1024 元素豁免阈值保持既有截断语义与既有测试零改动）；恶意 20 字节报文内存峰值 O(1) |
| DiagnosticInfo 递归深度限制 | PRT-03 | ✅ v0.21.0 已实现 | `MaxDiagnosticDepth = 100` 超限拒绝（ErrTooLong）；ResponseHeader/OpenSecureChannelResponse/通知内嵌诊断全调用点传 depth |
| 订阅泵与 mapper 自愈 | PRT-04 / PRT-18 | ✅ v0.21.0 已实现 | pumpLoop 全退出路径关闭 pubCh（sync.Once 防双关，pubCh 置 nil 防复用已关通道）；mapper 感知通道关闭自动重连重建订阅 |
| HTTP 端点 | 契约面 | ✅ 保持 42 不变 | 本轮零端点变更（安全面走 env 开关与启动告警，非 REST 面）；tests/contract 路由计数与文档一致性守卫绿 |

## 17. v0.22.0 开发轮处置登记（2026-08-28）

> 依据：.cluster/edgeflow-audit/tasklist-audit.csv P1 批次（T-05~T-11，7 项 / 24 人日）+ .cluster/edgeflow-v0220/plan.md。状态：✅ 已完成（P1 全闭环；P2 留后续版本）。

| 项 | 对应审计编号 | 处置 | 结论/理由 |
|---|---|---|---|
| T-05 重注册事件顺序+幽灵节点 | CHN-07（验收 2/3） | ✅ 代码既有，测试钉死 | cloudhub 事件序断言 + registry 无幽灵/缺陷签名测试（import 环分域） |
| T-06 发布状态机契约对齐 | CLD-01/02/04/06 | ✅ 全部完成 | 4 类终态写点接 assertReleaseTransition；digest 失败写事件+同源失败预算；failFast 与 head 解耦；8 新测试 |
| T-07 边缘下行幂等落盘 | CHN-03 | ✅ 全部完成 | dedup_keys SQLite（TTL 24h/1 万条上限/批量淘汰）；edgehub 双写回源；edgecore 装配降级兼容；重启去重集成测试 |
| T-08 慢客户端防线+注册风暴 | CHN-02 + CHN-07（验收 1） | ✅ 全部完成 | ack 前置；字节配额默认 64MiB（env 可配）；广播内存 gauge；可选广播闸门 |
| T-09 API 归属校验收口 | CLD-06 + canary 语义 | ✅ 全部完成 | ownedRelease 7 端点；10 端点行为收敛；API-SPEC §7.11 + §4 404 行；契约测试新文件 |
| T-10 容器迁移复核+容错 | CHN-05 | ✅ 全部完成（含审计口径修正） | 迁移后 Inspect 复核（主线补）；容错单测 4 用例（R2 交付）；「90s 固化快照」澄清为 Absent 保留窗口 |
| T-11 吊销链可配收紧 | SEC-04 | ✅ 全部完成 | CRL 严格/OCSP 新鲜度双开关（默认关）；LoadTLSConfigWithOptions 接线；SECURITY.md 部署建议 |
| 行为修复配套 | 审计纪律 | ✅ 逐条明示 | e2e 辅助函数 mnist 硬编码依赖旧归属缺陷→新增显式模型名 helper（v0170/v0180 共 3 处切换）；其余既有测试零改动 |
| 验证 | — | ✅ 全绿 | build/vet/单测全绿；contract 全绿（42 端点守卫）；e2e 全量 190s 绿 |


## 18. v0.23.0 开发轮处置登记（2026-08-28）

> 依据：.cluster/edgeflow-audit/tasklist-audit.csv P2 批次（T-12~T-19，8 项 / 24 人日）+ T-20 收官勾稽 + .cluster/edgeflow-v0230/plan.md。状态：✅ 已完成（P2 全闭环 + 台账 71 条全量勾稽：55 ✅ / 14 📌 登记 / 2 🗑️ 撤销，零悬挂）。

| 项 | 对应审计编号 | 处置 | 结论/理由 |
|---|---|---|---|
| T-12 观测面补齐 | CHN-14/19/20、CLD-13 | ✅ 闭环 | 丢事件计数/续约队列水位 metrics/listFailed 告警 env（默认关）/paused kind 日志/Grafana 面板随仓库交付（deploy/grafana/） |
| T-13 锁序工程化 | CHN-11/23 | ✅ 闭环 | lease_registry 文件头权威段 + LOCK ORDER 断言 6 处 + ARCHITECTURE §13；26 个 Lock() 调用点核对无嵌套持有 |
| T-14 契约口径统一 | CLD-07/08/10/12 | ✅ 闭环 | summary 恒现 / releases 列表信封化（唯一破坏性变更，COMPATIBILITY 登记）/ errors.Is(io.EOF) / 422 文案细分；§7.12 错误码映射表 |
| T-15 输入卫生 | CLD-11、PRT-24、CHN-15/16、SEC-06 | ✅ 闭环 | mirror 预检+编码三重收敛（短形式宽松裁决）；CRD 11 处标记；Validate Version 格式校验；rand 失败即报错（OPC-UA nonce + newID + newReleaseID 三落点统一）；CRLF 消毒（cloudcore+cloudhub） |
| T-16 OPC-UA 健壮性 | PRT-05~23 族 | ✅ 闭环 | 13 修 2 登记（PRT-11/12 建议级 §23.2）；含 mappers 域 PRT-19/20/21 接手（锁外重连/Stop 快照/预算门）；-race 全绿 |
| T-17 keadm token 卫生 | SEC-03 | ✅ 闭环 | env 0600 钉死 + --token-file（file 优先）+ README 脱敏 + KEADM §2.1；目录 0700 登记不修（§23.1） |
| T-18 云端安全面 | SEC-05/07/08 | ✅ 闭环 | CheckOrigin 白名单（缺 Origin 放行语义）；Helm existingSecret（互斥 fail 守卫）；--cloudcore-port 1-65535 |
| T-19 测试增强 | CHN-17/08 | ✅ 闭环 | nextBackoff 边界 16 子例（实现零改动）；CHN-08 文档标注裁决（DEPLOYMENT §2.5，SetReadDeadline 留待 RTT 基线） |
| T-20 勾稽收官 | 台账 71 条 | ✅ 闭环 | 勾稽总表 55✅+14📌+2🗑️=71；主线直接实施 CHN-13（eventbus 等待有界化）与 CHN-16 pkg/protocol 落点（删时间戳兜底）；encodeUA 基线缺陷修复 |

> 残余票与登记项：见 docs/KNOWN-ISSUES.md §23（登记不修 14 条理由与重估触发条件 + 域外残余票 R-1~R-4）。

## 19. v0.24.0 开发轮处置登记（2026-08-29，MQTT 功能轮：审计驱动阶段收官后的第一个功能版本）

> 依据：.cluster/edgeflow-v0240/plan.md（范围裁定 F-1~F-4）。状态：✅ 已完成（四项全闭环；端点 42 不变、零新依赖、默认行为不变、既有测试零改动）。

| 项 | 范围 | 处置 | 结论/理由 |
|---|---|---|---|
| F-1 MQTT 协议栈 | pkg/mqtt/ | ✅ 闭环 | MQTT 3.1.1 九种报文 codec（varint+主题双校验、ErrMalformed 哨兵族、限量读防 DoS、golden 字节测试）+ client（Dial/Subscribe/Publish/Close、读泵分发、QoS1 PUBACK 等待、KeepAlive、无自动重连由上层负责）+ MatchTopic（MQTT-4.7 通配含 $ 系惯例）；26 测试 -race 绿 |
| F-2 MQTT Mapper | mappers/mqtt/ + cmd/edgecore | ✅ 闭环 | 订阅型采集（与轮询型 modbus、订阅+轮询 opcua 并列第三种接入）；EDGEFLOW_MQTT_* 五环境变量 opt-in；payload 三形态容错解析；监管循环断线重连；台账触点全覆盖；装配段+测试 9 例 |
| F-3 测试 broker + e2e | pkg/mqttsim/ + tests/e2e | ✅ 闭环 | 进程内 broker（CONNECT 校验/SUBACK/分发/PINGREQ/出站队列 32 丢弃计数）9 测试 -race 绿；TestMQTTDeviceE2E 真实装配全数据面；e2e 全套 288s 绿 |
| F-4 §23 残余清理 | contract/keadm/GUIDE | ✅ 闭环 | R-1 注释口径修正（断言零动）；SEC-03 附产物目录 0700 opt-in（EDGEFLOW_JOIN_DIR_MODE 默认 0755 不变）；OPCUA-GUIDE 无漂移确认 |

> 残余票与登记项：见 docs/KNOWN-ISSUES.md §24（R-6 sim 本地 codec 收敛 + R-5 守卫注释 + MQTT 能力边界说明）。下一步候选：R-6 收敛、OPC-UA Basic256Sha256 加密通道、MQTT QoS2/TLS。

## 20. v0.25.0 开发轮处置登记（2026-08-30，MQTT 硬化轮：残余闭环 + TLS 加密传输）

| 子任务 | 落点 | 状态 | 说明 |
|---|---|---|---|
| F-1 R-6 codec 收敛 | pkg/mqtt + pkg/mqttsim | ✅ 闭环 | 导出三函数薄包装（EncodePacket/DecodePacket/ValidateTopicFilter）；sim 747→447 行共享单一实现；坏客户端 SUBSCRIBE 宽容通道（KNOWN-ISSUES §25.2）；pkg/mqtt 35 + mqttsim 11 测试全绿（含 -race） |
| F-2 R-5 守卫口径 | tests/contract | ✅ 闭环 | 注释同扫两文件口径 + 错误信息带来源文件（file 字段）；断言零变化，42 端点绿 |
| F-3 MQTT TLS 全栈 | pkg/mqtt + pkg/mqttsim + mappers/mqtt + tests/e2e | ✅ 闭环 | client Options.TLSConfig（tls.DialWithDialer + ServerName 自动回填 + Clone 防突变）；NewBrokerTLS；EDGEFLOW_MQTT_TLS_CA / EDGEFLOW_MQTT_TLS_INSECURE 全 opt-in（fail-fast）；TestMQTTTLSDeviceE2E 全环 TLS 闭环；TLS 增量 12 测试全绿（含 -race） |

> 残余票与登记项：见 docs/KNOWN-ISSUES.md §25（mqttsim TLS 测试定位、INSECURE 边界、client mTLS 未含）。下一步候选：MQTT QoS2、client mTLS（双向认证）、OPC-UA Basic256Sha256 加密通道、mapper 配置文件化。

## 21. v0.26.0 开发轮处置登记（2026-08-31，MQTT QoS2 ＋ client mTLS ＋ mapper 配置文件化）

> 依据：.cluster/edgeflow-v0260/plan.md（范围裁定：QoS2 + mTLS + mapper 配置文件化三维；OPC-UA Basic256Sha256 体量大留 v0.27.0）。状态：✅ 已完成（端点 42 不变、零新依赖、默认行为与 v0.25.0 逐字兼容、既有测试零改动）。

| 子任务 | 落点 | 状态 | 说明 |
|---|---|---|---|
| F-1 MQTT QoS2 | pkg/mqtt + pkg/mqttsim | ✅ 闭环 | PUBREC/PUBREL/PUBCOMP codec（PUBREL flags=0x02 字节级规范）；client Options.EnableQoS2 门控（默认 false 冻结一致）+ 上行四次握手状态机 + readPump 下行 QoS2（pendingDownQoS2 暂存，handler 恰好一次）；sim pendingQoS2（PUBREL 确认后才 fanout）；v0260 测试 7 例（含 -race） |
| F-2 client mTLS | mappers/mqtt + pkg/mqttsim | ✅ 闭环 | EDGEFLOW_MQTT_TLS_CERT/_TLS_KEY（成对 fail-fast）+ env→file 回退链 + connect() 证书对注入；sim 零新 API（NewBrokerTLS 接受任意 tls.Config）；mTLS 全链路 + fail-fast 五例 + 无证书拒绝 |
| F-3 mapper 配置文件化 | mappers/mqtt/config.go | ✅ 闭环 | EDGEFLOW_MQTT_CONFIG（.yaml/.yml/.json 手写 parser 零依赖）；优先级 With>env>file>默认；坏文件软失败（log.Errorf 继续）；B-R2 worker 产出主线验收保留 |

> 残余票与登记项：见 docs/KNOWN-ISSUES.md §26（QoS2 进程内 exactly-once 边界、mqttsim mTLS 测试定位、测试 CA EKU 嵌套陷阱、OPC-UA Basic256Sha256 未含）。下一步候选：OPC-UA Basic256Sha256 加密通道、QoS2 会话恢复（in-flight 持久化）、MQTT 5.0 特性评估。
