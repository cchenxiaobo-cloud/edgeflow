# EdgeFlow v0.5.0 Release Notes

> 状态：⏳ 开发轮进行中（2026-08-24；验证证据为占位，构建/集成轮实测后回填）。
> 版本决策：新功能（外部 etcd 模式，兑现 v0.4.0 预留项）→ minor（v0.5.0）。
> 核心主题：**外部 etcd 支持（方案④）+ 单写者形态铁律 + 明文护栏**——`EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` 非空即直连共享 etcd 集群（跳过 embed），业务层零改动；多副本 active-active 明确不支持；非回环+无 TLS 拒绝启动。
> 配套：docs/ARCHITECTURE.md（决策 R14）、docs/DEPLOYMENT.md §10.7（配置/拓扑/迁移/排障）、docs/KNOWN-ISSUES.md §5（L1/L5/L7）。

---

## 一、主题概述

1. **外部 etcd 模式（本版本核心价值，兑现 v0.4.0 承诺）**：cloudcore 以 `go.etcd.io/etcd/client/v3`（v3.5.33，零新增依赖）**直连共享 etcd 集群**作为云端状态持久化事实源——`EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` 非空即外部模式，**跳过内嵌 embed**（不建数据目录、不占 12379/12380 端口、忽略 DATA_DIR/CLIENT_URL/PEER_URL/QUOTA/COMPACTION/STRICT）。三存储包装（registry/devicestatus）与 HTTP API **零改动**（v0.4.0 冻结的 `KVStore` 接口即替换面）。embed 模式行为逐位不变（回归锚点）。
2. **单写者形态铁律（多副本 active-active 明确不支持）**：心跳/Offline 判活是各副本**内存瞬态**（不落盘），多副本会把彼此存活的节点误判离线，并从共享键空间删键 + 级联删设备 Desired（活节点数据丢失，且无信号）。v0.5.0 受支持形态 = **replicaCount=1**（embed 与外部模式一律如此，Chart 模板 `{{ fail }}` 渲染守卫双保险）；真多活（etcd lease 心跳）划 v0.6+。
3. **护栏与故障语义（外部依赖不静默降级）**：外部模式连接失败/无 quorum = **无条件拒绝启动**（clientv3 懒连接，靠显式启动连通性检查——线性一致读元键，至多 3 次尝试、最坏 ≈17s（预算 ≤20s）——落实 fail-fast）；非回环端点 + 无 TLS = 拒绝启动（逃生门 `EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE=1`）；运行中断连由 clientv3 自动重连、应用层零重试，读路径纯内存不受影响；/healthz 不反映 etcd（避免 K8s 批量重启放大故障）。

## 二、核心特性明细

| 特性 | 说明 |
|------|------|
| 外部模式触发 | `EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS`（逗号分隔 http(s) URL）非空 = 外部直连，跳过 embed；未设置/空 = **embed 模式逐位不变**（v0.4.0 行为）；`ETCD_ENABLED=false` 总开关优先一刀切纯内存（配错的外部配置不阻断逃生） |
| 端点校验（fail-fast） | 空条目/非合法 URL/缺端口/带路径|query|fragment|userinfo/混合 scheme 均拒绝启动并点名原因；host 回环**允许**（外部模式不做回环限制）；错误文案统一 `etcdstore: 环境变量 EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS=... 非法: <原因>` |
| TLS | `EDGEFLOW_CLOUDCORE_ETCD_TLS_CA` 非空即启用（服务端证书校验）；`_TLS_CERT/_TLS_KEY` 同设即 mTLS（只设其一 fail-fast）；TLS 启用 ⇔ 全部端点 https（混配拒绝）；`MinVersion=TLS1.2`、`InsecureSkipVerify` 恒 false；本地测试路径 = 自签 CA + 临时 embed 模拟外部集群 |
| 启动连通性检查 | `clientv3.New`（懒连接）后显式 Get `SchemaVersionKey`（线性一致读，验证集群可服务含 quorum，非仅端口可达）；失败至多 3 次尝试（单次 5s、间隔 1s，最坏 ≈17s、预算 ≤20s）→ 拒绝启动；错误文案区分 `Unavailable`（不可达）与 `PermissionDenied`（鉴权引导：请在 etcd 侧为 /edgeflow/ 授权） |
| schemaVersion 钩子 | `/edgeflow/_meta/schemaVersion`（v0.4.0 预留键）启动期核对：无键→写 `"1"`；==1→no-op；≠1→Warn 不阻断；失败仅告警（两模式共用，装配区实现） |
| 明文护栏 | 非回环端点 ∧ 未启用 TLS → 装配期拒绝启动；显式逃生门 `EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE=1` 放行并启动期大告警（仅限可信内网/开发） |
| embed 字段忽略 + Warn | 外部模式下显式设置的 DATA_DIR/CLIENT_URL/PEER_URL/QUOTA/COMPACTION/STRICT 被忽略（不校验不生效），启动期 `Warn`"仅 embed 生效"（防误以为已加密/已限配） |
| 运行中故障语义 | clientv3 指数退避自动重连、端点 failover；断连期写失败返回 error 且内存不动（读路径纯内存出最后一致状态），恢复后自动续写；**无应用层重试**（L3）；共享集群键空间 `/edgeflow/` 根固定（L2） |
| 多副本 | **单写者铁律**：embed/外部模式 replicaCount 均必须 1；Chart `{{ fail }}` 渲染守卫（embed=脑裂、external=误判离线删键） |
| Chart | `cloudcore.etcd.external.{enabled,endpoints,tls.{ca,cert,key},allowInsecure}`（默认全关，endpoints 默认空）；external.enabled=true → 注入 ENDPOINTS/TLS/逃生门 env、不创建 PVC（数据在共享集群）；etcd.enabled=false → 强制 `EDGEFLOW_CLOUDCORE_ETCD_ENABLED=false`、忽略 external.* |

