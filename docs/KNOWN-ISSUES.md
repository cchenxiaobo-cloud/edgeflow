# EdgeFlow 已知限制与问题台账（KNOWN-ISSUES）

> 最后更新：2026-08-25（v0.7.0 开发轮）
> 收录原则：只登记**已实现功能上的已知边界/脆弱点**；未实现功能见 `docs/ROADMAP.md §8-§11` 与 `docs/PROGRESS.md §5`。每轮开发/发布时复查本表，已闭环项移出。

---

## 1. v0.2.0 开发轮登记（2026-08-18）

> §1 四条已于 v0.3.0 开发轮全部闭环（commit `714d5ba`，2026-08-19），详见各行「计划」列标注；原文保留备查。

| # | 位置 | 限制 | 影响 | 计划 |
|---|------|------|------|------|
| ① | `cmd/edgecore/device_mapper.go` `collectMapperReports` | 周期采集汇入影子时按 `default` 命名空间**硬编码**（Collect 接口只返回属性值、不携带 ns） | Modbus 多 ns 部署（如 `EDGEFLOW_MODBUS_NAMESPACE=plant-a`）时，云端设备列表出现 `default/mb-sensor-01` 与 `plant-a/mb-sensor-01` 双条目；指令路径（`Route` 按 ns 路由）正确，不受影响 | ✅ v0.3.0 已修（2026-08-19）：采集汇入影子改按 mapper 自身命名空间写入（与注册路由同源判定），仅显式设置 `EDGEFLOW_MODBUS_NAMESPACE` 时才改变 ns，默认行为与 v0.2.0 逐字节一致 |
| ② | `edge/pkg/edgehub/client_test.go` 退避重置测试 | 仍用**实时时间阈值**断言（≥500ms / <500ms，依赖 50ms vs 800ms 差异） | 慢 CI 机器上第二次断言有 flake 风险（本地 `-count=5` 稳定）；复核项 m3 本批跳过 | ✅ v0.3.0 已修（2026-08-19）：新增 `Options.BackoffSleepFunc` 注入点（nil=默认退避，非 nil 接管休眠），测试改为注入计数断言，移除实时时间阈值 |
| ③ | `cmd/cloudcore/main.go` `syncPod` 400 分支 | `err.Error()` **裸拼 JSON** 响应（`` `{"error":"invalid resources: `+err.Error()+`"}` ``） | 当前校验错误文案不含 JSON 敏感字符，"碰巧不炸"；结构脆弱，任何含引号/反斜杠的路径都会产出非法 JSON | ✅ v0.3.0 已修（2026-08-19）：400 分支改 `json.Marshal` 结构化输出（与 409 分支同构），畸形输入也产出合法 JSON，响应结构不变（`{"error":...}`） |
| ④ | `EDGEFLOW_EDGECORE_RECONCILE_INTERVAL` / `EDGEFLOW_EDGECORE_REPORT_INTERVAL` / `EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL` | 合法区间 1s~10m；传 300ms 等越界值**静默回落默认值**（仅 Warn 日志，`durationFromEnv`） | 运维误配周期时不易察觉实际生效值与配置不符 | ✅ v0.3.0 已修（2026-08-19）：`edgeCoreIntervalFromEnv` 启动日志三态明示——合法（来源：环境变量，请求值+生效值）/ 越界回落（Warn 说明）/ 未设置（来源：默认/配置文件），启动即可核对实际生效值 |

## 2. 复查记录

- 2026-08-18（v0.2.0 开发轮）：新增 ①②③④ 四条；历史已知问题（v0.1.x）仍见 `docs/API-SPEC.md §8` 与 `docs/HANDOFF.md §10`，未迁移本表。
- 2026-08-19（v0.3.0 开发轮）：§1 四条全部闭环并逐行标注（commit `714d5ba`）；新增 §3 登记四条（pkg/opcua 首阶段未实现边界 + 周期 env 日志热重载重复输出）。历史已知问题（v0.1.x）位置不变。
- 2026-08-24（v0.4.0 开发轮）：新增 §4 登记（嵌入式 etcd 坏库/坏 WAL 场景与降级行为 + POD/reported 短暂清空语义）；API-SPEC §9 首条"重启清空"已随分级持久化闭环，不再迁移本表。
- 2026-08-24（v0.5.0 开发轮）：新增 §5 登记三条（L1 无鉴权参数透传 / L5 多副本 SetDesired 整记录覆盖 / L7 GC 删除失败不重试——后两条为 v0.4.0 既有缺陷在多副本/外部集群场景下的新发现，v0.5.0 按单写者形态登记不修）；编号沿用全局序列（⑤⑥⑦），对应设计定稿限制清单 L1/L5/L7。
- 2026-08-25（v0.6.0 开发轮）：§5 ⑥⑦ 已随真多活闭环（SetDesired CAS / GC 重入队，见各行「计划」列标注），⑤ 排 v0.7 保留；新增 §6 登记十项（L12-L20，对应设计定稿 §10 + 主线裁决 D1-D4 修订口径）。
- 2026-08-25（v0.7.0 开发轮）：新增 §7 登记（L21-L29 对应设计定稿 §13.1 + 主线裁决 D9/P9 补项：孤儿 guard 自愈语义 L30、终态 release 键永久保留 L31）；§6 补充行（跨平台编译 CRL 文件锁）原编号 L21 与 v0.7.0 轮编号冲突，**更名 L20b**（沿用 §6 既有的 b 后缀子编号惯例，如 L13b）；§5⑤（L1）批注更新为**排 v0.8**（本设计未纳入 WBS）；§6 L12 的「续约失败计数」gauge 告警建议更新为排后续版本（v0.7.0 未纳入）。

