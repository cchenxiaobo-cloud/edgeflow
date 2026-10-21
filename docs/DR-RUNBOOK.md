# 灾备恢复预案（SOP）与演练指引

> v0.52.0（spec 0025 裁决 5）：DR-ASSESSMENT 差距 #4「恢复预案/演练流程」承接。
> 配套：cloud/pkg/drstore（治理面快照备份管理面）、scripts/dr-drill.sh（演练编排）。
> 对标：GB/T 20988-2025 第 3 级「恢复预案」要求；本 SOP 覆盖治理面（JSON 快照）
> 与数据面（嵌入式 etcd）两条恢复线。

## 1. 备份对象与 RPO 口径

| 对象 | 载体 | 备份方式 | RPO 口径 |
|---|---|---|---|
| 治理面快照（region/tag/logstore） | `EDGEFLOW_CLOUDCORE_*_FILE` JSON 快照 | drstore 备份管理面（API 或 drill 脚本） | 变更即落盘，RPO ≈ 0（以快照落盘为准） |
| 数据面（节点台账/影子/告警等） | 嵌入式 etcd（data/etcd）或外部 etcd | etcd snapshot（restore 线；外部集群用其原生快照） | 快照间隔决定（建议 ≤1h；外部 etcd 3 节点集群可近似 RPO=0） |

## 2. 演练四步（backup → verify → restore → 健康验证）

前置：演练环境与生产隔离；记录演练起止时间与操作人（审计口径）。

1. **备份**：`scripts/dr-drill.sh backup <base_url> <token>`
   —— API 创建治理面快照备份并自动 sha256 复核；预期输出「创建且复核通过」。
2. **（可选）主动校验**：`scripts/dr-drill.sh verify <base_url> <token> [backup_id]`。
3. **恢复**（演练时先手动改坏一份源快照文件，再走恢复线）：
   - 停止 cloudcore（**必须先停**——drill restore 检测到进程存活会拒绝执行，防写竞争）；
   - `scripts/dr-drill.sh restore <DR_DIR> <源快照路径> <backup_id> region.snapshot.json`；
   - 数据面（如需）：以 etcd snapshot 恢复 data/etcd（外部 etcd 集群按其运维手册）。
4. **健康验证**：重启 cloudcore → `GET /healthz` → 抽查 `/api/v1/regions`、
   `/api/v1/hierarchy/summary` 与备份时点一致 → `GET /api/v1/dr/backups/<id>?verify=true` 复核。

## 3. 轮转与异地副本

- 备份目录保留 `EDGEFLOW_CLOUDCORE_DR_KEEP` 份（默认 7），超限自动删最旧。
- **异地副本为部署侧职责**（DR 差距 #5 剩余项）：至少把 DR_DIR 纳入既有备份
  体系（rsync/对象存储），跨可用区保存；强制项见 docs/CONFIG-BASELINE.md。

## 4. 已知边界

- 本 SOP 未覆盖：多实例联邦下的区域层数据复制（结构性差距，DR-ASSESSMENT 差距
  #1 的 5 级路径，待专项立项）。
- drstore 校验语义 =「副本与 manifest 一致」，不含源与副本的时点一致性（备份即冻结）。
- 演练频率建议：季度一次全流程（backup→restore），月度一次 verify 抽查。
