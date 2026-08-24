# EdgeFlow M3-M5 里程碑收尾审计报告（audit-m35）

> 审计类型：**只读收尾核对**（未改动任何代码/文档）
> 审计时间：2026-08-14 20:20-20:40 (Asia/Shanghai)
> 审计基线：`docs/ROADMAP.md`（WBS 任务与验收标准）、`docs/PROGRESS.md`（§3 声明状态、§4G-4M 验证记录）
> 证据来源：git log（108 commits，HEAD `eacd35a`）、代码目录逐包核对、`release/v0.1.0/` 制品实测（`keadm-darwin-arm64 version` 实跑）、docs/ 审查报告（M3A/M3B/M4A~M4D/REVIEWS/RELEASE-REVIEW）、发布文档交叉比对
> 声明状态（PROGRESS §3）：M3 🟨 第 1-3 轮完成｜M4 🟨 主体+收尾完成｜M5 🟨 发布完成

---

## 1. 审计方法与范围

1. 从 ROADMAP.md 提取 M3-M5 覆盖任务（WBS 2.2/3.5/3.6/5.1-5.5；4.5/7.1-7.5/10.1-10.4/8.4/8.5；8.6/9.2-9.5 + M3/M4/M5 验收标准）。
2. 逐项对照：git log 提交证据、代码目录、测试/审查报告、release 归档、发布文档。
3. 特别核对 7.2/7.3/7.5、10.1、8.4、8.6 完整性、M5 验收（15min 部署 / kubectl get nodes Ready）、发布产物齐备性。
4. 独立实测：`release/v0.1.0/keadm-darwin-arm64 version` → `v0.1.0 gitCommit=1265214 buildTime=2026-08-14T20:02:54`（确认 P1-1 已闭环，二进制含 P2-4/P2-5 修复）；checksums.txt 7 条目齐备；images.json 标注 arm64 单架构。

> 注：全仓 `go test -race` 未重跑（只读审计，测试结论采信 PROGRESS/审查报告的多轮记录，且与 release 文档一致）。

---

## 2. 核对总表

### 2.1 M3 设备管理与数字孪生（声明：🟨 第 1-3 轮完成）

| # | 任务 | 声明状态 | 实际状态 | 证据 | 结论 |
|---|------|---------|---------|------|------|
| 1 | WBS 2.2 DeviceController（设备 CRD 控制器、状态同步） | 🟨 | 云端设备状态存储 + 查询/指令 API（内存态） | `cloud/pkg/devicestatus`（文件头自述"WBS 5.3 云端侧…待集群环境就绪后被 K8s apiserver 对接取代"）；`cmd/cloudcore/device_api.go` 头注释"WBS 5.3/2.2 **简化版**"；commit 744afaa | 🟨 **部分**：状态同步/API 有；**无 K8s 控制器**（无 Informer/Watch/reconcile，未接入真实 apiserver） |
| 2 | WBS 3.5 DeviceTwin 模块 | ✅ | Twin/TwinStore：desired/reported 合并、深拷贝、自动创建 | `edge/pkg/devicetwin`（覆盖率 100%）；4G 联调：指令 targetTemp 25 → 温度 31.05→25.59 收敛；desired 云端可见 | ✅ **完成** |
| 3 | WBS 3.6 EventBus（MQTT 消息总线） | ✅ | paho 封装：QoS1、AutoReconnect、OnConnect 订阅恢复、IsOnline | commit 2a0d0a3；4H 实测 mosquitto 收发/重连/竞态；docs/EVENTBUS-GUIDE.md | ✅ **完成**（⚠️ ROADMAP 依赖"**NATS MQTT POC**"未执行，改用 mosquitto，属已文档化偏差） |
| 4 | WBS 5.1 Mapper 框架（SDK、注册/注销） | ✅ | DeviceMapper 接口 + MapperRegistry（RWMutex、幂等 StartAll/StopAll、errors.Join） | commit 7d82c0c；`edge/pkg/mapper`（96.4%）；4G/4I 实测 | ✅ **完成** |
| 5 | WBS 5.2 设备协议适配（Modbus、OPC-UA、MQTT） | 🟨（M3 声明含 5.2） | **Modbus 完整**；OPC-UA 未做；MQTT 适配经 EventBus 数据面部分覆盖 | commit a290686"feat(modbus)…(WBS 5.2)"——**归属在 M4 主体轮（4K）完成**，M3 三轮内未交付 5.2；mappers/modbus + pkg/modbussim + op_ledger 台账；仓库无 OPC-UA 代码 | 🟨 **部分 + 里程碑归属偏移**：Modbus ✅（但属 M4 轮次完成）；OPC-UA ⬜；MQTT 仅 mock_sensor MQTT 模式（99b5624），无通用 MQTT 设备适配器 |
| 6 | WBS 5.3 设备 CRD 控制器（Device/DeviceModel CRD） | ✅（M0-2 类型） | **CRD 类型定义**（Device/DeviceModel/EdgeNode + DeepCopy + defaults + 11 测试）✅；**控制器 ⬜** | apis/edge/v1alpha1（commit a541128，M0-2）；API-SPEC §7 标注"CRD 尚未接入 K8s，示例 YAML 未在真实集群验证"；devicestatus 文件头自述迁移点 | 🟨 **部分**：类型层完成，控制器/接入 K8s 未做；M3 验收"CRD 可创建并驱动 Mapper 实例"未在真实集群验证 |
| 7 | WBS 5.4 设备数据流水线（采集→转换→上报→持久化） | ✅ | 采集（mapper）→ 转换（Twin desired/reported 合并）→ 上报（周期循环）→ 持久化（云端 devicestatus + SQLite op_ledger） | 4G/4I 联调全链路；99b5624 MQTT 遥测流；a290686 op_ledger 30 天清理 | ✅ **完成**（内存态云端存储为已知边界，K 系列已披露） |
| 8 | WBS 5.5 设备示例 Mapper | ✅ | mock_sensor（温湿度、MQTT 模式+本地降级）+ modbus mapper 示例 | mappers/mock_sensor（91.1%）、mappers/modbus；4G/4I/4K 实测 | ✅ **完成** |
| 9 | M3 验收：端到端延迟 ≤5s | 未声明验证 | **未测量** | 全库 grep：仅 ARCHITECTURE.md 作为目标值出现，无任何实测记录 | ⬜ **未验证** |

