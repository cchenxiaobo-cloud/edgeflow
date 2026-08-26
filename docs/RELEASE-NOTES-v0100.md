# EdgeFlow v0.10.0 发布说明（云端状态收官 + 发布执行增强 + 平台构建修复）

- **发布日期**：2026-08-26
- **版本基线**：v0.9.0（2026-08-26）→ v0.10.0
- **主题**：③ 设备 reported 写穿持久化（云端状态持久化收官）+ D6 批内并发（两次延后 P2）+ L20b Windows 交叉编译修复
- **兼容性**：API 只增不改；env 只增不改（BATCH_PARALLEL 默认 1=串行，零行为变化）；升级零迁移；边缘节点零动作

## 一、核心能力

### 1. 设备 reported 写穿持久化（③ 收官）

| 项 | 说明 |
|---|---|
| 键值 | `deviceShadowRecord` 扩展 `Properties`/`LastReportedAt`（旧数据零值兼容）——设备影子持久化 DTO 从"Desired-only"升级为"完整快照" |
| Upsert | 写穿完整快照：先算合并基准（保 Desired）→ etcd Put（5s）→ 成功后再提交内存（失败内存不动）；与 SetDesired CAS 路径共存（两路径写同一键完整快照，CAS 读基准合并写回天然保留 reported） |
| applyPut | 整值采用（reported 从"各副本本地瞬态"升级为"全局一致快照——最后写入者"，与 v0.9.0 EtcdPodStore 一致） |
| **行为变化** | cloudcore 重启后**设备属性（properties/LastReportedAt）立即可见**（不再 ≤1 上报周期短暂清空）——v0.4.0 登记的"Pod 状态/设备 reported 不落盘"全部收官 |
| E2E 实测 | 3 节点设备上报 → 杀 cloudcore 重启 → 立即查询 3 条属性（写穿生效） |

### 2. 发布批内并发（D6，v0.8/v0.9 两次延后）

- `EDGEFLOW_CLOUDCORE_RELEASE_BATCH_PARALLEL`（默认 **1=串行**，与 v0.7.0-v0.9.0 逐字节一致；≥1 非法 fail-fast）
- 批内信号量限流并行：`min(parallel, 批大小)`；单节点逻辑抽取 `deployBatchNode`（串行路径为回归锚点）
- failFast 语义：并发下本批在途节点执行完、后续批次中止（终态 head=failed + 未部署 skipped 与串行收敛一致）
- E2E 实测：batchSize=3 + parallel=2 → succeeded

### 3. Windows 交叉编译修复（L20b）

- `lockCRLFile/unlockCRLFile` 平台分文件：`crl_lock_unix.go`（syscall.Flock 原实现）+ `crl_lock_windows.go`（x/sys/windows LockFileEx/UnlockFileEx，零新增依赖）
- `GOOS=windows go build ./pkg/certs/ ./cmd/cloudcore/` 通过（构建断链修复，KNOWN-ISSUES §6 L20b 闭环）

## 二、升级注意

- **零迁移**：只新增 env 与可选行为；不设新 env 与 v0.9.0 逐字节一致（BATCH_PARALLEL 缺省 1）
- **设备属性重启语义变化**：展示持久化快照（最后一次上报）而非空 map；边缘重连后按上报周期翻新
- 回滚 v0.10.0 → v0.9.0：可选 `etcdctl del /edgeflow/devicestatus --prefix` 重建（v0.9.0 可读旧格式——DTO 扩展兼容；Desired 保留）
- 混跑禁令延续（全停再全起）；边缘节点零动作

## 三、验证摘要（实测）

- 全量 `go test -race ./...` 35 包全绿；go vet 干净
- 测试适配：UpsertNoPersist → UpsertPersists（写穿含 reported/保 Desired/新增键）、重启恢复保留 reported、序列化含 properties/lastReportedAt
- E2E 实测：设备属性重启持久化 ✅；batchSize=3 + parallel=2 发布 succeeded ✅
- `GOOS=windows` 交叉编译 ./pkg/certs/ ./cmd/cloudcore/ 通过 ✅；helm lint 0 failed

## 四、文档同步

KNOWN-ISSUES（③/L20b 闭环 + §10 登记）、API-SPEC（状态行/设备属性行为）、DEPLOYMENT（env +1）、README（当前版本/版本历史）、用户手册 v0.10.0（附录 A env +1/附录 E ③ 更新）、方案手册 v1.4.0（修订记录/1.5 节设备属性写穿）

## 五、遗留（非阻断，见 KNOWN-ISSUES §10）

- 测试辅助 syscall.Dup 仅 Unix（Windows 上不跑本仓库测试，测试环境边界）；Windows 制品未加入发布矩阵（12 制品口径不变，可后续加）
- 镜像 digest 级校验、hb 键重建计数、CN→角色映射透传项未做；训练平台/模型评测/A-B 切流（范围外延续）
