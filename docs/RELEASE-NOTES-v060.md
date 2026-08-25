# EdgeFlow v0.6.0 Release Notes

> 状态：⏳ 开发轮进行中（2026-08-25；代码/集成验证为占位，构建/集成轮实测后回填；**Chart 验证已完成**，见 §七.7.1）。
> 版本决策：新功能（真多活，兑现 v0.5.0 R14 预留方向）→ minor（v0.6.0）。
> 核心主题：**真多活（etcd Lease 心跳）**——心跳落盘为租约、判活 = etcd 视角 hb 键存在性、删除带 GuardedDelete 守卫、读一致 = Load 锚定 + watch 增量 + 重扫兜底、SetDesired/Register 条件写（CAS）；外部模式多副本 active-active 放开（v0.5.0 单写者铁律解除）；L5/L7 顺带闭环。
> 配套：docs/ARCHITECTURE.md（决策 R15）、docs/DEPLOYMENT.md §10.8（多副本部署指南）、docs/KNOWN-ISSUES.md §6（L12-L20）。

---

## 一、主题概述

1. **真多活（本版本核心价值，兑现 v0.5.0 R14 预留方向）**：外部 etcd 模式下 cloudcore 支持 `replicaCount>1` 的 active-active 多副本。v0.5.0 多副本不安全的根因是「心跳/判活是各副本内存瞬态」——v0.6.0 把**心跳落盘为 etcd 租约**（每次心跳 Grant 新租约 + Put 心跳键，键独立前缀 `/edgeflow/registry/heartbeats/<nodeID>`，只绑租约、到期自动删键 = 软离线），判活改由 **etcd 视角**（hb 键存在性，所有副本同一事实源），删除改走 **GuardedDelete 守卫**（hb 键有活租约即拒绝删除）——多副本对判活与删除均安全。embed/纯内存路径逐位不变（回归锚点）。
2. **语义变化明示（相对 v0.5.0）**：① 判活依赖 etcd 可用性——v0.5.0「etcd 故障期间判活/心跳完全不受影响」不再成立，替代承诺 = **有界 + 自愈**：故障 >TTL（默认 300s）→ 节点**全量软离线**，恢复后 ≤1 心跳周期**数分钟自愈**、**零数据删除**（软离线 + 24h 保留期 + 删除守卫）；② 健康检查绑定——多副本形态（外部模式 + MULTI_REPLICA）下 /healthz 反映 etcd 连接（失联 >TTL → 503 → K8s liveness 重启自愈），单副本/embed 保持 v0.5.0 进程存活语义；③ 写放大——每节点每心跳 2 次 RPC（Grant+Put），千节点 ≈ 67 写/s/副本，etcd 承载余量充足（L18）。
3. **L5/L7 顺带闭环**：SetDesired 改 etcd modRevision CAS（跨副本并发写不丢更新，KNOWN-ISSUES §5⑥ 已修）；GC 删除失败重入队（两阶段 GC：内存 pending → etcd 确认删除，KNOWN-ISSUES §5⑦ 已修）。

## 二、核心特性明细