### 2.2 M4 生产化与规模化（声明：🟨 主体+收尾完成）

| # | 任务 | 声明状态 | 实际状态 | 证据 | 结论 |
|---|------|---------|---------|------|------|
| 10 | WBS 4.5 安全传输层（mTLS 完整） | ✅ | 双向 TLS 强制（RequireAndVerifyClientCert）、TLS1.2+、wss 归一化、TLS off 完全兼容 | commit 0a7fcc2；4J 实测：云侧"mTLS 连接已认证（peer CN）"+ 拒绝路径 | ✅ **完成** |
| 11 | WBS 7.1 证书管理（CA 初始化、签发、轮换） | ✅ | CA/服务端/客户端证书幂等生成 + 签发 + 校验 ✅；**轮换人工编排、吊销（CRL/OCSP）未实现** | pkg/certs/certs.go 自述："证书轮换需人工编排（重新签发后滚动重启组件）；吊销（CRL/OCSP）未实现：一旦私钥泄露，需轮换 CA 并全量重签"；hack/gen-certs.sh | 🟨 **部分**：初始化/签发/幂等/校验完整；自动化轮换与吊销 ⬜（WBS 7 完成标准"CA 初始化、签发、轮换全流程可执行"仅人工路径成立） |
| 12 | WBS 7.2 RBAC（角色/权限定义） | （未声明） | **未实现**：无任何 RBAC/authz 代码，云端 HTTP API 无认证中间件 | 全库 grep `rbac|authz` 零命中；cloudhub/server.go 注释"M1 基础版不做来源校验"；git log 无相关提交 | ⬜ **未做**（且 Release Notes 未列入 known issues） |
| 13 | WBS 7.3 设备认证（设备身份注册、Token 认证） | （未声明） | **未实现**：token 为预留字段，edgecore 未消费 | cmd/keadm/join.go："Token 是接入令牌（必填；**当前版本 edgecore 尚未消费，预留做后续鉴权**）"；EDGEFLOW_EDGECORE_TOKEN 同样标注预留 | ⬜ **未做**（仅参数/占位；云边节点身份由 mTLS 承担，设备身份认证无） |
| 14 | WBS 7.4 云边认证（mTLS 握手、节点验证） | ✅ | 双向认证 + peer CN 审计行 + 未认证拒绝 | 4J 实测 + server_tls_test/client_tls_test；SECURITY.md | ✅ **完成**（⚠️ 跨主机 CA 分发缺口见 G12） |
| 15 | WBS 7.5 审计日志（操作审计、安全事件） | （未声明） | **未实现**：仅 TLS 连接一行日志；keadm ops-ledger 是运维操作台账非安全审计 | cloudhub/server.go:395 单行 log；无审计存储/查询 API；WBS 7 完成标准"审计日志产生**可查询记录**"不成立 | ⬜ **未做** |
| 16 | WBS 10.1 可观测性（Prometheus、Fluent Bit、告警） | （未声明） | **未实现**：无指标端点、无 Prometheus/Fluent Bit 相关代码 | 全库 grep `prometheus|fluent` 零命中；仅 healthz；Release Notes K15 承认"边缘节点资源上报未采集（memory 恒 0）" | ⬜ **未做**（M4 验收"Prometheus 采集到指标、Fluent Bit 采集到日志"未达成，且未列入 known issues） |
| 17 | WBS 10.2 升级回滚（OTA、灰度） | ✅ | keadm upgrade/rollback：备份模型 + ops-ledger + --simulate-failure + 事务化 restore + manifest 白名单 | commits 7aa035c / fe093e1；4L 实测：升级→失败→回滚 diff 一致、数据 hash 一致、异常三类兜底；UPGRADE.md | ✅ **完成**（灰度发布未实现，见 G8） |
| 18 | WBS 10.3 API 兼容性测试（K8s API 版本兼容矩阵） | （未声明） | **未实现**：无兼容矩阵文档/测试 | grep 仅 ARCHITECTURE.md 一处提及"WBS 10.3 API 兼容矩阵的通信侧对应物"（协议开放集合设计），无矩阵本身 | ⬜ **未做**（未列入 known issues） |
| 19 | WBS 10.4 安全扫描与修复（镜像扫描、依赖漏洞） | 🟨（延后登记） | **SBOM ✅**（sbom.json 33 组件，RELEASE-REVIEW 独立比对 go list 全一致）；**镜像漏洞扫描 ⬜**（无 trivy/grype 记录） | release/v0.1.0/sbom.json；M4C P2-⑥"cosign/SBOM 延后"→ SBOM 已补，cosign 签名仍无；grep trivy/CVE 零命中 | 🟨 **部分**：SBOM 完成；"镜像扫描无高危漏洞"验收未达成（未扫描） |
| 20 | WBS 8.4 性能测试（100 节点压测、内存 ≤256MB） | （未声明） | **未实现** | DEV-ENV.md："100 节点规模需单独脚本（M4 扩展）"；EDGED-POC.md："资源占用实测：**未做容器内存/CPU/节点规模压测** → WBS 8.4"；仓库无压测脚本/报告 | ⬜ **未做**（M4 验收"100 节点注册 ≥99%、延迟 ≤3s、EdgeCore 内存 ≤256MB"全部未验证，未列入 known issues） |
| 21 | 多架构镜像（amd64+arm64 可运行） | ✅（M4C） | **M4C 历史验证 ✅**（buildx manifest 双架构 + QEMU 交叉运行 --version 一致，commit 301daee）；**但 v0.1.0 发布镜像仅 arm64 单架构** | MULTIARCH.md §7（旧构建 f71684e 证据）；images.json archNote："本机 arm64 单架构构建（docker build，**非 buildx 多架构**）"；RELEASE-REVIEW P2-1 | 🟨 **部分/不一致**：能力验证过（旧构建），实际发布制品未复现双架构 manifest |
| 22 | WBS 8.5 Helm Chart（一键部署） | ✅ | Chart 完整化：values 真实化/资源/探针/env/卷挂载；helm lint 0 failed、install --dry-run 通过 | commit 3bb60ce；4J 实测；release Chart 包与源码逐字节一致（RELEASE-REVIEW） | ✅ **完成**（真实集群 `helm install` 未执行，见 G5） |

