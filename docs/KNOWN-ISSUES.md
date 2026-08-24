# EdgeFlow 已知限制与问题台账（KNOWN-ISSUES）

> 最后更新：2026-08-24（v0.5.0 开发轮）
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
- 2026-08-24（v0.4.0 开发轮）：新增 §4 登记（嵌入式 etcd 坏库/坏 WAL 场景与降级行为 + POD/reported 短暂清空语义）；API-SPEC §8 首条"重启清空"已随分级持久化闭环，不再迁移本表。
- 2026-08-24（v0.5.0 开发轮）：新增 §5 登记三条（L1 无鉴权参数透传 / L5 多副本 SetDesired 整记录覆盖 / L7 GC 删除失败不重试——后两条为 v0.4.0 既有缺陷在多副本/外部集群场景下的新发现，v0.5.0 按单写者形态登记不修）；编号沿用全局序列（⑤⑥⑦），对应设计定稿限制清单 L1/L5/L7。

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
| ③ | 云端 Pod 状态 / 设备 reported（properties/LastReportedAt） | v0.4.0 起**整表不落盘**（写穿范围仅注册元数据 + Desired）：cloudcore 重启后 pods 列表短时为空、设备 properties 为空 map `{}` | 重启窗口内查询 API 显示"暂时缺失"；≤1 上报周期（默认 30s）边缘重连后自愈，**非永久丢失**；监控/告警阈值需容忍该窗口 | API-SPEC §8 已登记为"短暂清空"语义；后续版本若需持久化，键空间 `/edgeflow/podstatus/...` 已预留，直接启用即 |
| ④ | `cloud/pkg/etcdstore` embed.Close() 幂等性 | v3.5.33 的 `close(e.errc)` 无 closeOnce 保护，二次 Close 会 `panic: close of closed channel` | 已在本层用 sync.Once 整体幂等化解（`Close()` 可安全重复调用）；集成层勿绕过 `EmbeddedEtcd` 直接持有 embed.Etcd | 已闭环（包内化解）；登记备忘 |

---

## 5. v0.5.0 开发轮登记（2026-08-24，外部 etcd 模式，对应设计定稿 L1/L5/L7）

> 背景：v0.5.0 新增外部 etcd 模式（`EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` 非空即直连共享集群，ARCHITECTURE 决策 R14）。以下 ⑤⑥ 是外部/多副本场景暴露的**既有设计边界**——v0.5.0 受支持形态为**单写者**（replicaCount=1，Chart 模板 fail 守卫兜底），因此 ⑤⑥ 在受支持形态下不触发，但仍登记、不修复（改动涉及键空间/GC/写穿联动，超配置级切换范围）；⑦ 为 v0.4.0 既有缺口在外部集群故障窗口的放大项。

| # | 位置 | 限制 | 影响 | 计划 |
|---|------|------|------|------|
| ⑤ | 外部模式连接层（clientv3 配置）；L1 无鉴权参数透传 | v0.5.0 **不实现任何 etcd 鉴权参数**（无 username/password/证书 CN→角色映射 env）；外部集群开启鉴权且未为 cloudcore 授权 → 启动探活返回 `PermissionDenied` → fail-fast 拒绝启动，错误文案引导"检查 etcd 鉴权 / 在 etcd 侧为 `/edgeflow/` 授权"（见 DEPLOYMENT §10.7.6） | 鉴权只能在 etcd 侧配置（§10.7.4 最小权限角色 readwrite `/edgeflow/` / mTLS `--client-cert-auth`）；安全边界 = TLS/mTLS + 网络隔离（非回环+明文默认拒绝启动，逃生门 `EDGEFLOW_CLOUDCORE_ETCD_ALLOW_INSECURE`）；明文+非回环未配逃生门时连启动都不可行，属护栏而非缺口 | v0.6+ 候选：`EDGEFLOW_CLOUDCORE_ETCD_USERNAME/PASSWORD`、CN→角色映射 |
| ⑥ | `cloud/pkg/devicestatus/etcd_store.go` `SetDesired`（L5 跨副本整记录覆盖） | `SetDesired` 是"读**本副本**内存合并 → 整记录 Put"（读-改-写）；多副本 active-active 时，副本 B 的内存基准可能旧于 etcd（副本 A 刚写的属性 P1 不在 B 内存），B 对同一设备写 P2 → 整记录覆盖 → **P1 丢失**。v0.4.0 单副本语义正确（自身写穿保证内存最新） | v0.5.0 单写者形态（replicaCount=1）下不触发；多副本部署已被 Chart fail 守卫与文档禁止（心跳/判活内存态是更根本的原因，见 ARCHITECTURE R14）。文档级缓解：同一设备的连续指令尽量落同一副本（粘性会话） | v0.6+ 候选：SetDesired 改 etcd 条件写（带 revision/Compare）或按 property 拆键；未修（零改动约束） |
| ⑦ | `cloud/pkg/registry/node_registry.go` `requeueGCEvent`（L7 死代码） | GC 删除失败**只告警、不重入队**：`requeueGCEvent` 存在但**从未被调用**（死代码）；注释声称"下一轮 CleanupLoop 重扫会再次产生事件"**不成立**——节点已从内存 map 移除，重扫不会再见到它 | etcd 残留**孤儿节点键**（及设备子树），直到该节点重新注册（再被 GC）或 cloudcore 重启（Load 读回为 Unknown → 保留期后再 GC 一次）。**外部模式放大可见性**：集群故障窗口内的失败删除会留孤儿；单机 embed 故障窗口短、影响小（v0.4.0 既有缺口） | 重启自愈 + 备份兜底；v0.6+ 修复候选：GC 删除失败时调用 `requeueGCEvent`（登记不修） |

---

*表中「位置」为登记时的代码位置，代码演进后以 commit 为准。*