## 三、配置表（环境变量，`EDGEFLOW_CLOUDCORE_*` 前缀）

### 3.1 新增（外部模式）

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` | 空（embed） | 逗号分隔 http(s) URL；**非空 = 外部模式**；逐条目校验 fail-fast（§二） |
| `EDGEFLOW_CLOUDCORE_ETCD_TLS_CA` | 空 | PEM CA 路径；非空 = 启用 TLS；启动期加载失败（不存在/不可读/坏 PEM）→ 拒绝启动 |
| `EDGEFLOW_CLOUDCORE_ETCD_TLS_CERT` | 空 | mTLS 客户端证书路径；与 KEY 同设/同缺（只设其一 fail-fast） |
| `EDGEFLOW_CLOUDCORE_ETCD_TLS_KEY` | 空 | mTLS 客户端私钥路径（同上） |
| `EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE` | 空（off） | **逃生门**：`1` 放行「非回环+无 TLS」启动（启动期大告警）；默认拒绝（明文护栏） |

> 外部模式下 v0.4.0 embed 变量（DATA_DIR/CLIENT_URL/PEER_URL/QUOTA/COMPACTION/STRICT）**不生效**（显式设置仅 Warn）；`ETCD_ENABLED=false` 优先于一切。

### 3.2 保留（embed 模式，v0.4.0 原位不变）

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `EDGEFLOW_CLOUDCORE_ETCD_ENABLED` | `true` | 总开关；`false` = 纯内存（v0.3.x 行为，不建目录/不占端口/不写盘） |
| `EDGEFLOW_CLOUDCORE_ETCD_DATA_DIR` | `data/etcd` | embed 数据目录（容器内 `/data/etcd`，挂载自 PVC） |
| `EDGEFLOW_CLOUDCORE_ETCD_CLIENT_URL` | `http://127.0.0.1:12379` | embed 客户端监听（只绑回环） |
| `EDGEFLOW_CLOUDCORE_ETCD_PEER_URL` | `http://127.0.0.1:12380` | embed peer 监听（单成员） |
| `EDGEFLOW_CLOUDCORE_ETCD_QUOTA_BACKEND_BYTES` | `268435456`（256MiB） | embed 后端配额 |
| `EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_MODE` | `periodic` | embed 自动压缩模式 |
| `EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_RETENTION` | `1h` | embed 压缩保留期 |
| `EDGEFLOW_CLOUDCORE_ETCD_STRICT` | 空（off） | embed 启动失败即拒绝启动；**外部模式无视**（恒 fail-fast，M7） |
| `EDGEFLOW_CLOUDCORE_NODE_RETENTION` | `24h` | 节点保留期（两模式同一口径） |

### 3.3 配置矩阵（M1-M8 简表，组合语义速查）

