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
| L16 | offlineSince（保留期时钟） | **重启重置 offlineSince**：保留期从重启时刻起算（v0.5.0 同款）；多副本下滚动重启会延长孤儿台账保留 | 孤儿台账多保留一个滚动窗口（≤2×TTL 量级）；GC 安全性不受影响（删除守卫在） | ✅ **v0.13.0 已闭环 DTO 部分**（L16 残余）：`NodeInfo.offlineAt`(ms) + `EdgeNode status.lastOfflineTime`(RFC3339) 双视图外露，JSON 宽容论证推翻原"改 JSON 影响兼容性"担忧（瞬态内存数据、不落盘）；offlineSince 重启重置语义不变。精确"最后在线"需持久化 → 后续候选 |
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
| L25 | 模型删除级联（非事务） | **级联删除非事务**：删除版本前缀/部署影子前缀/meta 中途崩溃 → 孤儿版本/部署键残留（按 meta 过滤不可见，占空间） | 启动加载只认 meta 存在的前缀（孤儿不加载）；残留键不可见。**v0.13.0 收官**（B）：guard 已清（v0.12.0 前）；元数据最后删已消除不可见孤儿窗口；GC 显式开启（RELEASE_GC_ENABLED=1）时 DeleteModel 级联清理该模型全部终态发布（头/逐节点/lock+内存） | 默认 GC-off 时残留键仍可 `etcdctl del /edgeflow/models --prefix` 手动清理；非事务级联崩溃窗口登记（原子化需 etcd 跨前缀 txn，后续候选） |
| L26 | 回滚守卫（RollbackRelease 前置） | **回滚被新版本接管 → 拒绝**：`release.version ≠ 模型当前 active 版本` → 409（文案引导显式 activate 或新发布）；API 校验通过后、控制器执行前被接管/被删 → 执行期复查中止（head=failed + 清 rollbackRequested + 未执行节点 skipped，D2/D4） | "回滚开倒车覆盖新部署"被架构性封堵；执行期复查后极端窗口收敛为明确终态 | 文档明示（API-SPEC §7.2/§7.3）；C3 用例覆盖 |
| L27 | 取消收敛（cancel 后 perNode 补齐） | cancel 置位后，未执行节点标 skipped 有 **≤1 扫描周期（默认 5s）补齐窗口**；pending→canceled 的 guard 释放同样 ≤1 扫描周期 | 查询方在窗口内可能看到 pending 残留 | 查询方容忍；文档明示（API-SPEC §7.2） |
| L28 | release/模型列表（List 端点） | **无分页（全量返回）**；N-4 同族：终态 release 常驻内存缓存无 GC | 数据量 = 任务/模型规模（当前可接受）；长运行内存线性增长 | 后续版本分页/GC（与 L28 同族登记） |
| L29 | 混合版本多副本（升级/回滚窗口） | **v0.6.0 与 v0.7.0 副本同连一集群未验证**：v0.7.0 只新增 `/edgeflow/models/` 前缀（旧版不读不写，理论无害）仍**建议同版本全量切换**；残留键可 `etcdctl del /edgeflow/models --prefix` 清理 | 升级/回滚窗口行为未实证 | 升级/回滚全停再全起（DEPLOYMENT §10.9.4） |
| L30 | 孤儿 guard 自愈（CreateRelease 存储层，D3） | **孤儿 guard 自愈语义登记**：guard CAS 成功、release 键写入前崩溃 → 孤儿 guard；创建重试时自愈——guard 冲突 → 读 guard 指向的 release 键，不存在（或已终态）→ **按值 CAS 删 guard**（CompareAndDelete expectRev，防误删新 guard）→ 重试一次；仍冲突 → 409。废弃"lock 过期"陈旧判据（孤儿场景 lock 键从未创建）；控制器不承担兜底（只扫内存 release） | 无自愈则该模型永久 409（仅剩手动 etcdctl）；自愈后单次重试即恢复 | 文档登记（ARCHITECTURE R16/API-SPEC §7.2）；S4 补"guard 写后崩溃→重试创建自愈"用例 |
| L31 | 终态 release 键保留策略（D9/N-1） | **终态 release 头与 perNode 键永久保留作审计痕迹**（不随模型删除级联清理；键路径带 releaseID 不随模型名走）；不可见、无功能影响 | 长期运行键空间累积（MB 量级可忽略；与 L25/L28 同族） | 登记为有意策略；`etcdctl del /edgeflow/models/releases --prefix` 可手动清理（按审计保留期自行权衡）。**v0.13.0 口径扩展**：GC 显式开启时模型删除级联全清该模型终态发布（审计痕迹随之清除，运维以 ops 台账/文档为凭）；默认关闭保持永久保留 |

## 8. v0.8.0 开发轮闭环登记（2026-08-26，运维与安全增强：etcd 鉴权/续约监控/模型运营性）

> 本开发轮闭环三个跨轮排期项（L1/L12/L28），并登记 GC 开启后的 L31 口径变更。提交：88b3765（L1）/ dc0df54（L12）/ 7a1941a（L28）。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| L1 | 外部 etcd 无鉴权参数透传 | ✅ **v0.8.0 闭环**：`EDGEFLOW_CLOUDCORE_ETCD_USERNAME/PASSWORD` 成对透传（只设其一 fail-fast；与 TLS/mTLS 正交；embed/纯内存忽略不串扰）；PermissionDenied 探活文案更新（引导 RBAC 配置 + mTLS CN 映射）。CN→角色映射仍由 etcd 侧 `--client-cert-auth` 配置（非透传项，文档指引） | 密码经 env 注入（K8s 建议挂 Secret 转 env）；无 URL 内凭证支持（有意，防日志泄露） |
| L12 | 续约失败无可观测性 | ✅ **v0.8.0 闭环**：`/metrics` 新增 `edgeflow_cloudcore_lease_renewal_failures_total`（counter，仅外部模式注入；0 值也输出便于面板基线）；建议告警：持续增长（如 5min 内 >N）→ etcd 侧异常/网络分区 | 无独立"hb 键重建"计数（自愈可观测性可后续加）；告警阈值需按判活 TTL 折算 |
| L28 | release/模型列表无分页 + 终态无 GC | ✅ **v0.8.0 闭环**：① 分页——GET models/versions/releases 支持 `limit`(1-1000)/`offset`(≥0)，响应头 `X-Total-Count`，缺省全量（零破坏）；**v0.13.0 补 deployments 同族分页（A′，listDeployments 漏网项）**；② GC——`GCReleases` 按 CreatedAt 保留最近 keep 条终态（默认 **关闭**，`EDGEFLOW_CLOUDCORE_RELEASE_GC_ENABLED=1` + `RELEASE_GC_KEEP` 默认 100 开启），删除旧终态头+逐节点结果，非终态/在途绝不删 | GC 开启后 L31 口径变更：终态键不再永久保留（按 keep 截断），审计痕迹以 ops 台账/文档为准；默认关闭保持原口径 |
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
| R-1+ | 镜像 digest 级校验（§9 R-1 升级） | ✅ **v0.11.0 闭环**：探活固化 manifest digest（CheckMirror 返回 HEAD Docker-Content-Digest；200 缺头 → ("",nil) 保持 v0.9.0 语义静默降级）；ModelRelease.MirrorDigest 承载期望值（off/warn 失败为空 → 全链路跳过）；边缘 PodStatus 上报 imageDigest（云端/wire/边缘三端 DTO，老边缘字段缺失兼容）；控制器三接入点比对（部署即时检查/推进期复查/终态复核），mismatch=perNode failed（reason=digest-mismatch）+ 与部署失败同权；E2E 五场景实测全过（match→succeeded / mismatch→failed / 老边缘跳过 / off 空 / 推进期 catch）。**v0.12.0 补端到端**：① 真实边缘采集闭环（R-1++ 双通道，见 §12）；② 复核端点自动化（D-1，见 §12） | ① 真实 edgecore 运行时采集已接入（**v0.12.0 闭环**，双通道声明式/运行时）；② 终态后晚到 mismatch 仍不回写（审计稳定设计取舍，**v0.12.0 起经复核端点 GET .../releases/{id}/digest 一键对比发现**，处置=人工回滚/重发）；③ ValidateMirror 支持显式 scheme（http:// 内网明文 registry，可信隔离网络） |
| L12+ | hb 键重建计数（§8 L12 残余） | ✅ **v0.11.0 闭环**：续约队列改 renewRequest{nodeID, repair}；三处修复性入口（applyDelete locallyServing / rescanOnce / gcSweepOne 守卫 0）经 enqueueRepairRenew 标记；worker grant 成功才计数（重试成功只计一次；正常心跳不计）；/metrics 第 8 项 edgeflow_cloudcore_lease_hb_rebuilds_total（仅外部模式注入，0 值输出）；单测 3 用例 + metrics 3 用例 | 告警建议：持续增长（如 5min 内 >N）→ 租约抖动/键被外部删除，与 renewal_failures 互补（MONITORING-ALERTING-v011） |
| L20b+ | 测试辅助平台隔离 + Windows 入矩阵（§10 L20b 残余） | ✅ **v0.11.0 闭环**：captureStderrFd 平台分文件（certs_stderr_unix_test.go 原实现 / certs_stderr_windows_test.go SetOutput 捕获 + pkg/log.Output 访问器）；GOOS=windows vet ./pkg/certs/ 通过；Makefile CROSS_PLATFORMS 3×6（+windows amd64/arm64 +keadm），cross-build 实测 18 制品（PE 格式） | edgecore-windows 仅验证编译与构建（主要部署面仍 linux/arm64），不承诺运行语义；发布矩阵 12→18 口径 |


