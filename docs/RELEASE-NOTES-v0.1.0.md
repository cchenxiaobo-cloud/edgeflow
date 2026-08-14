# EdgeFlow v0.1.0 Release Notes（发布说明）

> 状态：✅ 已编制（2026-08-14）｜发布制品制作中，制品字段待发布制品工程师回填。
> 配套文档：`docs/DEPLOYMENT.md`（部署）、`docs/UPGRADE.md`（升级回滚）、`docs/MULTIARCH.md`（多架构镜像）、
> `docs/RELEASE-LEDGER.md`（发布台账）、`docs/DRILL-SCHEDULE.md`（生产演练排期）。

---

## 0. English Summary（英文摘要）

**EdgeFlow v0.1.0 — First MVP Release**

EdgeFlow is an edge-computing platform (KubeEdge-inspired) built in Go with zero heavy
runtime dependencies. This first MVP release delivers:

- **M1 – Cloud-Edge communication**: CloudHub/EdgeHub WebSocket channel, node
  registration with heartbeat and exponential-backoff reconnect, reliable delivery
  (pending + ack + retry, WBS 4.6), PodSync downlink, MetaManager SQLite persistence.
- **M2 – Edge autonomy**: Edged with declarative reconcile, multi-replica
  pods (WBS 6.5), health self-healing with CrashLoopBackOff (WBS 6.4), PodStatus
  reporting (WBS 6.3), ConfigSync (WBS 6.2), offline autonomy with recovery sync.
- **M3 – Device management**: DeviceTwin/shadow, Mapper framework with mock sensor,
  device APIs and command dispatch, MQTT EventBus data plane (WBS 3.6), Modbus
  simulator + mapper with op ledger (WBS 5.2).
- **M4 – Production hardening**: mTLS (mutual TLS, WBS 7), multi-arch
  linux/amd64+arm64 OCI images (WBS 8.5), Helm chart (WBS 8.5), keadm installer CLI
  with upgrade/rollback (WBS 8.6 / 10.2), NodeController health scan (WBS 2.4).
- **M5 – Release documents**: API spec, deployment guide, handoff guide, examples
  (WBS 9.2–9.5), release notes, ledger and drill schedule.

**Quality gates**: 98 commits; 24 packages race-enabled tests green; total coverage
77.8%; golangci-lint 0 issues; 0 P0 / 0 P1 review findings; end-to-end demo
`examples/demo.sh` passes 3×.

**Known limitations**: real-cluster installation not yet executed (validated locally
via `helm install --dry-run` and single-host demo); images not pushed to a remote
registry; GitHub remote not configured. See §4 Known Issues.

**Upgrade**: `keadm upgrade --version=<v>` or `helm upgrade --install edgeflow
build/charts/edgeflow`. **Rollback**: `keadm rollback --latest` / `helm rollback
edgeflow <rev>` / image digest revert. See §5/§6.

---

## 1. 发布信息（Release Information）

| 项目 | 内容 |
|------|------|
| 版本号 | **v0.1.0**（Chart `appVersion: v0.1.0`，Makefile `VERSION=v0.1.0`） |
| 发布类型 | MVP 首发（First MVP Release） |
| 编制日期 | 2026-08-14 |
| 发布日期 | 【待回填】由发布制品工程师在制品归档完成时填写 |
| 发布基线 commit | `ca2051b`（docs: add M4-finish M5-prep delivery report，98 提交） |
| 发布制品 commit | 【待回填】制品归档对应的 commit |
| 制品位置 | `release/v0.1.0/`（【待回填】由发布制品工程师归档后生效，见 `docs/RELEASE-LEDGER.md`） |
| 镜像 | `edgeflow/cloudcore:v0.1.0`、`edgeflow/edgecore:v0.1.0`（多架构，见 §7） |
| Helm Chart | `build/charts/edgeflow`（version 0.1.0） |

---

## 2. 变更内容（What's New，按里程碑分组）

> WBS 编号见 `docs/ROADMAP.md`；各里程碑验证证据见 `docs/PROGRESS.md` 对应章节。