## 3. v0.3.0 开发轮登记（2026-08-19）

| # | 位置 | 限制 | 影响 | 计划 |
|---|------|------|------|------|
| ① | `pkg/opcua`（整体） | OPC-UA 首阶段仅交付 UA Binary 协议栈核心：**未实现** Read/Write/Subscribe 等任何服务请求、SecureChannel 打开（Conn 为裸传输，ChannelId 恒 0）、UA 节点模型/对象树、Discovery 端点、安全策略 Sign/SignAndEncrypt（仅 SecurityPolicy None 明文） | 当前 pkg/opcua 只能做底层编解码与 TCP 握手回环，无法直接驱动真实 OPC-UA 设备读写；明文传输无认证/完整性，**仅限可信隔离网络**（封闭 OT 网段/本机模拟） | 后续里程碑：SecureChannel 打开 → Read/Write 服务 → Mapper 层接入 → 安全策略 |
| ② | `pkg/opcua` 互操作 | 未与第三方 UA 栈（open62541/node-opcua）做互操作验证，本轮仅自研 mock 服务端回环（transport_test 真实 TCP 握手为自建对端） | wire 级编解码符合 Part 6 但未经验证真栈互认，存在字段约定偏差风险 | 下一里程碑安排 open62541/node-opcua 互操作 cross-check |
| ③ | `pkg/opcua/types.go` | `DiagnosticInfo` 仅空骨架（无字段/无位域语义）；Variant 解码维度位仅解析不发射（Encode 不写 Dimensions 位，仅 Decode 支持）；DateTime 负数（1601-01-01 之前）未测试 | 诊断信息无法承载；编码维度信息丢失（当前无消费方）；1601 年前时间戳行为未验证 | 随后续里程碑按需补齐 |
| ④ | `pkg/config/edgecore.go` 周期 env 日志 | 周期 env 三态启动日志（KNOWN-ISSUES §1 ④ 修复引入）在**热重载**（SIGHUP/mtime）时每次重载重复输出三条 Info | 日志量轻微增长（低频，量级小，仅热重载时） | 可接受，暂不处理；如后续日志规范收紧再评估 |

---

## 4. v0.4.0 开发轮登记（2026-08-24，嵌入式 etcd 持久化）

| # | 位置 | 限制 | 影响 | 计划 |
|---|------|------|------|------|
| ① | `data/etcd/member/wal/` 坏 WAL（内容损坏/截断） | **实测 etcd v3.5.33 在 raft 恢复阶段直接 panic**（`panic: cannot use none as id`，RestartNode），**不是返回 error**——设计初稿预期"启动失败 → error → 降级"，实际需装配层 `defer recover()` 兜底 | 无兜底则进程裸崩（系统级重启循环）；现有装配已 recover：默认降级纯内存 + 启动期大告警（数据未持久化），`EDGEFLOW_CLOUDCORE_ETCD_STRICT=1` 时 fail-fast 退出。恢复路径：etcdutl restore 或清空数据目录重收敛（边缘重连重新注册） | 已按实测修订设计 §6.5 与文档口径（DEPLOYMENT §10.5）；保持 recover 兜底 + 回归测试 `TestBadWALTolerance` |
| ② | `data/etcd/member/` 目录整体丢失 | embed 重建空库正常启动（旧数据不在、新写可用）——**单目录丢失 ≠ 崩溃，但等于全量丢数据** | 注册台账与设备 Desired 从零重建：节点靠边缘重连重新注册，Desired 靠指令重发；Pod/上报 ≤1 上报周期自愈 | 备份策略兜底：在线 `etcdutl snapshot save` / 离线 `cp -a data/etcd`（停进程后整体拷贝，**文件拷贝≠有效备份**），见 DEPLOYMENT §10.4 |
| ③ | 云端 Pod 状态 / 设备 reported（properties/LastReportedAt） | v0.4.0 起**整表不落盘**（写穿范围仅注册元数据 + Desired）：cloudcore 重启后 pods 列表短时为空、设备 properties 为空 map `{}` | 重启窗口内查询 API 显示"暂时缺失"；≤1 上报周期（默认 30s）边缘重连后自愈，**非永久丢失**；监控/告警阈值需容忍该窗口 | **v0.9.0/v0.10.0 闭环**：`/edgeflow/podstatus/` 键空间启用（EtcdPodStore 写穿，重启后 Pod 列表直接可见）；**v0.10.0 设备 reported 亦写穿**（EtcdDeviceStore.Upsert 完整快照 + applyPut 整值采用，重启后设备属性立即可见）——③ 全部闭环 |
| ④ | `cloud/pkg/etcdstore` embed.Close() 幂等性 | v3.5.33 的 `close(e.errc)` 无 closeOnce 保护，二次 Close 会 `panic: close of closed channel` | 已在本层用 sync.Once 整体幂等化解（`Close()` 可安全重复调用）；集成层勿绕过 `EmbeddedEtcd` 直接持有 embed.Etcd | 已闭环（包内化解）；登记备忘 |