| 行 | ETCD_ENABLED | ENDPOINTS | TLS_CA | TLS_CERT/KEY | 行为 |
|----|---|---|---|---|---|
| M1 | `false` | 任意 | 任意 | 任意 | **纯内存**（v0.3.x 兼容）；ENDPOINTS/TLS 配错不报错（总开关优先，逃生） |
| M2 | `true` | 空 | 任意 | 任意 | **embed**（v0.4.0 逐位不变，含降级/STRICT） |
| M3 | `true` | 非空 | 空 | 空 | **外部·明文**：全部端点必须 http；非回环+无 TLS → 拒绝启动（除非逃生门） |
| M4 | `true` | 非空（全 https） | 非空 | 空 | **外部·TLS 服务端认证**（CA 校验，客户端无证书） |
| M5 | `true` | 非空（全 https） | 非空 | 同设 | **外部·mTLS**（CA + 客户端证书双向） |
| M6 | `true` | 非空 | 任意 | 只设其一 | **fail-fast**（CERT/KEY 必须同设或同缺） |
| M7 | `true` | 非空 | 任意 | 任意 | 连接失败/启动检查不过 → **一律拒绝启动**（STRICT 无意义，恒 strict） |
| M8 | `true` | 非空 | 非空（CA 文件不存在/不可读） | — | **fail-fast**（启动期加载 CA 失败） |

## 四、变更清单

| 面 | 变更 | 说明 |
|----|------|------|
| 代码（config） | `cloud/pkg/etcdstore/config.go` 新增 `Endpoints/ExternalTLS{CAFile,CertFile,KeyFile}` 与 `External()/TLSEnabled()`；`ConfigFromEnv` 按 §1.4 顺序短路（Enabled=false → ENDPOINTS/TLS 分支 → embed 原路径逐位不动） | 实现：subagent_02（本轮文档编写时代码在途） |
| 代码（连接层） | `etcdstore` 新增外部连接工厂 `NewKVStoreWithTLS`（clientv3.New + TLS）+ `ProbeAlive`（Get SchemaVersionKey 线性一致探活，区分 Unavailable/PermissionDenied 文案）+ `EnsureSchemaVersion`（`/edgeflow/_meta/schemaVersion`，告警不阻断）；外部模式无 embed 层、不进 downgrade 分支（包装失败=拒绝启动） | 同上 |
| 代码（装配） | `cmd/cloudcore` etcd 分支三态：内存 / 外部（fail-fast + 无降级）/ embed（v0.4.0 不变 + schemaVersion 调用） | 同上 |
| Chart | `build/charts/edgeflow`：version/appVersion → 0.5.0；values 新增 `etcd.external.*`（默认全关）；模板 external env 分支 + 3 个 `{{ fail }}` 守卫（embed>1 / external>1 / external 无端点）；外部模式跳过 PVC；etcd.enabled=false 强制 ENABLED=false 并忽略 external.* | 本文档轮已完成并 helm 验证（见 §七） |
| 测试 | 解析全矩阵（M1-M8）、外部连通 fail-fast、TLS 自签 CA 三态、schemaVersion、E2E 重启恢复/断连恢复、外部模式无端口无目录验收；embed 回归零改动 | 实测数据待集成轮回填 |
| 文档 | ARCHITECTURE R14；DEPLOYMENT §10.7（配置/拓扑/迁移 runbook/排障）；KNOWN-ISSUES §5（L1/L5/L7）；本文件 | 已完成 |

## 五、升级注意事项（v0.4.0 → v0.5.0）

1. **默认行为不变**：不设置 `ENDPOINTS` 的存量部署 = embed 模式，配置/数据/Chart 默认值全部与 v0.4.0 一致，无需任何迁移动作。
2. **外部模式是显式选择**：设置 `EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` 即切换；首次切换请走 DEPLOYMENT §10.7.5 迁移 runbook（快照恢复或零迁移自愈），并确认外部集群建议配置（3 节点奇数/同地域/quota 256MiB/compaction 1h/最小权限角色）。
3. **Chart 守卫生效**：`replicaCount>1` 的存量 values 文件（若有）在 v0.5.0 渲染期直接失败并给出原因——两种模式一律单写者。
4. **安全基线**：外部模式默认拒绝「非回环+明文」；生产配置 TLS_CA（推荐 mTLS）+ etcd 侧 `--client-cert-auth`/最小权限角色。逃生门 `EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE=1` 仅供可信内网。
5. **依赖就绪时序**：外部 etcd 集群应先就绪再启动 cloudcore（启动检查最坏 ≈17s、预算 ≤20s，失败即拒绝启动、退出码非 0，由编排层重启策略处理）。