### 2.1 M1 云边通信链路（WBS 4）——新增

| 类别 | 内容 | WBS/提交 |
|------|------|----------|
| 新增 | CloudHub/EdgeHub WebSocket 通道（心跳、断线检测、指数退避自动重连） | WBS 4.1/4.2/4.3 |
| 新增 | 节点注册/注册表/EdgeNode API（`GET /api/v1/nodes`、`/api/v1/edgenodes`） | WBS 4.3 |
| 新增 | 可靠投递：pending + Ack 匹配 + 超时同 ID 重试 | WBS 4.6 |
| 新增 | PodSync 下发链路（`POST /api/v1/nodes/{id}/podsync`，200/404/504 语义） | WBS 4.6 |
| 新增 | MetaManager：SQLite（WAL）元数据持久化（KV + 节点 + Pod 数据） | WBS 4.3 |
| 修复 | 4.6 P2 收尾：pending 交叉清理、ErrAckFailed→502、ReliableSend 支持 context、downlink 原子化、云端 operation 校验 | commit 8321b0e |

### 2.2 M2 应用部署与边缘自治（WBS 6）——新增

| 类别 | 内容 | WBS/提交 |
|------|------|----------|
| 新增 | Edged：ContainerRuntime 接口（Mock/Docker 双实现）+ 声明式 reconcile | WBS 6.3 |
| 新增 | 多副本 Pod：副本命名 `edgeflow-ns-name-index`、补齐/收缩策略、缺口兜底 | WBS 6.5 |
| 新增 | 健康自愈：Inspect 非 Running 自动重启 + RestartCount + CrashLoopBackOff（3 次阈值/30s 退避/60s 稳定重置） | WBS 6.4 |
| 新增 | PodStatus 上报（30s 周期，env 可配）+ 云端 Pod API（`GET /api/v1/pods`） | WBS 6.3 |
| 新增 | 断网自治：云端不可达时容器持续运行、本地调谐不受影响、恢复后自动同步 | WBS 6.x |
| 新增 | ConfigSync 配置下发（云端五态校验 + 可靠投递 + 边缘落盘 + 自动 Ack） | WBS 6.2 |
| 修复 | 删除收敛（P1）：delete → 容器移除 → Absent 终态 → 云端列表收敛；Secret 日志脱敏（P1） | — |

### 2.3 M3 设备管理（WBS 5）——新增

| 类别 | 内容 | WBS/提交 |
|------|------|----------|
| 新增 | DeviceTwin/影子：desired/reported 合并语义、深拷贝、自动创建 | WBS 5.x |
| 新增 | Mapper 框架（DeviceMapper 接口 + MapperRegistry）+ MockSensor 温湿度模拟 | WBS 5.x |
| 新增 | 设备 API：`GET /api/v1/devices`、`/api/v1/nodes/{id}/devices`、`POST device-command`（五态错误语义） | WBS 5.x |
| 新增 | MQTT EventBus 数据面（QoS1、AutoReconnect、OnConnect 订阅恢复）+ Mapper 遥测发布/指令订阅 + 无 broker 降级本地模式 | WBS 3.6 |
| 新增 | Modbus：自实现协议模拟器（5 功能码 + 错误码 + unit ID 校验 + 连接上限）+ Modbus Mapper + op_ledger 台账（30 天清理） | WBS 5.2 |

### 2.4 M4 生产化（WBS 7/8/2.4/10.2）——新增