| 特性 | 说明 |
|------|------|
| 心跳落盘（grant-per-heartbeat） | 每次心跳 = `Grant(ttl)` + `Put(hbKey, {"lastSeen": <UnixMs>}, WithLease(leaseID))` 两条 RPC；**不 KeepAlive 流、不做租约 ID 持久化**（最新 Put 决定键绑定，旧租约空到期自散——覆盖写天然收敛，无「续约旧租约」bug 族、零恢复逻辑）。续约走异步队列（cap=4096，4 workers）不阻塞 CloudHub 读循环：内存 UpdateHeartbeat 即时返回，落盘失败按退避重试（重试窗口 ≤TTL），失败不影响内存态（节点仍 Ready 直至租约到期） |
| 判活三态（etcd 视角） | 无台账+无 hb 键 = **不存在**（API 404，与离线显式区分）；台账在+hb 在 = **Ready**（lastHeartbeatAt = hb 键值，跨副本精确）；台账在+hb 不在 = **Offline**（租约到期，offlineSince 起算 24h 保留期）；加载时 hb 未知 = **Unknown**（瞬时，≤1 扫描周期收敛）。事件源三路互兜：watch 增量（亚秒级）/ 周期 etcd 重扫（`NODE_SCAN_INTERVAL` 默认 30s）/ CloudHub 断开事件（立即；不主动 Revoke 租约，防撤销竞态） |
| 删除守卫 GuardedDelete | GC 删除 = txn `Compare(lease(hbKey)==0) → Delete(台账键)`；hb 键有活租约 → 拒绝删除 + 撤销删除恢复节点 Ready（**防误删活节点的根本保证**）。GC 改两阶段（内存 pending 集合 → etcd 确认删除；失败重入队幂等，重试粒度 30s） |
| watch 缓存同步 | 启动 `ListByPrefixRev`（revision 锚定）→ `Watch(prefix, rev+1)` 增量 → 断线/ErrCompacted 全量重放；应用器**只写内存、永不发 etcd 写**（防回写环，单测断言零写调用）；按 rev/ts **单调应用**；hb 删除事件先查「本副本是否在服务该节点」——是则忽略 + 修复性重写（防幽灵节点）；降级 = 周期重扫判活 + 告警（watch 是加速器不是正确性依赖） |
| SetDesired CAS | 读基准从内存改为 **etcd**（GetWithRev）→ 合并 → `PutIfUnchanged(modRev)`（create-if-absent 用 modRev=0）；冲突读刷新重试 ≤3 次，重试耗尽返回 error——**HTTP 仍 200 + 日志 `concurrent-write` 标记**（指令已 Ack 到边缘，v0.5.0 API 语义不翻转）。embed 与外部共用同一实现（单副本无并发 = CAS 恒成功，行为等价）。**L5 消除** |
| Register CAS upsert | 读 etcd 基准合并 → PutIfUnchanged 重试 ≤3；保留首见 RegisteredAt（重连/迁移不重置）；仅外部模式（embed 保持裸 Put 逐位不变） |
| NodeController 停用（外部模式） | 外部模式不创建 NodeController（`if !externalMode { nc.Start() }`），启动日志明示「判活由 etcd 租约机制承担」；`NODE_TIMEOUT` → lease TTL 默认源、`NODE_SCAN_INTERVAL` → 重扫周期（语义迁移，不丢弃，L20） |
| healthz 多副本绑定 | `External() ∧ EDGEFLOW_CLOUDCORE_MULTI_REPLICA`（"1"/"true" 生效）→ /healthz 反映 etcd 连接（周期探活/续约成功率，失联 >TTL → 503 → K8s liveness 重启自愈）；其余形态（embed、单副本外部）保持 v0.5.0 语义（healthz 恒 200 进程存活） |
| Chart | 渲染守卫收窄：**仅 embed 模式** `replicaCount>1` 渲染 `{{ fail }}`（脑裂）；外部模式放行多副本；`external.enabled=true ∧ replicaCount>1` 自动注入 `EDGEFLOW_CLOUDCORE_MULTI_REPLICA=1`；新增可选 `cloudcore.etcd.external.nodeLeaseTTL`（非空注入 `EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL`） |

## 三、配置表（环境变量，`EDGEFLOW_CLOUDCORE_*` 前缀）

### 3.1 新增（v0.6.0，仅外部模式消费）

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL` | **300s（5m）**（= 10× 心跳周期 30s） | 心跳租约 TTL = 外部模式判活阈值；**独立默认 300s，与 `NODE_TIMEOUT` 解耦**（主线裁决 D2：3m 是检测延迟不是故障免疫，绑定会使 v0.5.0「判活不受存储故障影响」承诺塌缩为 TTL）；显式设置（Go duration 或秒数）覆盖；**<90s Warn**（低于心跳周期 3 倍，抖动误判风险）；≤0/非法 **fail-fast**（对齐 `nodeRetentionFromEnv` 风格）；embed/纯内存模式显式设置 → 启动期 **Warn 忽略**（并入 warnEmbedFieldsIgnored 同族，不报错）。调参权衡见 DEPLOYMENT.md §10.8.4 |
| `EDGEFLOW_CLOUDCORE_MULTI_REPLICA` | 空（off） | 多副本标识（"1"/"true" 生效）；`External() ∧ MULTI_REPLICA` → /healthz 绑定 etcd 连接（§二 healthz 行）；Chart 在 `external.enabled=true ∧ replicaCount>1` 时自动注入；纯 env 派生，values 无独立开关字段 |

### 3.2 语义迁移（既有 env，外部模式）

| 环境变量 | v0.5.0 语义 | v0.6.0 外部模式语义 | embed/纯内存 |
|----------|------------|--------------------|-------------|
| `EDGEFLOW_CLOUDCORE_NODE_TIMEOUT` | NodeController 心跳超时阈值（默认 180s） | **不再作为外部模式判活阈值**（NodeController 停用）；判活阈值由 `NODE_LEASE_TTL` 独立决定（默认 300s，D2 与 NODE_TIMEOUT 解耦） | 不变（NodeController 阈值） |
| `EDGEFLOW_CLOUDCORE_NODE_SCAN_INTERVAL` | NodeController 扫描周期（默认 30s） | **etcd 重扫 + watch 重放冷却 + GC 周期** | 不变（NodeController 扫描周期） |
| `EDGEFLOW_CLOUDCORE_NODE_RETENTION` | 24h | **不变**（24h，hb 键在 TTL 已删，台账保留期独立） | 不变 |

### 3.3 键空间增量（唯一新增，既有键/值 JSON 逐字不变）

```
/edgeflow/registry/heartbeats/<nodeID>   # 新增：判活唯一事实源；值 {"lastSeen":<UnixMs>}（单调 ts）
                                         # 只绑租约：到期 etcd 自动删键 = 软离线事件；台账键/Desired 绝不绑租约