---

## 5. v0.5.0 开发轮登记（2026-08-24，外部 etcd 模式，对应设计定稿 L1/L5/L7）

> 背景：v0.5.0 新增外部 etcd 模式（`EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` 非空即直连共享集群，ARCHITECTURE 决策 R14）。以下 ⑤⑥ 是外部/多副本场景暴露的**既有设计边界**——v0.5.0 受支持形态为**单写者**（replicaCount=1，Chart 模板 fail 守卫兜底），因此 ⑤⑥ 在受支持形态下不触发，但仍登记、不修复（改动涉及键空间/GC/写穿联动，超配置级切换范围）；⑦ 为 v0.4.0 既有缺口在外部集群故障窗口的放大项。**v0.6.0 闭环说明**：⑥（SetDesired CAS）与 ⑦（GC 重入队）已随真多活（ARCHITECTURE R15）修复，见各行标注；⑤ 未修，**排 v0.8 保留**。

| # | 位置 | 限制 | 影响 | 计划 |
|---|------|------|------|------|
| ⑤ | 外部模式连接层（clientv3 配置）；L1 无鉴权参数透传 | v0.5.0 **不实现任何 etcd 鉴权参数**（无 username/password/证书 CN→角色映射 env）；外部集群开启鉴权且未为 cloudcore 授权 → 启动探活返回 `PermissionDenied` → fail-fast 拒绝启动，错误文案引导"检查 etcd 鉴权 / 在 etcd 侧为 `/edgeflow/` 授权"（见 DEPLOYMENT §10.7.6） | 鉴权只能在 etcd 侧配置（§10.7.4 最小权限角色 readwrite `/edgeflow/` / mTLS `--client-cert-auth`）；安全边界 = TLS/mTLS + 网络隔离（非回环+明文默认拒绝启动，逃生门 `EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE`）；明文+非回环未配逃生门时连启动都不可行，属护栏而非缺口 | **v0.6.0/v0.7.0 均未修，排 v0.8**：`EDGEFLOW_CLOUDCORE_ETCD_USERNAME/PASSWORD`、CN→角色映射（v0.8 候选） |
| ⑥ | `cloud/pkg/devicestatus/etcd_store.go` `SetDesired`（L5 跨副本整记录覆盖） | `SetDesired` 是"读**本副本**内存合并 → 整记录 Put"（读-改-写）；多副本 active-active 时，副本 B 的内存基准可能旧于 etcd（副本 A 刚写的属性 P1 不在 B 内存），B 对同一设备写 P2 → 整记录覆盖 → **P1 丢失**。v0.4.0 单副本语义正确（自身写穿保证内存最新） | v0.5.0 单写者形态（replicaCount=1）下不触发；多副本部署已被 Chart fail 守卫与文档禁止（心跳/判活内存态是更根本的原因，见 ARCHITECTURE R14）。文档级缓解：同一设备的连续指令尽量落同一副本（粘性会话） | ✅ **v0.6.0 已修**（ARCHITECTURE R15 §⑤）：SetDesired 改 etcd **modRevision CAS**——读基准从内存改为 etcd（GetWithRev），冲突读刷新重试 ≤3 次，重试耗尽返回 `ErrDesiredConflict`（HTTP 仍 200 + 日志 `concurrent-write` 标记，API 语义不翻转）；并发写不同 property 收敛为两值并存、同 property 收敛为最后提交者，**整记录覆盖消失**。embed 与外部共用同一实现（单副本无并发 = CAS 恒成功，行为等价） |
| ⑦ | `cloud/pkg/registry/node_registry.go` `requeueGCEvent`（L7 死代码） | GC 删除失败**只告警、不重入队**：`requeueGCEvent` 存在但**从未被调用**（死代码）；注释声称"下一轮 CleanupLoop 重扫会再次产生事件"**不成立**——节点已从内存 map 移除，重扫不会再见到它 | etcd 残留**孤儿节点键**（及设备子树），直到该节点重新注册（再被 GC）或 cloudcore 重启（Load 读回为 Unknown → 保留期后再 GC 一次）。**外部模式放大可见性**：集群故障窗口内的失败删除会留孤儿；单机 embed 故障窗口短、影响小（v0.4.0 既有缺口） | ✅ **v0.6.0 已修**（ARCHITECTURE R15 §③）：GC 改两阶段——外部模式内存 pending 集合（幂等去重）→ GuardedDelete 确认删除，瞬时失败保持 pending 下轮重试（重试粒度 30s）；**embed 路径启用 `requeueGCEvent`**（既定死代码，一行修复）→ 孤儿键必然在下轮 CleanupLoop 重试被清（v0.5.0 为永久孤儿直到重启） |