### 2.3 M5 MVP 发布与文档交付（声明：🟨 发布完成）

| # | 任务 | 声明状态 | 实际状态 | 证据 | 结论 |
|---|------|---------|---------|------|------|
| 23 | WBS 8.6 keadm 完整版（安装/注册/升级/批量/配置迁移） | ✅（基础+扩展） | init/join/reset/version ✅；upgrade/rollback/ops-ledger ✅（10.2 承载）；**批量操作 ⬜**；**配置迁移 = 备份保全**（upgrade 备份 env/service/install.sh + manifest 校验，非跨版本配置迁移） | cmd/keadm 9 文件 + 实测 `keadm version`；KEADM.md 标题仍为"**WBS 8.6 基础版**"（文档滞后，见 D8）；UPGRADE.md | 🟨 **部分**：基础+升级回滚完整；批量 ⬜、真迁移 🟨 |
| 24 | WBS 9.2 API 文档（API 参考、CRD 文档） | ✅ | API-SPEC.md：13 个 REST 端点 + 错误码表 + CRD 全字段归档 | REVIEWS.md R9.2 逐端点核对 ✅；commit f090a30（四文档 2023 行） | ✅ **完成** |
| 25 | WBS 9.3 部署指南（快速开始、生产部署） | ✅ | DEPLOYMENT.md：快速开始/镜像/Helm/mTLS 检查清单/升级回滚/卸载/排障 | REVIEWS.md R9.3（问题 R9.3-1~3 已处理） | ✅ **完成** |
| 26 | WBS 9.4 开发者指南 | ✅ | HANDOFF.md：代码结构/流程/测试规范/贡献指南/常见任务/交接检查单 | REVIEWS.md R9.4（R9.4-1~4 已处理） | ✅ **完成** |
| 27 | WBS 9.5 示例与教程 | ✅ | examples/demo.sh（11 步 + MQTT 可选段）DEMO PASS×3、0 残留 | REVIEWS.md §5；4L 实测 | ✅ **完成** |
| 28 | 发布产物：Release Notes/台账/演练排期/独立复核/二进制/Chart/镜像 | ✅ | Notes（中英双语，K1-K18 已知问题）✅；台账 ✅（digest 回填）；DRILL-SCHEDULE ✅（窗口【需确认】）；RELEASE-REVIEW ✅（P1-1 已闭环，**实测 keadm gitCommit=1265214 含修复**）；6 二进制 + Chart + checksums（7/7）+ SBOM + images.json 齐备 | release/v0.1.0/ 实查 + `keadm-darwin-arm64 version` 实跑；RELEASE-REVIEW 逐项复核记录 | ✅ **完成**（⚠️ Notes §1 待回填字段未清、台账时间线未回填，见 D5/D6；发布镜像单架构，见 G5；无远程推送为已披露环境边界） |
| 29 | M5 验收：新用户 15min 部署 | 未声明验证 | **单机 Demo 5 分钟可跑通**（DEPLOYMENT §0 快速开始）；**真实集群 15min 部署未验证** | REVIEWS.md §3；RELEASE-NOTES §4 E2"real-cluster installation not yet executed" | 🟨 **部分**（单机 ✅，集群 ⬜） |
| 30 | M5 验收：kubectl get nodes Ready | 未声明验证 | **无真实集群**：节点状态在 cloudcore 内存注册表（/api/v1/nodes），非 K8s Node 对象 | registry 包（内存态）；API-SPEC §7 已知缺口；keadm 产物需用户到真实集群执行（KEADM.md §6"本机验证边界"） | ⬜ **未验证**（无真实集群环境，验收条件无法达成） |
| 31 | M5 验收：CRD 字段文档齐全 / 温度传感器 Demo 跑通 | ✅ | API-SPEC 第二部分 CRD 全字段 ✅；demo.sh 温湿度 Demo PASS×3 ✅ | REVIEWS.md §2.1/§5 | ✅ **完成** |

