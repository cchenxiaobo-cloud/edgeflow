# EdgeFlow v0.4.0 Release Notes

> 状态：✅ 已定稿（2026-08-24，v0.4.0 开发轮）。
> 版本决策：新功能（云端持久化）→ minor（v0.4.0）。
> 核心主题：**云端内存态存储 → 嵌入式 etcd（方案③，写穿 write-through）**——注册元数据与设备 Desired 跨重启持久化，Pod 状态与上报属性短暂清空自愈。
> 配套：docs/ARCHITECTURE.md（决策 R13）、docs/API-SPEC.md §8、docs/DEPLOYMENT.md §10（配置/备份恢复）、docs/KNOWN-ISSUES.md §4、docs/ROADMAP.md。

---

## 一、主题概述

1. **云端状态持久化（本版本核心价值）**：以 `go.etcd.io/etcd/v3` embed（嵌入式单成员 etcd）替换云端三存储（registry / podstatus / devicestatus）的纯内存态，**注册台账与设备期望态（Desired）跨重启保留**；Pod 状态与上报属性（reported/properties）仍为内存态，重启后**短暂清空（≤1 上报周期，边缘重连自愈，非永久丢失）**——API-SPEC §9 首条已知限制"重启清空"随之闭环为分级持久化语义。
2. **写穿架构**：所有需要持久化的写（Register / SetDesired / GC 删除）**先 etcd 成功、再更新内存缓存**，"写成功 = 已持久化"；读路径全部走内存缓存（HTTP 热路径毫秒级响应，永不读 etcd）。心跳/Status/Pod/上报等瞬态不落盘（无写放大、无陈旧脏状态）。
3. **运维契约升级（与代码同 PR 的硬性变更）**：Helm Chart 新增 PVC（默认启用）+ 资源上调（requests 256Mi / limits 1Gi）+ etcd env 透传；**replicaCount 必须 = 1**（多副本各自 embed 会脑裂，显式禁止）；备份恢复走 `etcdutl snapshot`（文件拷贝 ≠ 有效备份）。

## 二、核心特性明细

| 特性 | 说明 |
|------|------|
| 嵌入式 etcd | single-member embed（etcd v3.5.33，Go 1.26.2 验证通过）；客户端只绑 `127.0.0.1:12379`/`12380`（避开标准 2379/2380，安全底线不上非回环） |
| 写穿持久化范围 | ✅ 落盘：节点注册元数据（NodeRecord：nodeID/nodeName/arch/os/edgecoreVersion/cpu/memory/ip/registeredAt）、设备 Desired（DeviceShadowRecord）；❌ 不落盘：心跳、Status/LastHeartbeatAt、Pod 状态全表、设备 Properties/LastReportedAt、Offline 标记（启动时按启动时刻播种 + 保留期 GC） |
| 键空间 | `/edgeflow/_meta/schemaVersion`、`/edgeflow/registry/nodes/<nodeID>`、`/edgeflow/devicestatus/<nodeID>/<ns>/<device>`；`/edgeflow/podstatus/...` 预留不启用；值全 JSON 小驼峰，etcdctl 可直接人读 |
| 内存读缓存 | 读路径（Get/List/ListByNode/ListAll/Count/ToEdgeNode）永不读 etcd；启动时前缀 Range 全量 Load → Seed 灌缓存（Status=Unknown、LastHeartbeatAt=0，等心跳翻新） |
| GC 级联 | 运行时惰性 GC（注册表写路径触发）+ 启动清理（Seed 播种 offlineSince 后扫一遍，超保留期删除）+ 定期清理循环 `CleanupLoop`（默认 1h，失败告警不阻断）；**节点删除级联 DeleteRange 其设备子树** |
| 坏库降级 | embed 启动失败：默认**降级纯内存 + 启动期大告警**（数据未持久化）；`EDGEFLOW_CLOUDCORE_ETCD_STRICT=1` 改为**拒绝启动**（fail-fast）。坏 WAL 实测为 raft 恢复阶段 panic（非 error），装配层 `defer recover()` 兜底后按同一 STRICT 语义处理 |
| 优雅关停 | 关停顺序：HTTP → hub → nodecontroller → ledger → **etcd 最后**（同步写穿无待刷缓冲，关停即数据完整） |

## 三、配置表（环境变量，`EDGEFLOW_CLOUDCORE_*` 前缀）

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `EDGEFLOW_CLOUDCORE_ETCD_ENABLED` | `true` | 总开关；`false` = 完全退回 v0.3.x 纯内存（不建目录、不占端口、不写盘） |
| `EDGEFLOW_CLOUDCORE_ETCD_DATA_DIR` | `data/etcd` | embed 数据目录（相对工作目录，自动创建；容器内为 `/data/etcd`） |
| `EDGEFLOW_CLOUDCORE_ETCD_CLIENT_URL` | `http://127.0.0.1:12379` | 客户端监听（只绑回环；非回环 URL 拒绝启动） |
| `EDGEFLOW_CLOUDCORE_ETCD_PEER_URL` | `http://127.0.0.1:12380` | peer 监听（单成员，仅 embed 内部） |
| `EDGEFLOW_CLOUDCORE_ETCD_QUOTA_BACKEND_BYTES` | `268435456`（256MiB） | 后端配额（三类数据量级为 MB，256MiB 充足；**显式调低拒绝 etcd 默认 2GB**） |
| `EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_MODE` | `periodic` | 自动压缩模式（`periodic` / `revision`） |
| `EDGEFLOW_CLOUDCORE_ETCD_AUTO_COMPACTION_RETENTION` | `1h` | 压缩保留期（periodic 模式为时长；支持 `1h` 或秒数） |
| `EDGEFLOW_CLOUDCORE_ETCD_STRICT` | 空（off） | `1` = embed 启动失败即拒绝启动（fail-fast）；默认关 = 降级内存 + 告警 |
| `EDGEFLOW_CLOUDCORE_NODE_RETENTION` | `24h` | 节点保留期（喂注册表 OfflineTTL + etcd GC 阈值，内存/etcd 同一口径） |