---

*表中「位置」为登记时的代码位置，代码演进后以 commit 为准。*

---

## 6. v0.6.0 开发轮登记（2026-08-25，真多活，对应设计定稿 §10 L12-L20 + 主线裁决 D1-D4 修订口径）

> 背景：v0.6.0 外部模式放开多副本（ARCHITECTURE 决策 R15）——心跳落盘为 etcd 租约（`/edgeflow/registry/heartbeats/<nodeID>`）、判活 = etcd 视角 hb 键存在性、GuardedDelete 守卫、watch 缓存同步、SetDesired/Register CAS。下列为本形态的**已知边界**（多数为设计取舍的有界降级，非缺陷）；「位置」为登记时的代码/机制位置，代码演进后以 commit 为准。

| # | 位置 | 限制 | 影响 | 计划 |
|---|------|------|------|------|
| L12 | 判活/续约路径（LeaseEtcdRegistry 心跳续约 + 三态判活） | **判活依赖 etcd 可用性**：quorum 丢失/全断 > lease TTL（默认 300s）→ 租约到期删 hb 键 → 节点**全量软离线**（有界：≤TTL+重试窗口）；etcd 恢复 → ≤1 心跳周期自动重建租约自愈。与 v0.5.0「etcd 故障期间判活完全不受影响（内存瞬态）」**语义差异**，已在 RELEASE-NOTES-v060 §一.2 明示 | etcd 故障 >TTL 时 API 显示节点短时 Offline（**零数据删除**：软离线 + 24h 保留期 + 删除守卫）；监控假警报需按 TTL 折算阈值 | 续约重试缓冲（<TTL 无感）+ 默认 TTL 300s 覆盖常见恢复窗口；文档明示（DEPLOYMENT §10.8.4）；建议 /metrics「续约失败计数」gauge 告警（**v0.8.0 已闭环**：renewal_failures_total；**v0.11.0 补 hb 键重建计数** hb_rebuilds_total，见 §11） |
| L13 | watch 同步（WatchPrefix 应用器 → 内存缓存） | **watch 延迟窗口**：跨副本读一致有界延迟（常态 ms 级；断线重放 ≤ 重扫周期 30s）；读可能短暂落后 | 多副本间 API 查询结果瞬时不一致（他副本刚写的 Desired/心跳 ≤1 重放周期内可见） | 判活/GC 正确性不依赖 watch（GuardedDelete + 周期重扫兜底）；API 粘性路由仅为体验建议（正确性无关，CAS 已保证） |
| L13b | 离线检出时延 | 离线检出时延上界 ≈ **2×TTL**（租约到期 + 续约重试窗口） | 断开事件丢失场景下 Offline 判定最多滞后 ≈2×TTL（默认 ≈10m）；正常断开仍由 CloudHub 90s 事件快路径判定，不受影响 | TTL 可配（调参权衡表见 DEPLOYMENT §10.8.4）；监控告警阈值按此折算 |
| L14 | hb 键值 lastSeen（跨副本展示精度） | lastHeartbeatAt 精度 = hb 键值 lastSeen（副本时钟写入）；**判活不看时间戳只看键存在性** → 时钟漂移绝不影响判活，仅影响展示精度 | 副本时钟偏差大时跨副本 lastHeartbeatAt 展示有偏差（不影响 Ready/Offline 判定） | 如需高精度可后续加时钟同步约束（etcd NTP 要求见 §10.7.4）；登记 |
| L15 | 混合版本多副本（升级/回滚窗口） | **混合版本多副本不支持**：v0.5.0 与 v0.6.0 cloudcore 副本同连一集群 = 旧版本无 hb 键视角/守卫，其 GC 会**误删活节点**（数据丢失） | 升级/回滚窗口内若新旧副本并存（如 K8s 滚动更新习惯）→ 数据丢失风险 | 升级/回滚必须**全停再全起**（scale 0 → 1，DEPLOYMENT §10.8.5/§10.8.6 runbook）；Chart 注释 + values 警示；不做运行时版本互斥（应用层无法可靠识别旧版本，文档级约束） |
| L16 | offlineSince（保留期时钟） | **重启重置 offlineSince**：保留期从重启时刻起算（v0.5.0 同款）；多副本下滚动重启会延长孤儿台账保留 | 孤儿台账多保留一个滚动窗口（≤2×TTL 量级）；GC 安全性不受影响（删除守卫在） | 如需精确：台账 DTO 加 offlineAt 字段（改 JSON → 与 v0.5.0 兼容性受影响），本轮不做，登记候选 |
| L17 | hb 键值解析（防御规则） | **hb 键值解析失败仍判活**（键在即活，只丢 lastSeen 精度）——绝不 fail-closed 判死 | 坏值仅 Warn + 保留键；判活正确性不受坏值影响 | 防御规则已入实现契约（§1.1）；登记 |
| L18 | 心跳写放大（续约路径） | **每节点每心跳 2 次 RPC**（Grant + Put）；千节点 × 30s 心跳 ≈ 67 写/s/副本 + ~60 RPC/s 读（重扫/Get） | etcd 负载较 v0.5.0 增（纯读）明显；3 节点承载余量充足（万级写/s 基线）；quota 256MiB/compaction 1h 下修订增长 ≈12MB/h，远低于配额 | 异步合并队列有背压（队列满丢弃 + Warn，下次心跳自然重入队）；etcd 侧容量基线沿用 DEPLOYMENT §10.7.4 |
| L19 | GC 级联（gcCascadeLoop 顺序） | **GC 级联 at-most-once**：副本在「删台账成功 → 级联删设备子树」之间崩溃且他副本未同步到 → 设备 Desired 孤儿残留（按节点过滤不可见，低危） | 陈年孤儿 Desired 占据键空间（MB 量级可忽略）；节点重注册后旧 Desired 不再可见（查询按节点过滤） | 节点重注册覆盖；根除需 txn 级联删除（etcd 无跨前缀事务，需自实现），后续候选 |
| L20 | 外部模式装配（NodeController 停用 + env 语义迁移） | `NODE_SCAN_INTERVAL` 在外部模式语义迁移为「etcd 重扫/GC 周期」；`NODE_TIMEOUT` **不再作为外部模式判活阈值**（NodeController 停用，判活阈值 = `NODE_LEASE_TTL` 独立默认 300s，D2 解耦）；embed/纯内存两 env 语义不变 | 运维按 v0.5.0 文档理解这两个 env 会在外部模式得到不同行为（NODE_TIMEOUT 调整不再影响判活；NODE_SCAN_INTERVAL 影响重扫粒度） | 启动日志三态明示「外部模式：判活由 etcd 租约机制承担，NodeController 停用；NODE_SCAN_INTERVAL→重扫周期；NODE_TIMEOUT 不再参与判活（NODE_LEASE_TTL 独立默认 300s）」（对齐 v0.3.0 env 日志惯例）；文档配置表标注两种语义（RELEASE-NOTES-v060 §三.3.2） |
| L20b | 跨平台编译（pkg/certs CRL 文件锁） | ✅ **v0.10.0 闭环**：平台分文件（crl_lock_unix.go syscall.Flock / crl_lock_windows.go x/sys/windows LockFileEx），GOOS=windows 交叉编译通过（原：`lockCRLFile/unlockCRLFile` 使用 `syscall.Flock`（UNIX 专属）——**Windows 交叉编译断链为 v0.5.0 引入的既有现状**（v0.4.0/v0.5.0 发布矩阵均不含 windows 制品，v0.6.0/v0.7.0 同）；不影响 darwin/linux 构建与运行（**原编号 L21，2026-08-25 更名 L20b 避免与 v0.7.0 轮编号冲突，见 §2 复查记录**） | 有 windows 构建诉求的团队需自行 patch（或等后续版本平台分文件：flock_unix.go / flock_windows.go） | ✅ 已闭环（v0.10.0，平台分文件；Windows 制品可加发布矩阵，测试辅助 syscall.Dup 的 Windows 兼容性为测试环境边界） |

