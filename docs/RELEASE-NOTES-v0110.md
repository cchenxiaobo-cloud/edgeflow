# EdgeFlow v0.11.0 发布说明（发布镜像可信化 + 可观测性补全 + 发布矩阵扩展）

- **发布日期**：2026-08-26
- **版本基线**：v0.10.0（2026-08-26）→ v0.11.0
- **主题**：R-1+ 镜像 digest 级校验（发布镜像可信化）+ L12+ hb 键重建计数（可观测性补全）+ L20b+ Windows 制品入发布矩阵（12→18）
- **兼容性**：API 只增不改；**本轮零新增 env**（digest 校验复用 `RELEASE_MIRROR_CHECK`）；升级零迁移；老边缘零动作

## 一、核心能力

### 1. 镜像 digest 级校验（R-1+，发布镜像可信化）

解决 R-1 存在性检查的盲区：**探活与边缘拉取之间的 TOCTOU**——HEAD 时 tag 指向 manifest A，边缘拉取时 tag 已被重指为 B。存在性检查（200=存在）无法发现，digest 比对可发现。

| 项 | 说明 |
|---|---|
| 探活固化 | `CheckMirror` 返回 manifest digest（HEAD 响应的 `Docker-Content-Digest`）；发布创建时固化至发布头 `mirrorDigest`（off 模式/HEAD 缺头/warn 失败 → 空，全链路跳过） |
| 边缘上报 | PodStatus 新增可选字段 `imageDigest`（云端/wire/边缘三端 DTO；老边缘字段缺失兼容——**不误伤不阻塞**） |
| 比对三接入点 | ① 部署即时检查（deployBatchNode 成功路径）；② 推进期复查（advance 每轮对 deployed 复核）；③ 终态复核（finish 前置全量复核） |
| 不一致语义 | mismatch 与部署失败**同权**：perNode failed（reason=`digest-mismatch: expected <exp> got <got>`）+ failFast 语义不变（failFast 下中止、剩余 skipped） |
| 三跳过 | expected 空（off/缺头）/ 边缘 digest 空（老边缘）/ DigestLookup nil（控制器未注入=回归锚点）任一成立即跳过比对 |
| 终态稳定 | 终态后晚到 mismatch 不回写（审计稳定）；运维经 `GET release.mirrorDigest` vs `GET pods.imageDigest` 对比发现，处置 = 人工回滚或重发 |

**行为变化面（仅开启 digest 校验时）**：`MIRROR_CHECK=fail|warn` 且 registry 返回 digest 的发布，终态判定新增"节点上报 digest 与发布时固化值一致"约束——不一致发布 failed（此前 succeeded）。这是有意的语义收紧。

### 2. hb 键重建计数（L12+，可观测性补全）

- 续约队列改 `renewRequest{nodeID, repair}`；三处修复性入口（applyDelete locallyServing / rescanOnce / gcSweepOne 守卫 0）经 `enqueueRepairRenew` 标记
- **grant 成功才计数**（重试成功只计一次；正常心跳不计）——`HBRebuildsCount`
- `/metrics` 第 8 项 `edgeflow_cloudcore_lease_hb_rebuilds_total`（仅外部模式注入，**0 值也输出**便于面板基线）；embed/纯内存不输出（7 项保持）

### 3. Windows 制品入发布矩阵（L20b+，12→18）

- `captureStderrFd` 平台分文件（certs_stderr_unix_test.go 原实现 / certs_stderr_windows_test.go SetOutput 捕获 + `pkg/log.Output` 访问器）——`GOOS=windows go vet ./pkg/certs/` 通过
- Makefile `CROSS_PLATFORMS` 3 组件 × 6 平台（+windows/amd64 +windows/arm64 +keadm）——cross-build 实测 18 制品（PE 格式）
- edgecore-windows 仅验证编译与构建，不承诺运行语义（主要部署面仍 linux/arm64）

### 4. ValidateMirror scheme 对齐（E2E 发现项）

- 镜像 ref 校验允许显式 scheme（`http://`/`https://`，内网明文 registry 场景）——与探活层 parseMirror 对齐，消除"API 收下后探活才炸"的不一致（API 只增不改）

## 二、升级注意

- **零迁移**：无新键空间、无 DTO 破坏、**零新增 env**；不设新 env 与 v0.10.0 逐字节一致
- **老边缘零动作**：v0.9.0/v0.10.0 边缘不报 `imageDigest` → 比对跳过；真实 v0.11.0 edgecore 同样不填（无运行时采集）→ 对真实边缘等效 off，直到运行时采集接入（KNOWN-ISSUES §11）
- 回滚 v0.11.0 → v0.10.0：新字段被旧版忽略（JSON 宽容）；混跑禁令延续（全停再全起）
- **指标行数变化**：外部模式 /metrics 7→8 项；监控面板为外部模式新增第 8 项（可选）

## 三、验证摘要（实测）

- 全量 `go test -race ./...` 35 包全绿；go vet 干净（darwin + `GOOS=windows ./pkg/certs/`）
- 单测：digest 探活 5 用例 + digest 控制器 8 用例（三接入点/三跳过/failFast 交互）+ hb 计数 3 用例 + metrics 3 用例
- **E2E 五场景实测**（本地 registry mock + sim `-digest` flag）：
  - A（match）→ succeeded + `mirrorDigest` 固化 ✅
  - B（mismatch）→ failed + reason=`digest-mismatch: expected ... got ...` ✅
  - C（老边缘无 digest 上报）→ succeeded（跳过比对）✅
  - D（off 模式）→ `mirrorDigest` 空 + succeeded ✅
  - E（推进期复查 catch：批间边缘上报变 mismatch）→ failFast 中止 head failed ✅
- `make cross-build` 18 制品（含 PE 格式 Windows 二进制）；helm lint 0 failed

## 四、文档同步

KNOWN-ISSUES（§6 L12/§9 R-1/§10 L20b 闭环标注 + §11 登记）、API-SPEC（状态行/§7.2/§7.3 字段/§1.2 指标 8 项）、DEPLOYMENT（RELEASE_MIRROR_CHECK 语义更新）、MONITORING-ALERTING-v011（指标 8 项+告警建议）、README（当前版本/版本历史/制品 18）、Chart 0.11.0、用户手册 v0.11.0（tex/md/PDF）、方案手册 v1.5.0（md/latex/PDF）

## 五、遗留（非阻断，见 KNOWN-ISSUES §11）

- 真实 edgecore 无运行时镜像 digest 采集（对真实边缘等效 off，需容器运行时接入）
- 终态后晚到 mismatch 不回写（审计稳定，运维人工对比发现）
- edgecore-windows 仅编译验证不承诺运行语义；Windows 制品已入矩阵（18 口径）