/edgeflow/registry/nodes/<nodeID>        # 既有：节点台账（值格式不变）
/edgeflow/devicestatus/...               # 既有：设备影子（值格式不变）
/edgeflow/_meta/schemaVersion            # 既有：schema 钩子（不 bump——业务键形态未变）
```

## 四、变更清单

| 面 | 变更 | 说明 |
|----|------|------|
| 代码（etcdstore） | 新文件 `lease.go` / `watch.go` / `atomic.go`；`KVStore` 冻结不动，新增扩展面 `ExtendedKV`（`LeaseKV`：GrantHeartbeatLease；`AtomicKV`：GetWithRev/PutIfUnchanged/GuardedDelete；`WatchKV`：WatchPrefix/ListByPrefixRev） | 实现：实施轮（本文档编写时代码在途） |
| 代码（registry） | 新文件 `lease_registry.go`（LeaseEtcdRegistry：三态判活、续约 worker、watch 应用器、两阶段 GC + GuardedDelete、重扫对账）；`etcd_registry.go` embed 侧 L7 一行修复（sweepGC 失败调用 `requeueGCEvent`，该函数已存在、幂等） | 同上 |
| 代码（devicestatus） | `etcd_store.go` SetDesired 替换为 CAS 循环（新增错误 sentinel `ErrDesiredConflict`）；新增 StartWatch/LoadAnchored | 同上 |
| 代码（装配） | `cmd/cloudcore`：nodecontroller 时长解析提前；外部分支 LoadAnchored→StartWatch→续约 worker→CleanupLoop（两阶段）；不创建 NodeController（启动日志三态明示）；warnEmbedFieldsIgnored 追加 LEASE_TTL | 同上 |
| Chart | version/appVersion → 0.6.0；守卫收窄（仅 embed 禁多副本）；外部模式 replicaCount>1 注入 `MULTI_REPLICA=1`；values 新增 `cloudcore.etcd.external.nodeLeaseTTL` | ✅ 本文档轮已完成并 helm 验证（§七.7.1） |
| 测试 | 单测 U1-U13（LEASE_TTL 解析/判活状态机/GuardedDelete 三态/CAS 冲突/watch 无环/embed 回归）；进程级 E2E E1-E12（双副本判活一致/并发不丢/孤儿清除/断连恢复/watch 重放/升级回滚/混合版本警示）；既有测试零改动通过 = embed 回归锚点 | 实测数据待集成轮回填 |
| 文档 | ARCHITECTURE R15；DEPLOYMENT §10.8（多副本部署指南）；KNOWN-ISSUES §6（L12-L20）+ §5⑥⑦ 闭环标注；本文件 | ✅ 已完成 |

## 五、升级注意事项（v0.5.0 → v0.6.0）

1. **默认行为不变**：不设置任何新 env 的外部模式单副本（replicaCount=1）→ 心跳/判活/保留/GC/Desired 的可观察结果与 v0.5.0 一致（差异仅：键空间多 heartbeat 前缀 + etcd 故障时的判活语义，均明示登记）。embed/纯内存部署零动作。
2. **升级 = 零迁移动作**（台账/Desired JSON、键空间全兼容）：走「全停再全起」（scale 0 → 1）——**禁止 v0.5.0 与 v0.6.0 副本混跑**（旧版无 hb 视角会 GC 误删活节点，L15）。步骤见 DEPLOYMENT.md §10.8.5。
3. **回滚 = 零脏键**：心跳键独立前缀 `/edgeflow/registry/heartbeats/`，v0.5.0 的 Load 前缀扫描扫不到（不读不写），残留租约 ≤TTL 自动到期；可选 `etcdctl del /edgeflow/registry/heartbeats --prefix` 显式清理。步骤见 §10.8.6。
4. **扩容多副本是可选项**：先以单副本升级验证（节点注册回、hb 键出现），再 `kubectl scale --replicas=2`（前置要求：同版本、3 节点 quorum、共享 endpoints，见 §10.8.1）。
5. **判活语义变化知悉**：etcd 故障 >TTL 会出现全量软离线（数分钟自愈、零数据删除）；需要更长免疫窗口调大 `NODE_LEASE_TTL`（权衡表见 §10.8.4）；监控告警阈值按「≈2×TTL」折算。
6. **healthz 语义变化知悉**：多副本形态（MULTI_REPLICA）下 /healthz 绑定 etcd（失联 >TTL → 503 → liveness 重启自愈）；单副本形态不变。

## 六、已知限制（v0.6.0 登记，详见 docs/KNOWN-ISSUES.md §6）

| # | 限制（一句话） | 缓解 |
|---|---------------|------|
| L12 | **判活依赖 etcd 可用性**：quorum 丢失/全断 >TTL → 节点短暂判离线（有界 ≤TTL+重试窗口），恢复 ≤1 心跳周期自愈；与 v0.5.0「判活不受存储故障影响」语义差异 | 续约重试缓冲（<TTL 无感）+ 默认 TTL 300s + 文档明示；建议 /metrics「续约失败计数」告警（排 v0.7） |
| L13 | **watch 延迟窗口**：跨副本读一致有界延迟（常态 ms 级；断线重放 ≤ 重扫周期 30s） | 判活/GC 正确性不依赖 watch（Guard + 重扫兜底） |
| L13b | 离线检出时延上界 ≈ 2×TTL（租约到期 + 重试窗口） | TTL 可配；监控告警阈值按此折算 |
| L14 | lastHeartbeatAt 精度：源自 hb 键值 lastSeen（副本时钟写入）；判活不看时间戳只按键存在性，时钟漂移不影响判活 | 如需高精度后续加时钟同步约束 |
| L15 | **混合版本多副本不支持**：v0.5.0 与 v0.6.0 副本同连一集群 = 旧版 GC 误删活节点 | 升级/回滚必须全停再全起（§10.8.5/§10.8.6）；Chart 注释警示 |
| L16 | 重启重置 offlineSince（保留期时钟从重启时刻起算）：滚动重启会延长孤儿台账保留 | GC 安全性不受影响（守卫在）；精确化（台账加 offlineAt 字段）改 JSON，本轮不做 |
| L17 | hb 键值解析失败仍判活（键在即活，只丢 lastSeen 精度） | 防御规则：绝不 fail-closed 判死；坏值 Warn + 保留键 |
| L18 | **心跳写放大**：每节点每心跳 2 次 RPC（Grant+Put）；千节点 ≈ 67 写/s + 60 RPC/s 读 | etcd 3 节点承载余量充足（万级写/s）；quota/compaction 沿用（256MiB/1h） |
| L19 | GC 级联 at-most-once：副本在「删台账成功 → 级联删设备子树」之间崩溃且他副本未同步 → Desired 孤儿残留（低危，按节点过滤不可见） | 节点重注册覆盖；根除需 txn 级联（后续候选） |
| L20 | `NODE_SCAN_INTERVAL/NODE_TIMEOUT` 在外部模式语义迁移（NodeController 停用） | 启动日志三态明示；文档配置表标注两种语义 |

## 七、验证摘要（v0.6.0 开发轮）

### 7.1 Chart 验证（已完成，helm v4.2.3）

| 场景 | 结果 |
|------|------|
| 默认 embed（replicaCount=1，无 external 设置） | ✅ `helm template` 成功：embed env 全套注入 + PVC 创建（与 v0.5.0 渲染一致）；无 MULTI_REPLICA/NODE_LEASE_TTL env |
| external 开（2 端点，replicaCount=1） | ✅ 成功：ENDPOINTS 注入、无 PVC（/data 回退 emptyDir）；无 MULTI_REPLICA（单副本不注入） |
| embed + replicaCount=2 | ✅ 渲染**失败**：`{{ fail }}`——多副本 embed 脑裂文案（exit≠0，v0.6.0 保留 embed 分支守卫） |
| external + replicaCount=2 | ✅ 渲染**成功**（v0.6.0 放行）：`replicas: 2` + 注入 `EDGEFLOW_CLOUDCORE_MULTI_REPLICA=1`；无 PVC |
| external + replicaCount=2 + nodeLeaseTTL=300s | ✅ 成功：另注入 `EDGEFLOW_CLOUDCORE_NODE_LEASE_TTL="300s"` |
| external + replicaCount=1 + nodeLeaseTTL=5m | ✅ 成功：注入 NODE_LEASE_TTL、**不注入** MULTI_REPLICA |
| external.enabled=true + endpoints 空 | ✅ 渲染**失败**：endpoints 不能为空（守卫保留） |
| `helm lint build/charts/edgeflow` | ✅ 1 chart linted, 0 failed |

### 7.2 代码/集成验证（回填）

| 项目 | 结果 |
|------|------|
| 全仓单测 `go test ./...`（含既有 embed/纯内存回归锚点） | ✅ 33+ 包全绿；新增 16 用例：registry LeaseEtcdRegistry 核心契约 7（注册写台账 / hb 事件三态 / 本地覆盖规则+修复性重写 / 应用器只读铁律 / GuardedDelete 活守卫拦截与放行 / EtcdHealthyWithin / LoadAnchored 双空间恢复+对账收敛）、devicestatus CAS 5（成功 / 冲突重试 merge 不丢 / 耗尽 ErrDesiredConflict / create-if-absent / 写失败保持内存）、cloudcore 4（healthz 200/503 / MULTI_REPLICA 解析表驱 / LEASE_TTL 边界） |
| 进程级 E2E（/tmp/edgeflow-e2e-v060，Phase A/B/C） | ✅ 全部通过：双副本同跑 healthz 200；心跳互见 lastHeartbeatAt 跨副本 0ms 差；kill 副本 ≤TTL（15s）判 Offline 且台账 2×TTL 后仍在；断开事件 0s / 租约到期 15s 双路径；修复性重写防误删（手动删 hb 键 → 重建+台账不误删）；孤儿台账+设备子树 GC+级联闭环（30s+30s 与 88s 两轮）；活节点守卫跨 GC 周期不误删（远古 registeredAt）；etcd 短断 8s 不判离线且 healthz 200；长断 >TTL healthz 503、恢复后 ≤1 心跳周期 Ready 自愈；单副本 healthz 恒 200（v0.5.0 语义）；embed 冒烟回归 |
| SetDesired 并发两值并存 | ✅ 单测级验证（冲突注入 → 重读基准 → merge 两值并存；耗尽 ErrDesiredConflict）；进程级双写未单独模拟（需 Ack 仿真边端，排未来 E2E 工具） |
| 交叉编译 | ✅ darwin/linux × amd64/arm64 × 3 cmd = 12 二进制（CGO_ENABLED=0）；windows 断链为既有现状（v0.5.0 起 Flock），登记排未来 |
| helm | ✅ lint 0 failed；(embed,2) 脑裂守卫 fail；(external,2) replicas=2+MULTI_REPLICA；(external,1) 不注入；nodeLeaseTTL→NODE_LEASE_TTL |
| 制品/合规（Trivy / SBOM / Chart 打包） | ⏳ 发布轮回填 |

## 八、后续里程碑（Roadmap）

- **v0.7+ 候选**：鉴权参数透传（L1，`EDGEFLOW_CLOUDCORE_ETCD_USERNAME/PASSWORD`、CN→角色映射）；/metrics 增「续约失败计数」gauge（L12 告警建议）；GC 级联 txn 化（L19 根除）；台账 offlineAt 字段（L16 精确化，需评估 JSON 兼容）；端点列表动态变化（AutoSyncInterval）。
- **既有 backlog** 不变（见 docs/ROADMAP.md、docs/PROGRESS.md §5）。