---

## 7. v0.7.0 开发轮登记（2026-08-25，模型仓库/版本管理/灰度发布，对应设计定稿 §13.1 + 主线裁决 D2/D3/D4/D9/P9）

> 背景：v0.7.0 云端新增模型仓库与灰度发布（ARCHITECTURE 决策 R16，API 契约 API-SPEC §7）——`/edgeflow/models/` 键空间（模型/版本台账 + guard 守卫 + release 头/逐节点结果 + 领跑锁 + 部署影子）、17 个新 REST 端点（14→31）、202/422 语义；边缘零代码改动（复用 PodSync/ConfigSync）。下列为本能力的**已知边界**（多数为设计取舍的有界降级，非缺陷）；「位置」为登记时的代码/机制位置，代码演进后以 commit 为准。编号 L21-L29 沿用设计定稿 §13.1（§6 原 L21 跨平台编译行已更名 L20b 消除冲突，见 §2 复查记录）。

| # | 位置 | 限制 | 影响 | 缓解/计划 |
|---|------|------|------|----------|
| L21 | 发布领跑锁（release lock，grant-per-claim） | **领跑者崩溃接管延迟 ≤ 锁 TTL（默认 60s）**：接管前该 release 批次不推进；接管后从 perNode 已部署节点继续（跳过已完节点），`NextBatchAt` 已持久化、节奏接续（不重复计 pause）。**N-3 同族**：批间崩溃若发生在「批完成写 NextBatchAt 前」，接管后旧 NextBatchAt（过去时间）→ 下一批立即开跑，少计一次 pause（影响轻微） | 崩溃接管窗口内发布推进停滞（≤TTL）；双执行者窗口极小且部署幂等（边缘去重 + 同键覆盖），正确性不受损 | LockTTL 可配（`EDGEFLOW_CLOUDCORE_RELEASE_LOCK_TTL`，>=15s，刷新 = max(5s, TTL/3)）；文档明示（DEPLOYMENT §10.9） |
| L22 | 纯内存模式（ETCD_ENABLED=false）模型存储 | **release 任务与部署影子重启丢失**（embed/外部 etcd 持久恢复）；模型/版本/发布台账在内存中，重启后需重新注册/创建 | 纯内存形态下发布任务不可跨重启追踪 | 三模式表文档明示（DEPLOYMENT §10.9.1）；生产建议 embed/外部模式 |
| L23 | 下发执行链路（DeployVersion 两步） | **半部署状态**：podsync 成功、config-sync 失败 → 节点已切镜像未切参数，perNode 计 failed（reason 可查） | 节点短暂处于"新镜像旧参数"状态；边缘声明式调谐保证最终一致 | 重试发布或回滚收敛；perNode reason 人类可读（node offline / ack timeout / edge rejected ack） |
| L24 | 回滚执行（rollback 逆序批量） | **回滚部分失败仍置 rolled_back**（尽可能回滚，不回滚中止）：失败节点 perNode.Status=failed + reason；head 置 rolled_back + 日志 Warn | 回滚后机群可能新旧版本混杂（明细 perNode 可查） | 人工复核 perNode 明细；必要时再次发起发布收敛 |
| L25 | 模型删除级联（非事务） | **级联删除非事务**：删除版本前缀/部署影子前缀/meta 中途崩溃 → 孤儿版本/部署键残留（按 meta 过滤不可见，占空间） | 启动加载只认 meta 存在的前缀（孤儿不加载）；残留键不可见 | 删除前清 guard；可选 `etcdctl del /edgeflow/models --prefix` 手动清理 |
| L26 | 回滚守卫（RollbackRelease 前置） | **回滚被新版本接管 → 拒绝**：`release.version ≠ 模型当前 active 版本` → 409（文案引导显式 activate 或新发布）；API 校验通过后、控制器执行前被接管/被删 → 执行期复查中止（head=failed + 清 rollbackRequested + 未执行节点 skipped，D2/D4） | "回滚开倒车覆盖新部署"被架构性封堵；执行期复查后极端窗口收敛为明确终态 | 文档明示（API-SPEC §7.2/§7.3）；C3 用例覆盖 |
| L27 | 取消收敛（cancel 后 perNode 补齐） | cancel 置位后，未执行节点标 skipped 有 **≤1 扫描周期（默认 5s）补齐窗口**；pending→canceled 的 guard 释放同样 ≤1 扫描周期 | 查询方在窗口内可能看到 pending 残留 | 查询方容忍；文档明示（API-SPEC §7.2） |
| L28 | release/模型列表（List 端点） | **无分页（全量返回）**；N-4 同族：终态 release 常驻内存缓存无 GC | 数据量 = 任务/模型规模（当前可接受）；长运行内存线性增长 | 后续版本分页/GC（与 L28 同族登记） |
| L29 | 混合版本多副本（升级/回滚窗口） | **v0.6.0 与 v0.7.0 副本同连一集群未验证**：v0.7.0 只新增 `/edgeflow/models/` 前缀（旧版不读不写，理论无害）仍**建议同版本全量切换**；残留键可 `etcdctl del /edgeflow/models --prefix` 清理 | 升级/回滚窗口行为未实证 | 升级/回滚全停再全起（DEPLOYMENT §10.9.4） |
| L30 | 孤儿 guard 自愈（CreateRelease 存储层，D3） | **孤儿 guard 自愈语义登记**：guard CAS 成功、release 键写入前崩溃 → 孤儿 guard；创建重试时自愈——guard 冲突 → 读 guard 指向的 release 键，不存在（或已终态）→ **按值 CAS 删 guard**（CompareAndDelete expectRev，防误删新 guard）→ 重试一次；仍冲突 → 409。废弃"lock 过期"陈旧判据（孤儿场景 lock 键从未创建）；控制器不承担兜底（只扫内存 release） | 无自愈则该模型永久 409（仅剩手动 etcdctl）；自愈后单次重试即恢复 | 文档登记（ARCHITECTURE R16/API-SPEC §7.2）；S4 补"guard 写后崩溃→重试创建自愈"用例 |
| L31 | 终态 release 键保留策略（D9/N-1） | **终态 release 头与 perNode 键永久保留作审计痕迹**（不随模型删除级联清理；键路径带 releaseID 不随模型名走）；不可见、无功能影响 | 长期运行键空间累积（MB 量级可忽略；与 L25/L28 同族） | 登记为有意策略；`etcdctl del /edgeflow/models/releases --prefix` 可手动清理（按审计保留期自行权衡） |

