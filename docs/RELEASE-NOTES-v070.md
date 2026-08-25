# EdgeFlow v0.7.0 Release Notes

> 状态：🚧 **v0.7.0 开发轮**（2026-08-25；本文档编写时实施/集成验证在途，验证摘要 §七 待回填；发布后状态翻转为已发布并回填实测数据——对齐 v0.6.0 流程）
> 版本决策：新功能（手册 F41 模型仓库与版本管理 + F42 模型灰度发布落地为正式能力）→ minor（v0.7.0）。
> 核心主题：**模型仓库 / 版本管理 / 灰度发布**——云端模型 + 版本两级台账（"镜像即模型、Tag 即版本"）、版本状态机（draft/active/archived）、灰度发布执行器（按节点白名单/按比例、分批、fail-fast、取消、回滚），全部经既有 REST 鉴权+审计链暴露；**边缘零代码改动**（复用 podsync + config-sync 幂等下发）。
> 配套：docs/ARCHITECTURE.md（决策 R16）、docs/API-SPEC.md §7（模型 API 契约）、docs/DEPLOYMENT.md §10.9（模型仓库与灰度发布）、docs/KNOWN-ISSUES.md §7（L21-L31）。

---

## 一、主题概述

1. **模型仓库与版本管理（F41，本版本核心价值之一）**：云端新增 `cloud/pkg/modelrepo`——Model（模型台账）/ ModelVersion（版本台账，状态机 draft→active→archived，activate 自动降级旧 active）/ 部署影子（版本—节点—时间台账）三级对象，etcd 键空间 `/edgeflow/models/`（写穿持久化 + AtomicKV CAS + 同模型在途发布 guard 守卫）；模型以"镜像确认 + 元数据台账"管理，镜像实体仍在客户镜像仓库（延续"镜像即模型"），sha256 摘要登记防篡改。
2. **灰度发布（F42，本版本核心价值之二）**：云端新增 `cloud/pkg/modelrelease` 发布控制器——目标选择（白名单/按比例，**创建时物化快照**）、分批调度（batchSize/pauseBetween）、fail-fast（默认开）、批次边界取消、逆序回滚；多副本形态下 release 级**领跑锁**（grant-per-claim，TTL 默认 60s）与崩溃接管（≤TTL，perNode 已部署跳过、NextBatchAt 持久化保节奏）。
3. **边缘零改动（兼容性承诺）**：发布/回滚完全复用既有 PodSync（镜像 Pod）+ ConfigSync（模型版本/参数 ConfigMap，保留键 + metadata 平铺载荷约定）经 ReliableSend 下发——云边协议**无新消息类型**、edgecore **无任何代码变更**（模型版本感知 = config-sync 载荷约定，MetaManager SQLite 落盘 + EdgeHub 幂等去重保证收敛）。既有 **14 端点逐字节不变**（**总端点数 14→31**，新增 17 个模型 API 端点，202/422 错误语义；auth+audit 自动覆盖）。
4. **三模式兼容**：纯内存（mutex 串行，release/影子重启丢失 L22 明示）/ embed（默认，写穿持久化）/ 外部 etcd 多副本（CAS + guard + watch 缓存同步 + 领跑锁接管）同实现、同行为口径（单副本 CAS 恒成功）。

## 二、核心特性明细