| 类别 | 内容 | WBS/提交 |
|------|------|----------|
| 新增 | mTLS：CA/服务端/客户端证书幂等生成、双向强制认证（TLS1.2+、私钥 0600、半套 fail-fast）、wss 自动升级、SAN 可注入 | WBS 7.x |
| 新增 | 多架构镜像：linux/amd64 + linux/arm64 OCI manifest 单 tag 双平台、distroless nonroot、版本注入 | WBS 8.5 |
| 新增 | Helm Chart 完整化：values 真实化、资源/探针/env、/data 可写挂载、podSecurityContext | WBS 8.5 |
| 新增 | keadm 安装管理 CLI：init（cloudcore.yaml）/ join（env+systemd+install.sh）/ reset（校验清单+确认）/ version | WBS 8.6 |
| 新增 | 升级回滚机制：keadm upgrade/rollback + 备份模型（manifest+sha256）+ ops-ledger 台账 + `--simulate-failure` 演练 | WBS 10.2 |
| 新增 | NodeController：心跳超时扫描（interval/timeout env 可配、时钟可注入），状态机 Offline→Ready 闭环 | WBS 2.4 |
| 修复 | TLSSAN 非法条目 fail-fast（exit 1）、reset 篡改文件跳过（keadm-manifest 校验和）、unit ID 1-247 校验、float→uint16 四舍五入（0.1°C 粒度） | M4C P2×4 修复 |

### 2.5 M5 发布（WBS 9）——新增

| 类别 | 内容 | WBS |
|------|------|-----|
| 新增 | 文档定稿：API-SPEC（9.2）、DEPLOYMENT（9.3）、HANDOFF（9.4）、examples/README + demo.sh（9.5） | WBS 9.2-9.5 |
| 新增 | 发布文档：本 Release Notes + 发布台账（RELEASE-LEDGER）+ 生产演练排期（DRILL-SCHEDULE） | WBS 9.x |
| 改进 | 一键端到端 Demo（`examples/demo.sh`）：构建→双端启动→节点注册→Pod 下发→设备数据→MQTT→清理，干净环境 DEMO PASS×3 | WBS 9.5 |

### 2.6 质量门汇总（发布基线）

| 指标 | 结果 | 依据 |
|------|------|------|
| Git 提交 | 98（基线 commit `ca2051b`） | `git log` |
| 包测试 | 24 包 `go test -race` 全绿 | PROGRESS §4K/§4L |
| 总覆盖率 | **77.8%**（门槛 ≥70%） | `go test -race -cover ./...` |
| Lint | `golangci-lint run ./...` 0 issues | — |
| 代码审查 | 各里程碑 0 P0 / 0 P1（P2 见 §4） | docs/CODE-REVIEW-*.md |
| E2E Demo | `bash examples/demo.sh` DEMO PASS×3，0 残留 | PROGRESS §4L |

---

## 3. 改进（Improvements）与修复（Fixes）摘要

除 §2 各里程碑所列新增外，本版本还包含以下跨轮改进/修复（均为 P1 级，已在对应里程碑闭环）：

- **删除收敛**：Pod 删除后容器移除、Absent 终态上报、云端列表正确收敛（M2 P1，端到端验证）。
- **Secret 日志脱敏**：配置下发日志不再打印 Secret 明文（M3 P1）。
- **重连并发竞态**：OnConnect 全程持 RLock，race 下并发 Subscribe/Unsubscribe 无 panic（M3 P1）。
- **旧命名迁移**：多副本旧命名容器优先清理，消除 churn（M3 三期 P1）。
- **文档-实现一致性**：M4B 文档连接串/TLS 变量修正、SAN 注入、CA 公私钥匹配校验（M4B P1×4）。
- **keadm 协调点**：init→join→upgrade→reset 全链路校验清单合并式更新、默认 node-id 清洗（M4D 收尾）。
- **Modbus 写入精度**：目标温度写入由截断改为四舍五入到 0.1°C（25.55→256）。

---

## 4. 已知问题（Known Issues / 剩余风险）

> 来源：各里程碑审查 P2 待办（`docs/CODE-REVIEW-M*.md`）与 `docs/PROGRESS.md` §5 Backlog。
> 分类：**待办**（登记未修）、**观察**（偶发/低危，需持续关注）、**环境**（非代码问题，发布流程相关）。

### 4.1 发布流程相关（环境风险，发布前需处理）