---

## 3. 特别核对结论

| 核对项 | 结论 |
|--------|------|
| 7.2 RBAC / 7.3 设备认证 / 7.5 审计日志是否实现（mTLS 轮只做了 7.1/7.4？） | **确认只做了 7.1+7.4**。7.2/7.3/7.5 均无代码：RBAC 零命中；token 为预留字段未被 edgecore 消费；审计仅一行 TLS 日志 + keadm ops-ledger（运维台账≠安全审计）。且 Release Notes 英文摘要写 "mTLS (mutual TLS, WBS 7)"，**未披露 7.2/7.3/7.5 缺失** |
| 10.1 可观测性（Prometheus/Fluent Bit）是否真实现 | **未实现**。无指标端点、无 Prometheus/Fluent Bit 代码；仅 healthz；K15 自认资源上报未采集 |
| 8.4 性能测试（100 节点压测）是否做过 | **未做过**。DEV-ENV/EDGED-POC 均自认未做，仓库无压测脚本/报告；"内存 ≤256MB、注册 ≥99%、延迟 ≤3s"全部未验证 |
| 8.6 keadm 完整版（升级/批量/配置迁移） | upgrade/rollback ✅（含 ops-ledger、simulate-failure、事务化 restore、manifest 白名单）；**批量 ⬜；配置迁移未实现**（仅备份/回滚保全产物，无跨版本配置迁移逻辑）；KEADM.md 自标"基础版" |
| M5 验收 15min 部署 / kubectl get nodes Ready（无真实集群） | 单机 demo.sh ~5 分钟 ✅；**真实集群路径全部未执行**（keadm 产物生成、helm install --dry-run、本地 registry、单机联调为上限）；节点状态非 K8s Node，`kubectl get nodes` 无从谈起——已披露为 E2 已知限制，但里程碑状态仍标"发布完成" |
| 发布产物齐备性（Notes/台账/演练/独立复核） | 齐备且质量高：RELEASE-REVIEW 10 项独立复核（7 通过/2 有条件/1 不一致→P1-1 已闭环，实测 keadm 重建含修复）；checksum 7/7、SBOM 33 组件与 go list 双向一致、Chart 逐字节一致、回滚方案四路径一致。遗留：Notes §1 待回填、台账时间线 ⏳（D5/D6）、发布镜像单架构（G5） |