## 8. v0.8.0 开发轮闭环登记（2026-08-26，运维与安全增强：etcd 鉴权/续约监控/模型运营性）

> 本开发轮闭环三个跨轮排期项（L1/L12/L28），并登记 GC 开启后的 L31 口径变更。提交：88b3765（L1）/ dc0df54（L12）/ 7a1941a（L28）。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| L1 | 外部 etcd 无鉴权参数透传 | ✅ **v0.8.0 闭环**：`EDGEFLOW_CLOUDCORE_ETCD_USERNAME/PASSWORD` 成对透传（只设其一 fail-fast；与 TLS/mTLS 正交；embed/纯内存忽略不串扰）；PermissionDenied 探活文案更新（引导 RBAC 配置 + mTLS CN 映射）。CN→角色映射仍由 etcd 侧 `--client-cert-auth` 配置（非透传项，文档指引） | 密码经 env 注入（K8s 建议挂 Secret 转 env）；无 URL 内凭证支持（有意，防日志泄露） |
| L12 | 续约失败无可观测性 | ✅ **v0.8.0 闭环**：`/metrics` 新增 `edgeflow_cloudcore_lease_renewal_failures_total`（counter，仅外部模式注入；0 值也输出便于面板基线）；建议告警：持续增长（如 5min 内 >N）→ etcd 侧异常/网络分区 | 无独立"hb 键重建"计数（自愈可观测性可后续加）；告警阈值需按判活 TTL 折算 |
| L28 | release/模型列表无分页 + 终态无 GC | ✅ **v0.8.0 闭环**：① 分页——GET models/versions/releases 支持 `limit`(1-1000)/`offset`(≥0)，响应头 `X-Total-Count`，缺省全量（零破坏）；② GC——`GCReleases` 按 CreatedAt 保留最近 keep 条终态（默认 **关闭**，`EDGEFLOW_CLOUDCORE_RELEASE_GC_ENABLED=1` + `RELEASE_GC_KEEP` 默认 100 开启），删除旧终态头+逐节点结果，非终态/在途绝不删 | GC 开启后 L31 口径变更：终态键不再永久保留（按 keep 截断），审计痕迹以 ops 台账/文档为准；默认关闭保持原口径 |
| N-4 | （L28 同族）终态 release 常驻内存无 GC | ✅ 随 L28 GC 闭环（三模式统一；纯内存模式也受益——防长运行内存线性增长） | — |