| # | 问题 | 影响 | 缓解 | 跟进 |
|---|------|------|------|------|
| E1 | **镜像未推送到远端仓库**（仅本地 registry 验证） | 真实集群无法拉取镜像，Helm 部署不可用 | 发布流程推送 ghcr.io 或私有仓库（`docs/MULTIARCH.md` §3）；Chart 配置 `imagePullSecrets` | 发布制品工程师（本轮） |
| E2 | **真实集群安装未执行**（helm 仅 `--dry-run=client` + 单机 Demo） | 集群环境差异（网络/存储/证书）未暴露 | 生产演练排期验证（`docs/DRILL-SCHEDULE.md`）；先测试环境全量演练 | 运维 + 研发（演练窗口，需确认） |
| E3 | **远程仓库未推送**（GitHub remote 未配置） | CI（lint+test+release.yml）未在真实远端运行 | 用户配置 SSH key → `git remote add origin` → push（ENV-SETUP.md §4.2） | 用户（P1 Backlog） |
| E4 | 镜像 `:latest` 无 immutable 保护、无 cosign 签名/SBOM | tag 可被覆盖，供应链可追溯性弱 | 按版本 tag 引用；签名/SBOM 依赖 registry + cosign 基础设施 | M4C P2-⑥，延后至 M5 发布阶段（本轮 SBOM 随制品产出，签名仍缺） |

### 4.2 代码级已知问题（P2 待办）

| # | 问题（来源） | 影响 | 缓解 | 跟进 |
|---|--------------|------|------|------|
| K1 | restoreBackup 非事务性：回滚中途失败留混合状态（M4D P2） | 已修复（commit fe093e1：staging 原子替换） | v0.1.0 制品已含（keadm 重建 gitCommit=1265214） | ✅ 已修复（v0.1.0 含） |
| K2 | findBackup/restoreBackup 未对 manifest 文件清单做白名单校验（M4D P2） | 已修复（commit ef21a98：白名单+路径穿越拒绝） | v0.1.0 制品已含（keadm 重建 gitCommit=1265214） | ✅ 已修复（v0.1.0 含） |
| K3 | MQTT broker 生命周期管理缺失（M3B P2） | broker 由外部 systemd/容器管理 | 文档化部署方式（EVENTBUS-GUIDE.md）；broker 晚启动时 command 订阅需重启 edgecore 恢复 | P2 Backlog |
| K4 | ConfigMap/Secret 同名同 ns 互覆盖（M3B P2） | 配置语义二义性 | 文档化决策：按资源类型隔离，需产品确认 | P2 Backlog（产品确认） |
| K5 | Secret 落盘明文（M3B P2） | 边缘节点磁盘泄露风险 | 生产建议加密盘/权限控制；日志已脱敏 | P2 Backlog（后续版本） |
| K6 | QoS1 不保证去重；Publish token.Wait() 半死连接窗口阻塞（M3B/M3 三期 P2） | 消费方需幂等；极端网络下发布阻塞 | EventBus 消费侧幂等处理；Disconnect 兜底 | P2 Backlog |
| K7 | configs 无增量通知（M3B P2） | 当前无消费方，轮询对账可用 | 已文档化 | P2 Backlog（有消费方时实现） |
| K8 | 设备 API 边界：byDevice 路由不含 namespace、空 deviceName 未校验、LastReportedAt 单调性、502 Desired 分叉、无 TTL/GC（M3A P2×5） | 多租户/异常输入场景语义不完整 | 单租户 MVP 范围内可接受；API-SPEC 已明确语义 | P2 Backlog |
| K9 | 边侧时钟偏差（LastReconcileAt 依赖本地时钟）；无批量上报（逐 Pod 一条消息）（M2B P2） | 时间戳可能不一致；上报消息数随 Pod 数线性增长 | 单机/小规模场景可接受 | P2 Backlog |
| K10 | store 全局单锁；节点删除后 Pod 状态残留（有意为之）；旧连接关闭窗口可投递（M2B P2） | 并发量低时无感；残留为设计决策；微秒级窗口 | 已文档化 | P2 Backlog |
| K11 | Replicas=0 无法表达 scale-to-zero（int 非指针）；RestartCount 未进 PodStatusPayload+不持久化；调谐轮串行 30s 超时延迟累积；32-bit hash 碰撞；滚动更新策略；进程级 liveness（M3 三期 P2 节选） | 规模化/滚动发布场景受限 | MVP 范围外；已登记 | P2 Backlog |
| K12 | TLS 握手无超时/并发上限（net/http 固有）；gen-certs 与 Go 侧 CN 约定依赖脚本参数（M4B P2） | 资源耗尽风险在暴露公网时需关注 | 部署于内网；CLIENT_CN 可配且默认对齐 | P2 Backlog |
| K13 | 存量压测 TestShutdownDuringNewConnections 偶发 race 观察项（M4B P2） | 偶发 flaky（10+ 复跑全绿，非本轮改动） | 持续观察，复现时修复 | 观察项 |
| K14 | TestQoS1Delivery 偶发端口抢占（M3B P2） | 偶发测试失败（13 连跑全绿） | 端口冲突时重跑 | 观察项 |
| K15 | 边缘节点资源上报未采集（`/api/v1/nodes` 的 memory 恒 0） | 资源监控缺失 | 文档化（DEPLOYMENT.md §9） | 后续版本 |
| K16 | mTLS 证书自动生成默认 SAN 仅本机 | 跨主机部署需手动注入 SAN | 文档已覆盖（`EDGEFLOW_CLOUDCORE_TLS_SAN`） | 后续支持配置文件方式 |
| K17 | cmd-edgecore 覆盖率缺口（早期 56.5%，随轮次改善） | 集成链路单测覆盖不足 | 端到端 Demo + 联调兜底 | P2 Backlog（补集成级单测） |
| K18 | 优雅退出最坏延迟 30s（在途 docker 命令）；docker run 冲突兜底未验标签（M2A P2） | 停止偏慢；极端冲突低危 | 已文档化 | P2 Backlog |