| 特性 | 说明 |
|------|------|
| 模型台账（Model） | 模型名唯一（`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`，禁 `/`）、description/type/metadata；PUT 更新（metadata 整表替换）；DELETE 前置（无 active 版本、无在途发布，级联 draft/archived 版本+部署影子） |
| 版本台账（ModelVersion） | "Tag 即版本"：mirror（镜像 ref 必带 tag，正则校验）+ sha256（`^sha256:[0-9a-f]{64}$`）+ archs（amd64/arm64 白名单，空=不限制）+ metadata（模型参数，发布时平铺进 config-sync）；状态机 draft→active→archived（两键 CAS + 失败补偿）；发布/回滚不改变版本状态 |
| 部署影子（DeploymentState） | `/edgeflow/models/deployments/<model>/<nodeID>`：podsync+config-sync 双 acked 后写穿；`GET .../deployments` 提供"版本—节点—时间"追踪；**派生台账整值覆盖、无 CAS 需求**（与 Desired 的差异为有意设计，R16/P9） |
| 灰度发布（ModelRelease） | 创建返回 **202**；target 二选一：nodeIDs 白名单（全部须已注册，422 列 unknownNodes）/ percentage（分母 = 创建时刻 Ready 节点，**n = ceil(ready × pct / 100)** 上取整，0 台 Ready → 422；字典序取前 n）；批次 = TargetNodes 切片（批内逐节点串行，**batchSize 控制批粒度非并发度**，D6）；failFast 默认 true；批间 pause 经 NextBatchAt 持久化（跨接管保节奏） |
| 发布状态机 | pending→running→succeeded / failed（fail-fast 中止或跑完有失败）/ canceled（批次边界生效，剩余节点 ≤1 扫描周期补 skipped）/ rolled_back（逆序批量、失败不回滚中止）；终态后 guard 释放，同模型可再发布；succeeded 前置防御性不变式（全部 perNode 终态 deployed 无 failed，D8） |
| 并发控制 | guard 键（create-if-absent）同模型在途互斥（409 含在途 releaseID）；全部业务写走 AtomicKV CAS（冲突重试 ≤3）；激活双键 CAS + 补偿；**孤儿 guard 自愈**（D3：冲突后读 release 键，不存在/终态 → 按值 CAS 删 guard 重试一次）+ 模型 meta 复查（D7）；**回滚执行期复查**（D2/D4：被新版本接管/PrevActive 被删 → 中止 failed） |
| 领跑锁（外部多副本） | `/edgeflow/models/releases/<id>/lock` 租约键，grant-per-claim（每 TTL/3 刷新重绑，默认 60s→20s，D5）；崩溃 ≤TTL 接管续跑（L21）；embed/内存单实例恒获取成功（逻辑空转） |
| config-sync 载荷约定 | ConfigMap `configs/edgeflow/edgeflow-model-<sanitized>`：保留键 model/version/mirror/sha256/type/releasedAt + 版本 metadata 平铺（冲突保留键优先 + Warn）；推理容器挂载即得"当前版本与参数"；回滚同通道把 version 改回 prevActive（边缘纯声明式收敛） |
| 命名约定 | `sanitize(name)` = 小写 + `.`→`-`；podName = cfgName = `edgeflow-model-<sanitized>`；namespace 固定 `edgeflow`；replicas=1（"该版本上机"，多副本由用户自行 podsync 编排） |
| 17 个新端点 | 模型 5 + 版本 6（CRUD4+activate+archive）+ 发布 5（创建/列表/详情/取消/回滚）+ 部署影子 1；列表 K8s List 风格；发布详情返回现算 summary（total/deployed/failed/pending/skipped） |
| 错误语义新增 | **202**（异步受理：发布创建/回滚置位）；**422**（业务前置不满足：目标版本非 active / 无 Ready 节点 / 白名单含未知节点 / 无 PrevActive）；409 家族扩展（状态机非法/在途发布/CAS 耗尽/回滚被接管） |

## 三、配置表（环境变量，`EDGEFLOW_CLOUDCORE_*` 前缀）

### 3.1 新增（v0.7.0，全部可选）

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `EDGEFLOW_CLOUDCORE_RELEASE_SCAN_INTERVAL` | `5s` | 发布控制器扫描周期（>0，非法 fail-fast，对齐既有 duration env 风格；含认领/推进/取消收敛/回滚执行/guard 释放） |
| `EDGEFLOW_CLOUDCORE_RELEASE_LOCK_TTL` | `60s` | 发布领跑锁租约 TTL（**>=15s**，非法 fail-fast）；刷新周期 = `max(5s, TTL/3)`（D5，默认 60s→20s）；**仅外部模式消费**，embed/纯内存显式设置 → Warn 忽略（并入 warnEmbedFieldsIgnored 族） |

### 3.2 键空间增量（唯一新增前缀，既有键/值 JSON 逐字不变）

```
/edgeflow/models/meta/<model>                        # 模型台账（写穿/CAS）
/edgeflow/models/versions/<model>/<version>          # 版本台账（写穿/CAS）
/edgeflow/models/guards/<model>                      # 在途发布守卫（create-if-absent；终态删除；孤儿自愈）
/edgeflow/models/releases/<releaseID>                # release 头（状态机 CAS 键）
/edgeflow/models/releases/<releaseID>/nodes/<nodeID> # 逐节点结果（独立键，CAS）
/edgeflow/models/releases/<releaseID>/lock           # 领跑锁（租约键，grant-per-claim，TTL 默认 60s）
/edgeflow/models/deployments/<model>/<nodeID>        # 部署影子（写穿）
/edgeflow/_meta/schemaVersion                        # 既有：schema 钩子（不 bump——业务键形态未变）
```

