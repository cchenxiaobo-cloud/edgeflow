# EdgeFlow v0.9.0 发布说明（云端状态持久化补全 + 发布运营性增强）

- **发布日期**：2026-08-26
- **版本基线**：v0.8.0（2026-08-26）→ v0.9.0
- **主题**：③ Pod 状态写穿持久化（KNOWN-ISSUES §3 ③ 闭环）+ R-1 发布前镜像存在性探活（v0.8.0 遗留 P2）
- **兼容性**：API 只增不改（创建发布新增可选探活行为，默认 off）；env 只增不改；升级零迁移；边缘节点零动作

## 一、核心能力

### 1. 云端 Pod 状态写穿持久化（③ 闭环）

| 项 | 说明 |
|---|---|
| 键空间 | `/edgeflow/podstatus/<nodeID>/<ns>/<podName>`（v0.4.0 预留，v0.9.0 启用） |
| 实现 | `EtcdPodStore`（podstatus/etcd_store.go）：Upsert/Delete **先 etcd 后内存**（失败内存不动、"写成功=已持久化"）；读路径全走内存缓存；`Load` 全量重建（坏键跳过）；`LoadAnchored`+`StartWatch` 外部多副本增量同步（只写内存防回写环） |
| 装配 | 三模式：纯内存/downgrade → 内存；embed → EtcdPodStore+Load；外部 → EtcdPodStore+LoadAnchored+StartWatch（装配失败降级内存+告警） |
| **行为变化** | cloudcore 重启后 **pods 列表直接可见**（不再 ≤1 上报周期短暂清空）；API-SPEC §9 语义更新 |
| E2E 实测 | 3 节点上报 → 杀 cloudcore 重启 → 立即查询 3 条 Pod（写穿生效，无需等上报） |

### 2. 发布前镜像存在性探活（R-1）

| 项 | 说明 |
|---|---|
| 新 env | `EDGEFLOW_CLOUDCORE_RELEASE_MIRROR_CHECK`（**off**/warn/fail，默认 off 零行为变化）+ `RELEASE_MIRROR_CHECK_TIMEOUT`（默认 5s）+ `REGISTRY_TOKEN`（私有 registry Bearer，可选） |
| 实现 | registry v2 `HEAD /v2/<repo>/manifests/<tag>`：私有 registry 直连（mirror 可带显式 http/https scheme）；Docker Hub 自动 token 换取（/v2/ 401 → WWW-Authenticate → Bearer）；404/401/超时语义化错误 |
| 语义 | warn = 探活失败仅告警（发布照常 202）；fail = 探活失败阻断（**422**，响应带 mirror 字段）；发布"成功"≠镜像可用（拉取在边缘）——探活是存在性检查 |
| 插入点 | createRelease 步骤 3（版本 active 校验）后 |
| E2E 实测 | fail 模式 + 不可达 registry → 422 阻断（探活真实执行） |

## 二、升级注意

- **零迁移**：只新增 env 与可选行为；不设新 env 与 v0.8.0 逐字节一致（探活默认 off；Pod 写穿自动启用但语义是"更好"——重启后 Pod 立即可见）
- **行为变化知悉**：Pod 状态现在写穿 etcd——重启后展示的是**持久化快照**（最后一次上报），而非空列表；边缘重连后仍按上报周期翻新（一致收敛）
- 混跑禁令延续（全停再全起）；回滚 v0.9.0 → v0.8.0 需清理 `/edgeflow/podstatus/` 前缀（旧版不读写，残留无害，可选 `etcdctl del /edgeflow/podstatus --prefix`）
- 私有 registry 探活：建议同时配置 `REGISTRY_TOKEN`（401 时 fail/warn 均如实报告）；Docker Hub 无需配置（自动换取）
- 边缘节点零动作（云边协议无变化）

## 三、验证摘要（实测）

- 全量 `go test -race ./...` 35 包全绿；go vet 干净
- 新增测试：`TestEtcdPodStoreWriteThrough`（写穿/故障注入内存不动/删除级联）、`TestEtcdPodStoreKeyValidation`、`TestEtcdPodStoreLoad`（重建+坏键跳过）、`TestParseMirror`（7 用例）、`TestCheckMirrorPrivate`（200/404/token 携带）、`TestCheckMirrorDockerHubFlow`（完整 token 流程）、`TestCheckMirrorTimeout`、`TestParseMirrorCheckMode`
- E2E 实测：Pod 上报 3 条 → **重启后立即可见**（写穿）；发布 succeeded（R-1 off 零影响）；R-1 fail 模式 422 阻断
- 12 制品交叉编译 + tgz + sbom + checksums 构建通过

## 四、文档同步

KNOWN-ISSUES（§3③ 闭环 + §9 登记）、API-SPEC（状态行/创建发布探活说明）、DEPLOYMENT（env 表 +3）、README（当前版本/版本历史）、用户手册 v0.9.0（附录 A env +3/附录 E ③ 更新）、方案手册 v1.3.0（修订记录/1.5 节 Pod 持久化）

## 五、遗留（非阻断，见 KNOWN-ISSUES §9）

- **设备 reported（properties/LastReportedAt）持久化未做**（与 Pod 原同族，另行延后）
- 镜像 digest 级校验未做（探活是存在性检查）；hb 键重建计数未做；CN→角色映射透传项未做（etcd 侧配置覆盖）
- D6 批内并发（P2，改变灰度语义，延后）；训练平台/模型评测/A-B 切流（范围外延续）