## 9. v0.9.0 开发轮闭环登记（2026-08-26，云端状态持久化补全 + 发布运营性增强）

> 提交：bdbf5a0（③ Pod 写穿）/ 516bf9a（R-1 镜像探活）。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| ③（Pod 部分） | 云端 Pod 状态不落盘 | ✅ **v0.9.0 闭环**：EtcdPodStore 写穿（Upsert/Delete 先 etcd 后内存、失败内存不动）+ 读路径内存缓存 + Load 全量重建 + LoadAnchored/StartWatch 外部多副本增量同步；键空间 `/edgeflow/podstatus/<nodeID>/<ns>/<podName>`（v0.4.0 预留）启用；E2E 实测：重启后 Pod 列表立即可见（不再短暂清空） | 设备 reported（properties/LastReportedAt）仍不落盘（延后，与 Pod 原同族登记）；写穿失败降级内存（Upsert 返回 error，上报自愈） |
| R-1 | 发布前镜像存在性探活 | ✅ **v0.9.0 实现**（P2 升级）：registry v2 HEAD（私有 registry 直连 + Docker Hub token 换取）；env `EDGEFLOW_CLOUDCORE_RELEASE_MIRROR_CHECK`（off/warn/fail，**默认 off 零行为变化**）+ `RELEASE_MIRROR_CHECK_TIMEOUT`（5s）+ `REGISTRY_TOKEN`（私有 registry Bearer）；warn=仅告警（发布照常 202）/fail=阻断 422（带 mirror 字段） | 探活是"存在性"检查非"可拉取"保证（拉取在边缘，PodStatus 暴露）；镜像 digest 级校验未做（后续版本）——**v0.11.0 已闭环**（R-1+：探活固化 mirrorDigest + 边缘 imageDigest 上报比对，见 §11） |


## 10. v0.10.0 开发轮闭环登记（2026-08-26，云端状态收官 + 发布执行增强 + 平台构建修复）