## 六、已知限制（v0.5.0 登记，详见 docs/KNOWN-ISSUES.md §5）

- **L1 无鉴权参数透传**：不支持 username/password/CN 映射；集群开启鉴权且未授权 → 启动 fail-fast（错误文案含引导）。缓解：etcd 侧最小权限角色（readwrite `/edgeflow/`）+ mTLS；v0.6+ 候选 `EDGEFLOW_CLOUDCORE_ETCD_USERNAME/PASSWORD`。
- **L5 跨副本 SetDesired 整记录覆盖**：多副本并发写同一设备互相丢更新（读本副本内存合并→整记录 Put）。v0.5.0 单写者形态（replicaCount=1）下不触发；v0.6+ 候选条件写/按 property 拆键——**v0.5.0 登记不修**。
- **L7 GC 删除失败不重试**（v0.4.0 既有，`requeueGCEvent` 死代码）：etcd 删除失败只告警不重入队，节点残留孤儿键（外部集群故障窗口放大可见性）；重启自愈（Load 读回 → 保留期后再 GC）。**v0.5.0 登记不修**；v0.6+ 修复候选：失败时调用 `requeueGCEvent`。
- **embed 字段在外部模式忽略 + Warn**（行为说明，非缺陷）：DATA_DIR/CLIENT_URL/PEER_URL/QUOTA/COMPACTION/STRICT 在外部模式不生效，显式设置仅启动期 Warn"仅 embed 生效"——防误以为 embed 配置（如 QUOTA/COMPACTION）已作用于外部集群，集群侧需自行配置（§三.3.2 附注）。
- 其余沿用 v0.4.0：坏 WAL 降级语义（embed）、Pod/上报短暂清空、/healthz 不反映 etcd（有意为之，L8）等。

## 七、验证摘要（v0.5.0 开发轮）

### 7.1 Chart 验证（已完成，helm v4.2.3）

| 场景 | 结果 |
|------|------|
| 默认 embed（无 external 设置） | ✅ `helm template` 成功：embed env 全套注入 + PVC 创建（与 v0.4.0 渲染一致） |
| external 开（2 端点 + mTLS + 逃生门） | ✅ 成功：`EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` 逗号连接 + TLS_CA/CERT/KEY + ALLOW_INSECURE=1 注入，无 PVC、volume 回退 emptyDir |
| embed + replicaCount=2 | ✅ 渲染**失败**：`{{ fail }}`——多副本 embed 脑裂（exit≠0） |
| external + replicaCount=2 | ✅ 渲染**失败**：`{{ fail }}`——外部模式单写者铁律（exit≠0） |
| external.enabled=true + endpoints 空 | ✅ 渲染**失败**：endpoints 不能为空 |
| etcd.enabled=false + external.enabled=true + replica=3 | ✅ 成功：强制 `EDGEFLOW_CLOUDCORE_ETCD_ENABLED=false`、忽略 external.*（纯内存多副本无数据安全面） |
| `helm lint build/charts/edgeflow` | ✅ 1 chart linted, 0 failed |

### 7.2 代码/集成验证（占位，构建后填实测）

| 项目 | 结果 |
|------|------|
| etcdstore 单测（解析全矩阵 M1-M8 / 外部连通 fail-fast / TLS 三态 / schemaVersion） | ⏳ 实施轮交付后回填 |
| 既有测试回归（embed 路径零改动锚点） | ⏳ 同上 |
| E2E：外部模式注册→重启恢复 / 断连恢复（写失败内存不动→恢复自愈）/ 无 12379/12380 监听、无 data/etcd 目录 | ⏳ 集成轮回填 |
| 全仓编译与三平台交叉编译 | ⏳ 构建轮回填 |
| 版本兼容实测（服务端 3.5.x 主支持；3.6.x 冒烟） | ⏳ 集成轮回填 |
| 制品/合规（Trivy 重扫零新增预期、SBOM 组件数同比、Chart 0.5.0 打包） | ⏳ 发布轮回填 |

## 八、后续里程碑（Roadmap）

- **v0.6+ 候选**：etcd lease 心跳真多活（单写者铁律的解除条件）；鉴权参数透传（L1）；SetDesired 条件写/按 property 拆键（L5）；GC 删除失败重入队（L7）；/metrics 增 `edgeflow_etcd_connected` gauge + 写失败计数（L8）；端点列表动态变化（AutoSyncInterval）（L10）。
- **既有 backlog** 不变（见 docs/ROADMAP.md、docs/PROGRESS.md §5）。