## 12. v0.12.0 开发轮闭环登记（2026-08-26，digest 校验端到端落地：真实边缘采集闭环 + 发布复核可观测性）

> 提交：6476cad（R-1++ / D-1 / F-1）。设计 Agent 定稿 design.md（双通道采集 + 复核端点 + F-1 潜在 bug 实测发现）。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| R-1++ | 真实 edgecore 运行时 digest 采集（§11 R-1+ 残余①） | ✅ **v0.12.0 闭环**：`ContainerRuntime` 新增 `ImageDigest(pod, index)`（Docker RepoDigests 查询，与云端 Docker-Content-Digest 同源；MockRuntime 注入点 SetImageDigest）；`digestOfImageRef` 声明式解析纯函数（期望镜像 ref 含 `@sha256:` 即 pin 上报，零运行时依赖）；`BuildStatusPayload` 双通道合并——**声明式优先、运行时兜底、仅 StateRunning 上报、失败降级空串不阻塞**；无缓存（30s 上报周期天然限频）、零新增 env；单测 6 组覆盖 | ① Docker daemon 本机不可用 → Tier2 真实拉取场景未本机执行（查询逻辑已 fakeRunner 单测 5 场景覆盖；环境缺口登记）；② pin 引用依赖发布方写 `@sha256:` 或运行时已拉取镜像有 RepoDigests（本地构建镜像无 RepoDigests → 空 → 跳过） |
| D-1 | 终态后晚到 mismatch 运维对比自动化（§11 R-1+ 残余②） | ✅ **v0.12.0 闭环**：新增 `GET /api/v1/models/{modelName}/releases/{releaseID}/digest`（只增不改，端点 31→32）——发布 mirrorDigest + 逐节点 currentImageDigest/releaseStatus/consistency（skipped=发布级未启用 / consistent / inconsistent / unknown=节点侧缺失）+ head 聚合；任意状态可查（非终态=进行中视图）；`nodeDigestOf` 提升为 `podstatus.NodeDigestOf`（控制器 DigestLookup 与端点共用同一闭包，口径一致）；E2E 实测 consistent/inconsistent/unknown/skipped 全矩阵 | 处置仍为人工回滚/重发（审计稳定不回写不变）；复核端点给出当前快照，晚到 mismatch 由运维经端点发现 |
| F-1 | finish ③ 终态复核读库失效（v0.11.0 latent bug，设计验证发现） | ✅ **v0.12.0 修复**：release.go 终态复核后 `results = results`（shadow 自赋值）改为 `results = latest`——终态判定不再用陈旧快照；若 ③ 是首个 catch mismatch 的接入点，head 不再错误 succeeded（此前 perNode 已 failed 而 head succeeded 状态分裂）；`go vet` 报 self-assignment 同步消除；专属单测 `TestReleaseDigestMismatchCaughtOnlyAtFinish`（batchSize=2 单批无推进期窗口 + lookup 调用计数，修复前 head=succeeded 暴露缺陷） | 无 |


## 13. v0.13.0 开发轮闭环登记（2026-08-26，模型生命周期与运维收尾）

> 提交：32b1a34（A′ / B / C）。设计 Agent 定稿 design.md（347 行六节；实测推翻 S1 候选 A——releases 分页 v0.8.0 已闭环，发现同族漏网项 A′）。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| A′ | 部署影子列表（ListDeployments 端点）无分页（L28 同族、v0.8.0 漏网） | ✅ **v0.13.0 闭环**：`GET .../deployments` 增加 `limit`(1-1000)/`offset`(≥0) 与响应头 `X-Total-Count`，缺省全量（零破坏），与 releases/versions/models 既有分页完全同构；分页在 API 层完成（存储层 ListDeployments 已按 NodeID 排序）；单测 8 边界 + E2E S1 实测（total=2/limit 切片/4 非法参数 400） | 无 |
| B | 模型删除级联收官（L25）：已删模型的终态发布在 GC 开启下永不可回收（控制器 gcIfEnabled 只对活模型触发） | ✅ **v0.13.0 闭环**：`WithReleaseGC` store 选项（variadic 向后兼容）+ DeleteModel 在 GC 显式开启时级联清理该模型全部终态发布（头键 + releases/<id>/ 前缀含 nodes/lock + 内存缓存；etcd deleteModelReleases best-effort 失败仅 Warn）；GC-off 默认 = L31 审计口径零变化；**零新增 env**（复用 RELEASE_GC_*）；单测 5 例 + E2E S3（GC-on 删模型后重建同名模型 releases 空）/S4（GC-off 回归） | 非事务级联崩溃窗口仍存在（原子化需 etcd 跨前缀 txn，登记后续候选）；GC-on 下审计痕迹随删除清除（运维以 ops 台账为凭，文档明示） |
| C | 节点台账无 offlineAt（L16 残余 DTO 部分） | ✅ **v0.13.0 闭环**：`NodeInfo.offlineAt`(毫秒,omitempty) + `EdgeNode status.lastOfflineTime`(RFC3339,omitempty) 双视图外露；Get/List/ListEdgeNodes 三入口一致填充（ListEdgeNodes 先填内部 0 值再映射）；三模式统一（wrapper 均委托内存 Registry）；在线/未知 = 字段省略；重复 MarkOffline 不刷新（首离时刻）；JSON 宽容论证推翻原登记担忧（瞬态内存不落盘）；单测 4 例 + E2E S2 实测三态（在线无字段/断开出现/恢复消失） | 精确"最后在线"需持久化（**v0.14.0 设计评估后登记后续候选（v0.15.0）**，需 registry 持久化改造，独立运维主题）；Seed 播种后 Unknown 节点 offlineAt=启动时刻（如实反映重启重置） |


## 14. v0.14.0 开发轮闭环登记（2026-08-27，OPC-UA 里程碑第二阶段：端到端 Mapper v1）