## 四、变更清单

| 面 | 变更 | 说明 |
|----|------|------|
| 代码（modelrepo） | 新包 `cloud/pkg/modelrepo`：`types.go`（数据模型/校验/状态机常量）、`store.go`（ModelStore 接口 + MemoryModelStore）、`etcd_store.go`（EtcdModelStore：写穿 + AtomicKV CAS + watch 应用器只写内存） | 实现：实施轮（本文档编写时代码在途） |
| 代码（modelrelease） | 新包 `cloud/pkg/modelrelease`：`plan.go`（SelectPercentageNodes/BuildBatches 等纯函数）、`deploy.go`（Deployer：podsync+config-sync 下发 + 错误映射）、`controller.go`（认领/推进/取消/回滚/guard 释放/领跑锁） | 同上 |
| 代码（装配/API） | `cmd/cloudcore`：`model_api.go`（17 端点 handler + `Register` 一次性挂载）、main.go/etcd.go 装配（新 env 解析、三模式存储装配、控制器 Run） | 同上 |
| Chart | version/appVersion → 0.7.0；无新增 values 必填项（可选透传发布控制器 env 注释行） | ✅ 本文档轮已完成并 helm 验证（§七.7.1） |
| 文档 | ARCHITECTURE R16 + §2.1/§2.2/§10；API-SPEC §7（17 端点 + 202/422 + config-sync 载荷 + 部署影子）+ §1.1 14→31；DEPLOYMENT §10.9；KNOWN-ISSUES §7（L21-L31）；ROADMAP 处置登记；API-COMPATIBILITY 追加 17 端点；手册 v1.1.0（第 4 章重写、附 C 状态翻转） | ✅ 已完成 |
| 测试 | 单测 V1/V2/S1-S4/A1-A3/C1-C4/H1-H3（10.1 清单）；进程级 E2E E1-E9（含三模式与双副本接管）；既有测试零改动通过 = 回归锚点 | 实测数据待集成轮回填 |

## 五、升级注意事项（v0.6.0 → v0.7.0）

1. **默认行为不变**：不设置任何新 env → 既有 14 端点、registry/devicestatus/判活等行为逐字节不变（新功能纯增量）；边缘节点**零动作**（edgecore 无需升级，模型版本感知 = config-sync 载荷约定）。
2. **升级 = 零迁移动作**：v0.7.0 只新增 `/edgeflow/models/` 前缀，既有键/JSON 逐字不变；走「全停再全起」（scale 0 → 1）惯例——混合版本多副本未验证（L29），**建议同版本全量切换**。步骤见 DEPLOYMENT.md §10.9.4。
3. **回滚 = 残留键无害**：v0.6.0 不读不写 `/edgeflow/models/` 前缀；可选 `etcdctl del /edgeflow/models --prefix` 显式清理（embed：127.0.0.1:12379）。
4. **新增端点鉴权/审计自动覆盖**：`EDGEFLOW_CLOUDCORE_AUTH=on` 时模型 API 同样需 Bearer Token（401 fail-fast），审计台账自动记录（action 按路由模式入库）。
5. **生产建议 embed/外部模式**：纯内存模式下 release 任务与部署影子重启丢失（L22 明示）。
6. **语义知悉**：百分比发布目标集合**以创建时快照为准**（不同模式/迁移后不跨模式可比）；batchSize 不是并发度（批内串行）；发布成功 ≠ 镜像可用（镜像拉取在边缘，PodStatus 会暴露拉取失败）。

## 六、已知限制（v0.7.0 登记，详见 docs/KNOWN-ISSUES.md §7）