---

## 5. 升级步骤（Upgrade）

> 完整机制见 `docs/UPGRADE.md`；部署细节见 `docs/DEPLOYMENT.md` §5。

### 5.1 keadm 产物升级（离线产物，WBS 10.2）

```bash
# 1. 备份当前产物 + 更新版本标记 + 台账（升级前自动备份到 backups/<ts>/）
keadm upgrade --version=v0.2.0 --output-dir=./keadm-out

# 2. 建议演练模式先验证回滚路径（产物不会被修改）
keadm upgrade --version=v0.2.0 --simulate-failure --output-dir=./keadm-out

# 3. 查询台账确认操作记录
KEADM_OPERATOR=<name> keadm ops-ledger --output-dir=./keadm-out
```

- 升级前预检：产物三件套（env/service/install.sh）存在；版本格式 `vX.Y.Z`；拒绝同版本升级。
- **数据兼容**：keadm 机制不触碰数据目录（SQLite 与用户文件全程不动）；升级前后建议对
  数据目录做 hash 基线比对（见 UPGRADE.md §5 演练）。
- 二进制/镜像侧升级：替换 edgecore 二进制或镜像 tag（`helm upgrade --set cloudcore.image.tag=...`），
  见 DEPLOYMENT.md §5.2。

### 5.2 Helm 升级（云端管理面）

```bash
helm upgrade --install edgeflow build/charts/edgeflow -f values-prod.yaml
# 或仅换镜像 tag
helm upgrade edgeflow build/charts/edgeflow --set cloudcore.image.tag=v0.2.0
helm history edgeflow   # 记录 REVISION，回滚用
```

- 云端 cloudcore 为内存态：升级重启后节点/Pod/设备查询数据清空，边缘重连后自动重新注册并恢复上报（自愈）。
- 边缘节点：建议先摘流（停业务 Pod）再替换二进制/重启，最后 `docker ps` 复核容器由新版本 Edged 接管。

---

## 6. 回滚说明（Rollback）