> 提交：413ddb6。设计 Agent 定稿 design.md（566 行六节；实测勘察 pkg/opcua 四缺口：SecureChannel/服务层/DiagnosticInfo/Mapper Client API）。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| OPC-A | pkg/opcua 无 SecureChannel/Session/Read 服务，Mapper 层无客户端 API——UA Binary 协议栈仅编解码/HEL/ACK，无法接入真实设备（WBS 5.2 第二阶段核心缺口） | ✅ **v0.14.0 闭环**：SecureChannel 层（OPN/CLO、Asymmetric/Symmetric 安全头、SequenceHeader 序列号与 RequestId 响应关联；OPN 后 conn.channelId 生效，既有导出 API 零变更）+ Session 匿名会话（CreateSession/ActivateSession/CloseSession）+ Read/Write 服务 + Client API（Open 全链路握手→Read 批量读→Write 写点→Close 有序关闭）+ DiagnosticInfo 位域补全（ResponseHeader 必需；Variant 内嵌 0x19 保持不支持）+ ParseNodeID 配置解析（与 NodeId.String() 互逆）+ server_api.go 服务端互操作导出面 | Browse 节点发现 / Subscription 订阅推送 / Sign-SignAndEncrypt 安全策略排除登记后续（点位显式配置即可用；明文策略延续 doc.go 既有边界）；第三方 UA 栈互操作 cross-check 待环境就绪（node-opcua/open62541） |
| OPC-B | OPC-UA 设备采集到云端属性可见的完整链路缺失（Mapper 框架协议无关但无 OPC-UA 实现） | ✅ **v0.14.0 闭环**：`pkg/opcuasim` 自研模拟服务器（6 点位表：温度收敛/humidity/pressure 游走/setpoint 可写，回环绑定默认 14840）+ `mappers/opcua` Mapper（Collect 批量读点转换 Variant→float64 契约 + HandleCommand 写点回读验证 + 台账 + 断线重连）+ edgecore env opt-in 装配（4 个 `EDGEFLOW_OPCUA_*` 登记）；云侧零改动（DeviceReport→devicestatus→/api/v1/devices 既有链路自然扩展） | Variant→float64 转换边界：数值/Boolean/String(ParseFloat) 支持，DateTime/Guid/ByteString/NodeId/StatusCode/QualifiedName/LocalizedText/ExtensionObject/数组不支持（跳过+Warn，文档明示） |
| OPC-C | OPC-UA Mapper 无写点能力（与 Modbus 双向能力不对齐） | ✅ **v0.14.0 闭环**（Phase B）：pkg/opcua 补 Write 服务；Mapper HandleCommand 命中点位名→Write(Double)→回读验证（容差 1e-6）→台账 down；只读节点写入被服务端拒绝（BadNotWritable）正确透传为 502 | 无 |
| E2E-BASE | tests/e2e 基建缺陷：cloudcore 嵌入式 etcd 数据目录为相对 cwd 共享路径（data/etcd），跨用例/跨运行残留旧台账导致注册与在线判定串台（TestOPCUADeviceE2E 排障发现，影响全部 e2e 用例的稳定性） | ✅ **v0.14.0 修复**：cloudEnv 为每个用例注入独立 `EDGEFLOW_CLOUDCORE_ETCD_DATA_DIR`（t.TempDir 下），同用例重启场景共享目录保持台账恢复语义；既有语义（autonomy 断连恢复依赖影子恢复）不受影响 | 无 |

> §13 B 残余（非事务级联崩溃窗口）同样经 v0.14.0 设计评估后**登记后续候选（v0.15.0）**（涉及 etcdstore 跨前缀事务接口 + DeleteModel 重构，独立运维主题，与 OPC-UA 大主题纪律冲突）。


## 15. v0.15.0 开发轮闭环登记（2026-08-27，OPC-UA 里程碑第三阶段：Subscription 订阅推送 + Browse 浏览发现）

> 提交：4da004f。设计：.cluster/edgeflow-v0150/design.md 六节；服务 TypeIds 以 OPC Foundation UA-Nodeset v1.04 官方 NodeIds.csv 核验（CreateSubscription=787/790、Publish=826/829、Browse=527/530、DataChangeNotification 内容=811 等）。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| OPC-D | Mapper 仅轮询批量 Read，点位多时网络开销随 report 周期线性放大；值变化无法即时感知 | ✅ **v0.15.0 闭环**：`EDGEFLOW_OPCUA_SUBSCRIPTION=on` 订阅采集模式——客户端 CreateSubscription/CreateMonitoredItems/Publish 全链路 + 泵模式唯一读 goroutine（RequestId 三级路由：waiter 表→pending 兜底→在途 Publish），推送写缓存快照、Collect 短路返回；KeepAlive 空通知识别；gap→重建订阅；断线全量重建；缺省 off 与 v0.14.0 行为逐字节一致 | HandleCommand 回读后同步刷新缓存已做；Republish 补帧未做（数据集无状态，重建即自愈）；EventNotificationList 只跳过不解码 |
| OPC-E | 真实设备点位需人工手写 EDGEFLOW_OPCUA_NODES，接入成本高且易错 | ✅ **v0.15.0 闭环**：客户端 Browse API + opcuasim 两级最小目录（Objects i=85 → opcua-sim ns=2;i=5000 → 6 变量）+ hack/opcua-browse CLI（输出可直接粘进 NODES 的 name=nodeId 行） | 目录仅两级静态模型；continuationPoint 分页恒空（requestedMax 尊重但数据量小于页）；BrowseNext 未发起 |
| INTEROP | 试解分派链缺陷：部分消费即判成功导致请求被错误分支吞掉（本轮实弹排障发现——Browse 帧曾被 Publish 试解器吃掉 34 字节后报错交回，但 Read 对更短帧曾直接误判成功） | ✅ **v0.15.0 修复**：全部请求解码器强制“字节全消费”校验（trailing bytes → ErrInvalidEncoding 交回分派链下一分支）；单包级 round-trip 探针覆盖 CS/CMI/Publish/Browse 互认边界 | 服务端多 chunk / 扩展头场景仍排除 |
| WIRE | 模拟器异步出站帧缺 MSG 帧头（对称头+序列头直写 TCP），客户端 ReadMessage 解析错位 | ✅ **v0.15.0 修复**：writeServerFrame 统一补 EncodeHeader（MsgSecureMessage + channelID + size） | 无 |

> 排除项延续：Sign/SignAndEncrypt、事件订阅（Alarm&Condition）、第三方 UA 栈互操作 cross-check。


## 16. v0.16.0 开发轮闭环登记（2026-08-27，AI 模型管理深化：定时维护窗口 + 发布暂停恢复 + 模型目录导出导入）

> 提交：757a1b0。设计：.cluster/edgeflow-v0160/design.md。契约变更：HTTP 端点 32→36（pause/resume/export/import），三守卫测试联动绿。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| MR-W | 发布创建即扫描认领，无法预约工业夜间/低峰维护窗口 | ✅ **v0.16.0 闭环**：`notBeforeMs` 创建参数（opt-in，0=立即行为不变）——控制器窗口未到不认领、不占领跑锁；API 校验 ≥0 且 ≥now-5min（钟漂护栏）；窗口期 InFlight 守 guard 同模型不并发 | 待调度队列无专门观测视图（列表接口 NotBeforeMs 可见）；cron 式重复发布不在范围 |
| MR-P | 灰度中途发现异常只能取消重来（已部署节点需回滚），缺"先停下观察"能力 | ✅ **v0.16.0 闭环**：POST pause/resume——running⇄paused 状态机扩展；批边界生效不中断在途下发；paused 保 active 身份续租领跑锁（多副本接管语义不变）；NextBatchAt 保持原节奏；paused 可直接 cancel，rollback 拒 paused | 持锁副本崩溃且无人接管期间发布保持 paused（安全但停滞，R1 登记） |
| MR-X | 开发→生产环境模型台账迁移靠手工 etcd 操作；灾备恢复无官方通道 | ✅ **v0.16.0 闭环**：GET export（models+versions 全量快照 JSON，schemaVersion=1）+ POST import 幂等 upsert——同 (model,version) 跳过计数、active 经 draft+activate 直通灾备语义、孤儿版本自动补建空壳模型、改段重放可达"新环境全量导入" | 导出/导入无分页与流式（1MiB import 上限内规模可用，R2/R3）；releases/deployments 明确不可迁移 |