配置解析 fail-fast（非法值报错退出，风格对齐 `nodecontroller.DurationsFromEnv`）。

## 四、体积与资源（实测）

| 指标 | 实测 | 说明 |
|------|------|------|
| cloudcore 单二进制 | **9.6-10MB**（darwin/arm64、linux/amd64、linux/arm64 三平台，CGO-free 静态链接；strip 后 6.8MB） | 嵌入式 etcd 增量约 **+5~8MB**，远低于风险审查预估 15-40MB；**v0.4.0 起 cloudcore 体积增大属预期**，edgecore/keadm 不受影响 |
| 运行期内存 | **RSS 31-34MB**（3s 稳态；etcd 单成员） | 低于预估 50-150MB；cloudcore 整体 +31MB 后 **Helm 资源上调为 requests 256Mi / limits 1Gi**（原 128Mi/512Mi 接近上限） |
| 启动耗时 | 冷启动（新目录）~253ms；热启动（WAL 重放）~1.9s | 风险审查预估 1-3s，10s 探针超时富余充足 |
| 优雅关停 | ~1s | 同步写穿无待刷缓冲，Close 即数据完整 |

## 五、升级注意事项（v0.3.0 → v0.4.0）

1. **默认启用**：`EDGEFLOW_CLOUDCORE_ETCD_ENABLED` 默认 `true`——升级后首次启动自动创建数据目录（默认 `data/etcd`）并重建空库；旧节点在保留期内（24h）重新注册。**无迁移脚本需求**（v0.3.0 无落盘数据）。
2. **Helm 部署必须带 PVC**：v0.4.0 Chart 默认创建 PVC（`cloudcore.etcd.persistence.enabled=true`，容量默认 1Gi，storageClass 可配）挂载 `/data`（etcd 数据在 `/data/etcd`）。**空 emptyDir 会使"持久化"名存实亡**（Pod 重建即丢）；存量部署升级需手工 `kubectl apply` PVC 或在 values 中确认 storageClass。
3. **replicaCount 必须 = 1**：嵌入式 etcd 为单成员；多副本各自 embed + 共享 PVC = **脑裂**，显式禁止（Chart 注释已写明）。
4. **备份恢复**：在线 `etcdutl snapshot save --endpoints=http://127.0.0.1:12379` 或离线冷备（停进程后 `cp -a data/etcd`）；**文件拷贝 ≠ 有效备份**（WAL 与 snapshot 不一致即损坏）。恢复必须 `etcdutl snapshot restore --data-dir=<全新目录>`。详见 docs/DEPLOYMENT.md §10。
5. **STOP 语义变化**：升级前已注册节点在重启后立即可见（Unknown 态，等心跳翻新 Ready），不再需要重新注册才可见；Pod/设备上报数据重启后短暂为空，≤1 上报周期自愈——监控告警阈值需容忍该窗口。

## 六、已知限制（v0.4.0 新增登记，详见 docs/KNOWN-ISSUES.md §4）

- **坏 WAL 崩溃语义**：`data/etcd/member/wal/` 损坏时 etcd 在 raft 恢复阶段 **panic**（v3.5.33 实测，非 error 返回）；装配层已 `defer recover()` 兜底 → 默认降级纯内存+告警 / `STRICT=1` 拒绝启动。运维上坏库的正确处理是清数据目录或从备份恢复（边缘重连重新注册收敛为兜底路径）。
- Pod 状态与上报属性重启后**短暂清空**（≤1 上报周期）；设备 Desired 不受影响。
- `Delete`/`Get`/`List*` 读方法未加 error 返回（Go 多值绑定兼容性约束）；Delete 的 etcd 失败以日志 + 返回 false 表达。
- nodeID 字符约束（`^[A-Za-z0-9._-]+$`）为新增硬约束，含 `/` 拒绝写入。
- 未实现（v0.5+）：外部 etcd（`EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS`）、运行中热切换、etcd Lease 心跳管理、备份主程序内置（脚本+文档）。

## 七、验证摘要（v0.4.0 开发轮实测）

| 项目 | 结果 |
|------|------|
| etcdstore 基础层单测 | ✅ 7/7（CRUD / 重启持久化 / kill -9 崩溃恢复 / 坏 WAL 容错 / 坏目录重建 / 配置 fail-fast） |
| registry 后端 | ✅ 既有 24 测零改动 + 新增 7 测全绿（`-race` 干净） |
| podstatus / devicestatus 后端 | ✅ 既有 18 测零改动 + 新增 13 测全绿（`-race` 干净） |
| 全仓编译 | ✅ `go build ./...` 通过；三平台交叉编译通过（9.6-10MB） |
| 集成装配 + E2E 重启验证 | （集成轮 subagent_07 交付后回填） |
| 制品/合规 | 发布轮（subagent_09）回填：二进制 × 三平台、Chart 0.4.0（含 PVC）、SBOM（组件数 33 → 100+，trivy 重扫）、checksums |

## 八、后续里程碑（Roadmap）

- **v0.5+**：embed → 外部 etcd 配置级切换（`EDGEFLOW_CLOUDCORE_ETCD_ENDPOINTS` + 可选 TLS，业务代码零改动；数据经 `etcdutl snapshot` 迁移）；对接 K8s apiserver（决策 R1 既定方向，store 接口即替换面）。
- **既有 backlog** 不变（见 docs/ROADMAP.md、docs/PROGRESS.md §5）。