> 回滚预案详见 `docs/UPGRADE.md` §3（异常路径表）与 `docs/MULTIARCH.md` §6（镜像回滚预案）。

| 场景 | 回滚命令/操作 | 数据说明 |
|------|---------------|----------|
| keadm 产物升级失败 | `keadm rollback --latest --output-dir=./keadm-out`（或 `--backup=<id>`） | 备份含 manifest + sha256 完整性校验；仅恢复三个产物文件，**不触碰数据目录**；失败时备份保留 + 人工 `cp` 路径 |
| 无可用备份 | 重新 `keadm join` 生成产物 | 无自动恢复路径 |
| Helm 云端回滚 | `helm rollback edgeflow <REVISION>`（REVISION 见 `helm history`） | 仅恢复 Helm 管理资源，**不恢复数据**（数据库在 PVC 中） |
| 镜像损坏/版本不对 | 优先回退单架构覆盖 tag，或 `docker buildx imagetools create -t <img>:v0.1.0 <img>@sha256:<旧 digest>` | 单架构 tag 始终是可用兜底产物（MULTIARCH.md §6） |
| 数据目录 | 升级前 `cp data/edgeflow.db data/edgeflow.db.bak`；SQLite 跨小版本兼容 | keadm 不管理数据，一致性由升级前快照保障 |

**数据兼容说明**：SQLite 元数据库（`data/edgeflow.db`）跨小版本兼容；keadm 升级/回滚全程不触碰
数据文件（演练实测 hash 一致）；回滚后边缘节点重启自动重新注册，状态由上报恢复。

---

## 7. 兼容性（Compatibility）

| 维度 | 要求 | 说明 |
|------|------|------|
| Go | 1.26+（构建要求 1.26.2） | `go.mod` `go 1.26.2`；镜像构建 `golang:1.26-alpine` |
| 架构 | **linux/amd64 + linux/arm64** | 二进制静态编译（CGO_ENABLED=0）+ OCI manifest 单 tag 双平台；两架构 `--version` 输出一致 |
| Kubernetes | 标准 Deployment/Service 资源（Chart 未用实验特性）；本地验证 Helm v4.2.3，兼容 Helm 3.x | **真实集群安装未执行**（见 §4 E2），以演练验证为准 |
| 容器运行时 | Docker（Edged 管理 Pod 容器） | 运行时接口可扩展（Mock/Docker 双实现） |
| 操作系统 | Linux（arm64 为边缘主架构；amd64 云端/边缘均可） | macOS 仅开发/演示（单机 Demo 支持） |
| MQTT | broker 可选（缺失时设备链路降级本地模式） | mosquitto 等任意 MQTT 3.1.1 broker |
| 数据面 | SQLite（modernc.org 纯 Go，无 cgo 依赖） | 无需 libc/glibc，distroless 可直接运行 |

---

## 8. 验证方式（How to Verify）

```bash
# 版本信息
./bin/cloudcore --version    # version=v0.1.0 gitCommit=... goVersion=go1.26.x

# 质量门
go test -race -cover ./...   # 24 包全绿，覆盖率 ≥70%
golangci-lint run ./...      # 0 issues
helm lint build/charts/edgeflow && helm install --dry-run=client edgeflow build/charts/edgeflow

# 端到端
bash examples/demo.sh        # 期望最终输出 DEMO PASS

# 镜像（构建后）
docker run --rm edgeflow/cloudcore:v0.1.0 --version
docker buildx imagetools inspect <registry>/edgeflow/cloudcore:v0.1.0   # amd64+arm64
```

---

## 9. 免责与确认事项（Caveats & Confirmations）

1. **发布前确认**：E1（镜像推送）与 E3（远程仓库）完成后方可对外发布；E2 由生产演练补足。
2. **已知问题清单**随发布制品归档同步更新（`docs/RELEASE-LEDGER.md` 复核栏）。
3. **本文件为发布基线文档**；制品字段（发布日期/制品 commit/checksum/SBOM/镜像 digest）
   由发布制品工程师回填，回填后本文件与台账保持一致。