## 17. v0.17.0 开发轮闭环登记（2026-08-27，发布任务运维深化：运行中可调参数 + 列表 status 过滤 + dryRun 预检）

> 提交：见 git log。契约变更：HTTP 端点 36→**37**（PATCH 运行中可调参数；status 过滤与 dryRun 复用既有端点形态不新增）。守卫测试联动绿。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| RO-A | 发布创建后执行参数不可变——批太大想改小、节奏太密想放慢、失败策略想放宽都只能取消重建（目标节点进度全丢） | ✅ **v0.17.0 闭环**：`PATCH .../releases/{id}` 运行中改 batchSize/pauseBetween/failFast（部分更新语义，未提供字段保持）；控制器 BuildBatches 每轮重切 → 下一批边界生效、不中断在途批、无追溯歧义；pending/running/paused 均可改、终态 409；CAS 并发安全（复用 UpdateReleaseHead 重试 ≤3） | 身份段（Version/Target/TargetNodes）仍不可变（运行期语义清晰优先，变更目标=取消重建） |
| RO-B | 发布列表只能全量翻页找状态，运维视图需要"只看在途/只看失败"时需客户端过滤 | ✅ **v0.17.0 闭环**：列表 `?status=` 过滤（逗号多值、合法枚举校验 400 族、与 limit/offset 正交、X-Total-Count 报过滤后总数） | 无残余 |
| RO-C | 创建发布前无预检通道——配错版本/写错节点名只能提交后看 4xx 或等 202 后人工盯 | ✅ **v0.17.0 闭环**：创建请求 `dryRun:true` 全量走真实校验链（模型 404/内容 400/非 active 422/探活阻断/节点物化）+ guard 等价只读判定，返回 200+wouldCreate 摘要（blockReason/inFlightID 可读原因）；**零落盘零 guard 键零 perNode 预写** | 预检结论为 TOCTOU 快照非承诺语义（响应已标注；真实创建以 CreateRelease guard CAS 兜底）——运维不应把 wouldCreate 当预约锁 |


## 18. v0.18.0 开发轮闭环登记（2026-08-27，发布面智能运维：失败预算自动暂停 + 发布事件时间线 + 全局部署影子查询）

> 提交：见 git log。契约变更：HTTP 端点 37→**38**（全局部署影子查询）；failureBudget/events 复用既有端点形态不新增路由。守卫测试联动绿。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| SO-A | 灰度失败只能靠人盯列表发现——多批长跑发布中途劣化无人知，跑完才看到一片 failed | ✅ **v0.18.0 闭环**：创建参数 `failureBudget`（≥1 启用，0=禁用行为不变）——批完成后 failed 计数达预算且未终态 → **自动 pause**（复用 paused 状态机与 guard 语义），人可介入排查后 resume 续跑剩余批次；head 带 `autoPausedAt` 与 autopause 事件 | 预算只在 failFast=false 跑完判定模式下生效（failFast 首败即中止置 failed，无累计窗口，语义登记）；预算创建后只读（PATCH 三执行参数不变） |
| SO-B | 发布流转过程是黑盒——暂停过几次、哪批完成、回滚何时请求都靠翻日志拼时间线 | ✅ **v0.18.0 闭环**：`ModelRelease.Events` 时间线——created/paused/resumed/cancelled/terminal/autopause/batch_done/rollback_requested 开放事件集，追加在 UpdateReleaseHead mutate 闭包内 = CAS 保护并发不丢；环形上限 32 条丢最旧保最新；随 release 详情返回、随 export/import 快照迁移 | 高频事件的完整审计长尾依赖外层台账/日志（键值正文不自带 history）；batch_done 为每批一条（长发布最旧事件会被截断，口径登记） |
| SO-C | 部署影子只能按模型逐个查——"这个节点现在装的是什么版本"这类跨模型问题要循环调 N 个端点 | ✅ **v0.18.0 闭环**：`GET /api/v1/deployments` 全局聚合（Memory 遍历两级 map/Etcd 读 watch 缓存同一口径）；model/nodeID 精确过滤可选；过滤后分页 X-Total-Count 同步；先 Model 再 NodeID 双字典序确定性排序 | 无残余（现有 per-model 端点不动，二者并存） |


## 19. v0.19.0 开发轮闭环登记（2026-08-27，发布面智能运维第二批：failureBudget 运行中可调 + 发布审计快照 + 全局发布查询）

> 提交：见 git log。契约变更：HTTP 端点 38→**40**（发布审计快照 + 全局发布查询）；PATCH 白名单扩展 failureBudget 不新增路由。守卫测试联动绿。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| SI-A | 失败预算创建后只读——预算设大了跑完才发现刹不住，只能重发一个发布 | ✅ **v0.19.0 闭环**：PATCH 白名单扩展 `failureBudget`（剩余执行面参数，与 batchSize/pauseBetween/failFast 同动线）；批边界生效语义同既有三参数——改小后下一批后立即适用，`=0` 运行中关闸（禁用自动暂停）；值域 [0,10000] 与创建校验同量级护栏；终态仍 409 | AutoPause 判定读的是当前 head 预算+派生 failed 计数，无持久计数器——改动的起算口径="自当下起的剩余批次"（非全程回溯），登记在案 |
| SI-B | 发布全景要调 N 个端点拼装——头 + 逐节点结果 + events 分开拉，审计取证繁琐且有时序缝隙 | ✅ **v0.19.0 闭环**：`GET .../releases/{id}/snapshot` 审计快照一次拉全（kind=ReleaseSnapshot / generatedAt / release 头含 events 时间线 / summary 五计数实时现算 total/deployed/failed/skipped/pending / nodes 恒非 nil）；跨模型引用钉 head.Model 一律 404 防目录穿越式枚举；GetModel(404) 先行与 v0.17.0 C-4 同链序纪律 | **非承诺语义**（generatedAt 后的写入不在快照内）；超大发布节点结果以分页列表为准（快照不带分页，口径登记）；**currentBatch 未纳入 summary**（设计评审 P0-2 登记：范围稿曾定六计数，实装以 NodeSummary 五计数交付，currentBatch 由消费方按 nodes 派生抽） |
| SI-C | 跨模型发布运维难——"现在有几个 running 发布""最近失败的三条"这类全局问题要循环 N 个模型的列表端点 | ✅ **v0.19.0 闭环**：`GET /api/v1/releases` 全局聚合（与 v0.18.0 /api/v1/deployments 对偶）；status 七态逗号多值过滤复用 v0.17.0 枚举；limit 缺省 100 上限 500 + offset≥0，X-Total-Count 报过滤后总数；CreatedAt 降序稳定 tie-break by ID | 响应为裸 `{"items":[…]}` 无 kind/apiVersion 信封（设计评审 P1-1 登记：与仓库 List 风格差异，已发布不回改，留待触碰该端点时收敛）；per-model 列表端点不动二者并存 |

## 20. v0.20.0 开发轮闭环登记（2026-08-27，发布生命周期收口：失败节点重试 + 终态发布归档删除 + 发布元数据）