---

## 4. 缺口清单（分级）

### P0（阻断发布）
- 无。P1-1（keadm 二进制不含 P2-4/P2-5 修复）已闭环：实测 `keadm-darwin-arm64 version` → gitCommit=1265214、buildTime=20:02:54，晚于修复提交 fe093e1（19:55:23）。

### P1（验收项缺失且文档未披露 / 声明与制品不一致）
- **G1｜WBS 7.2 RBAC、7.3 设备认证、7.5 审计日志未实现**，但 ROADMAP M4 覆盖声明、Release Notes "WBS 7" 表述未做区分，known issues 无对应条目 → 文档把"部分完成"表述为"完成"。
- **G2｜WBS 10.1 可观测性未实现**（M4 验收明确要求 Prometheus 指标 + Fluent Bit 日志），未列入 known issues。
- **G3｜WBS 8.4 性能测试未做**（100 节点注册 ≥99%、延迟 ≤3s、EdgeCore 内存 ≤256MB 三项 M4 验收全部无证据），未列入 known issues。
- **G4｜WBS 10.3 API 兼容矩阵未做**，未列入 known issues。
- **G5｜v0.1.0 发布镜像为 arm64 单架构**（images.json 自述"非 buildx 多架构"；多架构运行证据指向旧构建 f71684e），与 Release Notes §7"amd64+arm64 单 tag 双平台"声明不符——RELEASE-REVIEW P2-1 已提出，Notes/台账未修正。

### P2（建议补充/后续版本）
- **G6｜WBS 2.2/5.3 无真实 CRD 控制器**：仅有类型定义 + 内存态状态存储/API（代码自述"待 K8s apiserver 对接取代"）；"CRD 驱动 Mapper"验收未在真实集群验证。
- **G7｜WBS 5.2 仅 Modbus**：OPC-UA 适配未做；MQTT 仅 mock_sensor 数据面模式，无通用 MQTT 设备适配器；且 5.2 实际在 M4 轮交付（归属偏移，PROGRESS 未标注）。
- **G8｜8.6 批量操作未做、10.2 灰度发布未实现**：keadm 无批量 init/join；升级为整包替换，无灰度分批。
- **G9｜7.1 证书轮换人工、吊销未实现**（certs.go 自述；CA 泄露需全量重签）。
- **G10｜10.4 仅 SBOM，无镜像漏洞扫描**；cosign 签名延后（M4C P2-⑥ 已登记）。
- **G11｜E2E 规模与延迟指标缺口**：8.2/8.3 均为单节点联调，多节点（10+）E2E 未做；M3 验收"端到端延迟 ≤5s"从未测量。
- **G12｜mTLS 跨主机 CA 分发未自动化**：keadm join --tls 产物不含 CA；边缘首启自生成独立 CA 时与云端（各自独立 CA 池）握手必失败——DEPLOYMENT 要求"与云端共享同一 CA 目录"仅单机/人工拷贝成立，且自动生成模式下边缘需持有 **CA 私钥**（EnsureClientCert 用本地 CA key 签发），与 SECURITY.md"CA 私钥移出节点"建议冲突；多主机部署路径缺自动化引导。
- **G13｜发布缺 linux-arm64 二进制**：release/v0.1.0 仅 darwin-arm64 + linux-amd64（6 件中无 linux-arm64），边缘设备常见 arm64 平台需自行交叉编译。