> 提交：2e6e38c（③ 收官）/ f82488a（D6 + L20b）。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| ③（设备部分） | 设备 reported 不落盘 | ✅ **v0.10.0 闭环**：EtcdDeviceStore.Upsert 写穿完整快照（身份+Desired+reported，先 etcd 后内存、失败内存不动）；与 SetDesired CAS 路径共存（两路径写同一键完整快照，CAS 读基准合并写回天然保留 reported）；applyPut 整值采用（reported 从"各副本本地瞬态"升级为"全局一致快照——最后写入者"）；E2E 实测：重启后设备属性立即可见 | 写放大评估：设备上报 30s 周期 × 每设备一次 Put，量级 MB 可接受；多副本下 reported 收敛为最后写入者（与 Pod 一致） |
| D6 | 批内并发（v0.8/v0.9 两次延后） | ✅ **v0.10.0 实现**（P2 升级）：`EDGEFLOW_CLOUDCORE_RELEASE_BATCH_PARALLEL`（默认 1=串行，零行为变化；≥1 非法 fail-fast）；批内信号量限流并行（min(parallel, 批大小)）；failFast 语义：并发下本批在途执行完、后续批次中止（终态 head=failed + 未部署 skipped 与串行收敛一致）；E2E 实测 batchSize=3 + parallel=2 → succeeded | batchSize 仍是批粒度非并发度（并行度由本 env 独立控制） |
| L20b | Windows 交叉编译断链 | ✅ **v0.10.0 闭环**：lockCRLFile/unlockCRLFile 平台分文件（crl_lock_unix.go / crl_lock_windows.go，x/sys/windows LockFileEx）；GOOS=windows 交叉编译 ./pkg/certs/ ./cmd/cloudcore/ 通过 | 测试辅助 syscall.Dup 仅 Unix——**v0.11.0 已闭环**（captureStderrFd 平台分文件，GOOS=windows vet 通过）；Windows 制品已加入发布矩阵——**v0.11.0 已闭环**（12→18，见 §11） |


## 11. v0.11.0 开发轮闭环登记（2026-08-26，发布镜像可信化 + 可观测性补全 + 发布矩阵扩展）

> 提交：fd803c5（R-1+/L12+/L20b+）/ 36a40c9（ValidateMirror scheme 对齐）。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| R-1+ | 镜像 digest 级校验（§9 R-1 升级） | ✅ **v0.11.0 闭环**：探活固化 manifest digest（CheckMirror 返回 HEAD Docker-Content-Digest；200 缺头 → ("",nil) 保持 v0.9.0 语义静默降级）；ModelRelease.MirrorDigest 承载期望值（off/warn 失败为空 → 全链路跳过）；边缘 PodStatus 上报 imageDigest（云端/wire/边缘三端 DTO，老边缘字段缺失兼容）；控制器三接入点比对（部署即时检查/推进期复查/终态复核），mismatch=perNode failed（reason=digest-mismatch）+ 与部署失败同权；E2E 五场景实测全过（match→succeeded / mismatch→failed / 老边缘跳过 / off 空 / 推进期 catch） | ① 真实 edgecore 无运行时镜像 digest 采集（BuildStatusPayload 不填）→ 对真实边缘等效 off，需容器运行时接入（后续版本）；② 终态后晚到 mismatch 不回写（审计稳定；运维经 GET release.mirrorDigest vs GET pods.imageDigest 对比发现，处置=人工回滚/重发）；③ ValidateMirror 支持显式 scheme（http:// 内网明文 registry，可信隔离网络） |
| L12+ | hb 键重建计数（§8 L12 残余） | ✅ **v0.11.0 闭环**：续约队列改 renewRequest{nodeID, repair}；三处修复性入口（applyDelete locallyServing / rescanOnce / gcSweepOne 守卫 0）经 enqueueRepairRenew 标记；worker grant 成功才计数（重试成功只计一次；正常心跳不计）；/metrics 第 8 项 edgeflow_cloudcore_lease_hb_rebuilds_total（仅外部模式注入，0 值输出）；单测 3 用例 + metrics 3 用例 | 告警建议：持续增长（如 5min 内 >N）→ 租约抖动/键被外部删除，与 renewal_failures 互补（MONITORING-ALERTING-v011） |
| L20b+ | 测试辅助平台隔离 + Windows 入矩阵（§10 L20b 残余） | ✅ **v0.11.0 闭环**：captureStderrFd 平台分文件（certs_stderr_unix_test.go 原实现 / certs_stderr_windows_test.go SetOutput 捕获 + pkg/log.Output 访问器）；GOOS=windows vet ./pkg/certs/ 通过；Makefile CROSS_PLATFORMS 3×6（+windows amd64/arm64 +keadm），cross-build 实测 18 制品（PE 格式） | edgecore-windows 仅验证编译与构建（主要部署面仍 linux/arm64），不承诺运行语义；发布矩阵 12→18 口径 |