> 提交：见 git log。契约变更：HTTP 端点 40→**42**（retry + 终态归档删除）；releaseNotes 为头内嵌字段零新端点。守卫测试联动绿。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| LC-A | 失败发布只能整单重来——3 节点成功 1 节点失败时，重发等于对成功节点再滚一遍 | ✅ **v0.20.0 闭环**：POST .../retry 失败节点克隆重试——仅终态可 retry；克隆新发布（RetryOf 回指原发布），只继承 failed 子集为 TargetNodes（nodeIDs 可选缩围）；版本仍 active 复查防补发已删版本；guard 冲突照常 409 | retry 本身可再次 failed → 可再 retry（链式审计经 RetryOf 回溯）；无深度限制登记 |
| LC-B | 终态发布永久堆积——GC 是模型级批量且默认关，没有"就删这一条"的运维动作 | ✅ **v0.20.0 闭环**：DELETE .../releases/{id} 单条终态归档删除——非终态 409 与 GC 同源「在途绝不删」语义；双存储同步删头键+子键（对齐 GCReleases 删除路径）；200 返回被删快照供审计 | 无回收站/恢复机制——删除不可逆（快照响应体是唯一落地凭据），依赖 export 备份的口径不变 |
| LC-C | 发布缺变更注记——哪条发布对应哪个变更单/谁发起的，只能靠外部台账对 | ✅ **v0.20.0 闭环**：releaseNotes 元数据（≤1024 字节 opt-in）创建期一次性写入、创建后不可变（PATCH 白名单不含）；list/get/snapshot/global 全读取路径透出；retry 克隆继承 | 元数据仅自由文本不做结构化字段（变更单号格式不设约——分库直存会引入校验面）；1024 上限外无截断直接 400 |

## 21. v0.21.0 开发轮闭环登记（2026-08-28，安全默认值包 + 协议纵深包：审计 P0 修复——可见性告警 + opt-in 开关 + 报文纵深防御）