---

## 5. 过时记录清单（文档滞后）

| # | 位置 | 过时内容 | 现状 |
|---|------|---------|------|
| D1 | PROGRESS.md §6 里程碑状态表 | M1-M5 全部标 **⏳ 未开始** | 与 §3（M1 ✅、M2-M5 🟨 完成）直接矛盾，严重过时 |
| D2 | PROGRESS.md 文件头 | "最后更新：2026-08-13 09:25" | 内容已含 08-14 全部轮次（§4G-4M） |
| D3 | PROGRESS.md §5 Backlog | "M2 完整化（6.2/6.4/6.5）"、"M3 二期 MQTT EventBus"、"Helm Chart 骨架" 仍列待办 | 已分别在 4H/4I/4J 完成，未清理（RELEASE-REVIEW P3-3 亦指出） |
| D4 | ROADMAP.md §1.1/§1.2/§3/§5 | WBS 2/3/5/7/8/10 状态列仍 ⬜/🟨，里程碑表未更新 | 基线文档状态列自 M0 后未维护（任务已提示"可能滞后"，确认属实） |
| D5 | RELEASE-NOTES §1 + 文件头 | 发布日期/发布制品 commit/制品位置仍【待回填】；文件头"发布制品制作中"；"98 commits"（现 108） | 制品已归档（d17bdd5）+ keadm 重建（733d0ae）后未回填（RELEASE-REVIEW P3-2 确认） |
| D6 | RELEASE-LEDGER.md §2 时间线 | 步骤 5-9（归档/镜像/复核/发布）仍标 ⏳ 待执行 +【回填】 | 与 §3 制品清单"已归档"状态矛盾（RELEASE-REVIEW P2-2，未见后续修复 commit） |
| D7 | UPGRADE.md §4 | "真实节点回滚…留待 M5" | 本发布即 M5，措辞过时（RELEASE-REVIEW P3-4） |
| D8 | KEADM.md 标题/命令表 | 标题"WBS 8.6 **基础版**"，命令表无 upgrade/rollback | upgrade/rollback/ops-ledger 已实现并发布（7aa035c/fe093e1），文档未同步 |
| D9 | MULTIARCH.md §7 | 镜像运行证据（digest 307c75fa/81df9cdb）对应旧构建 f71684e | 与 v0.1.0 新 digest（2f9f2fbe/227f6050）无关联证据，易误读为发布制品验证（RELEASE-REVIEW P2-1/P3-5） |
| D10 | images.json | `generatedAt=19:58:00` 晚于归档 commit d17bdd5（19:55:55） | 时间戳前向偏差（RELEASE-REVIEW P3-1） |

---

## 6. 核对统计

- 核对项：**31**（M3 9 项 / M4 13 项 / M5 9 项）
- ✅ 完成：**16**（3.5、3.6、5.1、5.4、5.5、4.5、7.4、10.2、8.5、9.2、9.3、9.4、9.5、发布产物、CRD 文档、温度 Demo）
- 🟨 部分：**8**（2.2、5.2、5.3、7.1、10.4、多架构镜像、8.6、15min 部署）
- ⬜ 未做：**7**（7.2、7.3、7.5、10.1、10.3、8.4、kubectl get nodes Ready）
- 文档滞后：**10 处**（D1-D10）
- 缺口：P0 **0**｜P1 **5**（G1-G5）｜P2 **8**（G6-G13）

### 总体结论

代码与验证工程本身质量扎实（审查闭环、发布复核独立、P1-1 及时重建修复）；**核心风险不在代码而在"声明口径"**：M4 的 WBS 7 实际只完成 7.1/7.4（7.2/7.3/7.5 缺失），10.1/10.3/8.4 三项验收级任务完全未做，但这些缺失未进入 Release Notes 已知问题清单，里程碑状态却标"完成"；M5 验收的集群路径（15min 部署、kubectl get nodes）因无真实集群从未执行，仅以 E2 笼统披露。建议：① 在 Release Notes/台账补充 G1-G5 对应披露条目（含 7.2/7.3/7.5、10.1、8.4、10.3 的"未实现"状态与后续版本规划）；② 修正 G5 镜像架构表述或补双架构发布构建；③ 清理 D1-D10 过时记录。
