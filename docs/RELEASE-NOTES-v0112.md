# EdgeFlow v0.12.0 发布说明（digest 校验端到端落地：真实边缘采集闭环 + 发布复核可观测性）

- **发布日期**：2026-08-26
- **版本基线**：v0.11.0（2026-08-26）→ v0.12.0
- **主题**：R-1++ 真实 edgecore 双通道 digest 采集（KNOWN-ISSUES §11 残余①闭环）+ D-1 发布 digest 复核端点（残余②增强）+ F-1 finish③ 读库 shadow 自赋值修复（v0.11.0 latent bug，设计验证发现）
- **兼容性**：API 只增不改（端点 31→32）；**零新增 env**；升级零迁移；老边缘零动作

## 一、核心能力

### 1. 真实边缘 digest 采集闭环（R-1++）

v0.11.0 的 digest 校验对真实 edgecore 等效 off（BuildStatusPayload 不填 imageDigest）。v0.12.0 把采集闭环落地：

| 通道 | 数据源 | 语义 |
|------|--------|------|
| ① 声明式 | Desired Pod 镜像 ref 含 `@sha256:`（`digestOfImageRef` 解析，取末 @ 后 `^sha256:[0-9a-f]{64}$` + 小写归一） | pin 引用保证运行镜像与 digest 一致（docker pin 语义即防漂移），**零运行时依赖** |
| ② 运行时 | `ContainerRuntime.ImageDigest`：docker inspect `{{.Config.Image}}` → `image inspect {{json .RepoDigests}}` 首个 sha256 条目（与 registry HEAD 的 Docker-Content-Digest 同源） | 拉取后真实 digest，覆盖 tag 引用形态 |

合并规则：**声明式优先、运行时兜底、仅 `StateRunning` 上报、失败降级空串不阻塞**。采集在上报路径（30s 周期天然限频，每 Pod 最多 2 次 docker exec），无缓存、零新增 env。MockRuntime 提供 `SetImageDigest` 注入点使运行时通道可单测。

**行为变化面（有意的语义落地）**：真实 edgecore + docker 且 `MIRROR_CHECK=fail|warn` 且发布头有 digest 时，digest 校验对真实边缘**从等效 off 变为生效**——Desired Pod 镜像含 `@sha256:` 或边缘已拉取 tag 镜像时上报 digest，与发布 mirrorDigest 不一致 → perNode failed（reason=digest-mismatch）→ 发布 failed。

### 2. 发布 digest 复核端点（D-1）

新增 `GET /api/v1/models/{modelName}/releases/{releaseID}/digest`（只增不改）：

- 响应：`mirrorDigest` + 逐节点 `currentImageDigest`/`releaseStatus`/`consistency` + head 聚合
- `consistency` 取值：`skipped`=发布级未启用（mirrorDigest 空）/ `consistent`=两边非空且相等 / `inconsistent`=两边非空且不等 / `unknown`=节点侧缺失
- **任意状态可查**（非终态 = 进行中视图）；发布不存在 → 404
- `nodeDigestOf` 提升为 `podstatus.NodeDigestOf`：控制器 DigestLookup 与复核端点**共用同一闭包实例**（main.go / etcd.go 两处装配 + modelAPI 注入），口径一致
- 目的：把「终态后晚到 mismatch 人工双端点对比」变成 API 一键复核（处置仍为人工回滚/重发，审计稳定不回写不变）

### 3. finish③ 读库 shadow 自赋值修复（F-1）

设计验证实测发现 v0.11.0 latent bug：`finish()` 终态 digest 复核后 `results = results` 为 shadow 自赋值（外层 results 未刷新），终态判定用陈旧快照——若 ③ 是**首个** catch mismatch 的接入点（推进期②未 catch、部署即时检查①通过后才漂移），head 会错误置 succeeded（perNode 已 failed，状态分裂）。修复为 `results = latest` + 专属单测 `TestReleaseDigestMismatchCaughtOnlyAtFinish`；`go vet` 的 self-assignment 告警同步消除。

## 二、升级注意

- **零迁移**：API 只增不改（新端点纯新增）、**零新增 env**、无新键空间、无 DTO 破坏
- **老边缘零动作**：v0.9.0-v0.11.0 边缘不报 imageDigest → 跳过比对（不变）
- **行为变化面（仅开启 digest 校验时）**：真实 edgecore + docker 时 digest 校验端到端生效（见上）；这是 v0.12.0 核心交付，非回归
- 回滚 v0.12.0 → v0.11.0：新端点消失、采集行为消失，老数据兼容（JSON 宽容）；混跑禁令延续（全停再全起）

## 三、验证摘要（实测）

- 全量 `go test -race ./...` 35 包全绿；`go vet` 干净（含 F-1 修复消除的 self-assignment 告警）
- 单测：`digestOfImageRef` 13 用例 + `firstSha256RepoDigest` 6 用例 + `DockerImageDigest` 5 场景 + `BuildStatusPayload` 双通道 3 场景 + `TestReleaseDigestMismatchCaughtOnlyAtFinish` + `NodeDigestOf` 4 场景 + 复核端点 handler 7 场景 + 路由契约 17→18 + 文档契约 31→32
- **E2E Tier1（sim 云端侧 + 复核端点）五场景实测全过**：
  - A（match）→ succeeded + mirrorDigest 固化 + 复核 **consistent** + 节点 deployed ✅
  - B（mismatch）→ failed + reason=digest-mismatch + 复核 **inconsistent** + 节点 failed ✅
  - C（老边缘无 digest）→ succeeded + 复核 **unknown**（mirrorDigest 非空、节点侧空——晚到 mismatch 经复核可见）✅
  - D（off 模式）→ mirrorDigest 空 + succeeded + 复核 **skipped** ✅
  - E（非终态 running 中）→ 复核端点 200 + status=running（进行中视图）✅
- **Tier2（真实 edgecore + docker）环境缺口**：本机 Docker daemon 不可用 → R1-R3 物理拉取场景未本机执行；`DockerRuntime.ImageDigest` 查询逻辑已用 fakeRunner 单测 5 场景全覆盖（容器不存在/正常查询/无 RepoDigests/镜像不存在/daemon 不可用）

## 四、文档同步

KNOWN-ISSUES（§11 残余①②闭环标注 + §12 登记）、API-SPEC（§1.1 端点总览 17→18 + §7 复核端点小节）、API-COMPATIBILITY（§1 端点矩阵 31→32）、DEPLOYMENT（env 语义 + §12 复核端点示例）、MONITORING-ALERTING-v011（§7 digest-mismatch 处置指引）、README（当前版本/版本历史）、Chart 0.12.0、用户手册 v0.12.0（tex/md/PDF）、方案手册 v1.6.0（md/latex/PDF）

## 五、遗留（非阻断，见 KNOWN-ISSUES §12）

- Tier2 真实 docker 拉取场景待有 daemon 环境补测（查询逻辑已单测覆盖）；pin 引用依赖发布方写 `@sha256:` 或运行时镜像有 RepoDigests（本地构建镜像无 RepoDigests → 空 → 跳过）
- 终态后晚到 mismatch 仍不回写（审计稳定设计取舍；复核端点给出当前快照，运维经端点发现）