> 提交：见 git log。契约变更：**无**（HTTP 端点保持 42；全部开关默认关闭，缺省行为与 v0.20.0 逐字节一致）。守卫测试联动绿。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| SD-A | 管理 API 认证默认关闭且启动时无任何提示——生产误配置不可见 | ✅ **v0.21.0 闭环**（SEC-01）：cloudcore 启动时 auth 未启用 → WARN 提示全部 /api/v1/* 端点无差别开放及收紧路径；`EDGEFLOW_CLOUDCORE_AUTH_WARN=off` 可显式静默（缺省/非法值均视为开启）；Helm `cloudcore.auth.{enabled,apiToken,warnOff}` 透传 | WARN 仅可见性不动行为——认证仍需显式开启；告警文案指向的开关与真实 env 名一致（v0.21.0 修正过脱敏符残留） |
| SD-B | 云边接入未配令牌时任意主机可注册冒充节点并抢占同 ID 真节点——空值=不校验的裸奔面无显式收紧手段 | ✅ **v0.21.0 闭环**（SEC-02）：`EDGEFLOW_CLOUDHUB_REQUIRE_NODE_TOKEN=on` 且服务端未配令牌 → 拒绝**携带令牌**的注册（防伪造令牌探测抢占）；**无令牌注册仍接受**（裸奔兼容底线：全面关闸=配 nodeToken 或 mTLS 任一）；nodeToken 非空时既有校验不变，本开关无额外效果 | enforce=on 时连无令牌注册也拒绝的 fail-closed 形态经评审判定与裸奔兼容底线冲突未采纳（存量无令牌边缘会被锁死）——留待维护窗口协商机制（P1 决策异议）；抢占窗口对无令牌攻击者仍然存在（mTLS/nodeToken 才是根治） |
| SD-C | 无 mTLS 且无令牌同时成立（双重裸奔）无聚合提示，生产不可见 | ✅ **v0.21.0 闭环**（CHN-06）：hubTLS 为空且 nodeToken 为空 → 启动 WARN 聚合提示注册面与下发面均明文且无认证，仅限可信隔离网络；提示任一收紧路径即消除 | 与 SD-B 告警互补去重：enforce=on 时该维度输出 INFO 避免噪音；告警不阻断启动 |
| PR-A | OPC-UA 恶意报文声明超大数组 → 按声明长度预分配内存（16M 元素槽）OOM 放大 | ✅ **v0.21.0 闭环**（PRT-01/14）：Variant 数组与 StringList 解码前预检——声明元素数 × 元素最小编码字节数 > 剩余缓冲且 >1024 元素豁免阈值 → 直接 `ErrTooLong` 拒绝，不再按声明预分配；恶意 20 字节报文内存峰值从 16M 槽降为 O(1) | 1024 元素内（≤16KB）不拦截——小规模声明预分配无害且保持既有截断语义（ErrUnexpectedEOF，既有测试零改动）；元素最小编码字节表覆盖内置类型，未知类型仍由解码器拒绝 |
| PR-B | DiagnosticInfo 内层递归无深度限制——深嵌套报文（如 10 万层）触发深栈消耗 | ✅ **v0.21.0 闭环**（PRT-03）：`MaxDiagnosticDepth = 100`，`decodeDiagnosticInfo` 按 InnerDiagnosticInfo 嵌套深度计数超限拒绝（ErrTooLong）；ResponseHeader/OpenSecureChannelResponse/通知内嵌诊断全部调用点统一传 depth | 深度上限 100 为工程值（合规报文远低于此）；超限报错信息含实际深度与上限，便于定位 |
| PR-C | 订阅泵异常退出后 pubCh 不关闭——订阅方 for range 永久阻塞，goroutine 泄漏累积 | ✅ **v0.21.0 闭环**（PRT-04）：pumpLoop 一切退出路径（连接级故障/stopPump）在 defer 中关闭 pubCh（sync.Once 防双关），订阅方 range 可退出；退出时 c.pubCh 置 nil，下次 Subscribe 重建新通道不复用已关闭通道 | 白盒测试验证 goroutine 回收至基线；mapper 侧自愈见 PR-D |
| PR-D | 云边 mapper 依赖的订阅通道泵死后不自愈——需重启进程恢复采集 | ✅ **v0.21.0 闭环**（PRT-18）：OPC-UA mapper 感知 pubCh 关闭（泵异常退出）→ 自动重连并重建订阅，采集无需人工干预 | 自愈依赖重连成功；若端点持续不可达则按既有重连退避节奏持续尝试并告警 |

## 22. v0.22.0 开发轮闭环登记（2026-08-28，P1 缺陷修复包：审计 T-05~T-11 全闭环——云边通道防线 + 边缘幂等落盘 + 发布状态机契约 + API 归属收口 + 吊销链可配）

> 提交：见 git log。契约变更：**无新增端点**（HTTP 端点保持 42）；release 子资源跨模型引用 404 语义收口（原缺陷行为→正确行为，详见 §7.11）；全部新开关默认关闭，缺省行为与 v0.21.0 一致。守卫测试联动绿。

| 编号 | 问题面 | 闭环说明 | 残余与建议 |
|---|---|---|---|
| CHN-07 | 换 ID 重注册时事件顺序无验收钉死；注册风暴自我放大面（边缘等云端同步登记才 ack） | ✅ v0.22.0 闭环（T-05+T-08）：事件顺序（先 disconnect(old) 后 register(new)）+ 无幽灵 Ready 节点以测试钉死；handleRegister 改为**先回 RegisterAck 再执行登记类事件回调**——云端故障窗口内边缘立即可心跳，不再堆积重试 | ack 前置后事件回调失败不回滚 ack（边缘已视为注册成功）——依赖心跳/重连自愈，属可接受最终一致 |
| CHN-02 | 慢客户端发送缓冲只有消息数上限（64 条），无字节计量——大消息积压内存不可控不可观测 | ✅ v0.22.0 闭环（T-08）：单连接字节配额默认 64MiB（入队前丢弃+关连接，与消息数满同策略）；gauge `edgeflow_cloudcore_hub_send_buffer_bytes` 可观测；`WithBroadcastMemLimit` 可选广播内存闸门（默认不启用） | 配额仅云端入站方向；边缘侧接收缓冲（读泵）仍由 websocket 库默认管理——下行大对象建议继续走分片 |
| CHN-03 | 边缘下行指令去重仅内存——重启后云端重试同 msgID 重复执行 | ✅ v0.22.0 闭环（T-07）：dedup_keys SQLite 持久化（TTL 24h/上限 10000 条/批量淘汰最旧/三重清理时机）；未装配自动退化纯内存（兼容） | e2e 真实进程级重启用例未做（集成测试已覆盖同库两 Client 语义）；表无 compaction 之外的碎片整理——量级上限 1MB 无需 |
| CHN-05 | Index<0 旧命名容器迁移后无 Inspect 复核——外部 docker 干预可致「删除成功但仍在」误标完成 | ✅ v0.22.0 闭环（T-10）：迁移后 Inspect 复核，失败记 Unknown+下轮重试；外部干预容错单测 7 用例。**审计口径修正**：DockerRuntime.List 本就即时 exec，「90s 固化快照」实为 Absent 保留窗口（DefaultRemovedRetention），验收②语义上已被既有实现满足 | 复核为「删后即查」，极端 TOCTOU 窗口（查后 1ms 内被外部拉起）理论存在——下轮调谐会再次迁移收敛 |
| CLD-01/02 | 发布终态写点未接权威状态机断言；digest 复核失败不写事件不计失败预算 | ✅ v0.22.0 闭环（T-06）：setTerminal 统一漏斗 + 回滚完成/中止 + autopause 共 4 类写点接 assertReleaseTransition（违例拒落库+观测上报）；digest 失败写 head 事件并与批次失败同源计 failureBudget | 状态机表仅供测试对拍与断言（分散写点不引表跳转）；跨终态收敛（succeeded/canceled→failed 回滚中止豁免）属有意设计 |
| CLD-04 | canary 独占（同模型单在途）guard 语义只在代码，文档未登记 | ✅ v0.22.0 闭环（T-09）：API-SPEC §7.11 登记 guard create-if-absent CAS 语义 + 409 响应体 releaseID 回指；§4 状态码表 404 行补充 | 无 |
| CLD-06 | release 子资源端点归属校验不统一（7 端点缺跨模型 404）——可跨模型 id 枚举 | ✅ v0.22.0 闭环（T-09）：ownedRelease helper 统一接入 7 端点，10 端点行为收敛一致（跨模型引用 404 同语义同响应体） | 既有 e2e 辅助硬编码 mnist URL 依赖旧缺陷行为——已配套修复（RELEASE-NOTES §二.1），属审计要求的正确行为收敛 |
| SEC-04 | CRL 缺失静默放行（fail-open）；OCSP nextUpdate 过期不校验——吊销链不可配收紧 | ✅ v0.22.0 闭环（T-11）：`EDGEFLOW_CLOUDCORE_CRL_STRICT=on` 缺失即拒（fail-closed）+ `EDGEFLOW_CLOUDCORE_OCSP_FRESH=on` 过期拒绝；均默认 off（v0.21.0 行为）；SECURITY.md 部署建议已补 | fail-closed 要求吊销链运维到位（keadm cert revoke 生成 crl.pem）后再开启，否则产线握手误拒 |

## 23. v0.23.0 开发轮闭环登记（2026-08-28，P2 缺陷修复包：审计 T-12~T-20 收官——观测面 + 协议健壮性 + 契约统一 + 安全卫生 + 已知限制归档）

> 提交：见 git log。契约变更：**无新增端点**（HTTP 端点保持 42）；响应形态变化（summary 恒现、releases 全局列表信封化）与错误文案细分见 docs/API-COMPATIBILITY.md v0.23.0 小节。全部新开关/告警默认关闭或等于现行为（逐项见 RELEASE-NOTES-v0230 §行为变化）。守卫测试联动绿。
>
> 本节归档三类内容：①台账 71 条中裁决「登记不修」的条目（含理由与重估触发条件，接受为已知限制）；②CHN-08 文档标注裁决；③域外残余票。
> 处置依据（审计台账原文，ledger-consolidated.md CHN-08）：「服务端 readLoop 无读超时……对称加 SetReadDeadline 或部署文档标注 LB 空闲超时要求」——两个修复方向均被审计认可。本轮**选择文档标注路线**，裁决理由：①逐读 SetReadDeadline 会在慢链路（弱网/跨公网高 RTT）引入新的误杀面——读超时阈值需要与心跳周期、网络抖动、GC 暂停联合调参，在 90s 心跳体系下缺真实部署数据支撑，贸然收紧风险大于收益；②CHN-08 的实际危害（半开连接清理窗口跨公网偏大）仅在「经 LB/代理暴露 CloudHub」的部署形态兑现，直连形态 90s monitor 已兜底——按最小变更原则先以部署约束拦截危害面；③已有 v0.22.0 CHN-07（ack 前置）降低了对服务端清理窗口的敏感度（边缘立即可心跳重拨），危害进一步收敛。

| 编号 | 问题面 | 处置说明 | 残余与建议 |
|---|---|---|---|
| CHN-08 | 服务端 readLoop 无逐读超时——半开连接（跨公网/NAT 表项老化）清理窗口偏大，云端连接与内存占用短时虚高 | 📋 **v0.23.0 文档标注裁决**：docs/DEPLOYMENT.md §2.5 新增「经 LB/代理暴露 CloudHub 时的读超时要求」——LB/代理空闲/读超时必须 > 云端心跳超时（默认 90s，推荐 150s≈1.5×），禁用逐请求代理与 Upgrade 头改写；危害面限定在 LB/代理部署形态 | 服务端对称 SetReadDeadline（与心跳 3×deadline 对齐）留待后续轮：需先在真实跨公网链路采集 RTT/抖动基线再定阈值，避免慢链路误杀；直连部署不受影响（90s monitor 兑现清理） |

### 23.1 审计台账「登记不修」决策归档（12 条）

| 编号 | 问题面 | 不修理由（裁决口径） | 触发重估的条件 |
|---|---|---|---|
| CLD-05 | 多副本 DeleteModel + watch 回流可产生孤儿 releaseHead | 单一副本部署不受影响；多副本为远期形态 | 启用多副本 cloudcore 前 |
| CLD-09 | RequestRollback 包级 nowMs 直通时钟 | 行为无外部影响（仅测试时钟注入脱钩） | 时钟敏感逻辑重构时一并收敛 |
| CLD-14 | RequestRollback 双实现平行 | 结构平行但守卫文案一致，测试已钉行为；重构收益低 | 触碰 rollback 主路径时合并 |
| CLD-15 | 护栏常量三种放置风格 | 纯风格债；收敛方向 modelrepo/types.go | 下一轮触碰对应文件时顺手迁移 |
| CHN-09 | rescan 单向对账依赖 watch | watch 断开 + reloadAll 连续失败为多故障叠加；重连自愈路径存在 | 出现真实对账漂移事故 |
| CHN-10 | Register 失败后心跳被静默忽略 | 重连即自愈（重连走完整注册链）；失败集合重试属优化项 | 注册风暴期间出现指令丢失 |
| CHN-12 | pending 同 ID 替换仅 Warn | Ack 串扰窗口需同 CorrelationID 并发同 ID 请求（正常客户端不发生） | 引入高并发同 ID 请求模式前 |
| CHN-18 | 非法压缩帧二次解压 | 单帧成本有界 | 压缩面性能预算收紧时 |
| CHN-21 | ctx 取消错误缺 attempt 信息 | 审计原文判可选项，维持现状可接受 | 错误排查实践需要时 |
| CHN-22 | 漂移重建双 inspect 开销 | 规模化性能储备项；单轮开销有界 | 节点规模 >1k 或 inspect P95 超预算 |
| CHN-24 | mtime 同秒 + 同 size 跳过重试 | 1s 粒度多数场景成立；SIGHUP 强制通道已存在 | 秒内同尺寸更新误跳过复现 |
| SEC-03 附 | join 产物目录保持 0755（台账修复方向含 0700） | token 载体 edgecore.env 自身 0600 已兜底；目录 0700 改变既有用户可见性（非 opt-in） | 下一轮统一评估产物目录权限 |

### 23.2 OPC-UA 登记不修（2 条）

| 编号 | 问题面 | 不修理由 | 触发重估的条件 |
|---|---|---|---|
| PRT-11 | Close 后无 closed 哨兵（复用报底层 net 错误） | 需全路径插入哨兵并处理泵竞态，波及面大；台账定级建议项；Close 后复用必然收到可辨识 net 错误，无静默错误 | 复用语义需要明确错误类型时 |
| PRT-12 | PubAck 与泵投递 ack 窗口 | 锁内快照+立即 sendPublish 会在持锁状态做网络写（违反 sendMu 串行化设计），死锁风险 > 收益；服务端已有去重缓解 | 协议面重构时统一处理 |

### 23.3 域外残余票（本轮登记，下轮/主线窗口处理）

| 票 | 内容 | 来源 | 建议归属 |
|---|---|---|---|
| R-1 | tests/contract/api_contract_test.go:31-38 静态路由断言注释口径过期（称仅 main.go 注册，实际分散至 model_api.go 等）；守卫文件本轮禁触 | C 路 CLD-16 | 下一轮守卫维护窗口 |
| R-2 | cloud/pkg/cloudhub/server.go 尚余 Ack 之外个别日志点可复查消毒覆盖（本轮四个主锚点已覆盖） | 主线 SEC-06 收尾 | 触碰对应日志点时顺手补 |
| R-3 | SEC-05 白名单精确匹配无子域通配；如需 grafana.*.example.com 类通配需扩展（谨慎：通配重开注入面） | D 路缺口 4 | 出现真实通配需求时 |
| R-4 | keadm batch 的 --token-file 为「预读后按值透传」语义（与单节点 join 的 join 内读文件结果一致，测试钉死） | D 路缺口 7 | 语义差异造成实际问题时 |

## 24. v0.24.0 开发轮闭环登记（2026-08-29，MQTT 功能轮：协议栈 + 订阅型 Mapper + e2e；含 §23 残余 F-4 清理）

### 24.1 本轮修复闭环（§23 残余票 F-4）

| 票 | 处置 | 落点 |
|---|---|---|
| R-1 | 契约守卫静态路由断言文档注释口径修正（漂移实际在 27-31 行；断言零改动，守卫语义不变） | tests/contract/api_contract_test.go |
| SEC-03 附 | keadm join/batch 产物目录权限 opt-in：`EDGEFLOW_JOIN_DIR_MODE`（8 进制，≤0o777，非法值 fail-fast），默认 0o755 行为不变；`resolveJoinDirMode()` 单点实现 + 11 例测试（含 umask 钉扎） | cmd/keadm/join.go、batch.go |
| OPCUA-GUIDE 漂移 | 与 v0.23.0 行为变化逐点核查，无漂移（3 处疑似点逐一排除） | — |

### 24.2 实现注记（非缺陷，下轮收敛）

| 票 | 内容 | 现状合理性 | 收敛路径 |
|---|---|---|---|
| R-6 | `pkg/mqtt` codec 函数（encodePacket/decodePacket/validateTopicFilter）未导出，`pkg/mqttsim` 内建语义对齐的最小本地 codec（~330 行） | 并行开发期文件域禁触的兌底裁决；重复被封装在 sim 包内部，不污染公共 API，已 -race 验证 | 导出 M1 codec（EncodePacket/DecodePacket/ValidateTopicFilter）+ mqttsim 切换调用，收回重复；下轮或 v0.25.0 |

### 24.3 域外残余票（本轮登记，下轮处理）

| 票 | 内容 | 来源 | 建议归属 |
|---|---|---|---|
| R-5 | 契约守卫 TestContractRoutesNoExtraRoutesRegistered（~444 行）文档注释仍写「遍历 main.go」，实测同扫两文件；本轮守卫行域禁触未动 | O 路 O-R2 | 下一轮守卫维护窗口 |

### 24.4 MQTT 轮新增能力边界说明

- MQTT client **无自动重连**：断开后返回 ErrClientClosed，重连由上层 Mapper 监管循环负责（设计裁决，与 opcua mapper 锁外重连同构）。直接使用 `pkg/mqtt.Client` 的调用方需自行处理重连。
- mqttsim 定位为**测试 broker**：出站队列容量 32、满则丢弃+计数，不承诺生产级投递保证；生产部署使用真实 broker（EMQX/Mosquitto 等）。
- mapper QoS 仅支持 QoS0/QoS1（QoS2 在 client 层明确拒绝）；CleanSession 恒为 true。

## 25. v0.25.0 开发轮闭环登记（2026-08-30，MQTT 硬化轮：R-5/R-6 残余闭环 + TLS 加密传输全栈）

### 25.1 残余票闭环

| 票 | 处置 | 落点 |
|---|---|---|
| R-5 | 契约守卫口径修正：反向断言与背景注释改为「同扫 main.go 与 model_api.go」实际口径；错误信息带来源文件名（registeredRoute 增 file 字段）；断言语义零变化，42 端点守卫全绿 | tests/contract/api_contract_test.go |
| R-6 | codec 收敛：pkg/mqtt 导出 EncodePacket/DecodePacket/ValidateTopicFilter 薄包装（小写实现与既有测试零动）；mqttsim 删除本地 codec（747→447 行，净 -300）；新增 v0250_export_test.go 4 parity 用例 | pkg/mqtt/packet.go、pkg/mqttsim/sim.go |

### 25.2 R-6 关键裁决：坏客户端 SUBSCRIBE 宽容通道

冻结测试 TestSubscribeSubackEchoAndInvalidFilter 需将非法 filter（"a/#/b"）放上电线并期望 broker 逐 filter 回 SUBACK 0x80 且连接保持；pkg/mqtt 客户端级编解码器对非法 filter 双侧拒绝（对真实客户端是正确行为）。处置：mqttsim 的 encodePacket/decodePacket 垫片对 SUBSCRIBE 分流出最小宽容路径（encodePermissiveSubscribe/decodePermissiveSubscribe + varint 小助手），非 SUBSCRIBE 类型经 MultiReader 原样走 M1 严格管线，严格性零损失。

### 25.3 本轮登记项与能力边界

- **mqttsim TLS 为测试定位**：NewBrokerTLS 单自签证书（IsCA 便于直接充当测试 RootCA，SAN 含 127.0.0.1/localhost），无认证体系；生产 broker（EMQX/Mosquitto 等）TLS 走真实 CA。
- **EDGEFLOW_MQTT_TLS_INSECURE 仅限开发/测试**：开启时打 WARN 日志；生产禁用。
- **client mTLS（双向认证）未含**：本轮仅单向 TLS（客户端校验服务端）；登记 ROADMAP §20 下轮候选。
- mapper TLS 失败语义：CA 文件读失败/非 PEM → connect() fail-fast 报错（不进入无限重试）；ServerName 由 client 层从 addr host 自动回填（IP 直连需证书含 IP SAN，测试证书已含 127.0.0.1）。

## 26. v0.26.0 开发轮闭环登记（2026-08-31，MQTT QoS2 ＋ client mTLS ＋ mapper 配置文件化）

### 26.1 本轮登记项与能力边界

- **QoS2 为进程内 exactly-once**：client 与 sim broker 的 QoS2 状态机保证握手完整性（PUBLISH→PUBREC→PUBREL→PUBCOMP）与 handler 恰好一次分发；不做跨进程/重启级去重持久化（无 in-flight 落盘），进程崩溃后仍可能重复，与 MQTT 3.1.1 QoS2 语义一致（网络层恰好一次，端到端仍需业务幂等）。
- **EnableQoS2 默认关闭**：`Options.EnableQoS2=false`（默认）时 client 行为与 v0.24.0/v0.25.0 逐字一致（Publish qos=2 仍拒绝）；opt-in 后开启四次握手。门控与 v0250 TLS opt-in 模式同构。
- **mqttsim mTLS 为测试定位**：NewBrokerTLS 接受任意 tls.Config（本轮零新 API）；RequireAndVerifyClientCert 严格模式仅存在于测试装配。生产 broker mTLS 走真实 CA 体系。
- **mapper 证书对必须成对**：EDGEFLOW_MQTT_TLS_CERT 与 _TLS_KEY 只给其一 → connect() fail-fast；文件不存在/坏 PEM 同样 fail-fast（不进入重试循环）。
- **测试 CA 的 EKU 嵌套陷阱（排障遗产）**：Go x509 要求 CA 的 ExtKeyUsage 覆盖叶子用途；CA 带 `ExtKeyUsage:[ServerAuth]` 会使 ClientAuth 叶子链校验失败（`tls: bad certificate`），且该 TLS alert 会被 MQTT 解码层表现为 `malformed packet: fixed header`。自签测试 CA 不应设 EKU 约束。本轮排障记录见 RELEASE-NOTES-v0260 §2。
- **OPC-UA Basic256Sha256 未含**：按 v0.26.0 范围裁定移交下一轮（ROADMAP §21）。

### 26.2 复核轮修复与遗留说明（2026-08-31 独立复核）

- **P1 已修（连接级隔离）**：sim 端 pendingQoS2 初版以 PacketID 为 broker 全局键，多客户端并发同 id QoS2 会互相覆盖/误释放。已改为 `map[*simClient]map[uint16]*mqtt.Publish` 按连接隔离，`unregister` 时清理该连接的全部 parked 交换；新增回归测试 TestV0260SimQoS2PerConnIsolation（两连接同 PacketID 并发，各恰好投递一次）。
- **inbound QoS2 不跨连接（P2，文档级）**：client 与 sim 的 QoS2 暂存均为内存态，连接断开即丢失；重连后 broker 的迟到 PUBREL 会被当作未匹配报文丢弃（不再投递）。与进程内 exactly-once 边界（§26.1 第一条）一致，生产跨重连语义需 broker 会话恢复（MQTT 5.0 / 持久会话）。
- **出站 QoS2 超时重试可能重复消费（P2，文档级）**：Publish QoS2 在等 PUBREC 超时返回错误后，broker 侧仍可能完成后续握手并投递；调用方重试会造成下游重复。exactly-once 的「恰好一次」以握手完成为准，端到端去重仍需业务幂等或消息级去重 id（未暴露给上层）。
- **ackTimeout 包级 var（P2）**：为测试可缩超时而由 const 改 var；并行测试若并发改写会互相污染，当前测试串行使用无影响，后续可注入化。

### 27 QoS2 持久化边界（v0.27.0）

- **3.1.1 无跨连接会话（协议边界）**：client 端下行 parked 记录（Kind 'i'）保存的是「等 broker PUBREL」的凭据；若 broker 重启或永不重发 PUBREL，该交换无法完成。记录保留在盘、不影响新交换（同 PacketID 新记录会覆盖旧文件）；只有升级到 MQTT 5.0 会话语义（Session Expiry）才能根治。上行记录可完整自愈（Resume 重发）。
- **broker 重启恢复依赖 release leg**：sim broker 重启后把 parked 记录装进孤儿表，但不会自动投递（重启后订阅关系已失，自动 fanout 反而违背订阅语义）；恢复由「发送方重连后以同 PacketID 发 PUBREL」驱动。这是测试 broker 的合理边界，生产 broker 需持久会话支持。
- **记录目录信任域**：qos2-*.json 为本机明文 JSON（0600），与 v0.26.0 内存态同级信任域；未做加密与多进程并发锁，两端（client/broker）记录不应混放同一目录（混放时 broker 会跳过并保留 client 侧记录，功能不受损但目录语义变脏）。
- **持久化写失败语义**：client 上行 Publish 路径 fail-fast（宁可报错不可丢凭据）；下行 park 路径软失败（协议应答 PUBREC 不依赖磁盘健康，缺记录只损失恢复能力不损失正确性）。
- **Resume 非并发安全约束**：Resume() 必须在业务 Publish 开始前（或串行化）调用；与并发 Publish 同跑可能造成 PacketID 混用（回放沿用记录中的原 PacketID，与 nextID 计数器无协同）。上层 mapper 当前未自动调 Resume，属 opt-in API。
- **Resume 回放 PacketID 与 nextID 的碰撞窗口（缓解措施落地）**：回放沿用记录中的原 PacketID，而新连接的 atomic 计数器从 1 重新计数——若重连后先跑业务 Publish 再调 Resume（或并发），新分配 id 与回放 id 可能撞车（MQTT 3.1.1 中同连接同 id 并发在途属于协议违例，broker 行为未定义）。缓解：v0.27.0 在 Resume() 成功回放 N 条后，把 packetID 计数器快进到已见最大回放 id（atomic Cas 以单调推进），消除「回放后撞车」方向；「先 Publish 后 Resume」顺序仍可能撞（KNOWN-ISSUES 上条已约束 Resume 时序）。根治需 MQTT 5.0 会话语义（见 docs/MQTT5-EVALUATION.md §6）。


## 28. v0.28.0 开发轮处置登记（2026-09-01，OPC-UA 安全策略框架：Basic256Sha256 分段第一段）

- **SHA-1 使用边界**: Basic256Sha256 策略由 OPC-UA Part 6/7 规范强制绑定 SHA-1（指纹/签名/HMAC/密钥派生）。实现仅限规范要求路径，`//nolint:gosec` 逐处标注；协议外的现代场景应避免 SHA-1。登记为「规范强制的已知弱算法」，后续若 OPC-UA 生态全面转向 Sha256 系策略（Basic256Sha256 之外的 #Aes256_Sha256_RsaPss 等），在 v0.30.0+ 增补新策略而非替换本实现。
- **B256 通道未达端到端可用（分段边界，非缺陷）**: v0.28.0 仅交付策略框架；OPN 体加密（RequestHeader/ClientNonce/MessageSecurityMode 扩展）留 v0.28.1，MSG 对称覆盖留 v0.29.0。sim 对非 None 策略显式回 ERR Bad_SecurityPolicyRejected（0x80550000），不静默降级。
- **pin 校验语义**: B256 响应校验要求服务端证书与 OpenSecureChannelOptions.ServerCert 逐字节一致（严格 pin）。真实服务器证书轮换时需同步更新 pin——证书热加载/轮换机制待排（与 v0.27 轮凭证轮换自动化缺口同源）。
- **deriveKeys 与规范差异**: 密钥派生按 Part 6 §6.7.5.2 链式 SHA-1 近似实现（标准库无 HKDF）；段长/截断/补零按 Basic256Sha256 参数。与真实服务器互通前（v0.28.1 端到端）需按 OPC 基金会互操作样例向量复核一次。
- **SecurityPolicyBasic256（SHA1 系）未实现**: 与 Basic256Sha256 是不同策略，显式拒绝；如需支持在缺口清单登记。