| # | 限制（一句话） | 缓解 |
|---|---------------|------|
| L21 | 领跑者崩溃接管延迟 ≤ 锁 TTL（默认 60s）：接管前批次不推进 | LockTTL 可配；文档明示（DEPLOYMENT §10.9） |
| L22 | 纯内存模式：release 任务与部署影子**重启丢失** | 三模式表明示；生产建议 embed/外部 |
| L23 | 半部署状态：podsync 成功、config-sync 失败 → 节点已切镜像未切参数，计 failed | 重试发布/回滚收敛（边缘声明式调谐最终一致）；perNode reason 可查 |
| L24 | 回滚部分失败仍置 rolled_back（尽可能回滚） | perNode 明细 + Warn 日志；人工复核 |
| L25 | 级联删除非事务：模型删除中途崩溃 → 孤儿版本/部署键（不可见） | 删除前清 guard；启动加载只认 meta 存在前缀；可选 etcdctl 清理 |
| L26 | 回滚守卫：release.version ≠ 当前 active（被更新版本接管）→ 拒绝回滚 | 文案引导显式 activate 或新发布；执行期复查中止（D2/D4） |
| L27 | cancel 后 perNode 补齐 skipped 有 ≤1 扫描周期窗口 | 查询方容忍；文档明示 |
| L28 | release/模型列表无分页（全量返回）；终态 release 常驻内存无 GC（N-4） | 数据量 = 任务规模，当前可接受；后续版本分页/GC |
| L29 | 混合版本多副本（v0.6.0+v0.7.0 同连一集群）未验证 | 升级/回滚全停再全起；旧版不读新前缀，理论无害仍建议同版本 |
| L30 | 孤儿 guard 自愈语义（D3）：guard 写后崩溃 → 创建重试自愈（按值 CAS 删 guard + 重试一次） | 存储层实现 + 文档登记；S4 用例覆盖 |
| L31 | 终态 release 键永久保留作审计痕迹（D9/N-1），不随模型删除级联 | 登记为有意策略；etcdctl 可手动清理 |

## 七、验证摘要（v0.7.0 开发轮）

### 7.1 Chart 验证（已完成，helm v4.2.3）

| 场景 | 结果 |
|------|------|
| Chart version/appVersion = 0.7.0 | ✅ Chart.yaml 核对 |
| 默认 embed（replicaCount=1，无 external 设置） | ✅ `helm template` 成功：embed env 全套注入 + PVC 创建；无新增模型 env（可选注释行） |
| embed + replicaCount=2 | ✅ 渲染**失败**：`{{ fail }}`（多副本 embed 脑裂守卫，v0.6.0 语义不变） |
| external + replicaCount=1 / +2 | ✅ 渲染成功：ENDPOINTS 注入、无 PVC；replicaCount=2 时注入 `EDGEFLOW_CLOUDCORE_MULTI_REPLICA=1` |
| external.enabled=true + endpoints 空 | ✅ 渲染**失败**：endpoints 不能为空（守卫保留） |
| `helm lint build/charts/edgeflow` | ✅ 1 chart linted, 0 failed |

### 7.2 代码/集成验证（回填）

> ⏳ **占位：待实施完成后回填**（实施 Agent 与本文档并行工作；回填项：单测/race 全绿、E2E E1-E9、三处一致抽查 P10（§4.1 表 17 行 ↔ API-SPEC §7 ↔ 路由注册数）、交叉编译、制品校验）。

| 项目 | 结果 |
|------|------|
| 全仓单测 `go test ./...`（含既有回归锚点）+ `-race` | ⏳ 待回填 |
| 进程级 E2E E1-E9（注册→版本→发布（白名单/比例）→逐节点结果→取消→回滚→重启恢复→双副本接管→纯内存语义→14 端点回归） | ⏳ 待回填 |
| P10 三处一致抽查（§4.1 表 17 行 ↔ API-SPEC §7 契约 ↔ 路由注册数） | ⏳ 待回填 |
| 交叉编译 / 制品 / helm | ⏳ 待回填 |

## 八、后续里程碑（Roadmap）

- **v0.8+ 候选**：etcd 鉴权参数透传（L1，`EDGEFLOW_CLOUDCORE_ETCD_USERNAME/PASSWORD`、CN→角色映射）；/metrics 增「续约失败计数」gauge（L12 告警建议）；模型列表分页与终态 release GC（L28/N-4）；发布前镜像存在性探活（R-1，P2）；批内并发（D6 登记 P2）；训练平台/模型评测/A-B 按请求切流（范围外延续）。
- **既有 backlog** 不变（见 docs/ROADMAP.md）。